/*
Copyright (c) ILM.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package platform

// migration_inputs.go pins the SPEC A MIGRATION WAS STARTED AGAINST, so the engine cannot be
// redirected mid-flight.
//
// WHY. Almost nothing about a running migration is read from status: the source topology, the
// virtual host it drains, the broker it polls and the objects the cleanup deletes are all
// RE-DERIVED from the current spec on every pass, against the recorded from/to versions. That
// is what makes the engine crash-safe — and what makes it steerable. Edit
// spec.messaging.virtualHost, flip spec.messaging.mode, or change the provisioning mode while
// the migration is between phases and the next pass addresses a different broker, computes
// different object identities, or makes the "source" and "target" sets overlap. The migration
// then completes and reports success while the topology it was supposed to move off is left
// behind, unreferenced and full.
//
// WHAT IS PINNED. A FINGERPRINT — a hash, never the values — of the inputs those derivations
// read: the messaging mode and broker type, the effective source and target virtual hosts, the
// provisioning mode and the resolved messaging wiring (which Secret holds the administrator
// the drain authenticates as, under which keys, and which broker Service it reaches). Hashing
// is not obfuscation for its own sake: status is published, and a virtual host is a broker
// coordinate the engine may never write there.
//
// WHAT IS DELIBERATELY NOT PINNED, because these are the controls a stuck migration is steered
// with: spec.version (reverting it is how a reversible migration is aborted) and
// spec.messaging.managed.forceCutoverForVersion (the destructive escape hatch, which is
// USELESS if it cannot be set after the migration has blocked).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/pkg/bom"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

// guardMigrationInputs refuses to advance a migration whose migration-critical spec inputs no
// longer match the ones it started against. handled=true stops the pass.
//
// It also ADOPTS an unfingerprinted record — a migration recorded by an operator build (or a
// test fixture) that predates the fingerprint. Adoption records what the inputs are NOW and
// guards every pass from there, which is the most the engine can honestly claim about a record
// that never carried the value; it is not a way to clear a mismatch, because a mismatch means
// the field is set.
func (r *Reconciler) guardMigrationInputs(ctx context.Context, p *otilmv1alpha1.Platform) (bool, ctrl.Result, error) {
	u := p.Status.Upgrade
	fingerprint, ok := migrationInputFingerprint(p)
	if !ok {
		// A version this build does not carry: the phase's own source-bundle check reports
		// that in its own words rather than as drift.
		return false, ctrl.Result{}, nil
	}

	if u.InputsHash == "" {
		previous := u.InputsHash
		u.InputsHash = fingerprint
		if err := r.writeStatus(ctx, p); err != nil {
			u.InputsHash = previous
			res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, err)
			return true, res, aerr
		}
		return false, ctrl.Result{}, nil
	}

	if u.InputsHash == fingerprint {
		return false, ctrl.Result{}, nil
	}

	message := fmt.Sprintf(
		"the messaging migration from platform version %s to %s was started against a different messaging configuration; "+
			"spec.messaging.mode, spec.messaging.brokerType, spec.messaging.virtualHost, spec.provisioning.mode or the "+
			"messaging credentials wiring changed while it was in flight, and following that change could migrate the wrong "+
			"topology or leave the previous one behind — restore them to the values the migration started with, or revert "+
			"spec.version to %s to abort it while it is still fencing or draining",
		u.FromVersion, u.ToVersion, u.FromVersion)
	setMigrationCondition(p, metav1.ConditionFalse, reasonMigrationInputsChanged, message)
	res, err := r.steadyState(ctx, p, reasonMigrationInputsChanged, message)
	return true, res, err
}

// migrationInputFingerprint hashes the migration-relevant inputs of a recorded migration.
// ok=false means one of the two recorded versions is not carried by this build, so there is
// nothing meaningful to compare.
//
// The source-side facts are read from a DEEP COPY pinned to the migration's SOURCE version:
// the live spec.version is whatever the version resolution (and the engine's own re-pin) last
// put there, and a fingerprint that moved with it would compare nothing.
func migrationInputFingerprint(p *otilmv1alpha1.Platform) (string, bool) {
	u := p.Status.Upgrade
	if u == nil {
		return "", false
	}
	from, fromKnown := bom.BundleFor(u.FromVersion)
	to, toKnown := bom.BundleFor(u.ToVersion)
	if u.FromVersion == "" || u.ToVersion == "" || !fromKnown || !toKnown {
		return "", false
	}

	src := p.DeepCopy()
	src.Spec.Version = u.FromVersion
	conn := platformbuilder.ResolveMessagingConnection(src)

	// Every part is a NAME or a MODE, and the whole join is hashed before it is stored — the
	// virtual hosts in particular must never be recoverable from status.
	inputs := strings.Join([]string{
		"from=" + u.FromVersion,
		"to=" + u.ToVersion,
		"mode=" + p.Spec.Messaging.Mode,
		"broker=" + p.Spec.Messaging.BrokerType,
		"sourceVhost=" + platformbuilder.ManagedVirtualHostFor(src, from),
		"targetVhost=" + platformbuilder.ManagedVirtualHostFor(src, to),
		"provisioning=" + provisioningModeOf(p),
		"host=" + conn.Host,
		"port=" + fmt.Sprint(conn.Port),
		"credentialsSecret=" + conn.CredentialsSecretName,
		"administratorSecret=" + conn.AdministratorCredentialsSecretName,
		"usernameKey=" + conn.UsernameKey,
		"passwordKey=" + conn.PasswordKey,
	}, "\n")

	sum := sha256.Sum256([]byte(inputs))
	return hex.EncodeToString(sum[:]), true
}

// provisioningModeOf reads spec.provisioning.mode as written, rather than through
// ProvisioningDeploy: the latter also folds in the selected bundle's capability flag, which
// legitimately differs between the source and target versions and is not a spec change.
func provisioningModeOf(p *otilmv1alpha1.Platform) string {
	if p.Spec.Provisioning == nil {
		return ""
	}
	return p.Spec.Provisioning.Mode
}

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

// fence.go is the RENDER side of the messaging-migration fence. A migration stops the
// platform's message PRODUCERS by scaling them to zero; the render must then keep its hands
// off .spec.replicas for exactly those workloads, or the operator's next Server-Side Apply
// would re-assert the configured replica count and restart a producer the migration has
// deliberately stopped.
//
// MEMBERSHIP OF status.upgrade.fenced IS THE KEY — never the migration PHASE. A component
// omits replicas for as long as it appears in that list, and resumes sending them the moment
// the controller removes its entry (which the controller does only after it has patched the
// recorded count back). Keying the omission on a phase instead would decouple the two halves
// of the mechanism: a component would go on omitting replicas past the phase that restored
// it, or — worse — the render would re-assert the bundle's replica count on the very next
// apply while the engine still believed the producer was stopped, silently un-fencing it
// mid-migration.

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// FenceTarget names one workload the migration fence stops: the object name (which is also
// the component role) and the apps/v1 kind that component is rendered as. The controller
// records both on status.upgrade.fenced so every later patch — the per-reconcile re-assert
// and the restore — addresses the right object without re-deriving anything.
type FenceTarget struct {
	// Name is the workload's object name (the component's ResourceName).
	Name string
	// Kind is the effective apps/v1 workload kind: "Deployment" or "StatefulSet".
	Kind string
}

// MigrationFenceTargets returns the workloads a messaging migration must stop before the
// source virtual host can drain: the API GATEWAY (the door external traffic enters through),
// the SCHEDULER (timed jobs that publish on their own initiative) and the bundled
// PROVISIONING service (proxy provisioning traffic) — the platform's message PRODUCERS.
//
// CORE IS DELIBERATELY ABSENT. It is the CONSUMER that empties the queues the fence stops
// filling, so fencing it would stall the very drain the migration is waiting on.
//
// provisioning-rabbitmq is a target only when the operator actually renders it
// (provisioning.mode=deploy); an external provisioner is somebody else's workload and the
// operator cannot (and must not) scale it.
//
// The kind is taken from each component's effective workloadType, i.e. exactly what
// buildWorkload renders for it — the gateway in particular may legitimately be a StatefulSet.
func MigrationFenceTargets(p *otilmv1alpha1.Platform) []FenceTarget {
	targets := []FenceTarget{
		{Name: gatewayName, Kind: workloadKindName(p.Spec.Gateway.WorkloadType)},
		{Name: schedulerName, Kind: workloadKindName(p.Spec.Scheduler.WorkloadType)},
	}
	if ProvisioningDeploy(p) {
		targets = append(targets, FenceTarget{
			Name: provisioningName, Kind: workloadKindName(p.Spec.Provisioning.Deploy.WorkloadType),
		})
	}
	return targets
}

// workloadKindName maps a component's WorkloadType onto the apps/v1 Kind the render layer
// builds for it (see buildWorkload): a StatefulSet when explicitly requested, a Deployment
// for anything else — including the empty default.
func workloadKindName(k otilmv1alpha1.WorkloadKind) string {
	if k == otilmv1alpha1.WorkloadKindStatefulSet {
		return string(otilmv1alpha1.WorkloadKindStatefulSet)
	}
	return string(otilmv1alpha1.WorkloadKindDeployment)
}

// MigrationFenceActive reports whether the fence currently holds any workload down, i.e.
// whether status.upgrade.fenced lists anything. It is the cheap early-out for the
// per-reconcile re-assert: a platform with no migration in flight (and one whose fence has
// been fully lifted) does no work at all.
func MigrationFenceActive(p *otilmv1alpha1.Platform) bool {
	return p.Status.Upgrade != nil && len(p.Status.Upgrade.Fenced) > 0
}

// FenceOmitsReplicas reports whether the named component's workload must be rendered WITHOUT
// .spec.replicas because the migration fence holds it at zero.
//
// It answers from MEMBERSHIP of status.upgrade.fenced alone: true for exactly as long as the
// component has an entry there, false the instant that entry is removed. The component name
// is the workload's object name, which is what the controller records.
func FenceOmitsReplicas(p *otilmv1alpha1.Platform, component string) bool {
	if p.Status.Upgrade == nil {
		return false
	}
	for _, w := range p.Status.Upgrade.Fenced {
		if w.Name == component {
			return true
		}
	}
	return false
}

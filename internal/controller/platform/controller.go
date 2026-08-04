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

// Package platform implements the Kubernetes reconciler for Platform custom resources.
package platform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/internal/checksum"
	"github.com/OmniTrustILM/operator/internal/registration"
	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/OmniTrustILM/operator/pkg/capabilities"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// edgeRequeueAfter is how long the reconciler waits before re-checking an edge
// dependency (cert-manager / Gateway API CRDs) that is not yet served. The manager's
// dynamic RESTMapper re-discovers on a miss, so once the upstream operator is
// installed the next reconcile picks it up — no operator restart needed.
const edgeRequeueAfter = 2 * time.Minute

// adminRegisterRequeueAfter is how long the reconciler waits before retrying the
// admin registration when Core is not yet Ready, the admin cert is not yet issued, or
// a registration POST failed transiently. Shorter than the edge requeue since Core
// readiness changes on the scale of a pod start, not a CRD install.
const adminRegisterRequeueAfter = 15 * time.Second

// requeueMissingSecret is how long the reconciler waits to re-check a missing
// referenced Secret. It is a safety-net only: the Secret watch (platformsForSecret)
// re-enqueues the Platform the instant the Secret is created, so this requeue rarely
// fires — it just bounds the worst case if the watch event were ever missed.
const requeueMissingSecret = 30 * time.Second

// progressingRequeueAfter is the backstop re-check interval while the platform is
// Progressing (a required Deployment has not yet reached desired ready replicas). The
// Owns(&Deployment{}) watch already re-triggers reconcile on a Deployment status
// change, so readiness re-evaluates the instant pods come up; this requeue only bounds
// the worst case if that watch event were ever missed (no busy-loop).
const progressingRequeueAfter = 30 * time.Second

// statusConflictRequeueAfter is the short delay before re-applying status after a benign
// optimistic-lock conflict on the Platform status write (a competing reconcile won the race
// during bring-up). Kept short so the recomputed status lands promptly once the cache catches
// up; the conflict is rare, so this never busy-loops.
const statusConflictRequeueAfter = 1 * time.Second

// platformFinalizer is added to every Platform before the reconciler does any work, so
// deletion runs the operator's deletion handler (honoring spec.deletionPolicy) before
// the object is removed. It is namespaced under the API group.
const platformFinalizer = "platform.otilm.com/finalizer"

// Lifecycle status condition types and reasons. Available reflects measured readiness
// of the required Deployments (gated on a functional auth provider per the design);
// Progressing reflects an in-progress rollout. Reasons carry no identity material.
const (
	// conditionAvailable reports whether the platform's required Deployments (Core and
	// the auth provider) are measured ready.
	conditionAvailable = "Available"
	// conditionProgressing reports whether a required Deployment is still rolling out.
	conditionProgressing = "Progressing"
	// conditionDegraded reports a deterministic, won't-proceed failure (a rejected version,
	// the singleton loser, a render/apply error). It is set True by setDegradedMessage and
	// flipped False again by clearStaleDegraded once a reconcile pass succeeds.
	conditionDegraded = "Degraded"
	// reasonReconciling: children applied, a required Deployment not yet at desired
	// ready replicas.
	reasonReconciling = "Reconciling"
	// reasonAllReady: every required Deployment is measured ready.
	reasonAllReady = "AllComponentsReady"
	// reasonReconciled: a reconcile pass completed, so a previously-recorded Degraded
	// condition no longer holds.
	reasonReconciled = "Reconciled"
)

// Core workload coordinates the reconciler uses to gate readiness. They mirror the
// builder's Core component (clean Service name "core").
const (
	// coreDeploymentName is Core's Deployment/Service name (the per-namespace singleton
	// uses the unscoped component name).
	coreDeploymentName = "core"
	// authDeploymentName is the auth Deployment name (the auth provider).
	// Core's Available readiness is gated on it per the design (a functional auth
	// provider). It mirrors the builder's clean, unscoped component name.
	authDeploymentName = "auth"
	// gatewayWorkloadName is the api-gateway's workload/Service name — the door external
	// traffic enters through, which the messaging migration fences and reopens by name.
	gatewayWorkloadName = "api-gateway"
	// provisioningWorkloadName is the bundled provisioning service's workload/Service name.
	// The staged cutover addresses it by name because Core's provision-instance-queue init
	// container cannot complete until it answers.
	provisioningWorkloadName = "provisioning-rabbitmq"
)

// The first-admin CERTIFICATE registration is performed IN-POD by Core's postStart hook
// (register-admin.sh) — Core's local-admin API is localhost-only — so it is fire-and-forget
// and carries NO reconcile action or status condition here (unlike the password admin method,
// which the operator drives via the Keycloak admin API and reports on AdminUserReady).

// OIDC-provider status condition type and reasons. Like AdminUserReady, OIDCConfigured is
// an ADJUNCT signal: it reports the managed-Keycloak OIDC wiring without ever flipping the
// Platform to Degraded. It is set only for a managed Keycloak. Reasons carry no identity
// material.
const (
	// conditionOIDCConfigured reports whether the operator has WIRED OIDC for the managed
	// Keycloak: the Keycloak-generated "ilm" client secret has been fetched and relayed into the
	// operator-owned <platform>-oidc-client Secret, which Core reads IN-POD to self-register its
	// internal OIDC provider via the lifecycle.postStart hook (register-internal-keycloak.sh).
	// The operator does NOT PUT Core (its settings API is localhost-only).
	conditionOIDCConfigured = "OIDCConfigured"
	// reasonWaitingForKeycloakOIDC: the managed Keycloak (or its generated admin credentials)
	// is not yet Ready, so the OIDC wiring is deferred. A non-fatal, self-healing waiting state
	// (requeue).
	reasonWaitingForKeycloakOIDC = "WaitingForKeycloak"
	// reasonOIDCConfigured: the operator wired OIDC — the Keycloak client secret was fetched
	// and relayed into the operator-owned Secret Core reads in-pod to self-register the provider.
	reasonOIDCConfigured = "Configured"
	// reasonOIDCConfigFailed: a transient/unexpected OIDC-wiring failure (the Keycloak admin-API
	// client-secret fetch, or the client-Secret relay write); the reconcile requeues. The
	// message carries only an HTTP status code (fetch) or a generic phrase (relay).
	reasonOIDCConfigFailed = "OIDCConfigFailed"
)

// capabilityDetector is the subset of *capabilities.Detector the reconciler uses;
// tests inject a fake implementation backed by a stub RESTMapper.
type capabilityDetector interface {
	// Available reports whether the cluster serves the given GroupKind at some version.
	Available(gk schema.GroupKind, versions ...string) (bool, error)
}

// Reconciler reconciles a Platform object.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Capabilities detects whether the cluster serves a given upstream CRD/operator
	// (cert-manager, Gateway API). When nil, SetupWithManager populates it from the
	// manager's RESTMapper; tests may set a fake before reconciling.
	Capabilities capabilityDetector
	// OIDCRegistrar wires Core's internal OIDC provider to the managed Keycloak: it reads
	// the Keycloak-generated client secret from Keycloak's admin API and relays it into an
	// operator-owned Secret that Core reads in-pod to self-register its internal OIDC provider
	// (the operator never PUTs Core — its settings API is localhost-only).
	// When nil, SetupWithManager defaults it to the real HTTP client; tests inject a fake to
	// exercise the gating/idempotency without a live Keycloak or Core.
	OIDCRegistrar registration.OIDCRegistrar
	// Recorder emits Kubernetes Events on lifecycle transitions (Degraded, gated
	// waiting, missing Secret, singleton loser, prune). It is wired in cmd/main.go;
	// the event helpers are nil-safe so unit tests that omit it do not panic.
	Recorder record.EventRecorder
	// BrokerAdmins builds the RabbitMQ management-API client a messaging migration polls the
	// source virtual host's queue depths with, from the managed broker's management endpoint
	// and the administrator credentials the reconciler reads by reference. When nil it
	// defaults to the real HTTP client; tests inject a factory returning a scripted broker so
	// the drain is exercised without one.
	BrokerAdmins brokerAdminFactory
}

// eventf records a namespaced Event on the Platform, formatting the message from args.
// It is nil-safe (a no-op when no Recorder is wired, e.g. in lightweight unit tests).
// SECURITY: callers pass only generic reasons/messages — never secret values or
// connection coordinates.
func (r *Reconciler) eventf(p *otilmv1alpha1.Platform, eventType, reason, messageFmt string, args ...interface{}) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(p, eventType, reason, messageFmt, args...)
}

// event records a namespaced Event on the Platform with a fixed message (nil-safe).
// eventType is a parameter so the helper mirrors eventf as a general Normal/Warning
// recorder, even though current callers all record Warnings.
//
//nolint:unparam // eventType is intentionally a parameter for a reusable event recorder
func (r *Reconciler) event(p *otilmv1alpha1.Platform, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(p, eventType, reason, message)
}

// The Platform reconciler render-applies its children via Server-Side Apply and, after
// a successful apply of the full desired set, prunes children that are no longer
// rendered (a disabled component/edge or a changed edge type/source). The prune lists
// and deletes ONLY the managed kinds below, scoped by the operator's label selector AND
// a controller-owner-reference check — so `delete` is granted exactly on the kinds the
// prune removes (Deployments/StatefulSets/Services/ServiceAccounts/ConfigMaps/Secrets/
// Ingresses/NetworkPolicies/PodDisruptionBudgets/HorizontalPodAutoscalers and the
// unstructured cert-manager / Gateway API objects) and nowhere else (least privilege). It
// never deletes cluster-scoped or arbitrary kinds.
// +kubebuilder:rbac:groups=otilm.com,resources=platforms,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=otilm.com,resources=platforms/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=otilm.com,resources=platforms/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=issuers;certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters;poolers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rabbitmq.com,resources=rabbitmqclusters;vhosts;users;permissions;exchanges;queues;bindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.keycloak.org,resources=keycloaks;keycloakrealmimports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// fieldManager is the stable Server-Side Apply field owner the reconciler uses for
// every object it render-applies. A constant owner lets the apiserver track exactly
// the fields the operator manages, so API-defaulted fields it never sends don't churn.
const fieldManager = "ilm-operator"

// Reconcile composes the auth DB Secret, then server-side-applies every
// object RenderPlatform produces (Core plus the stateless components and their
// Services/ServiceAccounts/ConfigMaps), and finally records status.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var platform otilmv1alpha1.Platform
	if err := r.Get(ctx, req.NamespacedName, &platform); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Finalizer + deletion handling FIRST — and BEFORE any spec defaulting: the
	// finalizer-add path persists the whole object (r.Update), so any in-memory
	// defaulting done earlier would leak into the stored spec (this exact bug shipped:
	// the defaulted registry/repository were persisted on every first reconcile).
	if done, err := r.handleFinalizer(ctx, &platform); done || err != nil {
		return ctrl.Result{}, err
	}

	// Default the effective shared image REGISTRY on this fetched copy (never
	// persisted — the finalizer Update above already ran on the pristine object).
	// Repository defaulting is lazy inside ResolveImage (bundle-aware).
	platformbuilder.DefaultImageRegistry(&platform)

	// Singleton-per-namespace guard: only the oldest Platform in a namespace is
	// the active one. A newer Platform goes Degraded immediately. An admission
	// webhook will enforce this at create-time in a later milestone.
	if handled, res, err := r.checkSingletonGuard(ctx, &platform); handled || err != nil {
		return res, err
	}

	// PIN-ON-CREATE + DOWNGRADE GUARD + bundle resolution. resolvePlatformVersion makes an
	// empty spec.version follow the pinned status.observedVersion, refuses an explicit
	// downgrade, an unsupported version, and an upgrade onto an unreleased preview bundle
	// (each a terminal steady state), pins the resolved version onto the in-memory Platform,
	// and returns the version bundle. A handled=true result means a guard short-circuited
	// the reconcile.
	resolvedVersion, bundle, handled, res, err := r.resolvePlatformVersion(ctx, &platform)
	if handled || err != nil {
		return res, err
	}

	// MESSAGING-MIGRATION GATE. It sits HERE — immediately after version resolution and
	// BEFORE any gate that applies something belonging to the requested version — because a
	// version bump that RENAMES the messaging virtual host must be sequenced (fence the
	// producers, drain the source vhost, cut over, reclaim) rather than applied: rendering the
	// target topology beside a source vhost that still holds messages is exactly the outcome
	// the engine exists to prevent. It returns the version and bundle the REST of this
	// reconcile must use — the requested ones when no migration is in the way, the SOURCE ones
	// while a migration holds the platform back (it re-pins the in-memory spec.version to
	// match, so the builders resolve the same bundle). A handled=true result means the gate
	// short-circuited the reconcile.
	mig, handled, res, err := r.gateMessagingMigration(ctx, &platform, bundle, resolvedVersion)
	if handled || err != nil {
		return res, err
	}
	bundle = mig.bundle

	// Desired set: the keys of every object applied this reconcile, used by the
	// post-apply prune to garbage-collect de-rendered children. Populated as objects
	// are applied (Reconciler.apply / the composed-Secret reconcilers add their keys).
	desired := newDesiredSet()

	// Managed infrastructure (CloudNativePG / RabbitMQ / Keycloak operators): apply each
	// managed CR and reflect its provisioning state on its adjunct condition. A rejected
	// override / malformed realm JSON is the one hard error (degrade); otherwise it returns
	// the per-dependency ready/requeue flags. dbReady gates the auth-DB composition below.
	mgd, handled, res, err := r.gateManagedDependencies(ctx, &platform, bundle, desired)
	if handled || err != nil {
		return res, err
	}

	// Preflight the required-for-startup credential Secrets. The DB and messaging
	// credentials Secrets are resolved mode-agnostically: the caller's Secret (external) or
	// the CNPG-generated <cluster>-app / Topology-generated Core-user Secret (managed). For
	// a managed dependency that is not yet ready, gating has already requested a requeue, so
	// the generated Secret is preflighted only once that dependency is ready (avoiding a
	// spurious MissingSecret while the upstream operator bootstraps). Other referenced
	// Secrets (CA bundle, admin cert, API key) are read with tolerance at their own steps. A
	// change to ANY referenced Secret re-enqueues via the watch.
	if handled, res, err := r.preflightCredentialSecrets(ctx, &platform, mgd.dbReady, mgd.mqReady); handled || err != nil {
		return res, err
	}

	// Compose the auth/trusted-certs Secrets, then render and server-side-apply every base
	// object the Platform owns (everything except the edge). A composed-Secret or apply
	// failure routes by transience (requeue vs degrade); a handled=true result short-circuits
	// the reconcile.
	if handled, res, err := r.composeAndApplyBase(ctx, &platform, mig, desired, mgd.dbReady); handled || err != nil {
		return res, err
	}

	// Messaging-migration fence: re-claim .spec.replicas=0 on every workload listed in
	// status.upgrade.fenced, under the fence's own field manager. It sits HERE — immediately
	// after this pass's Server-Side Apply of the base objects, in the SAME pass — because the
	// apply is the only actor that can un-fence a producer: it force-owns the fields it sends,
	// so a fenced workload's zero has to be re-written behind it on EVERY reconcile, for as
	// long as the workload appears in the list. Membership is the whole condition; a platform
	// with no migration in flight has an empty list and this is a no-op.
	if ferr := r.enforceMigrationFence(ctx, &platform); ferr != nil {
		return r.applyOrDegrade(ctx, &platform, reasonMigrationFenceError, ferr)
	}

	// Edge / admin-cert / ServiceMonitors: each gated on the upstream CRDs its configured mode
	// needs. A missing prerequisite is a non-fatal waiting state surfaced on its own condition;
	// the rest of the platform stays Available and the reconcile requeues to self-heal. Returns
	// the combined requeue flag; a gated apply failure short-circuits (handled=true).
	requeue, handled, res, err := r.gateEdgeAdminAndMonitors(ctx, &platform, desired)
	if handled || err != nil {
		return res, err
	}

	// OIDC wiring (managed Keycloak only): once the managed Keycloak CR is Ready, the operator
	// reads the Keycloak-generated "ilm" client secret from Keycloak's admin API and relays it
	// into an operator-owned Secret (<platform>-oidc-client) that Core reads IN-POD — Core then
	// self-registers its internal OIDC provider via the lifecycle.postStart hook
	// (register-internal-keycloak.sh), because Core's settings API is localhost-only and a
	// cross-pod PUT fails. Like the edge/admin-cert it is an ADJUNCT: a
	// not-yet-ready Keycloak or a transient fetch/relay failure is a non-fatal
	// OIDCConfigured=False + requeue, never a platform-wide Degraded. No secret/token ever
	// reaches logs/status — the fetched secret is written only into the relayed Secret.
	//
	// ORDERING (critical): this MUST run BEFORE pruneOrphans. The relayed Secret is an
	// operator-OWNED, label-matched child, so the prune reclaims it unless its key is in the
	// desired set. reconcileOIDCProvider is the ONLY producer of that key (it adds the Secret
	// to desired on EVERY managed path — the relay, and the preserve-on-flap/short-circuit
	// early returns). Running after the prune would let the prune delete the freshly-relayed
	// Secret on the very next reconcile while OIDCConfigured stayed True — wedging Core, which
	// sources $INTERNAL_OAUTH_SECRET from it. So it sits with the other owned-child composers
	// (reconcileAuthDBSecret / reconcileTrustedCerts), all of which precede the prune. The
	// KeycloakReady signal it gates on is already set above by gateKeycloak.
	oidcRequeue := r.reconcileOIDCProvider(ctx, &platform, desired)

	// Prune de-rendered children, register the password admin, measure readiness, persist
	// status, and pick the soonest applicable requeue. A prune/readiness/status failure routes
	// by transience; the adjunct requeue flags collected across this pass decide the cadence.
	return r.finalizeReconcile(ctx, &platform, desired, mig, requeue, oidcRequeue, mgd)
}

// finalizeReconcile completes a successful reconcile pass: it prunes de-rendered children,
// registers the optional password admin, MEASURES readiness from the required Deployments,
// persists status, and picks the soonest applicable requeue. A prune / readiness-check / status
// failure routes by transience (requeue vs degrade); a benign optimistic-lock status conflict
// requeues quietly. requeue/oidcRequeue, the managed-dependency flags carried in mgd, and the
// migration gate's own cadence carried in mig feed the final requeue cadence.
func (r *Reconciler) finalizeReconcile(ctx context.Context, platform *otilmv1alpha1.Platform, desired desiredSet, mig migrationRender, requeue, oidcRequeue bool, mgd managedDependencyState) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Prune de-rendered children: after a SUCCESSFUL apply of the full desired set,
	// garbage-collect managed children no longer desired (a disabled component/edge or
	// a changed edge type/source). Scoped strictly to the operator's label selector +
	// a controller-owner-reference check over the managed kinds. A prune failure is
	// transient (e.g. a conflicting delete): requeue with backoff rather than crash.
	if err := r.pruneOrphans(ctx, platform, desired); err != nil {
		// A prune delete can conflict transiently (a competing delete / a stale cache): requeue
		// with backoff WITHOUT degrading. applyOrDegrade keeps a deterministic failure degrading.
		return r.applyOrDegrade(ctx, platform, "PruneError", err)
	}

	// Admin registration (certificate method) is performed IN-POD by Core's postStart hook
	// (register-admin.sh, rendered into the core-scripts ConfigMap by withCoreInPodScripts):
	// Core's local-admin API (POST /api/v1/local/admins) is LOCALHOST-ONLY, so a cross-pod POST
	// from the operator is rejected — the operator therefore delegates it to the Core pod (the
	// same pattern as the OIDC provider registration). There is no reconcile action or status
	// condition for it here; it is fire-and-forget, and Core's config-checksum rolls Core to
	// re-run the hook when the script changes.

	// Password admin (optional, registerAdmin.password.enabled + managed Keycloak): once the
	// managed Keycloak is Ready the operator idempotently creates the admin REALM USER (the
	// superadmin attribute) via the Keycloak admin API, with a password sourced from the
	// caller-provided Secret. Like the OIDC wiring it is an ADJUNCT — a not-yet-ready
	// Keycloak / missing password Secret / transient failure is a non-fatal AdminUserReady=False +
	// requeue, never a platform-wide Degraded. The password never reaches logs/status/events. It
	// creates NO operator-owned children (it only calls Keycloak), so its placement relative to
	// the prune is immaterial. It gates on the same KeycloakReady signal gateKeycloak set above.
	adminUserRequeue := r.reconcileAdminKeycloakUser(ctx, platform)

	// Readiness is MEASURED, not asserted: evaluate the required Deployments (Core +
	// the auth provider) and set Available/Progressing + the phase from what the
	// cluster actually reports. A still-rolling-out required Deployment is Progressing
	// (not Available), with a backstop requeue layered onto the Deployment watch.
	ready, err := r.requiredDeploymentsReady(ctx, platform)
	if err != nil {
		// A non-NotFound API error reading a required Deployment's status is transient (the
		// NotFound case is handled as not-ready, not an error): requeue with backoff WITHOUT
		// degrading — a momentary read failure is not a degraded platform.
		return r.applyOrDegrade(ctx, platform, "ReadinessCheckError", err)
	}
	r.setReadinessStatus(platform, ready)
	platform.Status.ObservedGeneration = platform.Generation
	// Report the version the operator actually reconciled against (spec.version, or the
	// operator's default when unset — and, while a messaging migration holds the platform
	// back, the version it is still RUNNING rather than the one it is moving to). It is set
	// only on this success path; an unknown version returned early (steadyState) and left the
	// prior ObservedVersion untouched.
	platform.Status.ObservedVersion = mig.version
	if err := r.Status().Update(ctx, platform); err != nil {
		// A competing write updated the Platform between our cached read and this status write
		// (common during bring-up: watched child Secrets/Deployments from the upstream operators
		// fire overlapping reconciles). The status is recomputed every reconcile, so on a benign
		// optimistic-lock conflict requeue and re-apply on the next pass — quietly — rather than
		// surfacing a noisy "object has been modified" Reconciler error + stacktrace.
		if apierrors.IsConflict(err) {
			logger.V(1).Info("Platform status update conflicted; requeuing to re-apply")
			return ctrl.Result{RequeueAfter: statusConflictRequeueAfter}, nil
		}
		return ctrl.Result{}, err
	}
	logger.Info("reconciled platform", "name", platform.Name, "ready", ready)
	// Pick the soonest applicable requeue. The password-admin / OIDC adjuncts, a managed
	// database still provisioning, and an edge / admin-cert dependency each re-check on
	// their own timescale; while Progressing we also lay down a backstop requeue (the
	// Deployment watch is the primary trigger).
	return nextRequeueResult(requeueSignals{
		migration: mig.requeue,
		adminUser: adminUserRequeue,
		oidc:      oidcRequeue,
		database:  mgd.dbRequeue,
		messaging: mgd.mqRequeue,
		keycloak:  mgd.kcRequeue,
		ready:     ready,
		edge:      requeue,
	}), nil
}

// requeueSignals carries the per-adjunct requeue flags Reconcile collects across a pass, so
// nextRequeueResult can pick the soonest applicable requeue in one place.
type requeueSignals struct {
	migration bool
	adminUser bool
	oidc      bool
	database  bool
	messaging bool
	keycloak  bool
	ready     bool
	edge      bool
}

// nextRequeueResult picks the soonest applicable requeue for a successfully reconciled
// Platform: an in-flight messaging migration first, then admin-user, OIDC, managed database,
// managed messaging, managed Keycloak, still-progressing required Deployments, and an edge /
// admin-cert dependency. No applicable signal means a steady state (no requeue).
//
// The migration comes FIRST because it is the only signal whose cadence drives a sequence
// forward rather than re-checking a dependency: its phases advance on nothing but the next
// reconcile, and it is decided ahead of every other gate, so deferring to one of theirs would
// let an unrelated dependency set the pace of an upgrade.
func nextRequeueResult(s requeueSignals) ctrl.Result {
	switch {
	case s.migration:
		// A migration phase is waiting on something it must re-check itself (the producers
		// winding down, the source virtual host emptying): look again shortly.
		return ctrl.Result{RequeueAfter: migrationRequeueAfter}
	case s.adminUser:
		// Keycloak-not-ready / password-Secret-missing / transient realm-user failure: retry
		// soon (same adjunct timescale as admin registration / OIDC wiring).
		return ctrl.Result{RequeueAfter: adminRegisterRequeueAfter}
	case s.oidc:
		// Keycloak/Core-not-ready or a transient OIDC-wiring failure: retry soon (same
		// adjunct timescale as admin registration).
		return ctrl.Result{RequeueAfter: adminRegisterRequeueAfter}
	case s.database:
		// A managed database is applied but not yet Ready (CNPG still bootstrapping or its
		// app Secret not yet generated): re-check soon (backstop; the Cluster/Secret Watch
		// already re-triggers on their status/creation). Also covers the CNPG-CRD-absent
		// case so it converges once CloudNativePG is installed (no operator restart).
		return ctrl.Result{RequeueAfter: databaseRequeueAfter}
	case s.messaging:
		// A managed broker is applied but not yet Ready (RabbitMQ still bootstrapping or its
		// Topology-generated Core-user Secret not yet created): re-check soon (backstop; the
		// Secret Watch already re-triggers on the Secret's creation). Also covers the
		// rabbitmq.com-CRD-absent case so it converges once the operators are installed.
		return ctrl.Result{RequeueAfter: messagingRequeueAfter}
	case s.keycloak:
		// A managed Keycloak is applied but not yet Ready (the Keycloak CR still provisioning),
		// or its realm-import ConfigMap is not yet present: re-check soon. There is NO direct
		// Watch on the Keycloak types (it would break the cache when the CRD is absent), so
		// this backstop — together with the Secret watch on the shared DB-credentials Secret —
		// is how the managed Keycloak converges. Also covers the k8s.keycloak.org-CRD-absent
		// case so it converges once the Keycloak Operator is installed (no operator restart).
		return ctrl.Result{RequeueAfter: keycloakRequeueAfter}
	case !s.ready:
		// A required Deployment is still rolling out: re-check shortly (backstop; the
		// Owns(&Deployment{}) watch already re-triggers on status changes).
		return ctrl.Result{RequeueAfter: progressingRequeueAfter}
	case s.edge:
		// An edge / admin-cert dependency is not yet served; re-check shortly so it
		// converges once the upstream operator is installed (no operator restart needed).
		return ctrl.Result{RequeueAfter: edgeRequeueAfter}
	default:
		return ctrl.Result{}
	}
}

// checkSingletonGuard enforces the singleton-per-namespace rule: only the oldest Platform in
// a namespace is the active one. When an older sibling exists, the current (loser) Platform is
// driven to a terminal steady state (Degraded + Event, no error) and handled=true is returned
// so the caller stops the reconcile. A List error is returned as a genuine API failure.
func (r *Reconciler) checkSingletonGuard(ctx context.Context, platform *otilmv1alpha1.Platform) (handled bool, res ctrl.Result, err error) {
	var siblings otilmv1alpha1.PlatformList
	if lerr := r.List(ctx, &siblings, client.InNamespace(platform.Namespace)); lerr != nil {
		return true, ctrl.Result{}, lerr
	}
	for i := range siblings.Items {
		s := &siblings.Items[i]
		if s.UID == platform.UID {
			continue
		}
		older := s.CreationTimestamp.Before(&platform.CreationTimestamp) ||
			(s.CreationTimestamp.Equal(&platform.CreationTimestamp) && s.Name < platform.Name)
		if older {
			// Deterministic, won't-proceed steady state: the loser will never be the
			// active Platform unless the older one is deleted (which re-enqueues it).
			// Record the condition + Event and stop WITHOUT an error so the reconcile
			// does not back off forever or spam error logs. Name/reason only — no leakage.
			res, err = r.steadyState(ctx, platform, "AnotherPlatformExists",
				fmt.Sprintf("namespace already has Platform %q; one Platform per namespace is supported", s.Name))
			return true, res, err
		}
	}
	return false, ctrl.Result{}, nil
}

// resolvePlatformVersion implements the PIN-ON-CREATE + DOWNGRADE GUARD + PREVIEW-UPGRADE
// GUARD and resolves the version bundle for this reconcile. effectivePlatformVersion makes an
// empty spec.version FOLLOW the pinned status.observedVersion (not the operator's current
// default), so upgrading the operator — which changes the built-in DefaultVersion — never
// silently upgrades a running platform; an explicit spec.version is the only upgrade trigger.
//
// It refuses an explicit DOWNGRADE, an UNSUPPORTED version, and an UPGRADE onto an unreleased
// (preview) bundle (each a terminal steady state → handled=true), pins the resolved version onto
// the IN-MEMORY Platform so the version-specific builders resolve the SAME bundle the reconciler
// gated on, and returns that bundle.
func (r *Reconciler) resolvePlatformVersion(ctx context.Context, platform *otilmv1alpha1.Platform) (resolvedVersion string, bundle bom.Bundle, handled bool, res ctrl.Result, err error) {
	effectiveVersion := effectivePlatformVersion(platform)

	// Refuse an explicit DOWNGRADE (spec.version strictly older than the running
	// observedVersion): a stateful platform that already self-migrated its schema cannot be
	// rolled back safely. Terminal steady state (Degraded + actionable message, no apply) until
	// the user corrects spec.version. Compared on the ORIGINAL spec.version, before the
	// in-memory pin below.
	if platform.Spec.Version != "" && platform.Status.ObservedVersion != "" &&
		isPlatformDowngrade(platform.Spec.Version, platform.Status.ObservedVersion) {
		res, err = r.steadyState(ctx, platform, reasonDowngradeForbidden,
			fmt.Sprintf("platform downgrade to %q is not supported (running %q); set spec.version to %q or higher",
				platform.Spec.Version, platform.Status.ObservedVersion, platform.Status.ObservedVersion))
		return "", bundle, true, res, err
	}

	// Resolve the version bundle ONCE per reconcile from the effective version (empty →
	// the operator's default). An UNKNOWN version is a deterministic user mistake, NOT a
	// crash: like the singleton loser it is a terminal steady state (no error, no busy
	// requeue) until a spec edit re-enqueues — surfaced as Degraded with an actionable
	// message listing the versions THIS operator build carries. The supported set grows
	// over time, so this is a runtime check, not a frozen CEL enum on the CRD.
	resolvedBundle, versionOK := bom.BundleFor(effectiveVersion)
	if !versionOK {
		res, err = r.steadyState(ctx, platform, "UnsupportedVersion",
			fmt.Sprintf("platform version %q is not supported by this operator; supported versions: %s",
				effectiveVersion, strings.Join(bom.SupportedVersions(), ", ")))
		return "", bundle, true, res, err
	}
	// resolvedVersion is the concrete version the bundle represents — reported on
	// status.ObservedVersion on success, which is what PINS it for the next reconcile.
	resolvedVersion = effectiveVersion
	if resolvedVersion == "" {
		resolvedVersion = bom.DefaultVersion
	}

	// Preview bundles are for fresh installs and explicit testing only: refuse to
	// UPGRADE a live platform onto an unreleased bundle — the messaging migration
	// engine that makes such a move safe ships separately, and the release-day flip
	// (Released=true) is what opens the path. Fresh installs (no observed version)
	// may pin a preview explicitly, and a migration already in flight to this exact
	// version is let through rather than stranded (see previewUpgradeRefused).
	if previewUpgradeRefused(platform, resolvedBundle, resolvedVersion) {
		res, err = r.steadyState(ctx, platform, reasonPreviewVersionUpgradeBlocked,
			fmt.Sprintf("version %s is a preview (unreleased) bundle; upgrading a running platform onto it is not supported — released versions: %s; "+
				"keep spec.version at %q, or wait for %s to be released",
				resolvedVersion, strings.Join(bom.SupportedVersions(), ", "), platform.Status.ObservedVersion, resolvedVersion))
		return "", bundle, true, res, err
	}

	// Pin the resolved version onto the IN-MEMORY Platform so the version-specific builders
	// (RenderPlatformBase → resolveBundle, which reads spec.version) resolve the SAME bundle the
	// reconciler gated on. In-memory only — never persisted (no spec write occurs after this
	// point; the finalizer Update ran earlier and returned), so it stays GitOps-safe.
	platform.Spec.Version = resolvedVersion
	return resolvedVersion, resolvedBundle, false, ctrl.Result{}, nil
}

// preflightCredentialSecrets preflights the required-for-startup credential Secrets (the DB
// and messaging credentials Secrets, resolved mode-agnostically). For a managed dependency
// that is not yet ready, gating has already requested a requeue, so the generated Secret is
// preflighted only once that dependency is ready (avoiding a spurious MissingSecret while the
// upstream operator bootstraps). A missing Secret is a deterministic steady-state requeue
// (handled=true); a non-NotFound read error is transient (also handled=true). Name only —
// never the Secret content.
func (r *Reconciler) preflightCredentialSecrets(ctx context.Context, platform *otilmv1alpha1.Platform, dbReady, mqReady bool) (handled bool, res ctrl.Result, err error) {
	dbCredRef := platformbuilder.ResolveDatabaseConnection(platform).CredentialsSecretName
	mqCredRef := platformbuilder.ResolveMessagingConnection(platform).CredentialsSecretName
	var preflightRefs []string
	if mqReady {
		preflightRefs = append(preflightRefs, mqCredRef)
	}
	if dbReady {
		preflightRefs = append(preflightRefs, dbCredRef)
	}
	for _, ref := range preflightRefs {
		if ref == "" {
			continue
		}
		var s corev1.Secret
		if gerr := r.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: ref}, &s); gerr != nil {
			if apierrors.IsNotFound(gerr) {
				// Deterministic won't-proceed steady state: a referenced Secret is absent.
				// The Secret watch re-enqueues the Platform the moment the Secret appears,
				// so a fixed requeue is enough — returning an error here would only back
				// off forever and spam error logs. Name only — never the Secret content.
				res, err = r.steadyStateRequeue(ctx, platform, "MissingSecret",
					fmt.Sprintf("referenced Secret %q not found", ref), requeueMissingSecret)
				return true, res, err
			}
			// A non-NotFound API error IS transient (the NotFound case is the deterministic
			// steadyStateRequeue above): requeue with backoff WITHOUT degrading. Name only.
			res, err = r.transientRequeue(ctx, platform, "SecretReadError", fmt.Errorf("reading referenced Secret %q: %w", ref, err))
			return true, res, err
		}
	}
	return false, ctrl.Result{}, nil
}

// managedDependencyState carries the per-dependency ready/requeue flags the managed-infra
// gates produce, so Reconcile can route the credential preflight and the final requeue.
type managedDependencyState struct {
	dbReady   bool
	dbRequeue bool
	mqReady   bool
	mqRequeue bool
	kcRequeue bool
}

// gateManagedDependencies reconciles the managed-infrastructure dependencies (CloudNativePG
// database, RabbitMQ messaging, Keycloak) in order and reflects each one's provisioning state
// on its adjunct condition. Each gate runs BEFORE the credential preflight + auth-DB
// composition because, for a managed dependency, the credentials live in an
// upstream-operator-GENERATED Secret that only exists once the CRs applied here have
// bootstrapped. Every adjunct (DatabaseReady/MessagingReady/KeycloakReady) is non-blocking: a
// not-yet-ready dependency never blocks the platform's Available condition — Core/scheduler
// wait on the generated Secret via their secretKeyRef exactly as they wait on an external
// Secret. A rejected override / malformed realm JSON is the one hard error (degrade); handled
// is true when a gate short-circuited the reconcile. dbReady gates the managed Keycloak so its
// StatefulSet is not created until the shared database accepts connections.
func (r *Reconciler) gateManagedDependencies(ctx context.Context, platform *otilmv1alpha1.Platform, bundle bom.Bundle, desired desiredSet) (mgd managedDependencyState, handled bool, res ctrl.Result, err error) {
	dbReady, dbRequeue, derr := r.gateDatabase(ctx, platform, bundle, desired)
	if derr != nil {
		// Either a deterministic rejected-override (degrade) or a transient apply conflict
		// (requeue without degrading) — applyOrDegrade routes by transience. Names a field
		// path only — never creds/coordinates.
		res, err = r.applyOrDegrade(ctx, platform, "ManagedDatabaseError", derr)
		return mgd, true, res, err
	}

	mqReady, mqRequeue, merr := r.gateMessaging(ctx, platform, bundle, desired)
	if merr != nil {
		// Deterministic rejected-override degrades; a transient apply conflict requeues. Names
		// a field path only — never creds/coordinates.
		res, err = r.applyOrDegrade(ctx, platform, "ManagedMessagingError", merr)
		return mgd, true, res, err
	}

	_, kcRequeue, kerr := r.gateKeycloak(ctx, platform, bundle, desired, dbReady)
	if kerr != nil {
		// Deterministic rejected-override / malformed realm JSON degrades; a transient apply
		// conflict requeues. Names a field path only — never creds/coordinates.
		res, err = r.applyOrDegrade(ctx, platform, "ManagedKeycloakError", kerr)
		return mgd, true, res, err
	}

	return managedDependencyState{
		dbReady:   dbReady,
		dbRequeue: dbRequeue,
		mqReady:   mqReady,
		mqRequeue: mqRequeue,
		kcRequeue: kcRequeue,
	}, false, ctrl.Result{}, nil
}

// composeAndApplyBase composes the operator-managed auth-DB and trusted-certs Secrets, then
// renders and server-side-applies every base object the Platform owns (everything except the
// edge). RenderPlatformBase is the single source of truth for the rendered output; the
// reconciler stays generic across kinds and never re-templates per type. The edge is applied
// separately, after its upstream-CRD prerequisites are confirmed (see gateEdge). A
// composed-Secret write or an SSA apply that fails routes by transience (requeue vs degrade);
// handled is true when a step short-circuited the reconcile. dbReady gates the auth-DB
// composition (skipped while a managed database is not yet ready).
//
// mig carries the migration gate's answer: the bundle every builder here resolves against, and
// (during a staged messaging cutover) whether Core's workload must be WITHHELD from this pass's
// apply so Core keeps running the pod template it is already Ready on.
func (r *Reconciler) composeAndApplyBase(ctx context.Context, platform *otilmv1alpha1.Platform, mig migrationRender, desired desiredSet, dbReady bool) (handled bool, res ctrl.Result, err error) {
	bundle := mig.bundle
	// Compose the auth .NET DB connection string into an operator-managed Secret
	// before applying workloads, so auth's secretKeyRef resolves. Skip it while a
	// managed database is not yet ready (its generated credentials Secret does not exist
	// yet) — gateDatabase already requested a requeue, and auth waits on the
	// composed Secret via its secretKeyRef in the meantime (no Degraded). While skipping,
	// PRESERVE any already-composed auth-db Secret in the desired set so a transient
	// DatabaseReady flap (e.g. the CNPG Cluster briefly not-Ready) never prunes a healthy
	// composed Secret out from under auth.
	if dbReady {
		if aerr := r.reconcileAuthDBSecret(ctx, platform, bundle, desired); aerr != nil {
			// A composed-Secret write can hit a transient SSA/update conflict (requeue, no
			// degrade); a credentials-Secret read failure is otherwise deterministic. Never
			// embeds the composed value/credentials.
			res, err = r.applyOrDegrade(ctx, platform, "AuthDBSecretError", aerr)
			return true, res, err
		}
	} else {
		desired.add(r, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: bundle.Wiring.AuthDBSecretName, Namespace: platform.Namespace}})
	}

	// Compose the trusted-certificates bundle (user CA + generated admin CA) into an
	// operator-managed Secret when the admin bootstrap requires it, and obtain its
	// checksum so a bundle change rolls Core. When no composition is needed (no admin CA
	// to fold in) the
	// Secret is not rendered and the checksum is empty (Core references the user's
	// Secret verbatim, unchanged).
	trustedCertsChecksum, tcerr := r.reconcileTrustedCerts(ctx, platform, desired)
	if tcerr != nil {
		// A transient SSA conflict applying the composed bundle requeues without degrading; a
		// Secret-read failure is otherwise deterministic. Never embeds cert material.
		res, err = r.applyOrDegrade(ctx, platform, "TrustedCertsError", tcerr)
		return true, res, err
	}

	// Fold the relayed OIDC client Secret AND Core's in-pod scripts ConfigMap into Core's
	// config-checksum alongside the trusted-certs bundle, so Core ROLLS when any of the three
	// changes — all live OUTSIDE Core's pod template (sourced/mounted by reference). The OIDC roll
	// runs the postStart WITH the relayed Keycloak client secret to register the internal provider
	// (instead of crash-looping until a restart); the scripts roll re-runs the postStart with a
	// changed script (e.g. the /kc OIDC URLs), which a lazily-synced ConfigMap volume would
	// otherwise not pick up until an unrelated restart. Each input is empty when not applicable
	// (external/absent Keycloak, or before the Secret exists), so it never causes a spurious roll.
	coreChecksum := combinedCoreChecksum(
		trustedCertsChecksum,
		r.oidcClientChecksum(ctx, platform),
		coreScriptsChecksum(platform),
	)

	// The gateway (Kong) reads its declarative config (kong.yml) only at boot from the global
	// ConfigMap, so a routing change (e.g. the managed-Keycloak /kc route) must ROLL the gateway to
	// take effect — a lazily-synced ConfigMap volume alone would not. Compute a checksum of that
	// config and stamp it on the gateway workload below, exactly as Core's config-checksum rolls
	// Core.
	gatewayChecksum := gatewayConfigChecksum(platform)

	// Render every base object the Platform owns (everything except the edge) and
	// server-side-apply each one.
	// FREEZE Core's config-checksum while it brings up its FIRST pod (running its initial DB
	// migration). Core's checksum/config folds in two LATE-ARRIVING inputs — the generated admin
	// issuer CA (cert-manager issues it seconds into bring-up) and the relayed OIDC client Secret
	// (after the managed Keycloak is Ready). If either lands WHILE Core's first migration is
	// running, the checksum change rolls Core and kills the migrating pod mid-flight, corrupting
	// the schema (schema_history drifts ahead of the tables; the non-idempotent cross-service
	// migration side effects then re-fail) and wedging Core in CrashLoopBackOff. So until Core is
	// Ready we re-stamp the checksum it ALREADY carries (a no-op apply — no roll); once Core is
	// Ready (its migration is done) the real checksum applies and rolls Core SAFELY — the DB is
	// migrated, so a roll is a Flyway no-op. Core is still ALWAYS created (its admin-cert / OIDC
	// secretKeyRefs are optional, so it must converge even when those inputs never arrive); only
	// the config-roll is held back. The Helm chart never hits the mid-migration roll because Helm
	// deploys Core once with all config up front — this makes the operator match that.
	coreExists, coreReady, coreFrozenChecksum := r.coreWorkloadStatus(ctx, platform)
	for _, obj := range platformbuilder.RenderPlatformBase(platform) {
		// A staged messaging cutover WITHHOLDS Core's workload until the target topology is
		// declared and the provisioning service is answering again: Core's proxy-path init
		// container retries against that service until it responds, so rolling Core onto the
		// target bundle any earlier produces a pod that can never become Ready. Everything else
		// (including Core's own Service/ServiceAccount/ConfigMaps) is applied normally, and the
		// withheld workload stays in the desired set so the post-apply prune keeps it.
		if mig.holdCore && isCoreWorkload(obj) {
			desired.add(r, obj)
			continue
		}
		// Stamp the per-component config checksums onto their pod templates so a change in config
		// that lives OUTSIDE the pod template rolls that component: Core (trusted-certs bundle +
		// relayed OIDC client Secret + in-pod scripts ConfigMap) and the gateway (kong.yml). Each
		// stamp is a no-op for non-matching objects / an empty checksum.
		coreStamp := coreChecksum
		if isCoreWorkload(obj) && coreExists && !coreReady && coreFrozenChecksum != "" {
			coreStamp = coreFrozenChecksum // freeze during the first migration; unfreeze once Core is Ready
		}
		platformbuilder.StampConfigChecksum(obj, coreStamp)
		platformbuilder.StampGatewayConfigChecksum(obj, gatewayChecksum)
		if aerr := r.apply(ctx, platform, obj, desired); aerr != nil {
			// An SSA apply can fail transiently (a Conflict from a competing field manager, an
			// API timeout) — requeue with backoff WITHOUT degrading; only a deterministic apply
			// failure degrades. applyOrDegrade routes by transience.
			res, err = r.applyOrDegrade(ctx, platform, "ApplyError",
				fmt.Errorf("applying %T %q: %w", obj, obj.GetName(), aerr))
			return true, res, err
		}
	}
	return false, ctrl.Result{}, nil
}

// gateEdgeAdminAndMonitors runs the three CRD-gated adjuncts that follow the base apply — the
// edge (cert-manager for cert-managed TLS, Gateway API for type=gatewayAPI), the optional
// admin-cert Certificate (cert-manager), and the ServiceMonitors (the Prometheus operator
// CRD). Each missing prerequisite is a non-fatal waiting state surfaced on its own condition;
// the rest of the platform stays Available and the reconcile requeues to self-heal. It returns
// the combined requeue flag; a gated apply that fails transiently routes by transience
// (handled=true) rather than degrading the platform.
func (r *Reconciler) gateEdgeAdminAndMonitors(ctx context.Context, platform *otilmv1alpha1.Platform, desired desiredSet) (requeue bool, handled bool, res ctrl.Result, err error) {
	edgeRequeue, eerr := r.gateEdge(ctx, platform, desired)
	if eerr != nil {
		// A gated apply (Ingress / Gateway / cert-manager objects) can fail transiently; route
		// by transience so an SSA conflict requeues rather than degrading the platform.
		res, err = r.applyOrDegrade(ctx, platform, "ApplyError", eerr)
		return false, true, res, err
	}
	requeue = edgeRequeue

	// Admin bootstrap (optional, source=generated): the cert-manager admin Certificate
	// is gated on cert-manager exactly like the edge. source=provided needs no cert-manager.
	adminRequeue, aerr := r.gateAdminCert(ctx, platform, desired)
	if aerr != nil {
		res, err = r.applyOrDegrade(ctx, platform, "ApplyError", aerr)
		return false, true, res, err
	}
	requeue = requeue || adminRequeue

	// ServiceMonitors: gated on the Prometheus operator CRD (monitoring.coreos.com), exactly
	// like the edge. Inactive (no component opts in) when no ServiceMonitor is requested.
	smRequeue, serr := r.gateServiceMonitors(ctx, platform, desired)
	if serr != nil {
		res, err = r.applyOrDegrade(ctx, platform, "ApplyError", serr)
		return false, true, res, err
	}
	requeue = requeue || smRequeue

	return requeue, false, ctrl.Result{}, nil
}

// handleFinalizer ensures the Platform carries the operator's finalizer before any
// work is done, and on deletion runs the deletion handler then removes the finalizer.
// It returns done=true when the reconcile should stop after this step: either the
// object is being deleted (handler ran, finalizer removed) or the finalizer was just
// added (the Update re-enqueues a fresh reconcile). It returns an error only on a
// genuine API failure so controller-runtime backs off.
func (r *Reconciler) handleFinalizer(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	if p.DeletionTimestamp != nil {
		if !controllerutil.ContainsFinalizer(p, platformFinalizer) {
			return true, nil // already finalized; nothing to do
		}
		if err := r.handleDeletion(ctx, p); err != nil {
			return true, err // requeue to retry teardown; do NOT remove the finalizer yet
		}
		controllerutil.RemoveFinalizer(p, platformFinalizer)
		if err := r.Update(ctx, p); err != nil {
			return true, err
		}
		return true, nil
	}

	if !controllerutil.ContainsFinalizer(p, platformFinalizer) {
		controllerutil.AddFinalizer(p, platformFinalizer)
		if err := r.Update(ctx, p); err != nil {
			return true, err
		}
		// The Update bumps the resourceVersion and re-enqueues; stop this pass so the
		// next reconcile proceeds with the finalizer in place (and a fresh object).
		return true, nil
	}
	return false, nil
}

// handleDeletion runs the operator's deletion teardown for a Platform being deleted,
// honoring spec.deletionPolicy:
//
//   - The Platform's own same-namespace children (Deployments/Services/ServiceAccounts/
//     ConfigMaps/Secrets/Ingress) carry a controller owner reference and are reclaimed by
//     owner-reference garbage collection under BOTH policies.
//   - Managed (upstream-operator) infrastructure — the CloudNativePG database, RabbitMQ
//     broker, and Keycloak instance — IS torn down or retained here per deletionPolicy via
//     the three handleManaged*Deletion calls: Retain leaves the upstream-operator CRs +
//     their data intact; Delete reclaims them. These CRs carry no controller owner ref and
//     are prune-excluded, so this handler is the only thing that deletes them.
//
// VERSION: teardown renders against every bundle version this operator ships
// (bom.AllVersions()) PLUS a legacy-scope variant of the version the platform is ACTUALLY
// RUNNING (teardownPlatformVersion — status.observedVersion, else spec.version) — the
// deduplicated UNION of all of them, so a partially applied upgrade's new objects, a blocked
// upgrade's live topology, and a custom-vhost platform's pre-vhost-scoping (legacy-named)
// topology are never orphaned. Each render is pinned onto a DEEP COPY so the finalizer-removal
// Update that follows never persists a spec change. See teardownRenderPlatforms and
// teardownGate.
//
// On a teardown failure the handler returns the non-nil error so handleFinalizer keeps the
// finalizer (requeues) and the teardown is retried, rather than removing the finalizer and
// orphaning a half-deleted managed CR.
// SECURITY: the Event carries only the policy name — never a secret or coordinate.
func (r *Reconciler) handleDeletion(ctx context.Context, p *otilmv1alpha1.Platform) error {
	policy := p.Spec.DeletionPolicy
	if policy == "" {
		policy = otilmv1alpha1.PlatformDeletionPolicyRetain
	}

	// Managed infrastructure (CloudNativePG database + RabbitMQ broker + Keycloak today) is
	// reclaimed or retained per `policy`. The managed CRs carry NO controller owner reference
	// and are prune-excluded, so they are NOT garbage-collected with the Platform — this
	// handler is the ONLY thing that deletes them, and only under Delete. Each handler renders
	// its own teardown version set (see the VERSION note above), so the Platform is passed
	// through UNPINNED.
	if err := r.handleManagedDatabaseDeletion(ctx, p, policy); err != nil {
		return err // requeue to retry teardown; the finalizer stays until it succeeds
	}
	if err := r.handleManagedMessagingDeletion(ctx, p, policy); err != nil {
		return err // requeue to retry teardown; the finalizer stays until it succeeds
	}
	if err := r.handleManagedKeycloakDeletion(ctx, p, policy); err != nil {
		return err // requeue to retry teardown; the finalizer stays until it succeeds
	}

	// FUTURE: clean cluster-scoped artifacts (ValidatingWebhookConfiguration,
	// ClusterRoles) here, scoped by the operator's labels — NOT via owner-ref GC
	// (cross-namespace/cluster-scoped owner references are invalid). None exist yet.

	r.eventf(p, corev1.EventTypeNormal, "Deleting",
		"Platform deletion in progress (deletionPolicy=%s)", policy)
	log.FromContext(ctx).Info("handling Platform deletion", "name", p.Name, "deletionPolicy", policy)
	return nil
}

// handleManagedDatabaseDeletion enforces the deletion-safety contract for a managed
// database on Platform deletion:
//
//   - Retain (default) → leave the CloudNativePG Cluster (and Pooler) and their data
//     intact; record a Warning Event naming the retained database so the operator is
//     visible (NO connection coordinate in the message). The operator deletes nothing.
//   - Delete → delete the Cluster (CloudNativePG garbage-collects its PVCs) and the
//     Pooler, then proceed. A NotFound is ignored (already gone); any other delete error
//     is returned so the finalizer keeps the Platform and the teardown is retried.
//
// It is a no-op for an external database (the operator provisions nothing to tear down).
func (r *Reconciler) handleManagedDatabaseDeletion(ctx context.Context, p *otilmv1alpha1.Platform, policy otilmv1alpha1.PlatformDeletionPolicy) error {
	return r.handleManagedInfraDeletion(ctx, p, policy, r.teardownGate(p, r.databaseGate), "RetainedDatabase", "DeletedDatabase")
}

// handleManagedMessagingDeletion enforces the deletion-safety contract for a managed
// broker on Platform deletion, mirroring handleManagedDatabaseDeletion: Retain leaves the
// RabbitmqCluster + its topology (and data) intact with a Warning Event; Delete reclaims
// the cluster and every topology CR (the RabbitMQ Cluster Operator GCs the PVCs). It is a
// no-op for an external broker. See handleManagedInfraDeletion for the shared semantics.
func (r *Reconciler) handleManagedMessagingDeletion(ctx context.Context, p *otilmv1alpha1.Platform, policy otilmv1alpha1.PlatformDeletionPolicy) error {
	return r.handleManagedInfraDeletion(ctx, p, policy, r.teardownGate(p, r.messagingGate), "RetainedMessaging", "DeletedMessaging")
}

// handleManagedKeycloakDeletion enforces the deletion-safety contract for a managed Keycloak
// on Platform deletion, mirroring handleManagedDatabaseDeletion: Retain leaves the Keycloak
// CR + its realm import (and the realm's data in the shared database) intact with a Warning
// Event; Delete reclaims the Keycloak CR and the realm import (the Keycloak Operator tears
// down the Keycloak deployment). It is a no-op for an external Keycloak. The deletion gate
// carries the FULL rendered set (CR + import) so both are reclaimed/retained. See
// handleManagedInfraDeletion for the shared semantics.
func (r *Reconciler) handleManagedKeycloakDeletion(ctx context.Context, p *otilmv1alpha1.Platform, policy otilmv1alpha1.PlatformDeletionPolicy) error {
	return r.handleManagedInfraDeletion(ctx, p, policy, r.teardownGate(p, r.keycloakDeletionGate), "RetainedKeycloak", "DeletedKeycloak")
}

// requiredDeploymentsReady reports whether every workload the platform requires to be
// Available is measured ready. Per the design, Available is "gated on a functional auth
// provider", so the required set is Core plus auth. Each required component is
// checked through workloadReady, which handles BOTH the Deployment (default) and the
// StatefulSet kind — so a component whose spec.<component>.workloadType=StatefulSet is
// gated on its StatefulSet's readiness exactly as a Deployment-typed one is gated on its
// Deployment's. A NotFound of both kinds is "not ready yet" (the child has not been applied
// this reconcile cycle); any other Get error is returned so the caller can back off.
func (r *Reconciler) requiredDeploymentsReady(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	for _, name := range []string{coreDeploymentName, authDeploymentName} {
		ready, err := r.workloadReady(ctx, p.Namespace, name)
		if err != nil {
			return false, err
		}
		if !ready {
			return false, nil
		}
	}
	return true, nil
}

// workloadReady reports whether the named component's workload in ns is ready, handling
// either workload kind: a component is rendered as EITHER a Deployment (default) or a
// StatefulSet (workloadType=StatefulSet) under the same name, so this checks the Deployment
// first and falls back to the StatefulSet. A component that exists as neither kind is
// not-ready (nil error); any non-NotFound Get error is returned so the caller backs off.
// When a kind switch is mid-flight both kinds may briefly exist (the new one applied, the
// old one not yet pruned); readiness keys off the Deployment when present, which is the
// conservative choice (the platform stays Available iff the still-present Deployment is
// ready), and switches to the StatefulSet only once the Deployment is gone.
func (r *Reconciler) workloadReady(ctx context.Context, ns, name string) (bool, error) {
	var dep appsv1.Deployment
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &dep)
	if err == nil {
		return deploymentReady(&dep), nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	// No Deployment of this name: the component may be a StatefulSet instead.
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sts); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil // neither kind exists yet — not ready
		}
		return false, err
	}
	return statefulSetReady(&sts), nil
}

// deploymentReady reports whether a Deployment has reached its desired ready replicas. It
// treats AvailableReplicas >= desired as ready, and additionally honors a present
// Available=False condition as not-ready.
func deploymentReady(dep *appsv1.Deployment) bool {
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if dep.Status.AvailableReplicas < desired {
		return false
	}
	// AvailableReplicas met the target; respect an explicit Available=False if present.
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == corev1.ConditionTrue
		}
	}
	return true
}

// statefulSetReady reports whether a StatefulSet has reached its desired ready replicas:
// ReadyReplicas >= desired (.spec.replicas, defaulting to 1 when unset, e.g. under an HPA).
// This mirrors deploymentReady's AvailableReplicas check for the StatefulSet status, so a
// StatefulSet-typed component gates Available exactly as a Deployment-typed one does.
func statefulSetReady(sts *appsv1.StatefulSet) bool {
	desired := int32(1)
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}
	return sts.Status.ReadyReplicas >= desired
}

// setReadinessStatus sets the Available/Progressing conditions and the Phase from the
// MEASURED readiness of the required Deployments. When ready: Available=True,
// Progressing=False, Phase=Running. While a required Deployment is still rolling out:
// Available=False, Progressing=True, Phase=Progressing. It never sets Degraded TRUE — a
// fatal error path uses degraded()/setDegraded() instead — but it does CLEAR a stale
// Degraded=True (this pass reached the success path, so the recorded failure no longer
// holds). Messages are generic (no secret/coordinate leakage).
func (r *Reconciler) setReadinessStatus(p *otilmv1alpha1.Platform, ready bool) {
	// Reaching here means the reconcile SUCCEEDED, so any Degraded=True recorded by an
	// earlier pass (a refused version, the singleton loser, a render/apply failure) is
	// history — drop it, or the platform would advertise Degraded=True forever after the
	// user corrected the spec, since only Available/Progressing were being updated.
	clearStaleDegraded(p)
	if ready {
		p.Status.Phase = otilmv1alpha1.PlatformPhaseRunning
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
			Type: conditionAvailable, Status: metav1.ConditionTrue, Reason: reasonAllReady,
			Message: "all required components are ready", ObservedGeneration: p.Generation,
		})
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
			Type: conditionProgressing, Status: metav1.ConditionFalse, Reason: reasonAllReady,
			Message: "platform reconcile complete", ObservedGeneration: p.Generation,
		})
		return
	}
	p.Status.Phase = otilmv1alpha1.PlatformPhaseProgressing
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionAvailable, Status: metav1.ConditionFalse, Reason: reasonReconciling,
		Message: "waiting for required components to become ready", ObservedGeneration: p.Generation,
	})
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionProgressing, Status: metav1.ConditionTrue, Reason: reasonReconciling,
		Message: "required components are rolling out", ObservedGeneration: p.Generation,
	})
}

// clearStaleDegraded flips a leftover Degraded=True to False on a SUCCESSFUL reconcile pass,
// so a platform that was degraded by a deterministic, user-correctable condition (a refused
// version — PreviewVersionUpgradeBlocked / DowngradeForbidden / UnsupportedVersion — the
// singleton loser, a render error) stops advertising Degraded once the cause is gone.
//
// It touches the condition ONLY when it is currently True: a platform that never degraded
// keeps a Degraded-free condition list rather than gaining a permanent Degraded=False entry.
func clearStaleDegraded(p *otilmv1alpha1.Platform) {
	if !meta.IsStatusConditionTrue(p.Status.Conditions, conditionDegraded) {
		return
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionDegraded, Status: metav1.ConditionFalse, Reason: reasonReconciled,
		Message: "reconcile succeeded; the reported failure no longer applies", ObservedGeneration: p.Generation,
	})
}

// gateEdge applies the platform edge (Ingress / Gateway API + cert-manager objects)
// only when every upstream-CRD prerequisite its configured mode needs is served by
// the cluster, reflecting the outcome on the EdgeReady condition. The edge is gated
// (not part of desired state) when nil/disabled, so any stale EdgeReady condition is
// dropped. See applyGatedObjects for the shared gating semantics.
func (r *Reconciler) gateEdge(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet) (bool, error) {
	gated := p.Spec.Edge != nil && p.Spec.Edge.Enabled
	return r.applyGatedObjects(ctx, p, desired, gatedObjects{
		conditionType:  "EdgeReady",
		readyMessage:   "Edge reconciled",
		active:         gated,
		dependencies:   platformbuilder.EdgeDependencies(p),
		objects:        platformbuilder.ResolveEdge(p),
		applyLabel:     "edge",
		detectionLabel: "edge",
	})
}

// gateAdminCert applies the optional admin-bootstrap cert-manager objects (the admin
// Certificate, and possibly a dedicated admin CA chain) only when cert-manager is
// served, reflecting the outcome on the AdminCertReady condition. It is gated (not
// part of desired state) unless registerAdmin is enabled with source=generated:
// source=provided supplies the Secret directly and needs no cert-manager, and a
// disabled bootstrap renders nothing — both drop any stale AdminCertReady condition.
// See applyGatedObjects for the shared gating semantics.
func (r *Reconciler) gateAdminCert(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet) (bool, error) {
	objs := platformbuilder.ResolveAdminCertObjects(p)
	return r.applyGatedObjects(ctx, p, desired, gatedObjects{
		conditionType:  "AdminCertReady",
		readyMessage:   "Admin certificate reconciled",
		active:         len(objs) > 0, // non-empty only for enabled + source=generated
		dependencies:   platformbuilder.AdminCertDependencies(p),
		objects:        objs,
		applyLabel:     "admin certificate",
		detectionLabel: "admin certificate",
	})
}

// gateServiceMonitors applies the per-component Prometheus ServiceMonitors only when the
// Prometheus operator CRD (monitoring.coreos.com/ServiceMonitor) is served by the
// cluster, reflecting the outcome on the ServiceMonitorsReady condition. It is gated (not
// part of desired state) when no component requests a ServiceMonitor, so a cluster
// without the Prometheus operator never fails the apply and any stale condition is
// dropped. See applyGatedObjects for the shared gating semantics.
func (r *Reconciler) gateServiceMonitors(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet) (bool, error) {
	objs := platformbuilder.ResolvePlatformServiceMonitors(p)
	return r.applyGatedObjects(ctx, p, desired, gatedObjects{
		conditionType:  "ServiceMonitorsReady",
		readyMessage:   "ServiceMonitors reconciled",
		active:         len(objs) > 0, // non-empty only when a component opts in
		dependencies:   platformbuilder.ServiceMonitorDependencies(p),
		objects:        objs,
		applyLabel:     "ServiceMonitor",
		detectionLabel: "ServiceMonitor",
	})
}

// gatedObjects bundles one set of cert-manager-gated render objects with the
// condition it reports on, for applyGatedObjects.
type gatedObjects struct {
	// conditionType is the status condition this gate manages (e.g. "EdgeReady").
	conditionType string
	// readyMessage is the condition message when the objects are applied.
	readyMessage string
	// active reports whether this gate is part of the Platform's desired state. When
	// false the condition is dropped and nothing is applied (neutral).
	active bool
	// dependencies are the upstream-CRD prerequisites to probe before applying.
	dependencies []platformbuilder.EdgeDependency
	// objects are the rendered objects to apply once all dependencies are present.
	objects []client.Object
	// applyLabel / detectionLabel name the gate in apply-error wrapping and detection
	// log lines (e.g. "edge", "admin certificate").
	applyLabel     string
	detectionLabel string
}

// applyGatedObjects applies a set of cert-manager-gated objects only when every
// upstream-CRD prerequisite is served by the cluster, and reflects the outcome on the
// gate's status condition. It returns requeue=true when a prerequisite is absent so
// the caller requeues to self-heal; it returns a non-nil error only for an actual
// apply failure (a transient detector error is treated as "absent" and requeued,
// never a hard failure), so a missing cert-manager / Gateway API never flips the
// whole Platform to Degraded. The edge and the admin bootstrap share this one path.
//
// Behaviour:
//   - gate inactive (nil/disabled feature) → drop any stale condition, apply nothing,
//     and DO NOT mark the objects desired — so the prune reclaims them (this is how
//     disabling the edge / changing its type prunes the now-stale objects).
//   - all deps present → apply the objects, set the condition True.
//   - a dep absent (or a transient detection error) → skip APPLYING all of this gate's
//     objects (e.g. an Ingress would otherwise reference a never-populated cert Secret),
//     set the condition False with the dep's actionable reason/message, request a
//     requeue — BUT still mark the rendered objects desired so the prune PRESERVES any
//     already-applied copies. This distinguishes a deliberately-disabled gate (prune)
//     from a still-enabled gate whose dependency merely flapped (keep): a transient
//     cert-manager outage must not delete a healthy Ingress.
//
// SECURITY: the condition message names only the spec field and the remedy — never a
// secret value or a connection coordinate (host/port/URI).
func (r *Reconciler) applyGatedObjects(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet, g gatedObjects) (bool, error) {
	if !g.active {
		// Not part of desired state; don't assert anything. Drop any stale condition
		// from a previous generation that had this feature on. The objects are left out
		// of the desired set so the post-apply prune reclaims any that still exist.
		meta.RemoveStatusCondition(&p.Status.Conditions, g.conditionType)
		return false, nil
	}

	// Probe each required upstream CRD. A transient detector error is folded into the
	// "not yet available" path (recorded as the dep's reason) rather than failing the
	// whole reconcile — the requeue will retry.
	for _, dep := range g.dependencies {
		available, derr := r.Capabilities.Available(dep.GroupKind, dep.Versions...)
		if derr != nil {
			log.FromContext(ctx).Info(g.detectionLabel+" dependency detection failed; treating as not yet available",
				"groupKind", dep.GroupKind.String(), "err", derr.Error())
			available = false
		}
		if !available {
			meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
				Type: g.conditionType, Status: metav1.ConditionFalse, Reason: dep.Reason,
				Message: dep.Message, ObservedGeneration: p.Generation, // no secret/coordinate leakage
			})
			// Preserve already-applied gated objects across a dependency flap: the gate
			// is still active (the feature is enabled), so mark its rendered objects
			// desired even though we are not applying them this reconcile. Otherwise a
			// transient cert-manager outage would prune a healthy edge/admin object.
			for _, obj := range g.objects {
				desired.add(r, obj)
			}
			// Non-fatal waiting state: record an Event so the wait is visible without
			// scraping conditions. Reason/message carry no secret/coordinate (the dep's
			// actionable text only).
			r.event(p, corev1.EventTypeWarning, dep.Reason, dep.Message)
			return true, nil
		}
	}

	// All prerequisites present: apply the gated objects.
	for _, obj := range g.objects {
		if err := r.apply(ctx, p, obj, desired); err != nil {
			return false, fmt.Errorf("applying %s %T %q: %w", g.applyLabel, obj, obj.GetName(), err)
		}
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: g.conditionType, Status: metav1.ConditionTrue, Reason: "Reconciled",
		Message: g.readyMessage, ObservedGeneration: p.Generation,
	})
	return false, nil
}

// reconcileAuthDBSecret composes auth's .NET database connection string and
// stores it in an operator-managed, owner-referenced Secret that auth reads
// via secretKeyRef — the only sanctioned place a composed credential lives.
//
// SECURITY (critical): the platform DB-credentials Secret is read read-only; the
// composed connection string and the password it contains are written ONLY to the
// derived Secret's stringData and are NEVER logged or placed into the Platform
// status, conditions, or events. Errors returned here carry no credential material.
//
// The DB-credentials Secret is assumed present: the reconcile preflight already
// degrades the Platform when database.credentials.secretRef is set but missing. When
// the ref is empty (no creds to compose from) this step is skipped.
func (r *Reconciler) reconcileAuthDBSecret(ctx context.Context, p *otilmv1alpha1.Platform, bundle bom.Bundle, desired desiredSet) error {
	// Resolve the DB connection mode-agnostically: external → the spec coordinates +
	// credentials.secretRef; managed → the CloudNativePG-generated <cluster>-rw Service
	// (or the Pooler) + the generated <cluster>-app Secret. The composition below is
	// identical in both modes — only the source of the coordinates/Secret differs.
	db := platformbuilder.ResolveDatabaseConnection(p)
	credRef := db.CredentialsSecretName
	if credRef == "" {
		return nil // nothing to compose from; preflight handles the "set but missing" case
	}
	// Use the version bundle's wiring resolved once in Reconcile (the AuthDB Secret name/
	// key and the .NET connection-string format are version-specific data).
	w := bundle.Wiring

	// Read the DB-credentials Secret read-only.
	var creds corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: credRef}, &creds); err != nil {
		return fmt.Errorf("reading database credentials Secret %q", credRef) // name only — never content
	}
	// Read the username/password under the EFFECTIVE in-Secret keys resolved on the
	// connection: the user's external key mapping (spec.database.credentials.usernameKey/
	// passwordKey) when set, else the wiring-profile defaults; for managed mode the upstream
	// CNPG keys. This is the SAME mapping the secretKeyRef wiring uses (sharedCredSecretEnv),
	// so the composed connection string and Core/scheduler read identical bytes.
	username := string(creds.Data[db.UsernameKey])
	password := string(creds.Data[db.PasswordKey])

	// Compose the .NET connection string. host/port come from the resolved connection: the
	// external coordinates (pgBouncer-fronted if the user points them there) or the managed
	// CNPG Service (the same pgBouncer-aware resolution).
	connStr := w.AuthDBConnectionString(db.Host, db.Port, db.Name, username, password)

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: w.AuthDBSecretName, Namespace: p.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = map[string]string{
			common.NameLabel:      w.AuthDBSecretName,
			common.InstanceLabel:  p.Name,
			common.ComponentLabel: "auth",
			common.PartOfLabel:    common.PartOfValue,
			common.ManagedByLabel: common.ManagedByValue,
		}
		secret.Type = corev1.SecretTypeOpaque
		// StringData holds the only copy of the composed credential; do not also set
		// Data (CreateOrUpdate preserves any prior Data, so clear it for idempotency).
		secret.Data = nil
		secret.StringData = map[string]string{w.AuthDBSecretKey: connStr}
		return controllerutil.SetControllerReference(p, secret, r.Scheme)
	})
	if err != nil {
		return err // err carries no credential material
	}
	// Record the composed Secret in the desired set so the post-apply prune keeps it
	// while DB credentials are configured (and prunes it if the ref is later removed).
	desired.add(r, secret)
	return nil
}

// reconcileTrustedCerts composes the trusted-certificates CA bundle and applies it as
// an operator-owned Secret via server-side apply, returning a checksum of the bundle
// so a change rolls Core. It composes ONLY when the admin bootstrap
// requires Core to additionally trust a generated admin CA (source=generated with a
// known admin CA): the bundle = the user's trusted CA (spec.trustedCertificates) +
// the admin issuer's CA cert, both read READ-ONLY. When no composition is needed it
// renders nothing and returns an empty checksum (Core references the user's Secret
// verbatim, unchanged — and an unset trustedCertificates renders no Secret at all).
//
// SECURITY: CA certs are public, but the discipline holds — the bundle bytes live only
// in the applied Secret and are NEVER logged or placed in status/conditions; the
// returned error carries no cert material (only Secret names).
func (r *Reconciler) reconcileTrustedCerts(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet) (string, error) {
	if !platformbuilder.ComposesTrustedCerts(p) {
		return "", nil // no admin CA to fold in; Core references the user's Secret as-is
	}

	var bundle []byte
	// User-provided trusted CA bundle (optional): read read-only and prepend. Missing
	// is tolerated — the admin bootstrap may be the only trust anchor.
	if ref := p.Spec.Common.TrustedCertificates.SecretRef; ref != "" {
		var userSecret corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: ref}, &userSecret); err != nil {
			return "", fmt.Errorf("reading trusted-certificates Secret %q", ref) // name only — never content
		}
		// Read the user's CA bundle under the EFFECTIVE input key (spec.trustedCertificates.
		// caKey override, else the wiring-profile default "ca.crt"). The composed bundle is
		// re-emitted under the fixed OUTPUT key (TrustedCertsBundleKey) so Core's secretKeyRef
		// (TrustedCertsSecretKey) lines up.
		bundle = appendPEM(bundle, userSecret.Data[platformbuilder.TrustedCertsInputKey(p)])
	}

	// Admin issuer CA cert: fold in so Core trusts the issuer of the admin client cert.
	// The CA keypair Secret may not be issued yet (cert-manager race) — tolerate it and
	// self-heal on the next reconcile (the admin-cert gate already requeues). cert-manager
	// CA keypair Secrets carry the CA cert under ca.crt (preferred) or tls.crt.
	caSrc := platformbuilder.AdminCABundle(p)
	if caSrc.Known {
		var caSecret corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: caSrc.SecretName}, &caSecret); err == nil {
			caPEM := caSecret.Data["ca.crt"]
			if len(caPEM) == 0 {
				caPEM = caSecret.Data["tls.crt"]
			}
			bundle = appendPEM(bundle, caPEM)
		}
	}

	secret := platformbuilder.BuildTrustedCertificatesSecret(p, bundle)
	if err := r.apply(ctx, p, secret, desired); err != nil {
		return "", fmt.Errorf("applying trusted-certificates Secret: %w", err) // no cert material
	}
	// Checksum over the composed bundle Secret; a bundle change changes the checksum,
	// which changes Core's pod-template hash and rolls Core cleanly.
	return checksum.ComputeSecretChecksum(secret), nil
}

// appendPEM appends a PEM blob to a running bundle, ensuring a separating newline so
// concatenated certificates do not run together (……END CERTIFICATE----------BEGIN……).
// Empty blobs are ignored.
func appendPEM(bundle, pem []byte) []byte {
	if len(pem) == 0 {
		return bundle
	}
	if len(bundle) > 0 && bundle[len(bundle)-1] != '\n' {
		bundle = append(bundle, '\n')
	}
	return append(bundle, pem...)
}

// oidcClientChecksum returns a SHA-256 checksum of the operator-owned OIDC client Secret
// (<platform>-oidc-client) for a managed Keycloak, or "" when Keycloak is not managed or the
// Secret does not exist yet (NotFound / transient read error → no stamp; self-heals on the next
// reconcile). Folding it into Core's config-checksum makes Core ROLL once reconcileOIDCProvider
// relays the Keycloak-generated client secret, so Core's postStart re-runs WITH the secret and
// registers the internal OIDC provider — turning the previous crash-loop-until-restart into one
// clean rollout. The value is hashed (never logged or placed in status): no secret leakage.
func (r *Reconciler) oidcClientChecksum(ctx context.Context, p *otilmv1alpha1.Platform) string {
	if !platformbuilder.KeycloakManaged(p) {
		return ""
	}
	var sec corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: platformbuilder.OIDCClientSecretName(p)}, &sec); err != nil {
		return "" // absent or transient read error: no stamp, no spurious roll
	}
	return checksum.ComputeSecretChecksum(&sec)
}

// coreWorkloadStatus reads Core's existing workload (a Deployment, or a StatefulSet for
// workloadType=StatefulSet) and reports whether it exists, whether it is Ready (>=1 ready replica
// — its first DB migration has completed and the pod is up), and the checksum/config annotation it
// currently carries. Used to FREEZE Core's config-checksum during its first bring-up (see the
// apply loop): while Core is not yet Ready the reconciler re-stamps coreChecksum-of-record so a
// late-arriving config input does not roll the migrating pod.
func (r *Reconciler) coreWorkloadStatus(ctx context.Context, p *otilmv1alpha1.Platform) (exists, ready bool, configChecksum string) {
	key := client.ObjectKey{Namespace: p.Namespace, Name: coreDeploymentName}
	var dep appsv1.Deployment
	if err := r.Get(ctx, key, &dep); err == nil {
		return true, dep.Status.ReadyReplicas > 0, dep.Spec.Template.Annotations[platformbuilder.ConfigChecksumAnnotation]
	}
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, key, &sts); err == nil {
		return true, sts.Status.ReadyReplicas > 0, sts.Spec.Template.Annotations[platformbuilder.ConfigChecksumAnnotation]
	}
	return false, false, ""
}

// isCoreWorkload reports whether a rendered object is Core's workload (the Deployment or
// StatefulSet named coreDeploymentName) — the object whose config-checksum is frozen during its
// first migration. Core's Service/ServiceAccount/etc. are unaffected (they carry no checksum).
func isCoreWorkload(obj client.Object) bool {
	if obj.GetName() != coreDeploymentName {
		return false
	}
	switch obj.(type) {
	case *appsv1.Deployment, *appsv1.StatefulSet:
		return true
	}
	return false
}

// combinedCoreChecksum folds the reconcile-time inputs that must ROLL Core when they change —
// the composed trusted-certs bundle, the relayed OIDC client Secret, and the in-pod scripts
// ConfigMap (register-internal-keycloak.sh) — into one stable checksum stamped on Core's pod
// template. All three are sourced/mounted by reference, so a content change does not alter Core's
// pod template on its own. Empty inputs are dropped; when none are present it returns "" so
// StampConfigChecksum stays a no-op (Core carries no checksum annotation), preserving the
// out-of-the-box render.
func combinedCoreChecksum(trustedCerts, oidcClient, scripts string) string {
	parts := map[string]string{}
	if trustedCerts != "" {
		parts["trustedCerts"] = trustedCerts
	}
	if oidcClient != "" {
		parts["oidcClient"] = oidcClient
	}
	if scripts != "" {
		parts["scripts"] = scripts
	}
	if len(parts) == 0 {
		return ""
	}
	return checksum.CombineChecksums(parts)
}

// coreScriptsChecksum returns a SHA-256 checksum of Core's in-pod scripts ConfigMap content
// (register-internal-keycloak.sh) for a managed Keycloak, or "" when Keycloak is not managed (no
// scripts ConfigMap is rendered). Folding it into Core's config-checksum rolls Core when the
// script changes — the postStart hook reads the mounted script only at container start and the
// ConfigMap volume updates lazily, so without this roll a script change (e.g. the /kc OIDC URLs)
// would not take effect until an unrelated restart. PURE: the script is deterministic from the
// spec, computed from the same builder the reconciler applies, so no cluster read is needed.
func coreScriptsChecksum(p *otilmv1alpha1.Platform) string {
	cm := platformbuilder.BuildCoreScriptsConfigMap(p)
	if cm == nil {
		return ""
	}
	return checksum.ComputeConfigMapChecksum(cm)
}

// gatewayConfigChecksum returns a SHA-256 checksum of the gateway's declarative config (kong.yml,
// held in the global ConfigMap). Stamping it on the gateway Deployment rolls Kong when the
// routing/plugins change — Kong reads KONG_DECLARATIVE_CONFIG only at boot and the ConfigMap
// volume updates lazily, so without this roll a kong.yml change (e.g. the managed-Keycloak /kc
// route) would not take effect until an unrelated restart. PURE: kong.yml is deterministic from
// the spec, computed from the same builder the reconciler applies, so no cluster read is needed.
func gatewayConfigChecksum(p *otilmv1alpha1.Platform) string {
	return checksum.ComputeConfigMapChecksum(platformbuilder.BuildGlobalConfigMap(p))
}

// apply server-side-applies a rendered, operator-owned object: it stamps the Platform
// as controller owner, populates the object's apiVersion/kind (SSA requires them on
// the wire), and patches with a stable field manager and ForceOwnership. Because the
// operator only ever sends the fields it renders, apiserver-defaulted fields it omits
// (Deployment strategy, Service clusterIP, etc.) are left to the apiserver and do not
// churn between reconciles.
// It also records the applied object's key in the desired set so the post-apply prune
// keeps it (a managed child absent from the desired set is pruned). For unstructured
// objects (cert-manager / Gateway API) whose GVK is preset, that GVK is preserved.
func (r *Reconciler) apply(ctx context.Context, p *otilmv1alpha1.Platform, obj client.Object, desired desiredSet) error {
	if err := controllerutil.SetControllerReference(p, obj, r.Scheme); err != nil {
		return err
	}
	gvks, _, err := r.Scheme.ObjectKinds(obj)
	if err != nil {
		return err
	}
	obj.GetObjectKind().SetGroupVersionKind(gvks[0]) // SSA needs apiVersion/kind populated
	// client.Apply (the ApplyPatchType patch) json.Marshals the typed object, so its
	// `omitempty` optional fields are omitted from the wire and stay un-owned by this
	// field manager — exactly the churn-free SSA semantics we want. The newer typed
	// client.Apply(ApplyConfiguration) is documented as unsafe for API-object-derived
	// values (it can't distinguish unset from zero), so we keep the patch form here.
	if err := r.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil { //nolint:staticcheck // SA1019: patch-based SSA is intentional for typed render objects; ApplyConfiguration path is unsafe for these
		return err
	}
	desired.add(r, obj) // record it as desired so the post-apply prune keeps it (nil-safe)
	return nil
}

// Reconcile error contract — three buckets, do not conflate them:
//
//   - DETERMINISTIC won't-proceed steady state (a missing referenced Secret, an unknown
//     spec.version, the singleton loser): use steadyState / steadyStateRequeue. NO error
//     is returned so the reconcile does not back off forever or spam error logs; a watch /
//     spec edit re-enqueues. Phase is Degraded (the platform genuinely cannot proceed) but
//     there is no retryable error.
//
//   - DETERMINISTIC error (a render / validation / rejected-override error, a NotFound on a
//     required referenced Secret, a malformed realm JSON): use degraded(). It sets
//     Phase=Degraded AND returns the error so controller-runtime requeues — but the cause is
//     a user mistake, so Degraded is the correct, visible signal until the spec is fixed.
//
//   - TRANSIENT error (an SSA Conflict from a competing field manager, an API server
//     timeout / 429 / 503, a context deadline, a network blip): use transientRequeue() —
//     directly, or via applyOrDegrade() which routes by isTransient. It returns the error
//     for controller-runtime's exponential backoff WITHOUT flipping the platform to
//     Degraded: the platform keeps its current phase (Available / Progressing) and the retry
//     converges. A retryable conflict is NOT a degraded platform, so it must not look like one.
//
// degraded marks the Platform Degraded and RETURNS THE ERROR so controller-runtime backs
// off and retries. Use it for a DETERMINISTIC error (see the contract above); for a
// transient API error prefer transientRequeue / applyOrDegrade so a retryable failure does
// not flip the platform to Degraded.
//
// SECURITY: the condition Message is the generic reason only; the cause (which may
// reference a Secret/coordinate by name) is logged but never placed in status/Event.
func (r *Reconciler) degraded(ctx context.Context, p *otilmv1alpha1.Platform, reason string, cause error) (ctrl.Result, error) {
	// Surface the ACTUAL cause in the Degraded condition + Warning event, not just the reason
	// code: an opaque "Message: ManagedMessagingError" tells the operator nothing about what
	// failed (e.g. which managed object the apply rejected, or that an upstream CRD/webhook is
	// not ready). Every caller crafts a LEAK-FREE cause (object kind/name + field path + the API
	// error — never a secret value or a connection coordinate), so it is safe to expose, and it
	// is what makes the condition actionable.
	msg := cause.Error()
	r.setDegradedMessage(ctx, p, reason, msg)
	r.event(p, corev1.EventTypeWarning, reason, msg)
	return ctrl.Result{}, cause
}

// applyOrDegrade routes a reconcile error by transience (see the error contract above): a
// TRANSIENT error (an SSA conflict, an API timeout / 429 / 503, a context deadline, a
// network blip) becomes a transientRequeue — the error is returned for backoff but the
// platform is NOT marked Degraded; a DETERMINISTIC error (a render/validation/rejected-
// override error, a NotFound on a required Secret, anything not recognised as transient)
// becomes a degraded(). It is the right choice for every apply / API-IO error site whose
// cause could be either (the SSA apply loop, the gated-object applies, the prune, the
// readiness/Secret reads, the composed-Secret reconcilers). cause is never nil at a call site.
//
// SECURITY: like degraded/transientRequeue, only the generic reason reaches status/Event;
// the cause (which may name a Secret/coordinate) is logged only.
func (r *Reconciler) applyOrDegrade(ctx context.Context, p *otilmv1alpha1.Platform, reason string, cause error) (ctrl.Result, error) {
	if isTransient(cause) {
		return r.transientRequeue(ctx, p, reason, cause)
	}
	return r.degraded(ctx, p, reason, cause)
}

// transientRequeue handles a genuinely TRANSIENT reconcile error: it returns the error so
// controller-runtime requeues with exponential backoff, but DOES NOT touch the platform's
// phase or conditions and records NO Warning Event — a retryable conflict / timeout is not a
// degraded platform. The cause is logged at a LOW level (Info, not Error) so a steady stream
// of benign conflicts neither spams error logs nor pages on a Degraded transition. The
// platform keeps whatever phase the prior successful reconcile set (Available / Progressing).
//
// SECURITY: the cause (which may name a Secret/coordinate) is logged only — never surfaced.
func (r *Reconciler) transientRequeue(ctx context.Context, p *otilmv1alpha1.Platform, reason string, cause error) (ctrl.Result, error) {
	log.FromContext(ctx).Info("transient reconcile error; requeueing with backoff (platform not degraded)",
		"reason", reason, "name", p.Name, "err", cause.Error())
	return ctrl.Result{}, cause
}

// isTransient reports whether an error is a RETRYABLE, non-deterministic failure that should
// requeue with backoff WITHOUT degrading the platform, as opposed to a deterministic user/
// render error. It recognises:
//   - an SSA write Conflict from a competing field manager (apierrors.IsConflict);
//   - server-side overload / unavailability the apiserver asks us to retry
//     (IsServerTimeout / IsTooManyRequests / IsServiceUnavailable / IsTimeout);
//   - a cancelled / timed-out context (context.DeadlineExceeded / context.Canceled);
//   - a transient network/REST error (a net.Error reporting Timeout());
//   - an admission webhook that is registered but not yet serving (a just-installed upstream
//     operator's webhook endpoint warming up) — see isWebhookNotReady.
//
// It deliberately does NOT treat IsNotFound / IsInvalid / IsBadRequest / IsForbidden as
// transient: those are deterministic (a missing required Secret, a rejected override, an RBAC
// gap) and must degrade so the operator sees an actionable, persistent signal.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsConflict(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsTimeout(err) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	// A transient transport/REST error (connection reset, temporary DNS, timeout) surfaces as
	// a net.Error whose Timeout() reports true — retryable, not a degraded platform.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// A managed-infra apply can briefly out-race a freshly-installed upstream operator's
	// admission webhook (CRD registered, webhook endpoint not yet serving) — a self-healing
	// race, not a degraded platform.
	return isWebhookNotReady(err)
}

// isWebhookNotReady reports whether err is an admission-webhook-unreachable failure from an
// upstream operator whose webhook endpoint is not yet serving. When an operator (CloudNativePG,
// the RabbitMQ Cluster + Messaging Topology operators, Keycloak) is installed, its CRDs become
// served — so the capability probe in gateManagedInfra passes — seconds before its webhook pod
// is Ready. An apply against its failurePolicy=Fail webhook in that window fails with an
// apiserver InternalError (500) that names the webhook call: e.g.
//
//	Internal error occurred: failed calling webhook "vhost.rabbitmq.com": failed to call
//	webhook: Post "https://...svc:443/...": dial tcp ...: connect: connection refused
//
// (and likewise "no endpoints available for service" when the service has no Ready endpoints).
// This clears itself within seconds once the webhook serves, so it is classified transient:
// the Platform requeues and converges rather than hard-degrading despite its dependencies being
// correctly installed. SECURITY: matches only the apiserver's own error vocabulary, not any
// user-supplied value, and never surfaces the message (transientRequeue logs it, does not set it
// on status).
func isWebhookNotReady(err error) bool {
	if !apierrors.IsInternalError(err) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "failed calling webhook") ||
		strings.Contains(msg, "failed to call webhook") ||
		strings.Contains(msg, "no endpoints available for service")
}

// steadyState marks the Platform Degraded for a DETERMINISTIC won't-proceed condition
// and returns NO error and NO requeue: the condition is terminal until an external
// change (a watch / a spec edit) re-enqueues the Platform. Used for the singleton
// loser, whose only exit is the older Platform being deleted (which re-enqueues this
// one). It records an Event and a generic, leak-free condition message.
//
//nolint:unparam // (ctrl.Result, error) matches the Reconcile return contract so callers use `return r.steadyState(...)`; both values are intentionally constant
func (r *Reconciler) steadyState(ctx context.Context, p *otilmv1alpha1.Platform, reason, message string) (ctrl.Result, error) {
	r.setDegradedMessage(ctx, p, reason, message)
	r.event(p, corev1.EventTypeWarning, reason, message)
	return ctrl.Result{}, nil
}

// steadyStateRequeue is steadyState with a fixed RequeueAfter, for a deterministic
// won't-proceed condition that a watch ALSO covers but where a periodic re-check is a
// cheap safety net (a missing referenced Secret: the Secret watch re-enqueues on
// creation, and the requeue re-checks regardless). Returns no error.
//
//nolint:unparam // error is intentionally always nil; the (ctrl.Result, error) shape matches the Reconcile return contract so callers use `return r.steadyStateRequeue(...)`
func (r *Reconciler) steadyStateRequeue(ctx context.Context, p *otilmv1alpha1.Platform, reason, message string, after time.Duration) (ctrl.Result, error) {
	r.setDegradedMessage(ctx, p, reason, message)
	r.event(p, corev1.EventTypeWarning, reason, message)
	return ctrl.Result{RequeueAfter: after}, nil
}

// setDegradedMessage sets phase Degraded with the given reason and an explicit,
// leak-free message, then persists status.
func (r *Reconciler) setDegradedMessage(ctx context.Context, p *otilmv1alpha1.Platform, reason, message string) {
	p.Status.Phase = otilmv1alpha1.PlatformPhaseDegraded
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionDegraded, Status: metav1.ConditionTrue, Reason: reason,
		Message: message, ObservedGeneration: p.Generation, // message must not leak secret values/coordinates
	})
	if err := r.Status().Update(ctx, p); err != nil {
		log.FromContext(ctx).Error(err, "failed to update Platform status", "phase", "Degraded")
	}
}

// SetupWithManager wires the controller.
//
// Deletion safety: the reconciler adds platformFinalizer before any work (see
// handleFinalizer) so deletion runs handleDeletion — which honors spec.deletionPolicy
// — before the object is removed (the platforms/finalizers RBAC marker above grants
// the finalizer update). The operator manages both external and managed (CloudNativePG/
// RabbitMQ/Keycloak) infrastructure: the Platform's same-namespace children (the rendered
// Deployments/Services/ServiceAccounts/ConfigMaps and the composed Secrets) are reclaimed
// via owner-reference garbage collection under both policies, while the owner-reference-less
// managed CRs are retained or torn down by handleDeletion per deletionPolicy. Cluster-scoped
// artifact cleanup (webhook config, ClusterRoles) is the remaining future hook on this path.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Detect upstream-operator / CRD availability (cert-manager, Gateway API) via the
	// manager's RESTMapper. The default is a dynamic mapper that re-discovers on a
	// miss, so a CRD installed after start-up is picked up without an operator restart.
	if r.Capabilities == nil {
		r.Capabilities = capabilities.New(mgr.GetRESTMapper())
	}
	// Default the OIDC registrar to the real HTTP client; tests inject a fake. It is
	// stateless, so one instance is shared across reconciles.
	if r.OIDCRegistrar == nil {
		r.OIDCRegistrar = registration.NewHTTPOIDCRegistrar()
	}
	// Drift correction: Owns wires owner-reference-based reconcile triggers for the
	// typed children, so an out-of-band edit/delete of any of them reconciles the
	// Platform back to desired. Ingress is added here too (the edge's typed object).
	//
	// The unstructured cert-manager / Gateway API objects cannot use a typed Owns
	// without registering their schemes (which would pin an incompatible k8s.io graph),
	// so their drift is corrected by the prune (an orphaned/extra object is deleted)
	// plus controller-runtime's periodic resync re-running the full render-apply — an
	// accepted trade-off for the small, rarely-edited edge object set.
	//
	// The Secret Watch re-enqueues a Platform when any referenced Secret it depends on
	// (DB/messaging credentials, trusted-cert bundle, admin cert, API key, the
	// CNPG-generated <cluster>-app Secret for a managed database, AND the Messaging-Topology-
	// generated per-user Secrets for a managed broker) changes, so a credential / CA
	// rotation reconciles (which re-composes auth-db / trusted-certificates, whose
	// checksum annotation then rolls the consumers) and the managed-DB / managed-broker
	// readbacks pick up the generated Secrets the instant the upstream operators create them.
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&otilmv1alpha1.Platform{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.platformsForSecret))

	// NOTE: the managed CloudNativePG Cluster, the managed RabbitMQ types (RabbitmqCluster +
	// the Topology kinds), AND the managed Keycloak types (Keycloak + KeycloakRealmImport)
	// are deliberately NOT watched directly. A controller-runtime informer Watch on a GVK
	// whose CRD is not served (e.g. CNPG / the RabbitMQ operators / the Keycloak Operator not
	// installed) FAILS its ListWatch and blocks the manager's cache sync — which would stall
	// ALL reconciles. Instead, the Secret Watch above re-enqueues the Platform when the
	// upstream operator creates a generated Secret the platform references
	// (referencedSecretNames resolves the DB credentials Secret AND the managed broker's
	// Topology-generated per-user Secrets mode-agnostically, so it covers both the CNPG
	// <cluster>-app Secret and the RabbitMQ Core/provisioner-user Secrets; the managed
	// Keycloak consumes that same shared DB-credentials Secret, so it needs no new Secret for
	// provisioning), and the databaseRequeueAfter / messagingRequeueAfter / keycloakRequeueAfter
	// backstops re-check each component's provisioning status until Ready — mirroring how the
	// edge leaves its (dynamically-gated) cert-manager / Gateway API objects unwatched.
	return builder.Complete(r)
}

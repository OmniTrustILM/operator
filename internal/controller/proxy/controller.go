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

package proxy

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	proxybuilder "github.com/OmniTrustILM/operator/internal/builder/proxy"
	"github.com/OmniTrustILM/operator/internal/checksum"
	"github.com/OmniTrustILM/operator/internal/monitoring"
	"github.com/OmniTrustILM/operator/pkg/capabilities"
)

const (
	finalizerName = "otilm.com/finalizer"
	requeueDelay  = 30 * time.Second

	condAvailable           = "Available"
	condProgressing         = "Progressing"
	condDegraded            = "Degraded"
	condServiceMonitorReady = "ServiceMonitorReady"
)

// serviceMonitorGK is the upstream GroupKind the ServiceMonitor capability gate probes.
var serviceMonitorGK = schema.GroupKind{Group: "monitoring.coreos.com", Kind: "ServiceMonitor"}

// Reconciler reconciles a Proxy object. The reconciliation loop never calls the
// platform: the Proxy CR is a pure consumer of the provisioning-issued config token
// (see docs/design/proxy-operator.md).
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Capabilities detects whether optional upstream CRDs (ServiceMonitor) are served.
	// Defaulted from the manager's RESTMapper in SetupWithManager when nil.
	Capabilities *capabilities.Detector
}

// The Proxy reconciler creates Deployments/Services/ServiceAccounts via CreateOrUpdate
// with owner references (removed by owner-ref GC), so those need no delete verb. It
// DOES explicitly Delete a now-disabled PodDisruptionBudget / ServiceMonitor. The
// child-resource and Secret/ConfigMap/event rules are shared with the Connector
// reconciler's markers; only the proxies rules are new.
// +kubebuilder:rbac:groups=otilm.com,resources=proxies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=otilm.com,resources=proxies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=otilm.com,resources=proxies/finalizers,verbs=update

// Reconcile drives the Proxy toward its desired state: finalizer, config-token
// checksum, child resources, status.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	px, err := r.fetchProxy(ctx, req)
	if px == nil {
		return ctrl.Result{}, err
	}

	done, err := r.handleFinalizer(ctx, req, px)
	if done || err != nil {
		return ctrl.Result{}, err
	}

	r.setInitialPhase(px)

	combinedChecksum, tokenValue, requeueResult, err := r.computeChecksums(ctx, px)
	if requeueResult != nil {
		return *requeueResult, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	r.clearConfigDegraded(px)
	r.checkTokenExpiry(px, tokenValue)

	smRequeue, err := r.reconcileChildResources(ctx, px, combinedChecksum)
	if err != nil {
		return ctrl.Result{}, err
	}

	currentDeploy, err := r.updateDeploymentStatus(ctx, px)
	if err != nil {
		return ctrl.Result{}, err
	}

	px.Status.ObservedGeneration = px.Generation
	px.Status.ReadyReplicas = currentDeploy.Status.ReadyReplicas
	px.Status.ObservedVersion = proxybuilder.ResolvedVersion(px)
	px.Status.ConfigChecksum = combinedChecksum

	if err := r.Status().Update(ctx, px); err != nil {
		logger.Error(err, "failed to update Proxy status")
		return ctrl.Result{}, err
	}

	settled := px.Status.Phase == otilmv1alpha1.ProxyPhaseRunning ||
		px.Status.Phase == otilmv1alpha1.ProxyPhaseScaledDown
	if smRequeue || !settled {
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
	return ctrl.Result{}, nil
}

// fetchProxy retrieves the Proxy CR; nil when deleted.
func (r *Reconciler) fetchProxy(ctx context.Context, req ctrl.Request) (*otilmv1alpha1.Proxy, error) {
	logger := log.FromContext(ctx)
	var px otilmv1alpha1.Proxy
	if err := r.Get(ctx, req.NamespacedName, &px); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		logger.Error(err, "unable to fetch Proxy")
		return nil, err
	}
	return &px, nil
}

// handleFinalizer manages the finalizer lifecycle. There is nothing to clean up
// today — children go via ownerRef GC, the user-applied token Secret is never
// owned, and broker topology is platform-owned — but the finalizer-first pattern
// matches the sibling Kinds and is the extension point for future cleanup.
func (r *Reconciler) handleFinalizer(ctx context.Context, req ctrl.Request, px *otilmv1alpha1.Proxy) (bool, error) {
	logger := log.FromContext(ctx)

	if px.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(px, finalizerName) {
			r.Recorder.Event(px, corev1.EventTypeNormal, monitoring.ReasonDeleting, "Proxy is being deleted")
			controllerutil.RemoveFinalizer(px, finalizerName)
			if err := r.Update(ctx, px); err != nil {
				logger.Error(err, "failed to remove finalizer")
				return false, err
			}
		}
		return true, nil
	}

	if !controllerutil.ContainsFinalizer(px, finalizerName) {
		controllerutil.AddFinalizer(px, finalizerName)
		if err := r.Update(ctx, px); err != nil {
			logger.Error(err, "failed to add finalizer")
			return false, err
		}
		// Re-fetch after update to avoid conflicts.
		if err := r.Get(ctx, req.NamespacedName, px); err != nil {
			return false, err
		}
	}
	return false, nil
}

// setInitialPhase marks Deploying/Progressing on first sight or generation change.
func (r *Reconciler) setInitialPhase(px *otilmv1alpha1.Proxy) {
	if px.Status.Phase == "" || px.Status.ObservedGeneration != px.Generation {
		px.Status.Phase = otilmv1alpha1.ProxyPhaseDeploying
		meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
			Type:               condProgressing,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: px.Generation,
			Reason:             "Reconciling",
			Message:            "Proxy resources are being reconciled",
		})
	}
}

// computeChecksums fetches the config-token Secret plus every referenced Secret and
// ConfigMap read-only, computes the combined checksum (the rollout trigger), and
// returns the raw token string for the local exp check. Values are hashed for drift
// detection, never logged or placed in status.
func (r *Reconciler) computeChecksums(ctx context.Context, px *otilmv1alpha1.Proxy) (string, string, *ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ref := px.Spec.ConfigTokenSecretRef
	checksums := make(map[string]string, 1+len(px.Spec.SecretRefs)+len(px.Spec.ConfigMapRefs))

	var tokenSecret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: px.Namespace}, &tokenSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", r.degradeAndRequeue(ctx, px, monitoring.ReasonMissingSecret,
				fmt.Sprintf("Secret %q not found", ref.Name)), nil
		}
		logger.Error(err, "failed to read config-token Secret")
		return "", "", nil, err
	}

	tokenBytes, ok := tokenSecret.Data[proxybuilder.TokenKey(px)]
	if !ok {
		return "", "", r.degradeAndRequeue(ctx, px, monitoring.ReasonMissingTokenKey,
			fmt.Sprintf("Secret %q has no key %q", ref.Name, proxybuilder.TokenKey(px))), nil
	}
	checksums["secret/"+ref.Name] = checksum.ComputeSecretChecksum(&tokenSecret)

	if requeue, err := r.checksumRefs(ctx, px, checksums); requeue != nil || err != nil {
		return "", "", requeue, err
	}

	return checksum.CombineChecksums(checksums), string(tokenBytes), nil, nil
}

// checksumRefs folds every spec.secretRefs / spec.configMapRefs object into the
// checksum map; a missing reference degrades and requeues (it self-heals via the
// Secret/ConfigMap watches once the object appears).
func (r *Reconciler) checksumRefs(ctx context.Context, px *otilmv1alpha1.Proxy, checksums map[string]string) (*ctrl.Result, error) {
	for _, sr := range px.Spec.SecretRefs {
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: sr.Name, Namespace: px.Namespace}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return r.degradeAndRequeue(ctx, px, monitoring.ReasonMissingSecret,
					fmt.Sprintf("Secret %q not found", sr.Name)), nil
			}
			return nil, err
		}
		checksums["secret/"+sr.Name] = checksum.ComputeSecretChecksum(&secret)
	}
	for _, cmr := range px.Spec.ConfigMapRefs {
		var cm corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Name: cmr.Name, Namespace: px.Namespace}, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				return r.degradeAndRequeue(ctx, px, monitoring.ReasonMissingConfigMap,
					fmt.Sprintf("ConfigMap %q not found", cmr.Name)), nil
			}
			return nil, err
		}
		checksums["configmap/"+cmr.Name] = checksum.ComputeConfigMapChecksum(&cm)
	}
	return nil, nil
}

// degradeAndRequeue sets Degraded/Failed for a deterministic config problem and
// schedules a retry (the Secret watch also fires when it is fixed).
func (r *Reconciler) degradeAndRequeue(ctx context.Context, px *otilmv1alpha1.Proxy, reason, message string) *ctrl.Result {
	logger := log.FromContext(ctx)
	meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
		Type:               condDegraded,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: px.Generation,
		Reason:             reason,
		Message:            message,
	})
	// A Failed proxy is not available — without this, a proxy that was once Running
	// would keep Available=True forever while Failed.
	meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
		Type:               condAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: px.Generation,
		Reason:             reason,
		Message:            message,
	})
	px.Status.Phase = otilmv1alpha1.ProxyPhaseFailed
	r.Recorder.Event(px, corev1.EventTypeWarning, reason, message)
	if err := r.Status().Update(ctx, px); err != nil {
		logger.Error(err, "failed to update status", "reason", reason)
	}
	return &ctrl.Result{RequeueAfter: requeueDelay}
}

// clearConfigDegraded resets a Degraded condition left by a config problem
// (MissingSecret, MissingConfigMap, MissingTokenKey, ConfigTokenExpired) once the
// referenced objects read healthily again — without it the condition would outlive the fix forever.
// checkTokenExpiry runs immediately after and re-asserts ConfigTokenExpired when the
// token is still expired, so clearing first is safe.
func (r *Reconciler) clearConfigDegraded(px *otilmv1alpha1.Proxy) {
	c := meta.FindStatusCondition(px.Status.Conditions, condDegraded)
	if c == nil || c.Status != metav1.ConditionTrue {
		return
	}
	switch c.Reason {
	case monitoring.ReasonMissingSecret, monitoring.ReasonMissingConfigMap,
		monitoring.ReasonMissingTokenKey, monitoring.ReasonTokenExpired:
		meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
			Type:               condDegraded,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: px.Generation,
			Reason:             "ConfigHealthy",
			Message:            "config-token Secret reads healthily",
		})
	}
}

// checkTokenExpiry surfaces a proactive Degraded/ConfigTokenExpired condition for an
// expired config token. Children are still rendered (running pods keep working until
// restarted); the condition turns a future crash loop into an actionable status.
func (r *Reconciler) checkTokenExpiry(px *otilmv1alpha1.Proxy, token string) {
	exp, found := tokenExpiry(token)
	if found && time.Now().After(exp) {
		msg := fmt.Sprintf("config token expired at %s; rotate the proxy credential in the platform UI", exp.UTC().Format(time.RFC3339))
		meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
			Type:               condDegraded,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: px.Generation,
			Reason:             monitoring.ReasonTokenExpired,
			Message:            msg,
		})
		r.Recorder.Event(px, corev1.EventTypeWarning, monitoring.ReasonTokenExpired, msg)
	}
}

// reconcileChildResources renders and applies the children. Returns smRequeue=true
// when a desired ServiceMonitor is gated on a missing upstream CRD (self-heal requeue).
func (r *Reconciler) reconcileChildResources(ctx context.Context, px *otilmv1alpha1.Proxy, combinedChecksum string) (bool, error) {
	if err := r.reconcileServiceAccount(ctx, px); err != nil {
		return false, err
	}
	if err := r.reconcileDeployment(ctx, px, combinedChecksum); err != nil {
		return false, err
	}
	if err := r.reconcileService(ctx, px); err != nil {
		return false, err
	}
	if err := r.reconcilePDB(ctx, px); err != nil {
		return false, err
	}
	return r.reconcileServiceMonitor(ctx, px)
}

// reconcileServiceAccount creates or updates the ServiceAccount for the Proxy.
func (r *Reconciler) reconcileServiceAccount(ctx context.Context, px *otilmv1alpha1.Proxy) error {
	desired := proxybuilder.BuildServiceAccount(px)
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = desired.Labels
		// Merge (never replace) the builder's annotations — e.g. a workload-identity
		// binding from spec.serviceAccount.annotations — so apiserver- or
		// third-party-managed annotations on the live object survive.
		if len(desired.Annotations) > 0 {
			if sa.Annotations == nil {
				sa.Annotations = make(map[string]string, len(desired.Annotations))
			}
			for k, v := range desired.Annotations {
				sa.Annotations[k] = v
			}
		}
		return ctrl.SetControllerReference(px, sa, r.Scheme)
	})
	return err
}

// reconcileDeployment creates or updates the Deployment for the Proxy.
func (r *Reconciler) reconcileDeployment(ctx context.Context, px *otilmv1alpha1.Proxy, combinedChecksum string) error {
	desired := proxybuilder.BuildDeployment(px, combinedChecksum)
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = desired.Labels
		deploy.Spec.Replicas = desired.Spec.Replicas
		deploy.Spec.Template = desired.Spec.Template
		// Selector is immutable; only set on create.
		if deploy.Spec.Selector == nil {
			deploy.Spec.Selector = desired.Spec.Selector
		}
		return ctrl.SetControllerReference(px, deploy, r.Scheme)
	})
	return err
}

// reconcileService creates or updates the Service for the Proxy.
func (r *Reconciler) reconcileService(ctx context.Context, px *otilmv1alpha1.Proxy) error {
	desired := proxybuilder.BuildService(px)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = desired.Labels
		svc.Spec.Type = desired.Spec.Type
		svc.Spec.Selector = desired.Spec.Selector
		svc.Spec.Ports = desired.Spec.Ports
		return ctrl.SetControllerReference(px, svc, r.Scheme)
	})
	return err
}

// reconcilePDB creates, updates, or prunes the PodDisruptionBudget for the Proxy.
func (r *Reconciler) reconcilePDB(ctx context.Context, px *otilmv1alpha1.Proxy) error {
	logger := log.FromContext(ctx)
	desired := proxybuilder.BuildPDB(px)
	if desired != nil {
		pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
			pdb.Labels = desired.Labels
			pdb.Spec.MinAvailable = desired.Spec.MinAvailable
			pdb.Spec.MaxUnavailable = desired.Spec.MaxUnavailable
			pdb.Spec.Selector = desired.Spec.Selector
			return ctrl.SetControllerReference(px, pdb, r.Scheme)
		})
		return err
	}
	pdb := &policyv1.PodDisruptionBudget{}
	key := types.NamespacedName{Name: proxybuilder.ChildResourceName(px), Namespace: px.Namespace}
	if err := r.Get(ctx, key, pdb); err == nil {
		if err := r.Delete(ctx, pdb); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to delete PDB")
			return err
		}
	}
	return nil
}

// reconcileServiceMonitor renders the ServiceMonitor behind the capability gate:
// when the upstream CRD is not served, the object is skipped, the adjunct
// ServiceMonitorReady condition reports an actionable reason, and the reconcile
// requeues to self-heal (the Platform controller's gateServiceMonitors precedent).
func (r *Reconciler) reconcileServiceMonitor(ctx context.Context, px *otilmv1alpha1.Proxy) (bool, error) {
	logger := log.FromContext(ctx)
	desired := proxybuilder.BuildServiceMonitor(px)

	available, err := r.Capabilities.Available(serviceMonitorGK, "v1")
	if err != nil {
		return false, err
	}
	if !available {
		if desired != nil {
			meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
				Type:               condServiceMonitorReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: px.Generation,
				Reason:             monitoring.ReasonServiceMonitorMissing,
				Message:            "monitoring.coreos.com/v1 ServiceMonitor CRD is not served; install the Prometheus operator to enable scraping",
			})
			return true, nil
		}
		// Not desired and not served: drop any stale adjunct condition.
		meta.RemoveStatusCondition(&px.Status.Conditions, condServiceMonitorReady)
		return false, nil
	}

	if desired != nil {
		sm := &monitoringv1.ServiceMonitor{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sm, func() error {
			sm.Labels = desired.Labels
			sm.Spec = desired.Spec
			return ctrl.SetControllerReference(px, sm, r.Scheme)
		}); err != nil {
			return false, err
		}
		meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
			Type:               condServiceMonitorReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: px.Generation,
			Reason:             "ServiceMonitorCreated",
			Message:            "ServiceMonitor is rendered",
		})
		return false, nil
	}

	// No longer desired: prune the object and drop the stale adjunct condition (a
	// lingering ServiceMonitorReady=True would claim scraping that no longer exists).
	meta.RemoveStatusCondition(&px.Status.Conditions, condServiceMonitorReady)
	sm := &monitoringv1.ServiceMonitor{}
	key := types.NamespacedName{Name: proxybuilder.ChildResourceName(px), Namespace: px.Namespace}
	switch err := r.Get(ctx, key, sm); {
	case err == nil:
		if err := r.Delete(ctx, sm); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to delete ServiceMonitor")
			return false, err
		}
	case !apierrors.IsNotFound(err):
		return false, err
	}
	return false, nil
}

// updateDeploymentStatus reads the Deployment and derives phase + conditions.
func (r *Reconciler) updateDeploymentStatus(ctx context.Context, px *otilmv1alpha1.Proxy) (*appsv1.Deployment, error) {
	logger := log.FromContext(ctx)
	var deploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: proxybuilder.ChildResourceName(px), Namespace: px.Namespace}, &deploy); err != nil {
		logger.Error(err, "failed to get Deployment status")
		return nil, err
	}

	var desired int32 = 1
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}
	ready := deploy.Status.ReadyReplicas

	switch {
	case desired == 0:
		// Deliberate scale-to-zero (spec.replicas: 0): a settled pause, not an error
		// and not progress — without this case the Proxy would sit in Deploying with
		// a 30s requeue loop forever.
		px.Status.Phase = otilmv1alpha1.ProxyPhaseScaledDown
		meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
			Type: condAvailable, Status: metav1.ConditionFalse, ObservedGeneration: px.Generation,
			Reason: "ScaledToZero", Message: "spec.replicas is 0; the proxy is deliberately paused",
		})
		meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
			Type: condProgressing, Status: metav1.ConditionFalse, ObservedGeneration: px.Generation,
			Reason: "ScaledToZero", Message: "no rollout in progress",
		})
	case ready == desired:
		r.setPhaseRunning(px, ready, desired)
	case ready > 0:
		// Partially ready — covers both scale-up (ready < desired) and the transient
		// scale-down overshoot (ready > desired).
		px.Status.Phase = otilmv1alpha1.ProxyPhaseUpdating
		meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
			Type: condProgressing, Status: metav1.ConditionTrue, ObservedGeneration: px.Generation,
			Reason: "RolloutInProgress", Message: fmt.Sprintf("%d/%d replicas ready", ready, desired),
		})
	default: // ready == 0 && desired > 0
		r.setPhaseForZeroReady(px, desired, &deploy)
	}
	return &deploy, nil
}

// setPhaseRunning sets the Running phase. The Degraded condition is cleared here
// only for workload reasons (ReplicaFailure) — config-token reasons are owned by
// clearConfigDegraded/checkTokenExpiry earlier in the same reconcile and must not
// be masked by a healthy Deployment.
func (r *Reconciler) setPhaseRunning(px *otilmv1alpha1.Proxy, ready, desired int32) {
	px.Status.Phase = otilmv1alpha1.ProxyPhaseRunning
	meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
		Type: condAvailable, Status: metav1.ConditionTrue, ObservedGeneration: px.Generation,
		Reason: "AllReplicasReady", Message: fmt.Sprintf("%d/%d replicas ready", ready, desired),
	})
	meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
		Type: condProgressing, Status: metav1.ConditionFalse, ObservedGeneration: px.Generation,
		Reason: "DeploymentComplete", Message: "Deployment rollout complete",
	})
	if c := meta.FindStatusCondition(px.Status.Conditions, condDegraded); c == nil ||
		c.Status == metav1.ConditionFalse || c.Reason == "ReplicaFailure" {
		meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
			Type: condDegraded, Status: metav1.ConditionFalse, ObservedGeneration: px.Generation,
			Reason: "Running", Message: "Proxy is running normally",
		})
	}
}

// setPhaseForZeroReady distinguishes a failing rollout from one still starting up.
func (r *Reconciler) setPhaseForZeroReady(px *otilmv1alpha1.Proxy, desired int32, deploy *appsv1.Deployment) {
	failed := false
	for _, c := range deploy.Status.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			failed = true
			break
		}
	}
	if failed {
		px.Status.Phase = otilmv1alpha1.ProxyPhaseFailed
		meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
			Type: condDegraded, Status: metav1.ConditionTrue, ObservedGeneration: px.Generation,
			Reason: "ReplicaFailure", Message: "Deployment pods are failing",
		})
		meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
			Type: condAvailable, Status: metav1.ConditionFalse, ObservedGeneration: px.Generation,
			Reason: "ReplicaFailure", Message: "Deployment pods are failing",
		})
	} else {
		px.Status.Phase = otilmv1alpha1.ProxyPhaseDeploying
		meta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
			Type: condProgressing, Status: metav1.ConditionTrue, ObservedGeneration: px.Generation,
			Reason: "WaitingForReplicas", Message: fmt.Sprintf("0/%d replicas ready", desired),
		})
	}
}

// SetupWithManager sets up the controller with the Manager. ServiceMonitor is
// deliberately NOT in Owns: a watch on a GVK whose CRD is not served fails its
// ListWatch and stalls the manager's cache sync (the same reason the Platform
// controller omits it); drift repair comes from periodic/triggered reconciles.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Capabilities == nil {
		r.Capabilities = capabilities.New(mgr.GetRESTMapper())
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&otilmv1alpha1.Proxy{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.findProxiesForSecret)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.findProxiesForConfigMap)).
		Complete(r)
}

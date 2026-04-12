/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder"
	"github.com/OmniTrustILM/operator/internal/checksum"
	"github.com/OmniTrustILM/operator/internal/monitoring"
	"github.com/OmniTrustILM/operator/internal/platform"
)

const (
	finalizerName       = "otilm.com/finalizer"
	requeueDelay        = 30 * time.Second
	registrationInitial = 5 * time.Second
	registrationMax     = 5 * time.Minute
	condAvailable       = "Available"
	condProgressing     = "Progressing"
	condDegraded        = "Degraded"
)

// ConnectorReconciler reconciles a Connector object.
type ConnectorReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=otilm.com,resources=connectors,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=otilm.com,resources=connectors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=otilm.com,resources=connectors/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile implements the 10-step reconciliation loop for Connector resources.
func (r *ConnectorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	startTime := time.Now()

	defer func() {
		monitoring.ReconciliationDurationSeconds.WithLabelValues(req.Name, req.Namespace).Observe(time.Since(startTime).Seconds())
	}()

	// ---------- Step 1: Fetch Connector CR ----------
	var conn otilmv1alpha1.Connector
	if err := r.Get(ctx, req.NamespacedName, &conn); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Connector resource not found, likely deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch Connector")
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return ctrl.Result{}, err
	}

	// ---------- Step 2: Finalizer management ----------
	if conn.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(&conn, finalizerName) {
			r.Recorder.Event(&conn, corev1.EventTypeNormal, monitoring.ReasonDeleting, "Connector is being deleted")
			controllerutil.RemoveFinalizer(&conn, finalizerName)
			if err := r.Update(ctx, &conn); err != nil {
				logger.Error(err, "failed to remove finalizer")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&conn, finalizerName) {
		controllerutil.AddFinalizer(&conn, finalizerName)
		if err := r.Update(ctx, &conn); err != nil {
			logger.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
		// Re-fetch after update to avoid conflicts.
		if err := r.Get(ctx, req.NamespacedName, &conn); err != nil {
			return ctrl.Result{}, err
		}
	}

	// ---------- Step 3: Set phase Deploying / Progressing ----------
	previousPhase := conn.Status.Phase
	if conn.Status.Phase == "" || conn.Status.ObservedGeneration != conn.Generation {
		conn.Status.Phase = otilmv1alpha1.ConnectorPhaseDeploying
		meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
			Type:               condProgressing,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: conn.Generation,
			Reason:             "Reconciling",
			Message:            "Connector resources are being reconciled",
		})
	}

	// ---------- Step 4: Compute checksums ----------
	checksums := make(map[string]string)
	for _, sr := range conn.Spec.SecretRefs {
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: sr.Name, Namespace: conn.Namespace}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("referenced Secret not found", "secret", sr.Name)
				meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
					Type:               condDegraded,
					Status:             metav1.ConditionTrue,
					ObservedGeneration: conn.Generation,
					Reason:             monitoring.ReasonMissingSecret,
					Message:            fmt.Sprintf("Secret %q not found", sr.Name),
				})
				conn.Status.Phase = otilmv1alpha1.ConnectorPhaseFailed
				r.Recorder.Eventf(&conn, corev1.EventTypeWarning, monitoring.ReasonMissingSecret, "Secret %q not found", sr.Name)
				if err := r.Status().Update(ctx, &conn); err != nil {
					logger.Error(err, "failed to update status for missing secret")
				}
				monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "requeue").Inc()
				return ctrl.Result{RequeueAfter: requeueDelay}, nil
			}
			return ctrl.Result{}, err
		}
		checksums["secret/"+sr.Name] = checksum.ComputeSecretChecksum(&secret)
	}

	for _, cmr := range conn.Spec.ConfigMapRefs {
		var cm corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Name: cmr.Name, Namespace: conn.Namespace}, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("referenced ConfigMap not found", "configmap", cmr.Name)
				meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
					Type:               condDegraded,
					Status:             metav1.ConditionTrue,
					ObservedGeneration: conn.Generation,
					Reason:             monitoring.ReasonMissingConfigMap,
					Message:            fmt.Sprintf("ConfigMap %q not found", cmr.Name),
				})
				conn.Status.Phase = otilmv1alpha1.ConnectorPhaseFailed
				r.Recorder.Eventf(&conn, corev1.EventTypeWarning, monitoring.ReasonMissingConfigMap, "ConfigMap %q not found", cmr.Name)
				if err := r.Status().Update(ctx, &conn); err != nil {
					logger.Error(err, "failed to update status for missing configmap")
				}
				monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "requeue").Inc()
				return ctrl.Result{RequeueAfter: requeueDelay}, nil
			}
			return ctrl.Result{}, err
		}
		checksums["configmap/"+cmr.Name] = checksum.ComputeConfigMapChecksum(&cm)
	}

	combinedChecksum := checksum.CombineChecksums(checksums)

	// ---------- Step 5: Reconcile child resources ----------

	// 5a. ServiceAccount
	desiredSA := builder.BuildServiceAccount(&conn)
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: desiredSA.Name, Namespace: desiredSA.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = desiredSA.Labels
		return ctrl.SetControllerReference(&conn, sa, r.Scheme)
	}); err != nil {
		logger.Error(err, "failed to reconcile ServiceAccount")
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return ctrl.Result{}, err
	}

	// 5b. Deployment
	desiredDeploy := builder.BuildDeployment(&conn, combinedChecksum)
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: desiredDeploy.Name, Namespace: desiredDeploy.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = desiredDeploy.Labels
		deploy.Spec.Replicas = desiredDeploy.Spec.Replicas
		deploy.Spec.Template = desiredDeploy.Spec.Template
		// Selector is immutable; only set on create.
		if deploy.Spec.Selector == nil {
			deploy.Spec.Selector = desiredDeploy.Spec.Selector
		}
		return ctrl.SetControllerReference(&conn, deploy, r.Scheme)
	}); err != nil {
		logger.Error(err, "failed to reconcile Deployment")
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return ctrl.Result{}, err
	}

	// 5c. Service
	desiredSvc := builder.BuildService(&conn)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: desiredSvc.Name, Namespace: desiredSvc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = desiredSvc.Labels
		svc.Spec.Type = desiredSvc.Spec.Type
		svc.Spec.Selector = desiredSvc.Spec.Selector
		svc.Spec.Ports = desiredSvc.Spec.Ports
		return ctrl.SetControllerReference(&conn, svc, r.Scheme)
	}); err != nil {
		logger.Error(err, "failed to reconcile Service")
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return ctrl.Result{}, err
	}

	// 5d. PDB
	desiredPDB := builder.BuildPDB(&conn)
	if desiredPDB != nil {
		pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: desiredPDB.Name, Namespace: desiredPDB.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
			pdb.Labels = desiredPDB.Labels
			pdb.Spec.MinAvailable = desiredPDB.Spec.MinAvailable
			pdb.Spec.Selector = desiredPDB.Spec.Selector
			return ctrl.SetControllerReference(&conn, pdb, r.Scheme)
		}); err != nil {
			logger.Error(err, "failed to reconcile PDB")
			monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
			return ctrl.Result{}, err
		}
	} else {
		// Delete PDB if it exists and is no longer needed.
		pdb := &policyv1.PodDisruptionBudget{}
		pdbKey := types.NamespacedName{Name: builder.ChildResourceName(&conn), Namespace: conn.Namespace}
		if err := r.Get(ctx, pdbKey, pdb); err == nil {
			if err := r.Delete(ctx, pdb); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to delete PDB")
				return ctrl.Result{}, err
			}
		}
	}

	// 5e. ServiceMonitor
	desiredSM := builder.BuildServiceMonitor(&conn)
	if desiredSM != nil {
		sm := &monitoringv1.ServiceMonitor{ObjectMeta: metav1.ObjectMeta{Name: desiredSM.Name, Namespace: desiredSM.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sm, func() error {
			sm.Labels = desiredSM.Labels
			sm.Spec = desiredSM.Spec
			return ctrl.SetControllerReference(&conn, sm, r.Scheme)
		}); err != nil {
			// If ServiceMonitor CRD is not installed, log a warning and continue.
			if meta.IsNoMatchError(err) {
				logger.Info("ServiceMonitor CRD not installed, skipping ServiceMonitor creation")
			} else {
				logger.Error(err, "failed to reconcile ServiceMonitor")
				monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
				return ctrl.Result{}, err
			}
		}
	} else {
		// Delete ServiceMonitor if it exists and is no longer needed.
		sm := &monitoringv1.ServiceMonitor{}
		smKey := types.NamespacedName{Name: builder.ChildResourceName(&conn), Namespace: conn.Namespace}
		if err := r.Get(ctx, smKey, sm); err == nil {
			if err := r.Delete(ctx, sm); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to delete ServiceMonitor")
				// Non-fatal: don't return error for optional resource.
			}
		}
	}

	// ---------- Step 6: Check Deployment status ----------
	var currentDeploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: builder.ChildResourceName(&conn), Namespace: conn.Namespace}, &currentDeploy); err != nil {
		logger.Error(err, "failed to get Deployment status")
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return ctrl.Result{}, err
	}

	var desiredReplicas int32 = 1
	if currentDeploy.Spec.Replicas != nil {
		desiredReplicas = *currentDeploy.Spec.Replicas
	}
	readyReplicas := currentDeploy.Status.ReadyReplicas

	switch {
	case readyReplicas == desiredReplicas && desiredReplicas > 0:
		// All replicas ready.
		conn.Status.Phase = otilmv1alpha1.ConnectorPhaseRunning
		meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
			Type:               condAvailable,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: conn.Generation,
			Reason:             "AllReplicasReady",
			Message:            fmt.Sprintf("%d/%d replicas ready", readyReplicas, desiredReplicas),
		})
		meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
			Type:               condProgressing,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: conn.Generation,
			Reason:             "DeploymentComplete",
			Message:            "Deployment rollout complete",
		})
		meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
			Type:               condDegraded,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: conn.Generation,
			Reason:             "Running",
			Message:            "Connector is running normally",
		})

	case readyReplicas < desiredReplicas && readyReplicas > 0:
		// Partially ready -- still updating.
		conn.Status.Phase = otilmv1alpha1.ConnectorPhaseUpdating
		meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
			Type:               condProgressing,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: conn.Generation,
			Reason:             "RolloutInProgress",
			Message:            fmt.Sprintf("%d/%d replicas ready", readyReplicas, desiredReplicas),
		})

	case readyReplicas == 0 && desiredReplicas > 0:
		// Check for failure conditions in the deployment.
		failed := false
		for _, cond := range currentDeploy.Status.Conditions {
			if cond.Type == appsv1.DeploymentReplicaFailure && cond.Status == corev1.ConditionTrue {
				failed = true
				break
			}
		}
		if failed {
			conn.Status.Phase = otilmv1alpha1.ConnectorPhaseFailed
			meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
				Type:               condDegraded,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: conn.Generation,
				Reason:             "ReplicaFailure",
				Message:            "Deployment pods are failing",
			})
		} else {
			// Not yet ready, but not explicitly failed -- still deploying.
			conn.Status.Phase = otilmv1alpha1.ConnectorPhaseDeploying
			meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
				Type:               condProgressing,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: conn.Generation,
				Reason:             "WaitingForReplicas",
				Message:            fmt.Sprintf("0/%d replicas ready", desiredReplicas),
			})
		}
	}

	// ---------- Step 7: Platform registration ----------
	if conn.Status.Phase == otilmv1alpha1.ConnectorPhaseRunning && conn.Spec.Registration != nil {
		needsRegistration := conn.Status.Registration == nil || conn.Status.Registration.UUID == ""
		if needsRegistration {
			endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", conn.Name, conn.Namespace, conn.Spec.Service.Port)
			regReq := platform.BuildRegistrationRequest(endpoint, conn.Spec.Registration)
			platformClient := platform.NewClient(conn.Spec.Registration.PlatformUrl)

			regResp, err := platform.Register(ctx, platformClient, regReq)
			if err != nil {
				var platformErr *platform.PlatformError
				if errors.As(err, &platformErr) {
					meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
						Type:               condDegraded,
						Status:             metav1.ConditionTrue,
						ObservedGeneration: conn.Generation,
						Reason:             monitoring.ReasonRegistrationFailed,
						Message:            fmt.Sprintf("Registration failed: %s", platformErr.Message),
					})
					r.Recorder.Eventf(&conn, corev1.EventTypeWarning, monitoring.ReasonRegistrationFailed, "Registration failed: %s", platformErr.Message)
					if platformErr.Retryable {
						// 5xx / network error: requeue with exponential backoff (5s → 5m).
						logger.Info("registration failed (retryable), will retry", "error", err)
						if err := r.Status().Update(ctx, &conn); err != nil {
							logger.Error(err, "failed to update status after registration failure")
						}
						monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "requeue").Inc()
						backoff := registrationBackoff(&conn)
						return ctrl.Result{RequeueAfter: backoff}, nil
					}
					// 4xx: don't requeue, manual intervention needed.
					logger.Info("registration failed (non-retryable), manual intervention needed", "error", err)
				} else {
					logger.Error(err, "registration failed with unexpected error")
					meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
						Type:               condDegraded,
						Status:             metav1.ConditionTrue,
						ObservedGeneration: conn.Generation,
						Reason:             monitoring.ReasonRegistrationFailed,
						Message:            fmt.Sprintf("Registration failed: %v", err),
					})
				}
			} else {
				now := metav1.Now()
				conn.Status.Registration = &otilmv1alpha1.RegistrationStatus{
					UUID:         regResp.UUID,
					Status:       otilmv1alpha1.RegistrationStatusValue(regResp.Status),
					RegisteredAt: &now,
				}
				r.Recorder.Eventf(&conn, corev1.EventTypeNormal, monitoring.ReasonRegistered,
					"Connector registered with platform, UUID: %s", regResp.UUID)
				logger.Info("connector registered with platform", "uuid", regResp.UUID)
			}
		}
	}

	// ---------- Step 8: Update CR status ----------
	conn.Status.ObservedGeneration = conn.Generation
	conn.Status.Replicas = currentDeploy.Status.Replicas
	conn.Status.ReadyReplicas = readyReplicas
	conn.Status.Endpoint = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", conn.Name, conn.Namespace, conn.Spec.Service.Port)
	conn.Status.CurrentImage = fmt.Sprintf("%s:%s", conn.Spec.Image.Repository, conn.Spec.Image.Tag)
	conn.Status.ConfigChecksum = combinedChecksum

	if err := r.Status().Update(ctx, &conn); err != nil {
		logger.Error(err, "failed to update Connector status")
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return ctrl.Result{}, err
	}

	// ---------- Step 9: Emit events ----------
	switch {
	case previousPhase == "" || previousPhase == otilmv1alpha1.ConnectorPhasePending:
		if conn.Status.Phase == otilmv1alpha1.ConnectorPhaseRunning {
			r.Recorder.Event(&conn, corev1.EventTypeNormal, monitoring.ReasonDeployed, "Connector deployed successfully")
		}
	case previousPhase == otilmv1alpha1.ConnectorPhaseFailed || previousPhase == otilmv1alpha1.ConnectorPhaseUpdating || previousPhase == otilmv1alpha1.ConnectorPhaseDeploying:
		if conn.Status.Phase == otilmv1alpha1.ConnectorPhaseRunning {
			r.Recorder.Event(&conn, corev1.EventTypeNormal, monitoring.ReasonRecovered, "Connector recovered and is now running")
		}
	}
	if previousPhase != "" && previousPhase != conn.Status.Phase && conn.Status.Phase == otilmv1alpha1.ConnectorPhaseUpdating {
		r.Recorder.Event(&conn, corev1.EventTypeNormal, monitoring.ReasonUpdated, "Connector spec changed, updating")
	}
	if conn.Status.Phase == otilmv1alpha1.ConnectorPhaseFailed {
		r.Recorder.Event(&conn, corev1.EventTypeWarning, monitoring.ReasonDegraded, "Connector is in a degraded state")
	}

	// ---------- Step 10: Determine requeue ----------
	monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "success").Inc()

	if conn.Status.Phase != otilmv1alpha1.ConnectorPhaseRunning {
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	return ctrl.Result{}, nil
}

// registrationBackoff computes exponential backoff for registration retries.
// Uses the registration status to determine retry count via the Degraded condition's
// last transition time. Backoff: 5s, 10s, 20s, 40s, 80s, 160s, 300s (capped at 5m).
func registrationBackoff(conn *otilmv1alpha1.Connector) time.Duration {
	for _, c := range conn.Status.Conditions {
		if c.Type == condDegraded && c.Status == metav1.ConditionTrue && c.Reason == monitoring.ReasonRegistrationFailed {
			elapsed := time.Since(c.LastTransitionTime.Time)
			// Double the backoff based on how long we've been in degraded state
			backoff := registrationInitial
			for backoff < elapsed && backoff < registrationMax {
				backoff *= 2
			}
			if backoff > registrationMax {
				backoff = registrationMax
			}
			return backoff
		}
	}
	return registrationInitial
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConnectorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&otilmv1alpha1.Connector{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.findConnectorsForSecret)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.findConnectorsForConfigMap)).
		Complete(r)
}

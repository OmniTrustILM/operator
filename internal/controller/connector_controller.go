/*
Copyright 2026 OmniTrust ILM.

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

// Package controller implements the Kubernetes reconciler for Connector custom resources.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	conn, err := r.fetchConnector(ctx, req)
	if conn == nil {
		return ctrl.Result{}, err
	}

	// ---------- Step 2: Finalizer management ----------
	done, err := r.handleFinalizer(ctx, req, conn)
	if done || err != nil {
		return ctrl.Result{}, err
	}

	// ---------- Step 3: Set phase Deploying / Progressing ----------
	previousPhase := r.setInitialPhase(conn)

	// ---------- Step 4: Compute checksums ----------
	combinedChecksum, requeueResult, err := r.computeChecksums(ctx, req, conn)
	if requeueResult != nil {
		return *requeueResult, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// ---------- Step 5: Reconcile child resources ----------
	if err := r.reconcileChildResources(ctx, req, conn, combinedChecksum); err != nil {
		return ctrl.Result{}, err
	}

	// ---------- Step 6: Check Deployment status ----------
	currentDeploy, err := r.updateDeploymentStatus(ctx, req, conn)
	if err != nil {
		return ctrl.Result{}, err
	}

	// ---------- Step 7: Platform registration ----------
	if result, err := r.handleRegistration(ctx, req, conn); err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	// ---------- Step 8: Update CR status ----------
	conn.Status.ObservedGeneration = conn.Generation
	conn.Status.Replicas = currentDeploy.Status.Replicas
	conn.Status.ReadyReplicas = currentDeploy.Status.ReadyReplicas
	conn.Status.Endpoint = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", conn.Name, conn.Namespace, conn.Spec.Service.Port)
	conn.Status.CurrentImage = fmt.Sprintf("%s:%s", conn.Spec.Image.Repository, conn.Spec.Image.Tag)
	conn.Status.ConfigChecksum = combinedChecksum

	if err := r.Status().Update(ctx, conn); err != nil {
		logger.Error(err, "failed to update Connector status")
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return ctrl.Result{}, err
	}

	// ---------- Step 9: Emit events ----------
	r.emitPhaseTransitionEvents(conn, previousPhase)

	// ---------- Step 10: Determine requeue ----------
	monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "success").Inc()

	if conn.Status.Phase != otilmv1alpha1.ConnectorPhaseRunning {
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	return ctrl.Result{}, nil
}

// fetchConnector retrieves the Connector CR. Returns nil conn if the resource is
// not found (deleted) or on error.
func (r *ConnectorReconciler) fetchConnector(ctx context.Context, req ctrl.Request) (*otilmv1alpha1.Connector, error) {
	logger := log.FromContext(ctx)
	var conn otilmv1alpha1.Connector
	if err := r.Get(ctx, req.NamespacedName, &conn); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Connector resource not found, likely deleted")
			return nil, nil
		}
		logger.Error(err, "unable to fetch Connector")
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return nil, err
	}
	return &conn, nil
}

// handleFinalizer manages adding/removing the finalizer. Returns done=true when
// the Connector is being deleted and no further reconciliation is needed.
func (r *ConnectorReconciler) handleFinalizer(ctx context.Context, req ctrl.Request, conn *otilmv1alpha1.Connector) (bool, error) {
	logger := log.FromContext(ctx)

	if conn.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(conn, finalizerName) {
			r.Recorder.Event(conn, corev1.EventTypeNormal, monitoring.ReasonDeleting, "Connector is being deleted")
			monitoring.ConnectorsManaged.Dec()
			controllerutil.RemoveFinalizer(conn, finalizerName)
			if err := r.Update(ctx, conn); err != nil {
				logger.Error(err, "failed to remove finalizer")
				monitoring.ConnectorsManaged.Inc() // restore on failure
				return false, err
			}
		}
		return true, nil
	}

	if !controllerutil.ContainsFinalizer(conn, finalizerName) {
		controllerutil.AddFinalizer(conn, finalizerName)
		if err := r.Update(ctx, conn); err != nil {
			logger.Error(err, "failed to add finalizer")
			return false, err
		}
		// Re-fetch after update to avoid conflicts.
		if err := r.Get(ctx, req.NamespacedName, conn); err != nil {
			return false, err
		}
	}

	return false, nil
}

// setInitialPhase sets the Deploying phase and Progressing condition when the
// Connector is first seen or its generation changes. Returns the previous phase.
func (r *ConnectorReconciler) setInitialPhase(conn *otilmv1alpha1.Connector) otilmv1alpha1.ConnectorPhase {
	previousPhase := conn.Status.Phase
	if conn.Status.Phase == "" {
		// First time we've seen this Connector -- count it.
		monitoring.ConnectorsManaged.Inc()
	}
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
	return previousPhase
}

// computeChecksums computes checksums for all referenced Secrets and ConfigMaps.
func (r *ConnectorReconciler) computeChecksums(ctx context.Context, req ctrl.Request, conn *otilmv1alpha1.Connector) (string, *ctrl.Result, error) {
	checksums := make(map[string]string)

	secretNames := make([]string, len(conn.Spec.SecretRefs))
	for i, sr := range conn.Spec.SecretRefs {
		secretNames[i] = sr.Name
	}
	secretResults, requeueResult, err := r.computeRefChecksums(ctx, conn, "Secret", secretNames, func(name string) (string, error) {
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: conn.Namespace}, &secret); err != nil {
			return "", err
		}
		return checksum.ComputeSecretChecksum(&secret), nil
	}, monitoring.ReasonMissingSecret)
	if requeueResult != nil {
		return "", requeueResult, nil
	}
	if err != nil {
		return "", nil, err
	}
	for _, sr := range secretResults {
		checksums[sr.key] = sr.checksum
	}

	cmNames := make([]string, len(conn.Spec.ConfigMapRefs))
	for i, cmr := range conn.Spec.ConfigMapRefs {
		cmNames[i] = cmr.Name
	}
	cmResults, requeueResult, err := r.computeRefChecksums(ctx, conn, "ConfigMap", cmNames, func(name string) (string, error) {
		var cm corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: conn.Namespace}, &cm); err != nil {
			return "", err
		}
		return checksum.ComputeConfigMapChecksum(&cm), nil
	}, monitoring.ReasonMissingConfigMap)
	if requeueResult != nil {
		return "", requeueResult, nil
	}
	if err != nil {
		return "", nil, err
	}
	for _, cr := range cmResults {
		checksums[cr.key] = cr.checksum
	}

	_ = req // used by caller for metrics labels
	return checksum.CombineChecksums(checksums), nil, nil
}

// reconcileChildResources reconciles the five child resources (SA, Deployment,
// Service, PDB, ServiceMonitor) for the given Connector.
func (r *ConnectorReconciler) reconcileChildResources(ctx context.Context, req ctrl.Request, conn *otilmv1alpha1.Connector, combinedChecksum string) error {
	if err := r.reconcileServiceAccount(ctx, conn); err != nil {
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return err
	}
	if err := r.reconcileDeployment(ctx, conn, combinedChecksum); err != nil {
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return err
	}
	if err := r.reconcileService(ctx, conn); err != nil {
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return err
	}
	if err := r.reconcilePDB(ctx, conn); err != nil {
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return err
	}
	if err := r.reconcileServiceMonitor(ctx, conn); err != nil {
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return err
	}
	return nil
}

// reconcileServiceAccount creates or updates the ServiceAccount for the Connector.
func (r *ConnectorReconciler) reconcileServiceAccount(ctx context.Context, conn *otilmv1alpha1.Connector) error {
	logger := log.FromContext(ctx)
	desiredSA := builder.BuildServiceAccount(conn)
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: desiredSA.Name, Namespace: desiredSA.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = desiredSA.Labels
		return ctrl.SetControllerReference(conn, sa, r.Scheme)
	}); err != nil {
		logger.Error(err, "failed to reconcile ServiceAccount")
		return err
	}
	return nil
}

// reconcileDeployment creates or updates the Deployment for the Connector.
func (r *ConnectorReconciler) reconcileDeployment(ctx context.Context, conn *otilmv1alpha1.Connector, combinedChecksum string) error {
	logger := log.FromContext(ctx)
	desiredDeploy := builder.BuildDeployment(conn, combinedChecksum)
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: desiredDeploy.Name, Namespace: desiredDeploy.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = desiredDeploy.Labels
		deploy.Spec.Replicas = desiredDeploy.Spec.Replicas
		deploy.Spec.Template = desiredDeploy.Spec.Template
		// Selector is immutable; only set on create.
		if deploy.Spec.Selector == nil {
			deploy.Spec.Selector = desiredDeploy.Spec.Selector
		}
		return ctrl.SetControllerReference(conn, deploy, r.Scheme)
	}); err != nil {
		logger.Error(err, "failed to reconcile Deployment")
		return err
	}
	return nil
}

// reconcileService creates or updates the Service for the Connector.
func (r *ConnectorReconciler) reconcileService(ctx context.Context, conn *otilmv1alpha1.Connector) error {
	logger := log.FromContext(ctx)
	desiredSvc := builder.BuildService(conn)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: desiredSvc.Name, Namespace: desiredSvc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = desiredSvc.Labels
		svc.Spec.Type = desiredSvc.Spec.Type
		svc.Spec.Selector = desiredSvc.Spec.Selector
		svc.Spec.Ports = desiredSvc.Spec.Ports
		return ctrl.SetControllerReference(conn, svc, r.Scheme)
	}); err != nil {
		logger.Error(err, "failed to reconcile Service")
		return err
	}
	return nil
}

// reconcilePDB creates, updates, or deletes the PodDisruptionBudget for the Connector.
func (r *ConnectorReconciler) reconcilePDB(ctx context.Context, conn *otilmv1alpha1.Connector) error {
	logger := log.FromContext(ctx)
	desiredPDB := builder.BuildPDB(conn)
	if desiredPDB != nil {
		pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: desiredPDB.Name, Namespace: desiredPDB.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
			pdb.Labels = desiredPDB.Labels
			pdb.Spec.MinAvailable = desiredPDB.Spec.MinAvailable
			pdb.Spec.Selector = desiredPDB.Spec.Selector
			return ctrl.SetControllerReference(conn, pdb, r.Scheme)
		}); err != nil {
			logger.Error(err, "failed to reconcile PDB")
			return err
		}
		return nil
	}

	// Delete PDB if it exists and is no longer needed.
	pdb := &policyv1.PodDisruptionBudget{}
	pdbKey := types.NamespacedName{Name: builder.ChildResourceName(conn), Namespace: conn.Namespace}
	if err := r.Get(ctx, pdbKey, pdb); err == nil {
		if err := r.Delete(ctx, pdb); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to delete PDB")
			return err
		}
	}
	return nil
}

// reconcileServiceMonitor creates, updates, or deletes the ServiceMonitor for the Connector.
func (r *ConnectorReconciler) reconcileServiceMonitor(ctx context.Context, conn *otilmv1alpha1.Connector) error {
	logger := log.FromContext(ctx)
	desiredSM := builder.BuildServiceMonitor(conn)
	if desiredSM != nil {
		sm := &monitoringv1.ServiceMonitor{ObjectMeta: metav1.ObjectMeta{Name: desiredSM.Name, Namespace: desiredSM.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sm, func() error {
			sm.Labels = desiredSM.Labels
			sm.Spec = desiredSM.Spec
			return ctrl.SetControllerReference(conn, sm, r.Scheme)
		}); err != nil {
			// If ServiceMonitor CRD is not installed, log a warning and continue.
			if meta.IsNoMatchError(err) {
				logger.Info("ServiceMonitor CRD not installed, skipping ServiceMonitor creation")
				return nil
			}
			logger.Error(err, "failed to reconcile ServiceMonitor")
			return err
		}
		return nil
	}

	// Delete ServiceMonitor if it exists and is no longer needed.
	sm := &monitoringv1.ServiceMonitor{}
	smKey := types.NamespacedName{Name: builder.ChildResourceName(conn), Namespace: conn.Namespace}
	if err := r.Get(ctx, smKey, sm); err == nil {
		if err := r.Delete(ctx, sm); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to delete ServiceMonitor")
			// Non-fatal: don't return error for optional resource.
		}
	}
	return nil
}

// updateDeploymentStatus checks the Deployment status and sets the appropriate
// phase and conditions on the Connector. Returns the current Deployment.
func (r *ConnectorReconciler) updateDeploymentStatus(ctx context.Context, req ctrl.Request, conn *otilmv1alpha1.Connector) (*appsv1.Deployment, error) {
	logger := log.FromContext(ctx)
	var currentDeploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: builder.ChildResourceName(conn), Namespace: conn.Namespace}, &currentDeploy); err != nil {
		logger.Error(err, "failed to get Deployment status")
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "error").Inc()
		return nil, err
	}

	var desiredReplicas int32 = 1
	if currentDeploy.Spec.Replicas != nil {
		desiredReplicas = *currentDeploy.Spec.Replicas
	}
	readyReplicas := currentDeploy.Status.ReadyReplicas

	r.setDeploymentPhase(conn, desiredReplicas, readyReplicas, &currentDeploy)

	return &currentDeploy, nil
}

// setDeploymentPhase updates phase and conditions based on replica counts.
func (r *ConnectorReconciler) setDeploymentPhase(conn *otilmv1alpha1.Connector, desiredReplicas, readyReplicas int32, deploy *appsv1.Deployment) {
	switch {
	case readyReplicas == desiredReplicas && desiredReplicas > 0:
		r.setPhaseRunning(conn, readyReplicas, desiredReplicas)

	case readyReplicas < desiredReplicas && readyReplicas > 0:
		conn.Status.Phase = otilmv1alpha1.ConnectorPhaseUpdating
		meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
			Type:               condProgressing,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: conn.Generation,
			Reason:             "RolloutInProgress",
			Message:            fmt.Sprintf("%d/%d replicas ready", readyReplicas, desiredReplicas),
		})

	case readyReplicas == 0 && desiredReplicas > 0:
		r.setPhaseForZeroReady(conn, desiredReplicas, deploy)
	}
}

// setPhaseRunning sets the phase to Running with all associated conditions.
func (r *ConnectorReconciler) setPhaseRunning(conn *otilmv1alpha1.Connector, readyReplicas, desiredReplicas int32) {
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
}

// setPhaseForZeroReady sets phase when no replicas are ready — either Failed
// (when ReplicaFailure detected) or Deploying (still starting up).
func (r *ConnectorReconciler) setPhaseForZeroReady(conn *otilmv1alpha1.Connector, desiredReplicas int32, deploy *appsv1.Deployment) {
	failed := false
	for _, cond := range deploy.Status.Conditions {
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

// handleRegistration performs platform registration when the Connector is
// Running and has a registration spec.
func (r *ConnectorReconciler) handleRegistration(ctx context.Context, req ctrl.Request, conn *otilmv1alpha1.Connector) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if conn.Status.Phase != otilmv1alpha1.ConnectorPhaseRunning || conn.Spec.Registration == nil {
		return ctrl.Result{}, nil
	}

	needsRegistration := conn.Status.Registration == nil || conn.Status.Registration.UUID == ""
	if !needsRegistration {
		return ctrl.Result{}, nil
	}

	endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", conn.Name, conn.Namespace, conn.Spec.Service.Port)
	regReq := platform.BuildRegistrationRequest(endpoint, conn.Spec.Registration)
	platformClient := platform.NewClient(conn.Spec.Registration.PlatformURL)

	regResp, err := platform.Register(ctx, platformClient, regReq)
	if err != nil {
		return r.handleRegistrationError(ctx, req, conn, err)
	}

	now := metav1.Now()
	conn.Status.Registration = &otilmv1alpha1.RegistrationStatus{
		UUID:         regResp.UUID,
		Status:       otilmv1alpha1.RegistrationStatusValue(regResp.Status),
		RegisteredAt: &now,
	}
	r.Recorder.Eventf(conn, corev1.EventTypeNormal, monitoring.ReasonRegistered,
		"Connector registered with platform, UUID: %s", regResp.UUID)
	logger.Info("connector registered with platform", "uuid", regResp.UUID)

	return ctrl.Result{}, nil
}

// handleRegistrationError processes registration errors, setting conditions
// and determining whether to requeue.
func (r *ConnectorReconciler) handleRegistrationError(ctx context.Context, req ctrl.Request, conn *otilmv1alpha1.Connector, err error) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var platformErr *platform.Error
	if !errors.As(err, &platformErr) {
		logger.Error(err, "registration failed with unexpected error")
		meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
			Type:               condDegraded,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: conn.Generation,
			Reason:             monitoring.ReasonRegistrationFailed,
			Message:            fmt.Sprintf("Registration failed: %v", err),
		})
		return ctrl.Result{}, nil
	}

	meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
		Type:               condDegraded,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: conn.Generation,
		Reason:             monitoring.ReasonRegistrationFailed,
		Message:            fmt.Sprintf("Registration failed: %s", platformErr.Message),
	})
	r.Recorder.Eventf(conn, corev1.EventTypeWarning, monitoring.ReasonRegistrationFailed, "Registration failed: %s", platformErr.Message)

	if platformErr.Retryable {
		// 5xx / network error: requeue with exponential backoff (5s -> 5m).
		logger.Info("registration failed (retryable), will retry", "error", err)
		if statusErr := r.Status().Update(ctx, conn); statusErr != nil {
			logger.Error(statusErr, "failed to update status after registration failure")
		}
		monitoring.ReconciliationsTotal.WithLabelValues(req.Name, req.Namespace, "requeue").Inc()
		backoff := registrationBackoff(conn)
		return ctrl.Result{RequeueAfter: backoff}, nil
	}

	// 4xx: don't requeue, manual intervention needed.
	logger.Info("registration failed (non-retryable), manual intervention needed", "error", err)
	return ctrl.Result{}, nil
}

// emitPhaseTransitionEvents emits Kubernetes events for notable phase transitions.
func (r *ConnectorReconciler) emitPhaseTransitionEvents(conn *otilmv1alpha1.Connector, previousPhase otilmv1alpha1.ConnectorPhase) {
	switch previousPhase {
	case "", otilmv1alpha1.ConnectorPhasePending:
		if conn.Status.Phase == otilmv1alpha1.ConnectorPhaseRunning {
			r.Recorder.Event(conn, corev1.EventTypeNormal, monitoring.ReasonDeployed, "Connector deployed successfully")
		}
	case otilmv1alpha1.ConnectorPhaseFailed, otilmv1alpha1.ConnectorPhaseUpdating, otilmv1alpha1.ConnectorPhaseDeploying:
		if conn.Status.Phase == otilmv1alpha1.ConnectorPhaseRunning {
			r.Recorder.Event(conn, corev1.EventTypeNormal, monitoring.ReasonRecovered, "Connector recovered and is now running")
		}
	}
	if previousPhase != "" && previousPhase != conn.Status.Phase && conn.Status.Phase == otilmv1alpha1.ConnectorPhaseUpdating {
		r.Recorder.Event(conn, corev1.EventTypeNormal, monitoring.ReasonUpdated, "Connector spec changed, updating")
	}
	if conn.Status.Phase == otilmv1alpha1.ConnectorPhaseFailed {
		r.Recorder.Event(conn, corev1.EventTypeWarning, monitoring.ReasonDegraded, "Connector is in a degraded state")
	}
}

// checksumResult holds the outcome of a single ref checksum computation.
type checksumResult struct {
	key      string
	checksum string
}

// computeRefChecksums fetches each referenced object and computes its checksum.
// refKind is "Secret" or "ConfigMap" (used for log messages and status).
// names are the ref names, fetchAndChecksum fetches the object and returns its checksum.
func (r *ConnectorReconciler) computeRefChecksums(
	ctx context.Context,
	conn *otilmv1alpha1.Connector,
	refKind string,
	names []string,
	fetchAndChecksum func(name string) (string, error),
	missingReason string,
) ([]checksumResult, *ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var results []checksumResult

	for _, name := range names {
		cs, err := fetchAndChecksum(name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("referenced "+refKind+" not found", strings.ToLower(refKind), name)
				meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
					Type:               condDegraded,
					Status:             metav1.ConditionTrue,
					ObservedGeneration: conn.Generation,
					Reason:             missingReason,
					Message:            fmt.Sprintf("%s %q not found", refKind, name),
				})
				conn.Status.Phase = otilmv1alpha1.ConnectorPhaseFailed
				r.Recorder.Eventf(conn, corev1.EventTypeWarning, missingReason, "%s %q not found", refKind, name)
				if statusErr := r.Status().Update(ctx, conn); statusErr != nil {
					logger.Error(statusErr, "failed to update status for missing "+strings.ToLower(refKind))
				}
				monitoring.ReconciliationsTotal.WithLabelValues(conn.Name, conn.Namespace, "requeue").Inc()
				return nil, &ctrl.Result{RequeueAfter: requeueDelay}, nil
			}
			return nil, nil, err
		}
		results = append(results, checksumResult{key: strings.ToLower(refKind) + "/" + name, checksum: cs})
	}
	return results, nil, nil
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

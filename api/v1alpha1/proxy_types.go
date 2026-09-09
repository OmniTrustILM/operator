/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProxyPhase represents the current phase of the Proxy.
type ProxyPhase string

// Proxy lifecycle phase constants.
const (
	ProxyPhasePending   ProxyPhase = "Pending"
	ProxyPhaseDeploying ProxyPhase = "Deploying"
	ProxyPhaseRunning   ProxyPhase = "Running"
	ProxyPhaseFailed    ProxyPhase = "Failed"
	ProxyPhaseUpdating  ProxyPhase = "Updating"
	// ProxyPhaseScaledDown is the settled state of a deliberately paused proxy
	// (spec.replicas: 0): not an error, not progressing.
	ProxyPhaseScaledDown ProxyPhase = "ScaledDown"
)

// ConfigTokenRef locates the provisioning-issued config token inside a Secret in the
// Proxy's namespace. The token is a JWT whose config claim carries the proxy's ENTIRE
// configuration (broker URL, queue coordinates, credentials, tuning); the operator
// injects it as PROXY_CONFIG_TOKEN via secretKeyRef and never reads its config claims.
// Key-name defaults match the proxy Helm chart's secret template so both install
// paths share one contract.
//
// SECURITY: the token embeds broker credentials — only a Secret NAME and key NAMES
// are held here, never the token value.
type ConfigTokenRef struct {
	// Name is the name of the Secret holding the config token.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// TokenKey is the Secret key holding the JWT config token.
	// +kubebuilder:default="configToken"
	// +optional
	TokenKey string `json:"tokenKey,omitempty"`

	// SigningKeyKey is the Secret key holding the optional HMAC signing key, injected
	// as PROXY_TOKEN_SIGNING_KEY via an optional secretKeyRef (the proxy verifies the
	// token signature only when the key is present in the Secret).
	// +kubebuilder:default="tokenSigningKey"
	// +optional
	SigningKeyKey string `json:"signingKeyKey,omitempty"`
}

// ProxyMetricsSpec configures the proxy's metrics endpoint and an optional Prometheus
// ServiceMonitor. It is a proxy-local block (not the shared MetricsSpec) because the
// proxy serves metrics at /metrics while the shared type's API-server default is
// /v1/metrics — an admission-applied default the builder could not distinguish from
// user intent (see docs/design/proxy-operator.md, metrics note).
type ProxyMetricsSpec struct {
	// Enabled indicates whether metrics are enabled.
	Enabled bool `json:"enabled"`

	// Path is the HTTP path of the metrics endpoint.
	// +kubebuilder:default="/metrics"
	// +optional
	Path *string `json:"path,omitempty"`

	// ServiceMonitor defines the ServiceMonitor configuration. Rendering is gated on
	// the Prometheus operator CRD being served (adjunct ServiceMonitorReady condition).
	// +optional
	ServiceMonitor *ServiceMonitorSpec `json:"serviceMonitor,omitempty"`
}

// ProxySpec defines the desired state of Proxy: one ILM proxy instance deployed from
// a provisioning-issued config token. All broker configuration travels inside the
// token; the CRD deliberately models none of it.
type ProxySpec struct {
	// ConfigTokenSecretRef names the Secret holding the proxy's config token (and,
	// when token signing is enabled, the signing key).
	// +kubebuilder:validation:Required
	ConfigTokenSecretRef ConfigTokenRef `json:"configTokenSecretRef"`

	// Image optionally overrides the BOM-resolved proxy image, per field.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`

	// Replicas is the number of desired replicas.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Resources defines the compute resource requirements.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Env sets additional process-level environment on the proxy container — e.g.
	// HTTPS_PROXY/HTTP_PROXY/NO_PROXY for corporate egress, which the token cannot
	// know. It is NOT a proxy-configuration channel: in token mode the proxy ignores
	// PROXY_* viper env overrides entirely. Never put secrets here — use SecretRefs.
	// +optional
	Env []EnvVar `json:"env,omitempty"`

	// SecretRefs references Kubernetes Secrets consumed as env (envFrom / secretKeyRef
	// with key mapping) or mounted as volumes — e.g. a private-PKI CA bundle for the
	// broker TLS connection. Values are projected by reference, never copied. The
	// operator-reserved env names (PROXY_CONFIG_TOKEN, PROXY_TOKEN_SIGNING_KEY) cannot
	// be remapped through a ref: the config-token wiring always wins.
	// +optional
	SecretRefs []SecretRef `json:"secretRefs,omitempty"`

	// ConfigMapRefs references Kubernetes ConfigMaps consumed as env or mounted as
	// volumes, mirroring the Connector CR.
	// +optional
	ConfigMapRefs []ConfigMapRef `json:"configMapRefs,omitempty"`

	// Volumes defines additional emptyDir volumes mounted into the proxy container.
	// +optional
	Volumes []VolumeSpec `json:"volumes,omitempty"`

	// SecurityContext tunes the per-workload runtime decisions of the container
	// security context (readOnlyRootFilesystem). The SCC-critical fields
	// (runAsNonRoot, no privilege escalation, dropped capabilities, seccomp) are
	// hardened by the shared builders and cannot be weakened from the CR.
	// +optional
	SecurityContext *SecurityContextSpec `json:"securityContext,omitempty"`

	// TerminationGracePeriodSeconds is the duration the pod needs to terminate
	// gracefully (in-flight broker messages drain before shutdown).
	// +optional
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// NodeSelector constrains the proxy pods to nodes with matching labels — e.g.
	// nodes permitted outbound egress in a segmented restricted zone.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allow the proxy pods to schedule onto tainted nodes (e.g. dedicated
	// egress nodes).
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// InitContainers are extra init containers run before the main container (e.g.
	// wait-for-dependency). They are SCC-hardened like every other container, so a
	// user init container cannot weaken pod security. The schema is preserved
	// opaquely (x-kubernetes-preserve-unknown-fields) to keep the CRD compact; the
	// Go type stays []corev1.Container so it marshals correctly and the kubelet
	// validates it.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	InitContainers []corev1.Container `json:"initContainers,omitempty"`

	// Sidecars are extra containers run alongside the main container (e.g. a vault
	// agent or log shipper). They are SCC-hardened like every other container. The
	// schema is preserved opaquely for the same CRD-size reason as InitContainers.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Sidecars []corev1.Container `json:"sidecars,omitempty"`

	// Affinity sets the pod's affinity/anti-affinity and node affinity rules. The
	// schema is preserved opaquely (x-kubernetes-preserve-unknown-fields) to keep
	// the CRD compact; the apiserver/kubelet still validate it as a PodSpec field.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// ServiceAccount overrides the dedicated ServiceAccount's name (e.g. to bind a
	// pre-created workload-identity ServiceAccount) and stamps extra annotations on
	// it (e.g. an AWS IRSA role-arn or a GCP/Azure workload-identity binding).
	// +optional
	ServiceAccount *ServiceAccountSpec `json:"serviceAccount,omitempty"`

	// Probes overrides the proxy's liveness/readiness/startup probes. Defaults match
	// the proxy's endpoints: /health (liveness, startup) and /ready (readiness) on
	// the HTTP port.
	// +optional
	Probes *ProbeSpec `json:"probes,omitempty"`

	// Metrics defines the metrics configuration.
	// +optional
	Metrics *ProxyMetricsSpec `json:"metrics,omitempty"`

	// PodDisruptionBudget defines an optional PDB for the proxy pods.
	// +optional
	PodDisruptionBudget *PDBSpec `json:"podDisruptionBudget,omitempty"`

	// PodAnnotations are arbitrary annotations added to the proxy pod template.
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// PodLabels are arbitrary labels added to the proxy pod template. They are merged
	// with the operator-managed labels; operator labels take precedence.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`
}

// ProxyStatus defines the observed state of Proxy. Conditions and fields carry names,
// phases, reasons, and checksums — never secret values or connection coordinates.
type ProxyStatus struct {
	// Phase is the current phase of the Proxy.
	// +optional
	Phase ProxyPhase `json:"phase,omitempty"`

	// ObservedGeneration is the most recent generation observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ReadyReplicas is the number of ready replicas.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ObservedVersion is the resolved proxy image tag (BOM default or spec.image override).
	// +optional
	ObservedVersion string `json:"observedVersion,omitempty"`

	// ConfigChecksum is a checksum of the referenced config-token Secret.
	// +optional
	ConfigChecksum string `json:"configChecksum,omitempty"`

	// Conditions represent the latest available observations of the Proxy's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.observedVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=prx
// +operator-sdk:csv:customresourcedefinitions:displayName="Proxy"
// +operator-sdk:csv:customresourcedefinitions:resources={{Deployments,apps/v1},{Services,v1},{ServiceAccounts,v1},{PodDisruptionBudgets,policy/v1}}

// Proxy is the Schema for the proxies API. It deploys one ILM proxy instance — the
// outbound-only broker bridge for restricted network zones — from a
// provisioning-issued config token.
type Proxy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProxySpec   `json:"spec,omitempty"`
	Status ProxyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProxyList contains a list of Proxy.
type ProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Proxy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Proxy{}, &ProxyList{})
}

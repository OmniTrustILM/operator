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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ConnectorPhase represents the current phase of the Connector.
type ConnectorPhase string

// Connector lifecycle phase constants.
const (
	ConnectorPhasePending   ConnectorPhase = "Pending"
	ConnectorPhaseDeploying ConnectorPhase = "Deploying"
	ConnectorPhaseRunning   ConnectorPhase = "Running"
	ConnectorPhaseFailed    ConnectorPhase = "Failed"
	ConnectorPhaseUpdating  ConnectorPhase = "Updating"
)

// RefType defines how a secret or configmap is referenced.
// +kubebuilder:validation:Enum=env;volume
type RefType string

// Reference type constants for secrets and configmaps.
const (
	RefTypeEnv    RefType = "env"
	RefTypeVolume RefType = "volume"
)

// AuthType defines the authentication type for connector registration.
// +kubebuilder:validation:Enum=none;basic;certificate;apiKey;jwt
type AuthType string

// Authentication type constants for connector registration.
const (
	AuthTypeNone        AuthType = "none"
	AuthTypeBasic       AuthType = "basic"
	AuthTypeCertificate AuthType = "certificate"
	AuthTypeAPIKey      AuthType = "apiKey"
	AuthTypeJWT         AuthType = "jwt"
)

// ImageSpec defines the container image configuration.
type ImageSpec struct {
	// Repository is the container image repository.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`

	// Tag is the container image tag.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Tag string `json:"tag"`

	// PullPolicy defines the image pull policy.
	// +kubebuilder:default="IfNotPresent"
	// +optional
	PullPolicy string `json:"pullPolicy,omitempty"`

	// PullSecrets is a list of secret names for pulling the image.
	// +optional
	PullSecrets []string `json:"pullSecrets,omitempty"`
}

// ServiceSpec defines the service configuration for the connector.
type ServiceSpec struct {
	// Port is the service port.
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// Type is the Kubernetes service type.
	// +kubebuilder:default=ClusterIP
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +optional
	Type string `json:"type,omitempty"`
}

// SecurityContextSpec defines security context settings for the connector pod.
type SecurityContextSpec struct {
	// RunAsNonRoot indicates that the container must run as a non-root user.
	// +kubebuilder:default=true
	// +optional
	RunAsNonRoot *bool `json:"runAsNonRoot,omitempty"`

	// ReadOnlyRootFilesystem indicates that the container has a read-only root filesystem.
	// +kubebuilder:default=true
	// +optional
	ReadOnlyRootFilesystem *bool `json:"readOnlyRootFilesystem,omitempty"`
}

// ProbeConfig defines the configuration for a single probe.
type ProbeConfig struct {
	// Path is the HTTP path to probe.
	// +optional
	Path string `json:"path,omitempty"`

	// InitialDelaySeconds is the number of seconds after the container starts before the probe is initiated.
	// +optional
	InitialDelaySeconds int32 `json:"initialDelaySeconds,omitempty"`

	// PeriodSeconds is how often (in seconds) to perform the probe.
	// +optional
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`

	// FailureThreshold is the number of consecutive failures before the probe is considered failed.
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
}

// ProbeSpec defines the probe configuration for the connector.
type ProbeSpec struct {
	// Liveness defines the liveness probe configuration.
	// +optional
	Liveness *ProbeConfig `json:"liveness,omitempty"`

	// Readiness defines the readiness probe configuration.
	// +optional
	Readiness *ProbeConfig `json:"readiness,omitempty"`

	// Startup defines the startup probe configuration.
	// +optional
	Startup *ProbeConfig `json:"startup,omitempty"`
}

// EnvVar defines an environment variable for the connector.
type EnvVar struct {
	// Name is the environment variable name.
	Name string `json:"name"`

	// Value is the environment variable value.
	Value string `json:"value"`
}

// RefKeyMapping defines the mapping of a key from a secret or configmap.
type RefKeyMapping struct {
	// SecretKey is the key in the secret to reference.
	// +optional
	SecretKey string `json:"secretKey,omitempty"`

	// EnvVar is the environment variable name to map to (for env type).
	// +optional
	EnvVar *string `json:"envVar,omitempty"`

	// Path is the file path to mount to (for volume type).
	// +optional
	Path *string `json:"path,omitempty"`
}

// SecretRef defines a reference to a Kubernetes secret.
type SecretRef struct {
	// Name is the name of the secret.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Type defines how the secret is consumed (env or volume).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=env;volume
	Type RefType `json:"type"`

	// MountPath is the path to mount the secret (required when type=volume).
	// +optional
	MountPath *string `json:"mountPath,omitempty"`

	// Keys defines the individual key mappings from the secret.
	// +optional
	Keys []RefKeyMapping `json:"keys,omitempty"`
}

// ConfigMapKeyMapping defines the mapping of a key from a configmap.
type ConfigMapKeyMapping struct {
	// ConfigMapKey is the key in the configmap to reference.
	// +optional
	ConfigMapKey string `json:"configMapKey,omitempty"`

	// EnvVar is the environment variable name to map to (for env type).
	// +optional
	EnvVar *string `json:"envVar,omitempty"`

	// Path is the file path to mount to (for volume type).
	// +optional
	Path *string `json:"path,omitempty"`
}

// ConfigMapRef defines a reference to a Kubernetes configmap.
type ConfigMapRef struct {
	// Name is the name of the configmap.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Type defines how the configmap is consumed (env or volume).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=env;volume
	Type RefType `json:"type"`

	// MountPath is the path to mount the configmap (required when type=volume).
	// +optional
	MountPath *string `json:"mountPath,omitempty"`

	// Keys defines the individual key mappings from the configmap.
	// +optional
	Keys []ConfigMapKeyMapping `json:"keys,omitempty"`
}

// EmptyDirSpec defines the configuration for an emptyDir volume.
type EmptyDirSpec struct {
	// Medium is the storage medium type (e.g., "", "Memory").
	// +optional
	Medium *string `json:"medium,omitempty"`

	// SizeLimit is the maximum size of the emptyDir volume.
	// +optional
	SizeLimit *string `json:"sizeLimit,omitempty"`
}

// VolumeSpec defines a volume to mount in the connector pod.
type VolumeSpec struct {
	// Name is the name of the volume.
	Name string `json:"name"`

	// MountPath is the path to mount the volume in the container.
	MountPath string `json:"mountPath"`

	// EmptyDir defines the emptyDir volume source.
	// +optional
	EmptyDir *EmptyDirSpec `json:"emptyDir,omitempty"`
}

// PDBSpec defines the PodDisruptionBudget configuration.
type PDBSpec struct {
	// Enabled indicates whether a PodDisruptionBudget should be created.
	Enabled bool `json:"enabled"`

	// MinAvailable is the minimum number/percentage of pods that must be available.
	// +optional
	MinAvailable *intstr.IntOrString `json:"minAvailable,omitempty"`
}

// LifecycleSpec defines lifecycle management settings for the connector.
type LifecycleSpec struct {
	// TerminationGracePeriodSeconds is the duration in seconds the pod needs to terminate gracefully.
	// +optional
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// PodDisruptionBudget defines the PDB configuration.
	// +optional
	PodDisruptionBudget *PDBSpec `json:"podDisruptionBudget,omitempty"`
}

// ServiceMonitorSpec defines the ServiceMonitor configuration for Prometheus.
type ServiceMonitorSpec struct {
	// Enabled indicates whether a ServiceMonitor should be created.
	Enabled bool `json:"enabled"`

	// Interval defines the scrape interval.
	// +optional
	Interval *string `json:"interval,omitempty"`

	// Labels are additional labels to add to the ServiceMonitor.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// MetricsSpec defines the metrics configuration for the connector.
type MetricsSpec struct {
	// Enabled indicates whether metrics are enabled.
	Enabled bool `json:"enabled"`

	// Path is the HTTP path for metrics endpoint.
	// +kubebuilder:default="/v1/metrics"
	// +optional
	Path *string `json:"path,omitempty"`

	// Port is the port for the metrics endpoint.
	// +kubebuilder:default=8080
	// +optional
	Port *int32 `json:"port,omitempty"`

	// ServiceMonitor defines the ServiceMonitor configuration.
	// +optional
	ServiceMonitor *ServiceMonitorSpec `json:"serviceMonitor,omitempty"`
}

// RegistrationAttribute defines a name/value pair for registration attributes.
type RegistrationAttribute struct {
	// Name is the attribute name.
	Name string `json:"name"`

	// Content is the arbitrary JSON value of the attribute.
	Content apiextensionsv1.JSON `json:"content"`
}

// RegistrationSpec defines the platform registration configuration for the connector.
type RegistrationSpec struct {
	// PlatformURL is the URL of the platform to register with.
	// +kubebuilder:validation:Required
	PlatformURL string `json:"platformUrl"`

	// Name is the registration name of the connector.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// AuthType defines the authentication type for registration.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=none;basic;certificate;apiKey;jwt
	AuthType AuthType `json:"authType"`

	// AuthAttributes defines authentication-related attributes.
	// +optional
	AuthAttributes []RegistrationAttribute `json:"authAttributes,omitempty"`

	// CustomAttributes defines custom registration attributes.
	// +optional
	CustomAttributes []RegistrationAttribute `json:"customAttributes,omitempty"`
}

// RegistrationStatusValue represents the registration state with the platform.
type RegistrationStatusValue string

// Registration status constants.
const (
	RegistrationStatusWaitingForApproval RegistrationStatusValue = "waitingForApproval"
	RegistrationStatusConnected          RegistrationStatusValue = "connected"
	RegistrationStatusFailed             RegistrationStatusValue = "failed"
	RegistrationStatusOffline            RegistrationStatusValue = "offline"
)

// RegistrationStatus defines the observed registration state.
type RegistrationStatus struct {
	// UUID is the unique identifier assigned by the platform.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// Status is the current registration status.
	// +optional
	Status RegistrationStatusValue `json:"status,omitempty"`

	// RegisteredAt is the timestamp when the connector was registered.
	// +optional
	RegisteredAt *metav1.Time `json:"registeredAt,omitempty"`
}

// ConnectorSpec defines the desired state of Connector.
type ConnectorSpec struct {
	// Image defines the container image configuration.
	Image ImageSpec `json:"image"`

	// Service defines the service configuration.
	Service ServiceSpec `json:"service"`

	// Replicas is the number of desired replicas.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Resources defines the compute resource requirements.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// SecurityContext defines the security context for the connector pod.
	// +optional
	SecurityContext *SecurityContextSpec `json:"securityContext,omitempty"`

	// PodAnnotations are arbitrary annotations added to the connector pod template.
	// Useful for integrations like Vault Agent Injector, Istio sidecar injection, etc.
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// PodLabels are arbitrary labels added to the connector pod template.
	// These are merged with the operator-managed labels; operator labels take precedence.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// Probes defines the probe configuration for the connector.
	// +optional
	Probes *ProbeSpec `json:"probes,omitempty"`

	// Env defines environment variables for the connector.
	// +optional
	Env []EnvVar `json:"env,omitempty"`

	// SecretRefs defines references to Kubernetes secrets.
	// +optional
	SecretRefs []SecretRef `json:"secretRefs,omitempty"`

	// ConfigMapRefs defines references to Kubernetes configmaps.
	// +optional
	ConfigMapRefs []ConfigMapRef `json:"configMapRefs,omitempty"`

	// Volumes defines additional volumes for the connector pod.
	// +optional
	Volumes []VolumeSpec `json:"volumes,omitempty"`

	// Lifecycle defines lifecycle management settings.
	// +optional
	Lifecycle *LifecycleSpec `json:"lifecycle,omitempty"`

	// Metrics defines the metrics configuration.
	// +optional
	Metrics *MetricsSpec `json:"metrics,omitempty"`

	// Registration defines the platform registration configuration.
	// +optional
	Registration *RegistrationSpec `json:"registration,omitempty"`
}

// ConnectorStatus defines the observed state of Connector.
type ConnectorStatus struct {
	// Phase is the current phase of the Connector.
	// +optional
	Phase ConnectorPhase `json:"phase,omitempty"`

	// ObservedGeneration is the most recent generation observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Replicas is the total number of replicas.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of ready replicas.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Endpoint is the service endpoint for the connector.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// CurrentImage is the currently deployed container image.
	// +optional
	CurrentImage string `json:"currentImage,omitempty"`

	// ConfigChecksum is a checksum of the current configuration.
	// +optional
	ConfigChecksum string `json:"configChecksum,omitempty"`

	// Conditions represent the latest available observations of the Connector's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Registration is the observed registration status.
	// +optional
	Registration *RegistrationStatus `json:"registration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=conn
// +operator-sdk:csv:customresourcedefinitions:displayName="Connector"
// +operator-sdk:csv:customresourcedefinitions:resources={{Deployments,apps/v1},{Services,v1},{ServiceAccounts,v1},{PodDisruptionBudgets,policy/v1}}

// Connector is the Schema for the connectors API.
type Connector struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConnectorSpec   `json:"spec,omitempty"`
	Status ConnectorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ConnectorList contains a list of Connector.
type ConnectorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Connector `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Connector{}, &ConnectorList{})
}

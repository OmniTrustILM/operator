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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// LifecycleSpec defines lifecycle management settings for the connector.
type LifecycleSpec struct {
	// TerminationGracePeriodSeconds is the duration in seconds the pod needs to terminate gracefully.
	// +optional
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// PodDisruptionBudget defines the PDB configuration.
	// +optional
	PodDisruptionBudget *PDBSpec `json:"podDisruptionBudget,omitempty"`
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
	// PlatformURL is the platform's BASE API URL — INCLUDING its /api prefix — that the
	// operator posts the connector registration to, e.g. https://ilm.example.com/api.
	//
	// Core serves its REST API under /api and the operator appends ONLY the versioned endpoint
	// (/v2/connector/register), so a platformUrl without the /api prefix produces a 404. A
	// platform served under an additional path prefix carries that here too, e.g.
	// https://gateway.example.com/ilm/api. A trailing slash is tolerated.
	// +kubebuilder:validation:Required
	PlatformURL string `json:"platformUrl"`

	// Name is the registration name of the connector.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// AuthType defines the authentication type for registration.
	// +kubebuilder:validation:Required
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
	// +kubebuilder:validation:XValidation:rule="has(self.repository) && has(self.tag)",message="image.repository and image.tag are required"
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

	// NodeSelector constrains the connector pods to nodes with matching labels —
	// e.g. nodes with reachability to an HSM or appliance network segment.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allow the connector pods to schedule onto tainted nodes.
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
	// +listType=map
	// +listMapKey=type
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

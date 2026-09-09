/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

// common_types.go defines the generic building-block spec types shared by the
// otilm.com CRDs (Connector, Platform, and Proxy). Keeping them in one place guarantees
// that every CRD renders these sub-objects with an identical schema. The
// canonical package doc lives in groupversion_info.go.

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ImageSpec defines a container image. It is shared by all otilm.com CRDs.
// All fields are optional at the schema level: CRDs backed by a version bundle
// (Platform) default the missing pieces from the bundle, while CRDs without one
// (Connector) require repository+tag via their own field validation.
// Image references are composed as registry/repository/name:tag, joining only
// the non-empty segments; a version-bundle-backed CRD (Platform) fills a missing
// name/tag from its bundle, while a CRD without one (Connector) supplies
// repository+tag directly (today its builder uses repository:tag). When Digest is
// set the reference is pinned by digest (registry/repository/name@digest) and the
// tag is ignored, so a deployment can pin an immutable image.
type ImageSpec struct {
	// Registry is the image registry host (optional; for example, registry.example.com).
	// +optional
	Registry string `json:"registry,omitempty"`

	// Repository is the image repository or path.
	// +optional
	Repository string `json:"repository,omitempty"`

	// Name is the image name (optional; defaults from the version bundle when applicable).
	// +optional
	Name string `json:"name,omitempty"`

	// Tag is the image tag (optional; defaults from the version bundle when applicable).
	// Ignored when Digest is set (a digest pins the image immutably).
	// +optional
	Tag string `json:"tag,omitempty"`

	// Digest pins the image by content digest (e.g. "sha256:abc123..."). When set it
	// takes precedence over Tag and the reference is composed as
	// registry/repository/name@digest, so the workload runs an immutable image.
	// +optional
	Digest string `json:"digest,omitempty"`

	// PullPolicy defines the image pull policy.
	// +kubebuilder:default="IfNotPresent"
	// +optional
	PullPolicy string `json:"pullPolicy,omitempty"`

	// PullSecrets is a list of secret names for pulling the image.
	// +optional
	PullSecrets []string `json:"pullSecrets,omitempty"`

	// Command overrides the container entrypoint (the container's command). When unset
	// the image's own ENTRYPOINT is used.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args overrides the container's command arguments. When unset the image's own CMD
	// is used.
	// +optional
	Args []string `json:"args,omitempty"`
}

// RefType defines how a secret or configmap is referenced.
// +kubebuilder:validation:Enum=env;volume
type RefType string

// Reference type constants for secrets and configmaps.
const (
	RefTypeEnv    RefType = "env"
	RefTypeVolume RefType = "volume"
)

// EnvVar defines an environment variable for a workload.
type EnvVar struct {
	// Name is the environment variable name.
	Name string `json:"name"`

	// Value is the environment variable value. It is optional so a valueless or
	// empty-string variable can be set, mirroring core/v1 EnvVar.
	// +optional
	Value string `json:"value,omitempty"`
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

// CredentialsRef references a Kubernetes Secret holding a username/password pair and
// lets the user map the in-Secret KEYS those values live under. It is shared by the
// typed infra credential references (database/messaging in external mode) so a user who
// brings an External-Secrets / Vault / CNPG-shaped Secret (keys like POSTGRES_USER /
// POSTGRES_PASSWORD) can point the operator at those keys instead of being forced to
// rename them to the operator's defaults.
//
// Only the INPUT keys (the keys read from the referenced Secret) are user-mappable; the
// OUTPUT contracts the operator wires from them — the target env-var names (JDBC_USERNAME/
// JDBC_PASSWORD, BROKER_USERNAME/BROKER_PASSWORD) and the composed .NET/JDBC connection
// strings — stay BOM-controlled application contracts and are NOT configurable here.
//
// SECURITY: only a Secret NAME and key NAMES are held here — never a credential value.
type CredentialsRef struct {
	// SecretRef is the name of the Secret holding the credentials. It is required for
	// external mode (the database/messaging XValidation enforces presence).
	// +optional
	SecretRef string `json:"secretRef,omitempty"`

	// UsernameKey is the key in the referenced Secret holding the username. When empty it
	// defaults to the operator's wiring-profile key ("username"), so a Secret already using
	// that key needs no mapping. Set it to read a differently-named key (e.g. "POSTGRES_USER").
	// +optional
	UsernameKey string `json:"usernameKey,omitempty"`

	// PasswordKey is the key in the referenced Secret holding the password. When empty it
	// defaults to the operator's wiring-profile key ("password"). Set it to read a
	// differently-named key (e.g. "POSTGRES_PASSWORD").
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// SecretRef defines a reference to a Kubernetes secret.
type SecretRef struct {
	// Name is the name of the secret.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Type defines how the secret is consumed (env or volume).
	// +kubebuilder:validation:Required
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
	Type RefType `json:"type"`

	// MountPath is the path to mount the configmap (required when type=volume).
	// +optional
	MountPath *string `json:"mountPath,omitempty"`

	// Keys defines the individual key mappings from the configmap.
	// +optional
	Keys []ConfigMapKeyMapping `json:"keys,omitempty"`
}

// StorageSpec defines the persistent-volume sizing for a managed stateful workload.
// It is shared by managed-infrastructure specs (the managed database today, the
// managed RabbitMQ cluster later) so every operator-provisioned stateful component
// requests storage with an identical schema. The values are passed through to the
// upstream operator's storage block verbatim.
type StorageSpec struct {
	// Size is the requested volume size as a Kubernetes quantity (e.g. "100Gi").
	// +kubebuilder:validation:Required
	Size string `json:"size"`

	// StorageClass names the StorageClass to provision from. When unset the cluster
	// default StorageClass is used.
	// +optional
	StorageClass *string `json:"storageClass,omitempty"`
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

// VolumeSpec defines a volume to mount in the workload pod.
type VolumeSpec struct {
	// Name is the name of the volume.
	Name string `json:"name"`

	// MountPath is the path to mount the volume in the container.
	MountPath string `json:"mountPath"`

	// EmptyDir defines the emptyDir volume source.
	// +optional
	EmptyDir *EmptyDirSpec `json:"emptyDir,omitempty"`
}

// ServiceSpec defines the service configuration for the workload.
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

// SecurityContextSpec defines security context settings for the workload pod.
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

// ProbeSpec defines the probe configuration for the workload.
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

// PDBSpec defines the PodDisruptionBudget configuration. MinAvailable and
// MaxUnavailable are mutually exclusive (a PodDisruptionBudget may set at most one); when
// both are set MinAvailable takes precedence and MaxUnavailable is ignored. When neither
// is set the builder defaults to minAvailable=1.
type PDBSpec struct {
	// Enabled indicates whether a PodDisruptionBudget should be created.
	Enabled bool `json:"enabled"`

	// MinAvailable is the minimum number/percentage of pods that must be available. It is
	// mutually exclusive with MaxUnavailable and takes precedence when both are set.
	// +optional
	MinAvailable *intstr.IntOrString `json:"minAvailable,omitempty"`

	// MaxUnavailable is the maximum number/percentage of pods that may be unavailable. It is
	// honoured only when MinAvailable is unset (a PodDisruptionBudget may carry at most one).
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// AutoscalingSpec configures a HorizontalPodAutoscaler (autoscaling/v2) for a component.
// When set on a component the operator renders an HPA targeting that component's
// Deployment and MUST omit .spec.replicas on the Deployment, so the HPA owns scaling and
// the operator's Server-Side-Apply field manager never clobbers the replica count the HPA
// writes each reconcile. At least one target utilization (CPU or memory) should be set;
// when both are unset the rendered HPA has no metrics and the autoscaler will not scale.
type AutoscalingSpec struct {
	// MinReplicas is the lower bound the autoscaler scales down to. When unset the
	// autoscaling/v2 default (1) applies.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the upper bound the autoscaler scales up to. It is required and must
	// be greater than or equal to MinReplicas.
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// TargetCPUUtilization is the target average CPU utilization (percentage of the
	// container's CPU request) the autoscaler maintains. When set it renders a Resource
	// metric on cpu. Requires the component to declare a CPU resource request.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	TargetCPUUtilization *int32 `json:"targetCPUUtilization,omitempty"`

	// TargetMemoryUtilization is the target average memory utilization (percentage of the
	// container's memory request) the autoscaler maintains. When set it renders a Resource
	// metric on memory. Requires the component to declare a memory resource request.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	TargetMemoryUtilization *int32 `json:"targetMemoryUtilization,omitempty"`
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

// MetricsSpec defines the metrics configuration for the workload.
type MetricsSpec struct {
	// Enabled indicates whether metrics are enabled.
	Enabled bool `json:"enabled"`

	// Path is the HTTP path for metrics endpoint.
	// +kubebuilder:default="/v1/metrics"
	// +optional
	Path *string `json:"path,omitempty"`

	// Port is the port for the metrics endpoint.
	// Currently reserved for future use when metrics are served on a separate port.
	// The ServiceMonitor uses the service port (spec.service.port) for scraping.
	// +kubebuilder:default=8080
	// +optional
	Port *int32 `json:"port,omitempty"`

	// ServiceMonitor defines the ServiceMonitor configuration.
	// +optional
	ServiceMonitor *ServiceMonitorSpec `json:"serviceMonitor,omitempty"`
}

// ServiceAccountSpec configures the ServiceAccount the operator renders for a
// component. The operator always renders a dedicated, least-privilege ServiceAccount
// per component; this lets the user override its name (e.g. to bind a pre-created IAM
// or workload-identity ServiceAccount) and stamp extra annotations on it (e.g. an AWS
// IRSA role-arn or a GCP workload-identity binding). No credentials are held here.
type ServiceAccountSpec struct {
	// Name overrides the ServiceAccount name the component uses (and is named after).
	// When unset the component's own name is used.
	// +optional
	Name *string `json:"name,omitempty"`

	// Annotations are extra annotations stamped onto the rendered ServiceAccount (e.g.
	// a cloud workload-identity binding). They are non-sensitive routing metadata only.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// WorkloadKind selects the apps/v1 workload kind the operator renders for a component.
// +kubebuilder:validation:Enum=Deployment;StatefulSet
type WorkloadKind string

// Workload kind constants for ComponentSpec.WorkloadType.
const (
	// WorkloadKindDeployment renders the component as an apps/v1 Deployment (the default,
	// fitting the stateless platform components).
	WorkloadKindDeployment WorkloadKind = "Deployment"
	// WorkloadKindStatefulSet renders the component as an apps/v1 StatefulSet.
	WorkloadKindStatefulSet WorkloadKind = "StatefulSet"
)

// ComponentSpec is the shared per-component override surface embedded (inline) by
// every platform component (core/auth/scheduler/auth-opa-policies/
// fe-administrator/utils/api-gateway). It mirrors the Connector CR's override
// surface so a platform component is as fully configurable as a standalone Connector,
// with component-specific extras layered on top by each component's own spec.
//
// Every field is optional: when unset the operator's derived defaults (image from the
// version bundle, replicas, wiring, probes, SCC-clean security) apply unchanged. When
// set, an override LAYERS onto those defaults (env is appended last-wins; secret/
// configmap refs and volumes are appended; init/sidecar containers are appended). A
// user-supplied SecurityContext and user-supplied init/sidecar containers are still
// SCC-hardened (fill-don't-replace), so the OpenShift restricted-v2 guarantees can
// never be weakened from the CR.
//
// SECURITY: no secret values are ever held here — sensitive values are referenced via
// SecretRefs (secretKeyRef), and Env is for non-sensitive inline variables only.
type ComponentSpec struct {
	// Image overrides the component's image settings, per field (registry/repository/
	// name/tag/digest/pullPolicy/command/args). Unset fields fall back to the shared
	// spec.image and then the version bundle.
	// +optional
	Image ImageSpec `json:"image,omitempty"`

	// Replicas is the desired replica count for this component. It is ignored when
	// Autoscaling is set (an HPA then owns scaling and the operator omits
	// .spec.replicas). When the platform's spec.highAvailability is enabled a component
	// that sets neither Replicas nor Autoscaling receives an HA-default replica count; an
	// explicit Replicas here overrides that default.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// WorkloadType selects the apps/v1 workload kind the operator renders for this
	// component: a Deployment (the default) or a StatefulSet. A StatefulSet gives the
	// component's pods a stable, ordinal network identity (a per-pod hostname under the
	// component's headless Service) and an ordered, one-at-a-time rolling update — useful
	// for a component that needs predictable pod names or strict rollout ordering. The
	// default, Deployment, fits the platform's stateless components (interchangeable pods,
	// parallel rollout). Switching the kind on an existing component is NOT a seamless
	// in-place mutation: the operator applies the new kind and prunes the old one, so the
	// component briefly restarts — safe because platform state lives in the database/broker,
	// not in the pod. The rendered StatefulSet currently carries no volumeClaimTemplates
	// (the platform components are stateless); persistent per-pod storage is a future knob.
	// All other shaping (the hardened pod template, replicas/HPA semantics, scheduling,
	// probes, env) is identical to the Deployment path. The allowed values are enumerated
	// on the WorkloadKind type.
	// +kubebuilder:default=Deployment
	// +optional
	WorkloadType WorkloadKind `json:"workloadType,omitempty"`

	// PodDisruptionBudget configures an optional PodDisruptionBudget for this component
	// (guarding voluntary-disruption availability). When unset and the platform's
	// spec.highAvailability is enabled the operator renders a default PDB (minAvailable 1)
	// for the stateless components; an explicit PodDisruptionBudget here overrides that
	// default.
	// +optional
	PodDisruptionBudget *PDBSpec `json:"podDisruptionBudget,omitempty"`

	// Autoscaling configures an optional HorizontalPodAutoscaler for this component. When
	// set the operator renders an HPA targeting this component's Deployment and OMITS
	// .spec.replicas on the Deployment, so the HPA owns the replica count (under
	// Server-Side Apply the operator's field manager never sends, and so never clobbers,
	// replicas). Autoscaling takes precedence over Replicas and over any HA-default
	// replica count.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// Resources overrides the container resource requirements (requests/limits) for this
	// component.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Env is a list of non-sensitive inline environment variables, appended AFTER the
	// operator-derived env so a user variable of the same name wins (last-wins
	// container-env semantics). Sensitive values must be supplied via SecretRefs.
	// +optional
	Env []EnvVar `json:"env,omitempty"`

	// SecretRefs references Kubernetes Secrets consumed as env (envFrom / secretKeyRef
	// with key mapping) or mounted as volumes, mirroring the Connector CR. Values are
	// projected by reference — never copied into the rendered objects.
	// +optional
	SecretRefs []SecretRef `json:"secretRefs,omitempty"`

	// ConfigMapRefs references Kubernetes ConfigMaps consumed as env (envFrom /
	// configMapKeyRef with key mapping) or mounted as volumes, mirroring the Connector CR.
	// +optional
	ConfigMapRefs []ConfigMapRef `json:"configMapRefs,omitempty"`

	// Volumes are additional pod volumes (emptyDir today) mounted into the main
	// container, appended to the operator's own volumes.
	// +optional
	Volumes []VolumeSpec `json:"volumes,omitempty"`

	// Probes overrides the component's liveness/readiness/startup probes. When unset the
	// operator's per-component default probes apply.
	// +optional
	Probes *ProbeSpec `json:"probes,omitempty"`

	// SecurityContext overrides the main container's security context. It is always
	// merged through SCC hardening (fill-don't-replace): the four SCC-critical fields
	// (runAsNonRoot, no privilege escalation, drop ALL capabilities, seccomp
	// RuntimeDefault) are guaranteed even if the user omits them, so a partial context
	// can never silently weaken pod security.
	// +optional
	SecurityContext *SecurityContextSpec `json:"securityContext,omitempty"`

	// PodAnnotations are arbitrary annotations merged onto the component's pod template
	// (e.g. for Vault Agent Injector or Istio sidecar injection).
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// PodLabels are arbitrary labels merged onto the component's pod template. They are
	// merged with the operator-managed labels; operator labels take precedence (they are
	// immutable selectors).
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// NodeSelector constrains the component's pods to nodes with matching labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity sets the component's pod affinity/anti-affinity and node affinity rules.
	// The schema is preserved opaquely (x-kubernetes-preserve-unknown-fields) rather than
	// expanding the full corev1.Affinity OpenAPI inline in seven component blocks, which
	// would bloat the CRD past the etcd object-size limit; the Go type stays corev1.Affinity
	// so it marshals correctly, and the apiserver/kubelet still validate it as a PodSpec field.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// Tolerations allow the component's pods to schedule onto tainted nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// InitContainers are extra init containers appended (after the operator's own) to run
	// before the main container. They are SCC-hardened (forced restricted-v2 fields) like
	// every other container, so a user init container cannot weaken pod security. The
	// schema is preserved opaquely (x-kubernetes-preserve-unknown-fields) rather than
	// expanding the full corev1.Container OpenAPI inline in seven component blocks (which
	// would bloat the CRD past the etcd object-size limit); the Go type stays
	// []corev1.Container so it marshals correctly and the kubelet validates it.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	InitContainers []corev1.Container `json:"initContainers,omitempty"`

	// Sidecars are extra containers appended (after the main container) to run alongside
	// it in the pod. They are SCC-hardened (forced restricted-v2 fields) like every other
	// container, so a user sidecar cannot weaken pod security. The schema is preserved
	// opaquely (x-kubernetes-preserve-unknown-fields) for the same CRD-size reason as
	// InitContainers; the Go type stays []corev1.Container.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Sidecars []corev1.Container `json:"sidecars,omitempty"`

	// ServiceAccount overrides the component's ServiceAccount name and annotations. When
	// unset the operator renders a dedicated, least-privilege ServiceAccount named after
	// the component.
	// +optional
	ServiceAccount *ServiceAccountSpec `json:"serviceAccount,omitempty"`

	// Service overrides the component's Service settings (port/type). When unset the
	// operator's default Service shape applies.
	// +optional
	Service *ServiceSpec `json:"service,omitempty"`

	// Metrics configures the component's metrics endpoint and an optional per-component
	// Prometheus ServiceMonitor. When metrics.serviceMonitor.enabled is true the operator
	// renders a ServiceMonitor for this component (gated on the Prometheus operator CRD).
	// +optional
	Metrics *MetricsSpec `json:"metrics,omitempty"`
}

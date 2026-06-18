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

// Package common provides generic, CRD-agnostic Kubernetes resource builders
// (the Component render model, image resolution, and SCC-clean component builders)
// shared across the operator's custom resources.
package common

import (
	"strings"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
	corev1 "k8s.io/api/core/v1"
)

// ResolveImage composes a component's image reference, applying per-field
// precedence (per-component value > shared value > version-bundle default) and
// joining only the non-empty registry/repository/name segments before ":tag".
// This serves a full Platform image (registry/repository/name:tag) and a
// Connector-style image (repository:tag, no registry/name) alike.
//
// bundleLookup resolves a component name to the selected version bundle's image
// coordinates (name/tag). The PLATFORM passes the bundle selected by spec.version
// (bundle.Lookup), so the bundle default tracks the chosen platform version; the
// version-agnostic CONNECTOR passes nil (it has no bundle), which — like an empty
// component name — skips the bundle fallback. common stays version-agnostic: it knows
// only "given a lookup, fill a missing name/tag", never which versions exist.
//
// A digest takes precedence over a tag (per-component digest > shared digest): when
// present the reference is pinned as registry/repository/name@digest and the tag is
// ignored, so a deployment can run an immutable image. The digest is NOT defaulted from
// the version bundle (the bundle carries tags only); it is purely a user override.
func ResolveImage(bundleLookup func(string) (bom.Image, bool), component string, shared, comp otilmv1alpha1.ImageSpec) (string, corev1.PullPolicy) {
	registry := pickImageField(comp.Registry, shared.Registry)
	repository := pickImageField(comp.Repository, shared.Repository)
	name := pickImageField(comp.Name, shared.Name)
	tag := pickImageField(comp.Tag, shared.Tag)
	digest := pickImageField(comp.Digest, shared.Digest)
	name, tag = fillFromBundle(bundleLookup, component, name, tag, digest)
	ref := joinImageRef(registry, repository, name, tag, digest)
	policy := corev1.PullPolicy(pickImageField(comp.PullPolicy, shared.PullPolicy))
	if policy == "" {
		policy = corev1.PullIfNotPresent
	}
	return ref, policy
}

// pickImageField returns a when non-empty, otherwise b — the per-field precedence
// (per-component value > shared value) used to resolve an image reference.
func pickImageField(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// fillFromBundle defaults a missing name/tag from the selected version bundle. The bundle
// carries no digest, so when a digest pins the image the tag is irrelevant and is left
// untouched. A nil bundleLookup (Connector) or empty component skips the fallback.
func fillFromBundle(bundleLookup func(string) (bom.Image, bool), component, name, tag, digest string) (string, string) {
	if bundleLookup == nil || (name != "" && (tag != "" || digest != "")) {
		return name, tag
	}
	b, ok := bundleLookup(component)
	if !ok {
		return name, tag
	}
	if name == "" {
		name = b.Name
	}
	if tag == "" {
		tag = b.Tag
	}
	return name, tag
}

// joinImageRef joins the non-empty registry/repository/name segments with "/" and pins the
// result with "@digest" (preferred) or ":tag". An empty join yields an empty reference.
func joinImageRef(registry, repository, name, tag, digest string) string {
	segments := make([]string, 0, 3)
	for _, s := range []string{registry, repository, name} {
		if s != "" {
			segments = append(segments, s)
		}
	}
	ref := strings.Join(segments, "/")
	if ref == "" {
		return ref
	}
	switch {
	case digest != "":
		ref += "@" + digest
	case tag != "":
		ref += ":" + tag
	}
	return ref
}

// EnvPair is a non-sensitive inline env var; sensitive values are projected via SecretEnv (secretKeyRef) or EnvFrom (envFrom), never inlined here.
type EnvPair struct {
	Name  string
	Value string
}

// SecretEnvRef maps a key in a Secret to a container env var via valueFrom
// (the operator never copies the value into the manifest).
type SecretEnvRef struct {
	EnvVar     string
	SecretName string
	SecretKey  string
	// Optional, when true, sets secretKeyRef.optional so the container starts even if
	// the Secret/key is not yet present (instead of wedging with
	// CreateContainerConfigError). Use for refs that may legitimately lag behind the
	// workload — e.g. a cert-manager-issued cert Secret that has not been minted yet.
	Optional bool
}

// ConfigMapEnvRef maps a key in a ConfigMap to a container env var via valueFrom.
type ConfigMapEnvRef struct {
	// EnvVar is the name of the environment variable to set in the container.
	EnvVar string
	// ConfigMapName is the name of the ConfigMap containing the key.
	ConfigMapName string
	// ConfigMapKey is the key within the ConfigMap whose value is projected.
	ConfigMapKey string
}

// FieldRefEnv maps a pod field to a container env var via the downward API
// (valueFrom.fieldRef). It is the general downward-API capability of the
// Component model — e.g. Core's PROXY_INSTANCE_ID sourced from metadata.name.
type FieldRefEnv struct {
	// EnvVar is the name of the environment variable to set in the container.
	EnvVar string
	// FieldPath is the downward-API field path projected into the variable
	// (e.g. "metadata.name", "metadata.namespace", "status.podIP").
	FieldPath string
}

// Probes are the optional liveness/readiness/startup probes for the main container.
type Probes struct {
	// Liveness is the optional liveness probe for the main container.
	Liveness *corev1.Probe
	// Readiness is the optional readiness probe for the main container.
	Readiness *corev1.Probe
	// Startup is the optional startup probe for the main container.
	Startup *corev1.Probe
}

// Component is the resolved, render-ready model for one platform component.
type Component struct {
	// Name is the component role (e.g. "core"). It names the workload/Service/SA and
	// drives the component label.
	Name string
	// ContainerName overrides the main container's name when set; otherwise the
	// container is named after Name. Some chart workloads name the container
	// differently from the component (e.g. auth's container is "auth").
	ContainerName string
	// Instance is the owning CR instance name; scopes resource names and the instance label.
	Instance    string
	Namespace   string
	Image       string
	PullPolicy  corev1.PullPolicy
	PullSecrets []string
	Replicas    int32
	// OmitReplicas, when true, makes BuildDeployment / BuildStatefulSet leave
	// .spec.replicas UNSET so an external controller owns it. It is set for an HPA-owned
	// component (Autoscaling configured): under Server-Side Apply the operator's field
	// manager must not send replicas, or it would clobber the count the
	// HorizontalPodAutoscaler writes each reconcile. Replicas is ignored when this is true.
	OmitReplicas bool
	// WorkloadType selects the apps/v1 workload kind the render layer builds for this
	// component: WorkloadKindStatefulSet renders a StatefulSet, anything else (the empty
	// default) renders a Deployment. The two share an identical hardened pod template
	// (buildPodTemplateSpec); only the enclosing workload object differs.
	WorkloadType otilmv1alpha1.WorkloadKind
	// Recreate, when true, sets the Deployment's strategy to Recreate (the old pod is fully
	// terminated before the new one starts) instead of the default RollingUpdate. It is set for
	// components that run DB SCHEMA MIGRATIONS at startup (Core, auth): a rolling update
	// would briefly run TWO migrating pods at once, which race their migrations through the
	// transaction-mode pooler (whose pooled connections don't preserve Flyway's session-scoped
	// advisory lock) and corrupt the schema. Recreate guarantees a single migrating pod per
	// generation. Ignored for a StatefulSet (which has its own update strategy).
	Recreate    bool
	Port        int32
	ServiceType corev1.ServiceType
	Env         []EnvPair
	SecretEnv   []SecretEnvRef
	// ConfigMapEnv lists env vars sourced via configMapKeyRef; rendered after Env and SecretEnv.
	ConfigMapEnv []ConfigMapEnvRef
	// FieldRefEnv lists env vars sourced from pod fields via the downward API
	// (valueFrom.fieldRef); rendered after Env, SecretEnv, and ConfigMapEnv.
	FieldRefEnv []FieldRefEnv
	// ExtraEnv carries fully-formed container env vars appended LAST (after Env,
	// SecretEnv, ConfigMapEnv, FieldRefEnv) so they win on a duplicate name. It is the
	// passthrough slot for user-supplied keyed secret/configmap refs rendered via the
	// shared key-mapping logic, whose secretKeyRef/configMapKeyRef shape the typed
	// SecretEnv/ConfigMapEnv helpers do not cover (e.g. arbitrary EnvVar names).
	ExtraEnv []corev1.EnvVar
	// EnvFrom carries whole-Secret / whole-ConfigMap envFrom sources (a SecretRef/
	// ConfigMapRef of type=env with no key mapping projects every key). Rendered on the
	// main container's EnvFrom.
	EnvFrom   []corev1.EnvFromSource
	Resources corev1.ResourceRequirements
	// Probes are the optional liveness/readiness/startup probes for the main container.
	Probes Probes
	// InitContainers are additional init containers to run before the main container (e.g. wait-for-dependency).
	InitContainers []corev1.Container
	// Sidecars are additional containers to run alongside the main container in the pod (e.g. OPA).
	Sidecars []corev1.Container
	// SidecarsFirst renders the sidecars BEFORE the main container in the pod's container list.
	// The kubelet starts containers in order and runs a container's postStart hook synchronously
	// before starting the next one, so a main container whose postStart waits on a sidecar's port
	// (e.g. Core's register-internal-keycloak.sh blocks on the OPA sidecar's :8181) DEADLOCKS
	// unless the sidecar is ordered first. Mirrors the Helm chart's core-deployment container order.
	SidecarsFirst bool
	// Volumes are the pod-level volumes; used together with VolumeMounts on the main container.
	Volumes []corev1.Volume
	// VolumeMounts are the volume mounts on the main container only.
	VolumeMounts []corev1.VolumeMount
	// Command overrides the main container's entrypoint (optional).
	Command []string
	// Args overrides the main container's command arguments (optional).
	Args []string
	// AdditionalPorts are extra named ports on the main container beyond the primary HTTP port.
	AdditionalPorts []corev1.ContainerPort
	// PrimaryPortName overrides the name of the primary container port (default "http").
	// Used when the workload exposes a non-HTTP primary port (e.g. Kong's consumer-http).
	PrimaryPortName string
	// ServicePorts overrides the Service's port list. When non-empty, BuildService
	// emits these verbatim instead of the single default "http" port — used by
	// multi-port workloads such as the Kong gateway (consumer/admin/status).
	ServicePorts []corev1.ServicePort
	// Lifecycle is the optional lifecycle hook configuration for the main container.
	Lifecycle *corev1.Lifecycle
	// ReadOnlyRootFilesystem, when non-nil, sets the main container's
	// securityContext.readOnlyRootFilesystem. It is a per-component opt-in (left nil
	// by default) because read-only root is a runtime behaviour change: it is safe
	// only for components whose every writable path is backed by a mounted volume
	// (e.g. an in-memory ephemeral /tmp). Sidecars/init containers carry their own
	// read-only-root decision on their corev1.Container SecurityContext (hardenContainer
	// fills the other SCC fields around it).
	ReadOnlyRootFilesystem *bool
	// SecurityContext, when non-nil, is merged onto the main container's SCC-clean
	// SecurityContext (fill-don't-replace): the four SCC-critical fields stay hardened
	// even if the caller leaves them unset, so a user-supplied context can never weaken
	// pod security. ReadOnlyRootFilesystem above still applies on top.
	SecurityContext *corev1.SecurityContext
	// PodAnnotations are extra annotations merged onto the pod template (e.g. user
	// PodAnnotations or a config checksum). Operator-set keys win on conflict.
	PodAnnotations map[string]string
	// PodLabels are extra labels merged onto the pod template. The operator-managed
	// labels (Labels()) always win, since they are immutable selectors.
	PodLabels map[string]string
	// NodeSelector constrains the pod to nodes with matching labels (passthrough).
	NodeSelector map[string]string
	// Affinity sets the pod's affinity/anti-affinity rules (passthrough).
	Affinity *corev1.Affinity
	// Tolerations allow the pod to schedule onto tainted nodes (passthrough).
	Tolerations []corev1.Toleration
	// ServiceAccountName overrides the ServiceAccount the pod uses and the rendered
	// ServiceAccount's name. When empty the component's ResourceName() is used.
	ServiceAccountName string
	// ServiceAccountAnnotations are stamped onto the rendered ServiceAccount (e.g. a
	// cloud workload-identity binding). They do not affect the Deployment.
	ServiceAccountAnnotations map[string]string
	// TerminationGracePeriodSeconds, when non-nil, sets the pod's termination grace
	// period (passthrough; the kubelet default of 30s applies when nil).
	TerminationGracePeriodSeconds *int64
	// LabelsOverride, when non-nil, replaces the standard label set returned by
	// Labels(). It exists for the pre-Component Kinds (Connector, Proxy) whose child
	// resources already carry a per-kind label scheme; new components should use the
	// standard labels.
	LabelsOverride map[string]string
	// SelectorLabelsOverride, when non-nil, replaces the immutable selector subset
	// returned by SelectorLabels(). REQUIRED when migrating a Kind whose Deployments
	// already exist in the field: a Deployment's .spec.selector is immutable, so the
	// historical per-kind selector labels must be preserved verbatim.
	SelectorLabelsOverride map[string]string
}

// SAName returns the ServiceAccount name the component's pod uses and the rendered
// ServiceAccount is named after: the explicit ServiceAccountName override when set,
// otherwise the component's ResourceName().
func (c Component) SAName() string {
	if c.ServiceAccountName != "" {
		return c.ServiceAccountName
	}
	return c.ResourceName()
}

// ResourceName returns the component name as the Kubernetes object name (e.g. "core").
// A Platform is a per-namespace singleton (today enforced at runtime by the controller;
// a create-time admission webhook is future work), so component names
// are stable and well-known — no instance prefix is needed. The owning CR is still
// recorded via the instance label/selector (see SelectorLabels and Labels).
func (c Component) ResourceName() string {
	return c.Name
}

func (c Component) instance() string {
	if c.Instance != "" {
		return c.Instance
	}
	return c.Name
}

// MainContainerName returns the name to give the main container: ContainerName when
// set, otherwise the component Name.
func (c Component) MainContainerName() string {
	if c.ContainerName != "" {
		return c.ContainerName
	}
	return c.Name
}

// SelectorLabels is the immutable name+instance subset used as the workload/pod
// selector — unique per component instance. SelectorLabelsOverride, when set,
// replaces it (per-kind schemes already deployed in the field are immutable).
func (c Component) SelectorLabels() map[string]string {
	if c.SelectorLabelsOverride != nil {
		return c.SelectorLabelsOverride
	}
	return map[string]string{NameLabel: c.Name, InstanceLabel: c.instance()}
}

// Labels returns the standard recommended labels for a component's resources.
// LabelsOverride, when set, replaces them (pre-Component per-kind schemes).
func (c Component) Labels() map[string]string {
	if c.LabelsOverride != nil {
		return c.LabelsOverride
	}
	return map[string]string{
		NameLabel:      c.Name,
		InstanceLabel:  c.instance(),
		ComponentLabel: c.Name,
		PartOfLabel:    partOfValue,
		ManagedByLabel: managedByValue,
	}
}

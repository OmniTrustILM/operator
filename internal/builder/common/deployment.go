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

package common

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// sccContainerSecurityContext returns the default SCC-clean (OpenShift restricted-v2)
// container SecurityContext: non-root, no pinned UID, no privilege escalation,
// drop ALL capabilities, seccomp RuntimeDefault. It intentionally does NOT set
// ReadOnlyRootFilesystem: read-only root is a per-component runtime decision (some
// workloads write outside their mounted scratch paths) and is wired in separately.
func sccContainerSecurityContext() *corev1.SecurityContext {
	allowPrivEsc := false
	runAsNonRoot := true
	return &corev1.SecurityContext{
		RunAsNonRoot:             &runAsNonRoot,
		AllowPrivilegeEscalation: &allowPrivEsc,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// hardenContainer enforces the OpenShift restricted-v2 SCC contract on a container's
// SecurityContext. The non-negotiable fields are FORCED to their hardened values so a
// caller (including a CR-supplied sidecar/init container or securityContext) can never
// weaken pod security: RunAsNonRoot=true, AllowPrivilegeEscalation=false,
// Privileged=false, drop ALL capabilities (and no added capabilities), seccomp
// RuntimeDefault. Non-SCC-critical caller fields are PRESERVED (e.g. a sidecar's
// ReadOnlyRootFilesystem, or an in-range RunAsUser/RunAsGroup) — those are left untouched
// so a legitimate caller refinement survives. This is fill-AND-force, stricter than a
// pure fill, precisely so a malicious or clumsy override (RunAsNonRoot: false,
// Privileged: true, capabilities.Add) is overridden rather than silently honored.
func hardenContainer(c *corev1.Container) {
	if c.SecurityContext == nil {
		c.SecurityContext = sccContainerSecurityContext()
		return
	}
	sc := c.SecurityContext
	// FORCE the non-negotiable SCC fields (these can never be weakened by a caller).
	runAsNonRoot := true
	sc.RunAsNonRoot = &runAsNonRoot
	allowPrivEsc := false
	sc.AllowPrivilegeEscalation = &allowPrivEsc
	privileged := false
	sc.Privileged = &privileged
	// Drop ALL capabilities and strip any caller-added capabilities; restricted-v2 forbids
	// adding capabilities. Preserve nothing of the caller's Capabilities block.
	sc.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
}

// buildPodTemplateSpec renders the shared, SCC-clean (OpenShift restricted-v2) pod
// template for a component: the env (inline + secretKeyRef + configMapKeyRef + fieldRef +
// ExtraEnv + envFrom), the main container, the init/sidecar containers, the pod-level
// security context, scheduling (nodeSelector/affinity/tolerations), volumes, the service
// account, and the merged pod labels/annotations. It is the SINGLE source of the pod shape
// so BuildDeployment and BuildStatefulSet enclose an IDENTICAL template — every container
// (main, init, sidecar) is hardened (RunAsNonRoot, drop ALL caps, no privilege escalation,
// seccomp RuntimeDefault) regardless of the enclosing workload kind.
func buildPodTemplateSpec(c Component) corev1.PodTemplateSpec {
	var pullSecrets []corev1.LocalObjectReference
	for _, s := range c.PullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: s})
	}

	runAsNonRoot := true

	mainCtr := buildMainContainer(c)
	containers := buildContainerList(c, mainCtr)
	initContainers := hardenInitContainers(c.InitContainers)

	podSpec := corev1.PodSpec{
		ServiceAccountName: c.SAName(),
		ImagePullSecrets:   pullSecrets,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   &runAsNonRoot,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers:   containers,
		NodeSelector: c.NodeSelector,
		Affinity:     c.Affinity,
		Tolerations:  c.Tolerations,
	}
	if len(initContainers) > 0 {
		podSpec.InitContainers = initContainers
	}
	if c.TerminationGracePeriodSeconds != nil {
		podSpec.TerminationGracePeriodSeconds = c.TerminationGracePeriodSeconds
	}
	if len(c.Volumes) > 0 {
		podSpec.Volumes = c.Volumes
	}

	// Pod template labels: user PodLabels first, then the operator-managed labels (which
	// always win — they are the immutable selector). Pod template annotations are the
	// user PodAnnotations verbatim (the controller stamps any config checksum on top via
	// StampConfigChecksum, which wins on conflict).
	podLabels := mergeLabels(c.PodLabels, c.Labels())

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: copyPodAnnotations(c.PodAnnotations)},
		Spec:       podSpec,
	}
}

// buildContainerEnv builds the main container's env in order: inline, secretKeyRef,
// configMapKeyRef, ExtraEnv, then fieldRef. Kubernetes resolves a duplicate env name to the
// LAST entry in the list, so that order IS the precedence, and it encodes two rules:
//
//   - ExtraEnv wins over the operator's own Env/SecretEnv/ConfigMapEnv. It is the passthrough
//     slot for user-supplied keyed Secret/ConfigMap refs, and overriding an operator default
//     is what that surface is for.
//   - FieldRefEnv wins over EVERYTHING, user-supplied sources included. Its entries are not
//     configuration: they are the pod's OWN IDENTITY, read through the downward API, and only
//     the operator ever puts anything there. Core's PLATFORM_INSTANCE_ID on a StatefulSet is
//     the case that makes this load-bearing — it is the pod ORDINAL, which is what keeps each
//     replica's certificate serial numbers distinct. A keyed ref mapped onto that name would
//     otherwise hand every replica one shared id, silently, and duplicate serials are not a
//     failure the platform can detect afterwards. A user who genuinely wants to supply the id
//     sets spec.core.instanceId, which is validated against multi-replica core.
//
// An env entry whose NAME is empty is omitted. A version's wiring profile leaves an
// env-var name empty to OMIT that variable for that platform version (data-driven
// version-gating, e.g. 2.17.0 has no provisioning/proxy/broker-vhost env), so the builder
// must never emit a nameless container env var. A no-op for a fully-populated profile.
func buildContainerEnv(c Component) []corev1.EnvVar {
	env := make([]corev1.EnvVar, 0, len(c.Env)+len(c.SecretEnv)+len(c.ConfigMapEnv)+len(c.FieldRefEnv)+len(c.ExtraEnv))
	for _, e := range c.Env {
		if e.Name == "" {
			continue
		}
		env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}
	for _, s := range c.SecretEnv {
		if s.EnvVar == "" {
			continue
		}
		selector := &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: s.SecretName},
			Key:                  s.SecretKey,
		}
		if s.Optional {
			optional := true
			selector.Optional = &optional
		}
		env = append(env, corev1.EnvVar{
			Name:      s.EnvVar,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: selector},
		})
	}
	for _, cm := range c.ConfigMapEnv {
		if cm.EnvVar == "" {
			continue
		}
		env = append(env, corev1.EnvVar{
			Name: cm.EnvVar,
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cm.ConfigMapName},
					Key:                  cm.ConfigMapKey,
				},
			},
		})
	}
	env = append(env, c.ExtraEnv...)
	// The downward-API entries go LAST, after every user-supplied source — see the doc above.
	for _, f := range c.FieldRefEnv {
		if f.EnvVar == "" {
			continue
		}
		env = append(env, corev1.EnvVar{
			Name: f.EnvVar,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: f.FieldPath},
			},
		})
	}
	return env
}

// buildContainerPorts builds the main container's ports: the primary port first (named
// "http" unless overridden), then any additional ports.
func buildContainerPorts(c Component) []corev1.ContainerPort {
	primaryPortName := c.PrimaryPortName
	if primaryPortName == "" {
		primaryPortName = "http"
	}
	ports := make([]corev1.ContainerPort, 0, 1+len(c.AdditionalPorts))
	ports = append(ports, corev1.ContainerPort{Name: primaryPortName, ContainerPort: c.Port})
	ports = append(ports, c.AdditionalPorts...)
	return ports
}

// buildMainContainer assembles and hardens the component's main container.
//
// Hardening starts from the user-supplied SecurityContext when set (e.g. a CR override) and
// ALWAYS runs it through hardenContainer (fill-don't-replace): the four SCC-critical fields
// stay hardened even if the user omits or weakens them, so a CR can never bypass OpenShift
// restricted-v2. ReadOnlyRootFilesystem is a per-component runtime decision (see
// Component.ReadOnlyRootFilesystem): it is only set when the component opts in, because a
// workload that writes outside a mounted scratch path would CrashLoop under a read-only
// root. A user-supplied readOnlyRootFilesystem in c.SecurityContext is preserved unless the
// component explicitly sets ReadOnlyRootFilesystem (the operator-derived value wins, since
// it reflects the component's validated writable-path reality).
func buildMainContainer(c Component) corev1.Container {
	mainCtr := corev1.Container{
		Name:            c.MainContainerName(),
		Image:           c.Image,
		ImagePullPolicy: c.PullPolicy,
		Env:             buildContainerEnv(c),
		EnvFrom:         c.EnvFrom,
		Resources:       c.Resources,
		Ports:           buildContainerPorts(c),
		VolumeMounts:    c.VolumeMounts,
		Command:         c.Command,
		Args:            c.Args,
		Lifecycle:       c.Lifecycle,
		LivenessProbe:   c.Probes.Liveness,
		ReadinessProbe:  c.Probes.Readiness,
		StartupProbe:    c.Probes.Startup,
	}
	mainCtr.SecurityContext = c.SecurityContext
	hardenContainer(&mainCtr)
	if c.ReadOnlyRootFilesystem != nil {
		mainCtr.SecurityContext.ReadOnlyRootFilesystem = c.ReadOnlyRootFilesystem
	}
	return mainCtr
}

// buildContainerList builds the full container list. Sidecars normally follow the main
// container, but when SidecarsFirst is set they precede it: the kubelet starts containers in
// order and runs a container's postStart hook synchronously before starting the next, so a
// main container whose postStart waits on a sidecar (e.g. Core's OIDC postStart waits for the
// OPA sidecar on :8181) would deadlock if that sidecar started after it. See
// Component.SidecarsFirst.
func buildContainerList(c Component, mainCtr corev1.Container) []corev1.Container {
	hardenedSidecars := make([]corev1.Container, 0, len(c.Sidecars))
	for _, s := range c.Sidecars {
		hardenContainer(&s)
		hardenedSidecars = append(hardenedSidecars, s)
	}
	containers := make([]corev1.Container, 0, 1+len(c.Sidecars))
	if c.SidecarsFirst {
		containers = append(containers, hardenedSidecars...)
		containers = append(containers, mainCtr)
	} else {
		containers = append(containers, mainCtr)
		containers = append(containers, hardenedSidecars...)
	}
	return containers
}

// hardenInitContainers returns a hardened copy of the component's init containers.
func hardenInitContainers(in []corev1.Container) []corev1.Container {
	initContainers := make([]corev1.Container, len(in))
	for i, ic := range in {
		hardenContainer(&ic)
		initContainers[i] = ic
	}
	return initContainers
}

// copyPodAnnotations returns a defensive copy of the user PodAnnotations (verbatim), or nil
// when there are none. The controller stamps any config checksum on top via
// StampConfigChecksum, which wins on conflict.
func copyPodAnnotations(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// BuildDeployment renders a Deployment for a component with SCC-clean
// (OpenShift restricted-v2) pod security: non-root, no pinned UID, all caps dropped,
// seccomp RuntimeDefault. The SCC-clean container SecurityContext is applied to
// every container (main, init, sidecar) that does not already carry one. The pod template
// is built by the shared buildPodTemplateSpec (identical to the StatefulSet path).
func BuildDeployment(c Component) *appsv1.Deployment {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: c.ResourceName(), Namespace: c.Namespace, Labels: c.Labels()},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: c.SelectorLabels()},
			Template: buildPodTemplateSpec(c),
		},
	}
	// Set .spec.replicas only when the component is NOT HPA-owned. For an HPA-owned
	// component we leave it unset so the operator's Server-Side-Apply field manager never
	// sends (and so never clobbers) the replica count the HorizontalPodAutoscaler writes.
	if !c.OmitReplicas {
		replicas := c.Replicas
		dep.Spec.Replicas = &replicas
	}
	// Recreate strategy for DB-migrating components: fully terminate the old pod before starting
	// the new one, so a roll never runs two schema-migrating pods concurrently (which would race
	// their Flyway migrations through the transaction-mode pooler and corrupt the schema). When
	// false (the default), the apps/v1 default RollingUpdate is used.
	if c.Recreate {
		dep.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	}
	return dep
}

// mergeLabels returns a new map with the user labels overlaid by the operator labels
// (operator labels win, since they are the immutable selector). When the user map is
// empty the operator labels are returned as-is.
func mergeLabels(user, operator map[string]string) map[string]string {
	if len(user) == 0 {
		return operator
	}
	out := make(map[string]string, len(user)+len(operator))
	for k, v := range user {
		out[k] = v
	}
	for k, v := range operator {
		out[k] = v
	}
	return out
}

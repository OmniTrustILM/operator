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

// Package golden is the operator's cross-variant full-render regression net. For each
// use-case VARIANT it builds a Platform CR, renders the operator's OWN output via the
// builders (RenderPlatform), serializes the rendered objects to stable, canonical YAML,
// and asserts it is byte-for-byte equal to a checked-in golden file
// (test/golden/<variant>.golden.yaml). The golden files ARE the operator's spec: any
// render change that alters the shipped objects shows up here as a precise diff against
// the recorded output, across the full breadth of shipped use cases.
//
// The matrix is data-driven: a []variant table drives TestGoldenVariants, each entry
// pairing a variant name with the Platform CR that enables its feature set. The variant
// set covers the breadth the project ships: the minimal smoke, the three cert-managed /
// BYO edge modes (internal CA chain, ACME / letsEncrypt, bring-your-own TLS Secret), the
// Gateway API edge, the utils on/off toggle, external messaging, the gateway
// cors / request-logging / management-UI options, and the managed CloudNativePG database
// with the default-on pooler and with the pooler opted out (direct-connection wiring).
//
// Regenerating the golden files: when an intentional render change updates the shipped
// objects, re-record every golden with
//
//	UPDATE_GOLDEN=1 go test ./test/golden/...
//
// then review the resulting diff (it is the operator's new spec) before committing.
package golden

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	// namespace is the namespace every variant renders into.
	namespace = "ilm"
	// instanceName is the Platform CR name (and resource-prefix) every variant uses, so
	// the recorded golden names are stable across runs.
	instanceName = "ilm"
	// goldenDir is the directory holding the checked-in golden files.
	goldenDir = "testdata"
	// docSeparator joins serialized objects into a single multi-document YAML stream.
	docSeparator = "---\n"
	// version2190 pins a variant to the 2.19.0 bundle — the default bundle, pinned here
	// explicitly so the variant is stable across future default moves.
	version2190 = "2.19.0"
)

// updateGolden, when true (UPDATE_GOLDEN=1 in the environment, or -update on the test
// binary), rewrites every golden file from the current render instead of comparing.
var updateGolden = flag.Bool("update", envTruthy("UPDATE_GOLDEN"),
	"rewrite the golden files from the current render (also honored via UPDATE_GOLDEN=1)")

// envTruthy reports whether the named env var is set to a truthy value (1/true/yes).
func envTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ---- shared CR building blocks ----------------------------------------------

// ptr returns a pointer to v, for setting optional pointer CR fields.
func ptr[T any](v T) *T { return &v }

// baseDatabaseSpec / baseMessagingSpec are the minimum DB + messaging wiring every
// variant needs (the platform always requires DB + messaging credentials). External DB
// + broker keep the render deterministic without a live cluster.
func baseDatabaseSpec() otilmv1alpha1.DatabaseSpec {
	return otilmv1alpha1.DatabaseSpec{
		Mode: "external", Host: "db.example.com", Port: 5432, Name: "ilm",
		Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "core-secret"},
	}
}

func baseMessagingSpec() otilmv1alpha1.MessagingSpec {
	return otilmv1alpha1.MessagingSpec{
		Mode: "external", BrokerType: "rabbitmq", Host: "messaging-service",
		Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "messaging-secret"},
	}
}

// fullFeatureSpec returns a PlatformSpec with the platform's default-enabled features
// wired (trusted certificates, provisioning, registerAdmin). Variants extend this with
// edge / utils / gateway / messaging as needed. The secret references are never rendered
// into object data (the operator injects them by reference at apply time).
func fullFeatureSpec() otilmv1alpha1.PlatformSpec {
	replicas := int32(1)
	return otilmv1alpha1.PlatformSpec{
		Common: otilmv1alpha1.CommonSpec{
			TrustedCertificates: otilmv1alpha1.TrustedCertificatesSpec{SecretRef: "trusted-certificates"},
		},
		Database:  baseDatabaseSpec(),
		Messaging: baseMessagingSpec(),
		Core: otilmv1alpha1.CoreSpec{
			ComponentSpec: otilmv1alpha1.ComponentSpec{Replicas: &replicas},
		},
		Provisioning: &otilmv1alpha1.ProvisioningSpec{
			APIURL: "https://prov.example.com", APIKeySecretRef: "provisioning-secret",
		},
		RegisterAdmin: &otilmv1alpha1.RegisterAdminSpec{
			Enabled:     true,
			Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "provided", SecretRef: ptr("admin-certificate-secret")},
		},
	}
}

// platformFor wraps a PlatformSpec into a named Platform CR in the golden namespace.
func platformFor(spec otilmv1alpha1.PlatformSpec) *otilmv1alpha1.Platform {
	return &otilmv1alpha1.Platform{
		TypeMeta:   metav1.TypeMeta{APIVersion: "otilm.com/v1alpha1", Kind: "Platform"},
		ObjectMeta: metav1.ObjectMeta{Name: instanceName, Namespace: namespace},
		Spec:       spec,
	}
}

// ---- the variant matrix -----------------------------------------------------

// variant is one row of the matrix: a use-case name and the Platform CR enabling its
// feature set. The render of that CR is snapshotted to <name>.golden.yaml.
type variant struct {
	name     string
	platform *otilmv1alpha1.Platform
}

// variants returns the snapshot matrix, covering the breadth of shipped use cases.
func variants() []variant {
	return []variant{
		minimalVariant(),
		edgeInternalVariant(),
		edgeLetsEncryptVariant(),
		edgeBYOSecretVariant(),
		edgeGatewayAPIVariant(),
		utilsOnVariant(),
		externalMessagingVariant(),
		gatewayOptionsVariant(),
		managedDBVariant(),
		managedDBNoPoolerVariant(),
		version2190Variant(),
		timeQualityVariant(),
		coreInstanceIDVariant(),
		coreStatefulSetVariant(),
	}
}

// minimalVariant: external DB + messaging, no edge, utils off. The smoke baseline; it
// is ALSO the utils-OFF render (utils defaults off), so the utils on/off behavior is
// covered by the diff between this golden and utils-on's.
func minimalVariant() variant {
	return variant{name: "minimal", platform: platformFor(fullFeatureSpec())}
}

// edgeInternalVariant: Ingress edge, TLS source internal (the self-signed CA chain:
// selfsigned-issuer -> ca-certificate -> ca-issuer), utils on.
func edgeInternalVariant() variant {
	spec := fullFeatureSpec()
	spec.Utils = otilmv1alpha1.UtilsSpec{Enabled: true}
	spec.Edge = &otilmv1alpha1.EdgeSpec{
		Enabled: true, Type: "ingress", ClassName: ptr("nginx"), Host: "ilm.example.com",
		TLS: &otilmv1alpha1.EdgeTLSSpec{Source: "internal"},
	}
	return variant{name: "edge-internal", platform: platformFor(spec)}
}

// edgeLetsEncryptVariant: Ingress edge, TLS source letsEncrypt (staging ACME issuer);
// no self-signed CA chain is rendered.
func edgeLetsEncryptVariant() variant {
	spec := fullFeatureSpec()
	spec.Edge = &otilmv1alpha1.EdgeSpec{
		Enabled: true, Type: "ingress", ClassName: ptr("nginx"), Host: "ilm.example.com",
		TLS: &otilmv1alpha1.EdgeTLSSpec{
			Source: "letsEncrypt",
			LetsEncrypt: &otilmv1alpha1.LetsEncryptSpec{
				Email: "ops@example.com", Environment: "staging",
			},
		},
	}
	return variant{name: "edge-letsencrypt", platform: platformFor(spec)}
}

// edgeBYOSecretVariant: Ingress edge, TLS source secret (bring-your-own). No
// cert-manager objects are rendered; the Ingress references the caller's TLS Secret.
func edgeBYOSecretVariant() variant {
	spec := fullFeatureSpec()
	spec.Edge = &otilmv1alpha1.EdgeSpec{
		Enabled: true, Type: "ingress", ClassName: ptr("nginx"), Host: "ilm.example.com",
		TLS: &otilmv1alpha1.EdgeTLSSpec{Source: "secret", SecretRef: ptr("ilm-ingress-tls")},
	}
	return variant{name: "edge-byo-secret", platform: platformFor(spec)}
}

// edgeGatewayAPIVariant: the Gateway API edge (an operator-owned Gateway + HTTPRoute),
// with a BYO TLS Secret so no cert-manager objects are rendered.
func edgeGatewayAPIVariant() variant {
	spec := fullFeatureSpec()
	spec.Edge = &otilmv1alpha1.EdgeSpec{
		Enabled: true, Type: "gatewayAPI", Host: "ilm.example.com",
		TLS:        &otilmv1alpha1.EdgeTLSSpec{Source: "secret", SecretRef: ptr("ilm-ingress-tls")},
		GatewayAPI: &otilmv1alpha1.GatewayAPISpec{GatewayClassName: ptr("istio")},
	}
	return variant{name: "edge-gatewayapi", platform: platformFor(spec)}
}

// utilsOnVariant: utils enabled — the utils Deployment/Service/SA must be
// PRESENT (the utils-OFF render is the minimal variant), and the gateway's /utils route
// follows the same gate.
func utilsOnVariant() variant {
	spec := fullFeatureSpec()
	spec.Utils = otilmv1alpha1.UtilsSpec{Enabled: true}
	return variant{name: "utils-on", platform: platformFor(spec)}
}

// externalMessagingVariant: an external broker referenced by host/port, with a global
// additionalEnv passthrough applied to every component. The messaging ConfigMap
// publishes only host/port (no broker server-config); the broker itself is delegated
// (no StatefulSet/Secret rendered).
func externalMessagingVariant() variant {
	spec := fullFeatureSpec()
	spec.Messaging.Host = "test.messaging.com"
	spec.Messaging.Port = 7896
	spec.AdditionalEnv = []otilmv1alpha1.EnvVar{{Name: "OTEL_SDK_DISABLED", Value: "false"}}
	return variant{name: "external-messaging", platform: platformFor(spec)}
}

// gatewayOptionsVariant: the gateway cors + request-logging options plus the messaging
// management-UI expose toggle. With an external broker the operator gates the /mq
// management route to managed messaging only, so it is omitted here.
func gatewayOptionsVariant() variant {
	spec := fullFeatureSpec()
	spec.Gateway = otilmv1alpha1.GatewaySpec{
		Cors:    otilmv1alpha1.GatewayCorsSpec{Enabled: true, Origins: []string{"*"}, ExposedHeaders: []string{"X-Auth-Token"}},
		Logging: otilmv1alpha1.GatewayLoggingSpec{Request: true},
	}
	spec.Messaging.Management.Expose = true
	return variant{name: "gateway-options", platform: platformFor(spec)}
}

// managedDBSpec returns fullFeatureSpec with the database switched to MANAGED (a
// CloudNativePG Cluster the operator delegates), keeping messaging external so only the
// DB render differs from the external variants. Version/Storage are pinned explicitly so
// the rendered CNPG image and PVC are deterministic and never track a BOM default bump.
func managedDBSpec() otilmv1alpha1.PlatformSpec {
	spec := fullFeatureSpec()
	spec.Database = otilmv1alpha1.DatabaseSpec{
		Mode: "managed",
		Managed: &otilmv1alpha1.ManagedDatabaseSpec{
			Instances: 3,
			Version:   "18",
			Storage:   otilmv1alpha1.StorageSpec{Size: "10Gi"},
		},
	}
	return spec
}

// managedDBVariant: a managed CloudNativePG database with the DEFAULT-ON pooler. Locks the
// full managed render — the CNPG Cluster plus the PgBouncer Pooler — alongside the wiring
// that routes the platform through the <cluster>-pooler Service.
func managedDBVariant() variant {
	return variant{name: "managed-db", platform: platformFor(managedDBSpec())}
}

// managedDBNoPoolerVariant: a managed CloudNativePG database with the pooler opted OUT
// (pgBouncer.managed=false). Locks the no-pooler render and the direct-connection wiring
// difference (the platform connects to <cluster>-rw, and no Pooler object is rendered).
func managedDBNoPoolerVariant() variant {
	spec := managedDBSpec()
	spec.Database.PgBouncer = &otilmv1alpha1.PgBouncerSpec{Managed: false}
	return variant{name: "managed-db-no-pooler", platform: platformFor(spec)}
}

// version2190Variant: a platform PINNED to the 2.19.0 bundle — the default bundle, pinned here
// explicitly so the variant is stable across future default moves — with proxy support on so
// the provision-instance-queue init container renders too. It is the byte-for-byte record of
// what "spec.version: 2.19.0" ships: the 2.19.0 image tags (core / frontend-administrator
// 2.19.0, auth 1.7.0, scheduler 1.1.1), the 2.19.0 wiring (the LOGGING_LEVEL_COM_OTILM rename),
// and the renamed proxy exchange (czertainly-proxy → ilm-proxy) in the queue-registration
// request. Every other variant now renders the same 2.19.0 default, so this variant's value is
// the explicit pin plus the proxy path, not a version delta.
func version2190Variant() variant {
	spec := fullFeatureSpec()
	spec.Version = version2190
	spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{Enabled: true}
	return variant{name: "version-2190", platform: platformFor(spec)}
}

// timeQualityVariant: the 2.19.0 platform with BOTH time-quality controls on — Core's
// integration env and the monitor sidecar (external broker, so the monitor's credentials come
// from an explicit Secret reference). It is the byte-for-byte record of the private sidecar
// image resolution, the pod-level pull-secret union, the monitor's broker wiring and its
// resource block.
//
// The RESOURCES are set here explicitly and on purpose: fullFeatureSpec supplies no resources
// at all (VERIFIED: test/golden/golden_test.go:121-140), so a variant that left them unset
// would record a golden with no resources block and prove nothing about the one sidecar field
// that is neither image nor credentials.
func timeQualityVariant() variant {
	spec := fullFeatureSpec()
	spec.Version = version2190
	spec.Common.Image.PullSecrets = []string{"registry-credentials"}
	spec.Messaging.TimeQuality = otilmv1alpha1.TimeQualitySpec{Enabled: true}
	spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{
		Enabled:     true,
		Image:       otilmv1alpha1.ImageSpec{PullSecrets: []string{"private-registry-credentials"}},
		Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "time-quality-monitor-credentials"},
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("150M"),
			},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("300M")},
		},
	}
	return variant{name: "time-quality", platform: platformFor(spec)}
}

// coreInstanceIDVariant: a 2.19.0 platform with an EXPLICIT spec.core.instanceId on a
// single-replica Core. It is the byte-for-byte record of the plain-value PLATFORM_INSTANCE_ID
// render — the shape the CEL guards exist to protect — and its diff against version-2190's
// golden isolates the plain-value PLATFORM_INSTANCE_ID render, since that golden also runs
// with proxy support enabled (so the diff additionally covers PROXY_ENABLED/ENABLE_PROXIES,
// the PROXY_INSTANCE_ID fieldRef, and the provision-instance-queue init container), which is
// what makes an accidental fieldRef (the derived shape) or a missing gate visible in review.
func coreInstanceIDVariant() variant {
	spec := fullFeatureSpec()
	spec.Version = version2190
	spec.Core.InstanceID = ptr(int32(7))
	spec.Core.Replicas = ptr(int32(1))
	return variant{name: "core-instance-id", platform: platformFor(spec)}
}

// coreStatefulSetVariant: a 2.19.0 platform whose Core renders as a multi-replica StatefulSet
// with NO explicit instanceId — the shipped multi-replica shape. It is the byte-for-byte
// record of the two things that shape depends on and nothing else asserts together: the
// downward-API PLATFORM_INSTANCE_ID projected from apps.kubernetes.io/pod-index, and the
// verify-instance-id init container standing FIRST, ahead of wait-for-auth.
func coreStatefulSetVariant() variant {
	spec := fullFeatureSpec()
	spec.Version = version2190
	spec.Core.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
	spec.Core.Replicas = ptr(int32(3))
	return variant{name: "core-statefulset", platform: platformFor(spec)}
}

// ---- the snapshot test ------------------------------------------------------

// TestGoldenVariants renders each variant, serializes it to canonical YAML, and asserts
// byte-equality against its checked-in golden file. With -update (or UPDATE_GOLDEN=1) it
// rewrites the golden files instead.
func TestGoldenVariants(t *testing.T) {
	for _, v := range variants() {
		t.Run(v.name, func(t *testing.T) {
			got, err := renderToYAML(v.platform)
			if err != nil {
				t.Fatalf("render variant %q: %v", v.name, err)
			}

			path := filepath.Join(goldenDir, v.name+".golden.yaml")

			if *updateGolden {
				if err := os.MkdirAll(goldenDir, 0o755); err != nil { //nolint:gosec // test fixture dir
					t.Fatalf("create golden dir: %v", err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // golden fixture, world-readable by design
					t.Fatalf("write golden %q: %v", path, err)
				}
				t.Logf("updated golden file %s (%d bytes)", path, len(got))
				return
			}

			want, err := os.ReadFile(path) //nolint:gosec // path is a fixed in-repo fixture name
			if err != nil {
				t.Fatalf("read golden %q: %v (run `UPDATE_GOLDEN=1 go test ./test/golden/...` to create it)", path, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("render for variant %q does not match golden file %s.\n"+
					"If this change is intentional, regenerate with `UPDATE_GOLDEN=1 go test ./test/golden/...` and review the diff.\n\n%s",
					v.name, path, firstDiff(want, got))
			}
		})
	}
}

// renderToYAML renders the operator's objects for a CR and serializes them to a stable,
// canonical multi-document YAML stream: each typed object is converted to unstructured
// with its GVK populated from the scheme, the objects are sorted by (apiVersion, kind,
// namespace, name), and each is marshaled via sigs.k8s.io/yaml (which sorts map keys),
// so the output is deterministic across runs.
func renderToYAML(p *otilmv1alpha1.Platform) ([]byte, error) {
	scheme, err := operatorScheme()
	if err != nil {
		return nil, err
	}

	rendered := platformbuilder.RenderPlatform(p)
	objs := make([]*unstructured.Unstructured, 0, len(rendered))
	for _, o := range rendered {
		u, err := toUnstructured(scheme, o)
		if err != nil {
			return nil, err
		}
		normalize(u)
		objs = append(objs, u)
	}

	sortObjects(objs)

	var buf bytes.Buffer
	for _, u := range objs {
		doc, err := yaml.Marshal(u.Object)
		if err != nil {
			return nil, fmt.Errorf("marshal %s/%s: %w", u.GetKind(), u.GetName(), err)
		}
		buf.WriteString(docSeparator)
		buf.Write(doc)
	}
	return buf.Bytes(), nil
}

// operatorScheme registers the types RenderPlatform can emit so typed objects convert to
// unstructured with their apiVersion/kind set (the managed-infra / edge objects are
// already unstructured with a preset GVK).
func operatorScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := otilmv1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := monitoringv1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// toUnstructured converts a typed client.Object to *unstructured.Unstructured, ensuring
// the GVK is set (typed objects often leave TypeMeta empty); an already-unstructured
// object is returned as-is with its preset GVK.
func toUnstructured(scheme *runtime.Scheme, o client.Object) (*unstructured.Unstructured, error) {
	if u, ok := o.(*unstructured.Unstructured); ok {
		return u, nil
	}
	gvks, _, err := scheme.ObjectKinds(o)
	if err != nil {
		return nil, fmt.Errorf("object kinds for %T: %w", o, err)
	}
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		return nil, fmt.Errorf("to unstructured %T: %w", o, err)
	}
	u := &unstructured.Unstructured{Object: content}
	if len(gvks) > 0 {
		u.SetGroupVersionKind(gvks[0])
	}
	return u, nil
}

// normalize drops non-semantic noise the scheme converter injects so the golden records
// desired API state, not serializer artifacts: the empty status sub-object and the
// always-nil metadata.creationTimestamp that runtime conversion adds for typed objects.
func normalize(u *unstructured.Unstructured) {
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	// The converter also stamps a nil creationTimestamp inside the pod-template metadata
	// of workloads; drop it there too.
	unstructured.RemoveNestedField(u.Object, "spec", "template", "metadata", "creationTimestamp")
	// JobTemplate / volumeClaimTemplate-style nested templates are not used by the
	// platform render, so the two paths above cover every emitted workload.
}

// sortObjects orders the rendered objects deterministically by (apiVersion, kind,
// namespace, name) so the golden stream is stable regardless of render append order.
func sortObjects(objs []*unstructured.Unstructured) {
	sort.SliceStable(objs, func(i, j int) bool {
		return sortKey(objs[i]) < sortKey(objs[j])
	})
}

// sortKey is the stable ordering key for a rendered object.
func sortKey(u *unstructured.Unstructured) string {
	return strings.Join([]string{u.GetAPIVersion(), u.GetKind(), u.GetNamespace(), u.GetName()}, "\x00")
}

// firstDiff returns a short, line-oriented description of the first differing line
// between want and got, to make a snapshot mismatch actionable without dumping the whole
// stream.
func firstDiff(want, got []byte) string {
	wantLines := strings.Split(string(want), "\n")
	gotLines := strings.Split(string(got), "\n")
	n := len(wantLines)
	if len(gotLines) < n {
		n = len(gotLines)
	}
	for i := 0; i < n; i++ {
		if wantLines[i] != gotLines[i] {
			return fmt.Sprintf("first diff at line %d:\n  golden: %q\n  render: %q", i+1, wantLines[i], gotLines[i])
		}
	}
	if len(wantLines) != len(gotLines) {
		return fmt.Sprintf("streams differ in length: golden has %d lines, render has %d lines", len(wantLines), len(gotLines))
	}
	return "streams differ (no line-level diff found; check trailing bytes)"
}

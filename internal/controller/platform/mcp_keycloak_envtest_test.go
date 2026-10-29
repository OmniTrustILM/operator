/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/internal/checksum"
)

// keycloakOperatorCRDs stand in for the Keycloak Operator's Keycloak and KeycloakRealmImport
// CRDs: the same group, version and names, an open schema and a status subresource. envtest serves
// only the operator's own CRDs, and a managed Keycloak reaches Ready only where its kinds are served.
func keycloakOperatorCRDs() []*apiextensionsv1.CustomResourceDefinition {
	gvk := platformbuilder.ManagedKeycloakGVK()
	open := true
	crd := func(kind, singular, plural string) *apiextensionsv1.CustomResourceDefinition {
		return &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: plural + "." + gvk.Group},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: gvk.Group,
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Kind: kind, ListKind: kind + "List", Singular: singular, Plural: plural,
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
					Name: gvk.Version, Served: true, Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object", XPreserveUnknownFields: &open,
					}},
					Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
				}},
			},
		}
	}
	return []*apiextensionsv1.CustomResourceDefinition{
		crd(gvk.Kind, "keycloak", "keycloaks"),
		crd(platformbuilder.ManagedKeycloakRealmImportGVK().Kind, "keycloakrealmimport", "keycloakrealmimports"),
	}
}

// This spec runs the MCP step through the live reconciler. It serves the Keycloak Operator's kinds
// and plays that operator's part (the Keycloak CR goes Ready, the initial-admin Secret appears), so
// the managed Keycloak becomes Ready and the fake registrar completes the step.
var _ = Describe("MCP reconcile step", func() {
	It("relays the ilm-mcp client secret, sets MCPConfigured=True and rolls the MCP server", func() {
		const ns = "ilm-mcp-configured"

		By("serving the Keycloak Operator's kinds")
		_, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{CRDs: keycloakOperatorCRDs()})
		Expect(err).NotTo(HaveOccurred())
		fakeCaps.setGroup(keycloakGroup, true)
		DeferCleanup(fakeCaps.setGroup, keycloakGroup, false)
		fakeOIDC.setOutcome(nil)
		fakeOIDC.setMCPOutcome(testMCPClientSecretValue, nil)

		p := oidcPlatform(ns, &otilmv1alpha1.KeycloakSpec{
			Mode: "managed", Realm: "ilm", Managed: &otilmv1alpha1.ManagedKeycloakSpec{Instances: 1},
		})
		p.Spec.Version = platformVersion221
		p.Spec.Common.HostName = testEdgeHost
		p.Spec.MCP.Enabled = true
		Expect(k8sClient.Create(ctx, p)).To(Succeed())

		By("playing the Keycloak Operator: the initial-admin Secret exists and the Keycloak CR reports Ready")
		Expect(k8sClient.Create(ctx, keycloakAdminSecret(p))).To(Succeed())
		Eventually(func() error {
			kc := &unstructured.Unstructured{}
			kc.SetGroupVersionKind(platformbuilder.ManagedKeycloakGVK())
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: platformbuilder.ManagedKeycloakName(p), Namespace: ns}, kc); err != nil {
				return err
			}
			if err := unstructured.SetNestedSlice(kc.Object, []interface{}{
				map[string]interface{}{"type": conditionTypeReady, "status": string(metav1.ConditionTrue)},
			}, "status", "conditions"); err != nil {
				return err
			}
			return k8sClient.Status().Update(ctx, kc)
		}, platformTimeout, platformInterval).Should(Succeed())
		// Nothing watches the Keycloak CR; a Deployment status change reconciles the Platform at once.
		markRequiredDeploymentsReady(ns)

		By("verifying MCPConfigured=True")
		Eventually(func(g Gomega) {
			var got otilmv1alpha1.Platform
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: ns}, &got)).To(Succeed())
			cond := meta.FindStatusCondition(got.Status.Conditions, conditionMCPConfigured)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(reasonMCPConfigured))
		}, platformTimeout, platformInterval).Should(Succeed())

		By("verifying the Platform owns the relayed Secret, which holds the client secret")
		var secret corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: platformbuilder.MCPClientSecretName(p), Namespace: ns}, &secret)).To(Succeed())
		Expect(string(secret.Data[platformbuilder.MCPClientSecretKey])).To(Equal(testMCPClientSecretValue))
		Expect(secret.Labels).To(HaveKeyWithValue(common.ComponentLabel, mcpServerWorkloadName))
		Expect(metav1.GetControllerOf(&secret)).NotTo(BeNil())

		By("verifying a later pass stamps the Secret's checksum on the MCP server, which rolls it")
		Eventually(func(g Gomega) {
			var dep appsv1.Deployment
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mcpServerWorkloadName, Namespace: ns}, &dep)).To(Succeed())
			g.Expect(dep.Spec.Template.Annotations).To(
				HaveKeyWithValue(platformbuilder.ConfigChecksumAnnotation, checksum.ComputeSecretChecksum(&secret)))
		}, platformTimeout, platformInterval).Should(Succeed())
	})
})

// Once the required Deployments are ready nothing requeues the Platform, so only the Secret watch can
// notice a Secret created afterwards.
var _ = Describe("MCP external identity", func() {
	// secretWatchTimeout is shorter than the steady-state requeue backstops (adminRegisterRequeueAfter for
	// an adjunct step, progressingRequeueAfter for a Deployment still rolling out), so a condition that
	// turns within it was not turned by a timer.
	const secretWatchTimeout = 10 * time.Second

	It("reports MissingSecret, then External once the client Secret is created", func() {
		const ns = "ilm-mcp-external"

		By("creating a Platform on spec.version develop, as the development clusters do, with MCP signing in through an external provider")
		p := oidcPlatform(ns, &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"})
		p.Spec.Version = developVersion
		p.Spec.Common.HostName = testEdgeHost
		p.Spec.MCP.Enabled = true
		externalIdentity(p)
		Expect(k8sClient.Create(ctx, p)).To(Succeed())

		By("verifying MCPConfigured=False/MissingSecret, naming the Secret, once the platform is Running so that nothing is left to requeue it")
		markRequiredDeploymentsReady(ns)
		Eventually(func(g Gomega) {
			var got otilmv1alpha1.Platform
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: ns}, &got)).To(Succeed())
			g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))
			cond := meta.FindStatusCondition(got.Status.Conditions, conditionMCPConfigured)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(reasonMCPMissingSecret))
			g.Expect(cond.Message).To(ContainSubstring(testMCPIdPSecret))
			g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionDegraded)).To(BeFalse(), "the adjunct never degrades the platform")
		}, platformTimeout, platformInterval).Should(Succeed())

		By("creating the client Secret the spec references")
		secret := idpClientSecret(p, idpSecretData(testMCPIdPSecretKey))
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		By("verifying the Secret watch turns it to MCPConfigured=True/External, with no requeue timer to do it")
		Eventually(func(g Gomega) {
			var got otilmv1alpha1.Platform
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: ns}, &got)).To(Succeed())
			cond := meta.FindStatusCondition(got.Status.Conditions, conditionMCPConfigured)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(reasonMCPExternal))
		}, secretWatchTimeout, platformInterval).Should(Succeed())

		By("verifying the MCP server rolls to the Secret: its checksum is the Secret's")
		Eventually(func(g Gomega) {
			var dep appsv1.Deployment
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mcpServerWorkloadName, Namespace: ns}, &dep)).To(Succeed())
			g.Expect(dep.Spec.Template.Annotations).To(
				HaveKeyWithValue(platformbuilder.ConfigChecksumAnnotation, checksum.ComputeSecretChecksum(secret)))
		}, secretWatchTimeout, platformInterval).Should(Succeed())

		By("verifying the operator relays no client secret of its own, and that neither a secret nor a URL reaches the status")
		var relayed corev1.Secret
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: platformbuilder.MCPClientSecretName(p), Namespace: ns}, &relayed))).To(BeTrue())
		var got otilmv1alpha1.Platform
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: ns}, &got)).To(Succeed())
		status, err := json.Marshal(got.Status)
		Expect(err).NotTo(HaveOccurred())
		for _, leak := range []string{testMCPIdPSecretValue, testMCPIdPIssuer, testMCPIdPJWKS, testMCPIdPToken} {
			Expect(string(status)).NotTo(ContainSubstring(leak))
		}
	})
})

// This spec runs the env overrides of the MCP server through the live reconciler, which applies its
// Deployment server-side. The apiserver refuses an applied container env that names a variable twice,
// so a spec.mcp.env entry for a variable the operator renders too has to leave one entry, the user's.
var _ = Describe("MCP env overrides", func() {
	const proxyOverride = "http://override.example.com:3128"

	DescribeTable("applies the MCP Deployment with one entry per variable, holding the user's value",
		func(ns string, set func(p *otilmv1alpha1.Platform), name, value string) {
			By("creating a Platform whose MCP env names a variable the operator renders as well")
			p := oidcPlatform(ns, &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"})
			p.Spec.Version = developVersion
			p.Spec.Common.HostName = testEdgeHost
			p.Spec.MCP.Enabled = true
			externalIdentity(p)
			set(p)
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("verifying the Deployment is applied, with the variable once and the user's value")
			applyFailure := func() string {
				var got otilmv1alpha1.Platform
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: ns}, &got); err != nil {
					return "the Platform could not be read"
				}
				cond := meta.FindStatusCondition(got.Status.Conditions, conditionDegraded)
				if cond == nil || cond.Status != metav1.ConditionTrue {
					return "the Platform is not Degraded"
				}
				return "the Platform is Degraded with " + cond.Reason + ": " + cond.Message
			}
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mcpServerWorkloadName, Namespace: ns}, &dep)).To(Succeed())
				var values []string
				for _, ctr := range dep.Spec.Template.Spec.Containers {
					if ctr.Name != mcpServerWorkloadName {
						continue
					}
					for _, e := range ctr.Env {
						if e.Name == name {
							values = append(values, e.Value)
						}
					}
				}
				g.Expect(values).To(Equal([]string{value}))
			}, platformTimeout, platformInterval).Should(Succeed(), applyFailure)
		},
		Entry("a proxy variable that spec.common.proxy renders", "ilm-mcp-env-proxy", func(p *otilmv1alpha1.Platform) {
			p.Spec.Common.Proxy.HTTP = "http://proxy.example.com:3128"
			p.Spec.MCP.Env = []otilmv1alpha1.EnvVar{{Name: "HTTP_PROXY", Value: proxyOverride}}
		}, "HTTP_PROXY", proxyOverride),
		Entry("a security setting that the Platform has no field for", "ilm-mcp-env-jws", func(p *otilmv1alpha1.Platform) {
			p.Spec.MCP.Env = []otilmv1alpha1.EnvVar{{Name: "ILM_MCP_SECURITY_JWS_ALGORITHMS", Value: "RS256,ES256"}}
		}, "ILM_MCP_SECURITY_JWS_ALGORITHMS", "RS256,ES256"),
	)
})

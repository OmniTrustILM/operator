/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

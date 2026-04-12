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

package controller

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder"
)

const (
	timeout  = 30 * time.Second
	interval = 250 * time.Millisecond
)

// helper to create a unique namespace for each test
func createTestNamespace(name string) string {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, ns)).To(Succeed())
	return name
}

// helper to build a minimal Connector CR
func newConnector(name, namespace string) *otilmv1alpha1.Connector {
	replicas := int32(1)
	return &otilmv1alpha1.Connector{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: otilmv1alpha1.ConnectorSpec{
			Image: otilmv1alpha1.ImageSpec{
				Repository: "registry.example.com/connector",
				Tag:        "1.0.0",
				PullPolicy: "IfNotPresent",
			},
			Service: otilmv1alpha1.ServiceSpec{
				Port: 8080,
				Type: "ClusterIP",
			},
			Replicas: &replicas,
		},
	}
}

var _ = Describe("Connector Controller", func() {

	// ---------------------------------------------------------------
	// Test 1: Create a minimal Connector and verify child resources
	// ---------------------------------------------------------------
	Context("TestCreateConnectorBasic", func() {
		var ns string
		const connName = "basic-conn"

		BeforeEach(func() {
			ns = createTestNamespace("test-create-basic")
		})

		It("should create ServiceAccount, Deployment, and Service with correct names, labels, and owner references", func() {
			conn := newConnector(connName, ns)
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())

			key := types.NamespacedName{Name: connName, Namespace: ns}

			By("verifying ServiceAccount is created")
			Eventually(func(g Gomega) {
				var sa corev1.ServiceAccount
				g.Expect(k8sClient.Get(ctx, key, &sa)).To(Succeed())
				g.Expect(sa.Labels).To(HaveKeyWithValue(builder.NameLabel, connName))
				g.Expect(sa.Labels).To(HaveKeyWithValue(builder.ManagedByLabel, builder.ManagerName))
				g.Expect(sa.Labels).To(HaveKeyWithValue(builder.ComponentLabel, builder.ComponentValue))
				g.Expect(sa.OwnerReferences).To(HaveLen(1))
				g.Expect(sa.OwnerReferences[0].Name).To(Equal(connName))
				g.Expect(sa.OwnerReferences[0].Kind).To(Equal("Connector"))
			}, timeout, interval).Should(Succeed())

			By("verifying Deployment is created")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
				g.Expect(dep.Labels).To(HaveKeyWithValue(builder.NameLabel, connName))
				g.Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
				g.Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("registry.example.com/connector:1.0.0"))
				g.Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
				g.Expect(dep.OwnerReferences).To(HaveLen(1))
				g.Expect(dep.OwnerReferences[0].Name).To(Equal(connName))
			}, timeout, interval).Should(Succeed())

			By("verifying Service is created")
			Eventually(func(g Gomega) {
				var svc corev1.Service
				g.Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())
				g.Expect(svc.Labels).To(HaveKeyWithValue(builder.NameLabel, connName))
				g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
				g.Expect(svc.Spec.Ports).To(HaveLen(1))
				g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
				g.Expect(svc.OwnerReferences).To(HaveLen(1))
				g.Expect(svc.OwnerReferences[0].Name).To(Equal(connName))
			}, timeout, interval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// Test 2: Update Connector image tag and verify Deployment changes
	// ---------------------------------------------------------------
	Context("TestUpdateConnectorImage", func() {
		var ns string
		const connName = "update-image-conn"

		BeforeEach(func() {
			ns = createTestNamespace("test-update-image")
		})

		It("should update the Deployment image when the Connector image tag changes", func() {
			conn := newConnector(connName, ns)
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())

			key := types.NamespacedName{Name: connName, Namespace: ns}

			By("waiting for Deployment to be created")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
				g.Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("registry.example.com/connector:1.0.0"))
			}, timeout, interval).Should(Succeed())

			By("updating the Connector image tag")
			Eventually(func(g Gomega) {
				var latest otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &latest)).To(Succeed())
				latest.Spec.Image.Tag = "2.0.0"
				g.Expect(k8sClient.Update(ctx, &latest)).To(Succeed())
			}, timeout, interval).Should(Succeed())

			By("verifying Deployment has the new image")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
				g.Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("registry.example.com/connector:2.0.0"))
			}, timeout, interval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// Test 3: Secret checksum change triggers annotation update
	// ---------------------------------------------------------------
	Context("TestSecretChecksumChange", func() {
		var ns string
		const connName = "checksum-conn"
		const secretName = "my-secret"

		BeforeEach(func() {
			ns = createTestNamespace("test-secret-checksum")
		})

		It("should update the Deployment checksum annotation when a referenced Secret changes", func() {
			By("creating the Secret")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: ns,
				},
				Data: map[string][]byte{
					"key": []byte("value-v1"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("creating the Connector that references the Secret")
			conn := newConnector(connName, ns)
			conn.Spec.SecretRefs = []otilmv1alpha1.SecretRef{
				{
					Name: secretName,
					Type: otilmv1alpha1.RefTypeEnv,
				},
			}
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())

			key := types.NamespacedName{Name: connName, Namespace: ns}

			By("waiting for Deployment and capturing initial checksum")
			var initialChecksum string
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
				cs, ok := dep.Spec.Template.Annotations[builder.ChecksumAnnotation]
				g.Expect(ok).To(BeTrue(), "checksum annotation should exist")
				g.Expect(cs).NotTo(BeEmpty())
				initialChecksum = cs
			}, timeout, interval).Should(Succeed())

			By("updating the Secret data")
			Eventually(func(g Gomega) {
				var s corev1.Secret
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: ns}, &s)).To(Succeed())
				s.Data["key"] = []byte("value-v2")
				g.Expect(k8sClient.Update(ctx, &s)).To(Succeed())
			}, timeout, interval).Should(Succeed())

			By("triggering reconciliation by touching the Connector")
			Eventually(func(g Gomega) {
				var latest otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &latest)).To(Succeed())
				if latest.Annotations == nil {
					latest.Annotations = map[string]string{}
				}
				latest.Annotations["test/trigger"] = "reconcile"
				g.Expect(k8sClient.Update(ctx, &latest)).To(Succeed())
			}, timeout, interval).Should(Succeed())

			By("verifying the checksum annotation has changed")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
				cs := dep.Spec.Template.Annotations[builder.ChecksumAnnotation]
				g.Expect(cs).NotTo(Equal(initialChecksum), "checksum should change after secret update")
			}, timeout, interval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// Test 4: Missing Secret sets Degraded condition
	// ---------------------------------------------------------------
	Context("TestMissingSecretDegraded", func() {
		var ns string
		const connName = "missing-secret-conn"

		BeforeEach(func() {
			ns = createTestNamespace("test-missing-secret")
		})

		It("should set the Degraded condition when a referenced Secret does not exist", func() {
			conn := newConnector(connName, ns)
			conn.Spec.SecretRefs = []otilmv1alpha1.SecretRef{
				{
					Name: "nonexistent-secret",
					Type: otilmv1alpha1.RefTypeEnv,
				},
			}
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())

			key := types.NamespacedName{Name: connName, Namespace: ns}

			By("verifying Degraded condition is True with MissingSecret reason")
			Eventually(func(g Gomega) {
				var c otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
				g.Expect(c.Status.Conditions).NotTo(BeEmpty())

				var found bool
				for _, cond := range c.Status.Conditions {
					if cond.Type == "Degraded" && cond.Status == metav1.ConditionTrue {
						g.Expect(cond.Reason).To(Equal("MissingSecret"))
						g.Expect(cond.Message).To(ContainSubstring("nonexistent-secret"))
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue(), "should have a Degraded=True condition")
			}, timeout, interval).Should(Succeed())

			By("verifying phase is Failed")
			Eventually(func(g Gomega) {
				var c otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
				g.Expect(c.Status.Phase).To(Equal(otilmv1alpha1.ConnectorPhaseFailed))
			}, timeout, interval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// Test 5: PDB lifecycle (create when enabled, delete when disabled)
	// ---------------------------------------------------------------
	Context("TestPDBLifecycle", func() {
		var ns string
		const connName = "pdb-conn"

		BeforeEach(func() {
			ns = createTestNamespace("test-pdb-lifecycle")
		})

		It("should create PDB when enabled and delete it when disabled", func() {
			By("creating a Connector without PDB")
			conn := newConnector(connName, ns)
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())

			key := types.NamespacedName{Name: connName, Namespace: ns}

			By("waiting for Deployment to confirm the reconciler ran")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
			}, timeout, interval).Should(Succeed())

			By("verifying no PDB exists")
			Consistently(func() bool {
				var pdb policyv1.PodDisruptionBudget
				err := k8sClient.Get(ctx, key, &pdb)
				return apierrors.IsNotFound(err)
			}, 2*time.Second, interval).Should(BeTrue(), "PDB should not exist when lifecycle.podDisruptionBudget is not set")

			By("enabling PDB on the Connector")
			Eventually(func(g Gomega) {
				var latest otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &latest)).To(Succeed())
				minAvail := intstr.FromInt32(1)
				latest.Spec.Lifecycle = &otilmv1alpha1.LifecycleSpec{
					PodDisruptionBudget: &otilmv1alpha1.PDBSpec{
						Enabled:      true,
						MinAvailable: &minAvail,
					},
				}
				g.Expect(k8sClient.Update(ctx, &latest)).To(Succeed())
			}, timeout, interval).Should(Succeed())

			By("verifying PDB is created")
			Eventually(func(g Gomega) {
				var pdb policyv1.PodDisruptionBudget
				g.Expect(k8sClient.Get(ctx, key, &pdb)).To(Succeed())
				g.Expect(pdb.Spec.MinAvailable.IntValue()).To(Equal(1))
				g.Expect(pdb.OwnerReferences).To(HaveLen(1))
				g.Expect(pdb.OwnerReferences[0].Name).To(Equal(connName))
			}, timeout, interval).Should(Succeed())

			By("disabling PDB on the Connector")
			Eventually(func(g Gomega) {
				var latest otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &latest)).To(Succeed())
				latest.Spec.Lifecycle.PodDisruptionBudget.Enabled = false
				g.Expect(k8sClient.Update(ctx, &latest)).To(Succeed())
			}, timeout, interval).Should(Succeed())

			By("verifying PDB is deleted")
			Eventually(func() bool {
				var pdb policyv1.PodDisruptionBudget
				err := k8sClient.Get(ctx, key, &pdb)
				return apierrors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue(), "PDB should be deleted when disabled")
		})
	})

	// ---------------------------------------------------------------
	// Test 6: Delete Connector and verify finalizer removal and owner references
	// ---------------------------------------------------------------
	// Note: envtest does NOT run the Kubernetes garbage collector, so owner-reference
	// based cascade deletion does not happen. Instead we verify that:
	//   (a) child resources carry the correct controller owner reference (guaranteeing
	//       real-cluster GC will clean them up), and
	//   (b) the finalizer is removed and the Connector is fully deleted.
	Context("TestDeleteConnectorCascade", func() {
		var ns string
		const connName = "delete-conn"

		BeforeEach(func() {
			ns = createTestNamespace("test-delete-cascade")
		})

		It("should remove the finalizer on deletion and child resources should have owner references", func() {
			conn := newConnector(connName, ns)
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())

			key := types.NamespacedName{Name: connName, Namespace: ns}

			By("waiting for child resources to be created with owner references")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
				g.Expect(dep.OwnerReferences).To(HaveLen(1))
				g.Expect(dep.OwnerReferences[0].Name).To(Equal(connName))
				g.Expect(*dep.OwnerReferences[0].Controller).To(BeTrue())

				var svc corev1.Service
				g.Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())
				g.Expect(svc.OwnerReferences).To(HaveLen(1))
				g.Expect(svc.OwnerReferences[0].Name).To(Equal(connName))
				g.Expect(*svc.OwnerReferences[0].Controller).To(BeTrue())

				var sa corev1.ServiceAccount
				g.Expect(k8sClient.Get(ctx, key, &sa)).To(Succeed())
				g.Expect(sa.OwnerReferences).To(HaveLen(1))
				g.Expect(sa.OwnerReferences[0].Name).To(Equal(connName))
				g.Expect(*sa.OwnerReferences[0].Controller).To(BeTrue())
			}, timeout, interval).Should(Succeed())

			By("verifying the Connector has a finalizer")
			Eventually(func(g Gomega) {
				var c otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
				g.Expect(c.Finalizers).To(ContainElement("otilm.com/finalizer"))
			}, timeout, interval).Should(Succeed())

			By("deleting the Connector")
			Eventually(func(g Gomega) {
				var c otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
				g.Expect(k8sClient.Delete(ctx, &c)).To(Succeed())
			}, timeout, interval).Should(Succeed())

			By("verifying the Connector is fully deleted (finalizer removed)")
			Eventually(func() bool {
				var c otilmv1alpha1.Connector
				err := k8sClient.Get(ctx, key, &c)
				return apierrors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())
		})
	})

	// ---------------------------------------------------------------
	// Test 7: Drift correction reverts manual changes to child resources
	// ---------------------------------------------------------------
	Context("TestDriftCorrection", func() {
		var ns string
		const connName = "drift-conn"

		BeforeEach(func() {
			ns = createTestNamespace("test-drift-correction")
		})

		It("should revert manual changes to the Service port after reconciliation", func() {
			conn := newConnector(connName, ns)
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())

			key := types.NamespacedName{Name: connName, Namespace: ns}

			By("waiting for the Service to be created with port 8080")
			Eventually(func(g Gomega) {
				var svc corev1.Service
				g.Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())
				g.Expect(svc.Spec.Ports).To(HaveLen(1))
				g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
			}, timeout, interval).Should(Succeed())

			By("manually modifying the Service port to 9999")
			Eventually(func(g Gomega) {
				var svc corev1.Service
				g.Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())
				svc.Spec.Ports[0].Port = 9999
				g.Expect(k8sClient.Update(ctx, &svc)).To(Succeed())
			}, timeout, interval).Should(Succeed())

			By("verifying the Service port is reverted to 8080 by the reconciler")
			Eventually(func(g Gomega) {
				var svc corev1.Service
				g.Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())
				g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
			}, timeout, interval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// Test 8: Status fields are populated correctly
	// ---------------------------------------------------------------
	Context("TestStatusReporting", func() {
		var ns string
		const connName = "status-conn"

		BeforeEach(func() {
			ns = createTestNamespace("test-status-reporting")
		})

		It("should populate observedGeneration, endpoint, and currentImage in status", func() {
			conn := newConnector(connName, ns)
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())

			key := types.NamespacedName{Name: connName, Namespace: ns}

			By("verifying observedGeneration is set")
			Eventually(func(g Gomega) {
				var c otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
				g.Expect(c.Status.ObservedGeneration).To(BeNumerically(">", 0))
			}, timeout, interval).Should(Succeed())

			By("verifying endpoint is populated")
			Eventually(func(g Gomega) {
				var c otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
				expectedEndpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:8080", connName, ns)
				g.Expect(c.Status.Endpoint).To(Equal(expectedEndpoint))
			}, timeout, interval).Should(Succeed())

			By("verifying currentImage is populated")
			Eventually(func(g Gomega) {
				var c otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
				g.Expect(c.Status.CurrentImage).To(Equal("registry.example.com/connector:1.0.0"))
			}, timeout, interval).Should(Succeed())
		})
	})
})

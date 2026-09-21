/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io"

// connectorTableRow returns Connector's rendered table row keyed by column name.
func connectorTableRow(namespace, name string) (map[string]any, error) {
	cfgCopy := rest.CopyConfig(cfg)
	gv := otilmv1alpha1.GroupVersion
	cfgCopy.GroupVersion = &gv
	cfgCopy.APIPath = "/apis"
	cfgCopy.NegotiatedSerializer = serializer.NewCodecFactory(scheme.Scheme).WithoutConversion()

	restClient, err := rest.RESTClientFor(cfgCopy)
	if err != nil {
		return nil, err
	}

	raw, err := restClient.Get().
		SetHeader("Accept", tableAccept).
		Namespace(namespace).Resource("connectors").Name(name).
		DoRaw(ctx)
	if err != nil {
		return nil, err
	}

	var table metav1.Table
	if err := json.Unmarshal(raw, &table); err != nil {
		return nil, err
	}
	if len(table.Rows) != 1 {
		return nil, fmt.Errorf("expected 1 table row for %s/%s, got %d", namespace, name, len(table.Rows))
	}

	row := make(map[string]any, len(table.ColumnDefinitions))
	for i, col := range table.ColumnDefinitions {
		if i < len(table.Rows[0].Cells) {
			row[col.Name] = table.Rows[0].Cells[i]
		}
	}
	return row, nil
}

var _ = Describe("Connector print columns", func() {
	Context("TestReadyPrintColumn", func() {
		var ns string
		const connName = "print-columns-conn"

		BeforeEach(func() {
			ns = createTestNamespace("test-print-columns")
		})

		It("should render the ready replica count under Ready in the server-side table", func() {
			conn := newConnector(connName, ns)
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())

			key := types.NamespacedName{Name: connName, Namespace: ns}

			By(waitingDeployment)
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
			}, timeout, interval).Should(Succeed())

			By("simulating one ready replica via the Deployment status subresource")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
				dep.Status.Replicas = 1
				dep.Status.ReadyReplicas = 1
				dep.Status.AvailableReplicas = 1
				g.Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())
			}, timeout, interval).Should(Succeed())

			By(triggerReconcile)
			Eventually(func(g Gomega) {
				var latest otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &latest)).To(Succeed())
				if latest.Annotations == nil {
					latest.Annotations = map[string]string{}
				}
				latest.Annotations[triggerAnnotation] = "print-columns"
				g.Expect(k8sClient.Update(ctx, &latest)).To(Succeed())
			}, timeout, interval).Should(Succeed())

			By("waiting for status.readyReplicas to reach 1")
			Eventually(func(g Gomega) {
				var c otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
				g.Expect(c.Status.ReadyReplicas).To(Equal(int32(1)))
			}, timeout, interval).Should(Succeed())

			By("verifying the Ready cell carries that count")
			row, err := connectorTableRow(ns, connName)
			Expect(err).NotTo(HaveOccurred())
			Expect(row).To(HaveKey("Ready"))
			Expect(row["Ready"]).To(BeEquivalentTo(1), "Ready cell must carry the ready replica count")
			Expect(row["Phase"]).To(Equal(string(otilmv1alpha1.ConnectorPhaseRunning)))
		})
	})
})

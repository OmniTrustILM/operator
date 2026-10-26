/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

// The builder tests resolve images from in-memory specs. This one goes through the apiserver,
// which applies the CRD's defaults to every image object a write carries: the one a user sets
// to pin a digest, and the empty one a Go-typed write sends for each unset component. A
// pullPolicy default stored there would override spec.common.image.pullPolicy.

import (
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

var _ = Describe("Shared image pull policy", func() {
	It("reaches every component that sets no pull policy of its own", func() {
		const ns = "ilm-common-pull-policy"
		p := lifecyclePlatform(ns, func(p *otilmv1alpha1.Platform) {
			p.Spec.Common.Image.PullPolicy = string(corev1.PullAlways)
			p.Spec.Scheduler.Image.Digest = "sha256:" + strings.Repeat("a", 64)
		})
		Expect(k8sClient.Create(ctx, p)).To(Succeed())

		By("waiting for the reconciler's finalizer Update, a Go-typed write of the whole spec")
		Eventually(func(g Gomega) {
			g.Expect(controllerutil.ContainsFinalizer(getPlatform(ns), platformFinalizer)).To(BeTrue())
		}, platformTimeout, platformInterval).Should(Succeed())

		// The gateway sets no image at all; the scheduler pins a digest but no pull policy.
		policies := map[string]corev1.PullPolicy{}
		for _, name := range []string{gatewayWorkloadName, schedulerWorkloadName} {
			var dep appsv1.Deployment
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &dep)
			}, platformTimeout, platformInterval).Should(Succeed())
			policies[name] = dep.Spec.Template.Spec.Containers[0].ImagePullPolicy
		}
		Expect(policies).To(HaveEach(corev1.PullAlways))
	})
})

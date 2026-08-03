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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	"github.com/OmniTrustILM/operator/test/utils"
)

// platformTestNamespace is the namespace used for the Platform CR and its prerequisite
// Secrets, kept isolated from the operator namespace and the Connector test namespace.
const platformTestNamespace = "ilm-platform-e2e"

// -------------------------------------------------------------------------
// Platform E2E Tests (external-mode reconcile)
//
// These specs mirror the Connector e2e: they apply a real Platform CR against the
// operator the suite already deployed (see the "Manager" BeforeAll) and assert the
// operator does its job. They deliberately exercise EXTERNAL mode (bring-your-own DB
// and broker) with the edge disabled, so the only upstream prerequisite is the
// operator itself — no CloudNativePG / RabbitMQ / Keycloak operators are needed.
// (Managed-infra e2e — real CNPG/RabbitMQ/Keycloak operators installed and the
// rendered CRs reconciled — is a separate follow-up task.)
//
// IMAGE TOLERANCE (the key design point): these specs do NOT assert that the ILM
// application pods reach Running. Two reasons make that unreliable and not the point
// of an external-mode reconcile test:
//   1. The external database and broker named in the CR (postgres.example.com /
//      rabbitmq.example.com) do not exist, so the JVM/.NET components cannot connect
//      and would crash-loop even if their images pulled.
//   2. With no spec.image overrides the platform components resolve to bare bundle
//      coordinates (e.g. "core:2.18.0", "kong:3.9.1"), which Kubernetes expands to
//      docker.io/library/... — those repositories are private/absent, so the pods sit
//      in ImagePullBackOff on a stock Kind node.
// What we assert instead is what the operator is actually responsible for: it RENDERS
// and OWNS the full set of component/gateway objects (Deployments + Services +
// ServiceAccounts), the shared ConfigMaps (messaging-configmap, the gateway's
// global-configmap = kong.yml), and the one composed Secret (auth-db), each
// with a controller owner reference back to the Platform; it PROGRESSES the Platform
// status (conditions + a Progressing/Running phase); it PRUNES a component that gets
// disabled; and it CLEANS UP the owner-ref'd children + the Platform object on delete.
// -------------------------------------------------------------------------

// platformExternalModeSpecs registers the Platform external-mode reconcile specs as a
// Context. It is invoked from WITHIN the top-level "Manager" Ordered Describe (see
// e2e_test.go) rather than declared as its own top-level Describe, so the specs run
// against the operator the "Manager" BeforeAll deploys and BEFORE its AfterAll
// undeploys it (a separate top-level Describe could be ordered after that teardown,
// running with no operator and no Platform CRD). This mirrors how the Connector specs
// reuse the deployed operator.
func platformExternalModeSpecs() {
	Context("Platform external-mode reconcile", Ordered, func() {
		const platformName = "ilm-e2e"
		const dbSecretName = "ilm-e2e-db"
		const messagingSecretName = "ilm-e2e-messaging"

		// platformComponents are the workload component roles the operator renders for an
		// external Platform with utils enabled (and the edge disabled). Each role is
		// the name of its Deployment, Service, and ServiceAccount, and the value of the
		// app.kubernetes.io/component label and the app.kubernetes.io/name label. The
		// gateway's role is "api-gateway"; the front-end is "fe-administrator".
		platformComponents := []string{
			"core",
			"auth",
			"scheduler",
			"fe-administrator",
			"auth-opa-policies",
			"utils",
			"api-gateway",
		}

		BeforeAll(func() {
			By("creating the Platform test namespace (idempotent — tolerate a reused cluster)")
			Expect(utils.CreateNamespaceIdempotent(platformTestNamespace)).
				To(Succeed(), "Failed to create Platform test namespace")

			By("creating the external database credentials Secret (basic-auth: username/password)")
			err := utils.ApplyResource("secret", "generic", dbSecretName,
				"-n", platformTestNamespace,
				"--type=kubernetes.io/basic-auth",
				"--from-literal=username=ilm",
				"--from-literal=password=e2e-db-password",
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create DB credentials Secret")

			By("creating the external messaging credentials Secret (basic-auth: username/password)")
			err = utils.ApplyResource("secret", "generic", messagingSecretName,
				"-n", platformTestNamespace,
				"--type=kubernetes.io/basic-auth",
				"--from-literal=username=ilm",
				"--from-literal=password=e2e-broker-password",
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create messaging credentials Secret")
		})

		AfterAll(func() {
			By("cleaning up the Platform if it still exists")
			cmd := exec.Command("kubectl", "delete", "platform", platformName,
				"-n", platformTestNamespace, "--ignore-not-found", "--timeout=120s")
			_, _ = utils.Run(cmd)

			By("deleting the Platform test namespace")
			utils.DeleteNamespace(platformTestNamespace)
		})

		AfterEach(func() {
			specReport := CurrentSpecReport()
			if specReport.Failed() {
				By("Fetching Platform object (yaml) for debugging")
				cmd := exec.Command("kubectl", "get", "platform", platformName,
					"-n", platformTestNamespace, "-o", "yaml")
				out, err := utils.Run(cmd)
				if err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Platform object:\n%s", out)
				}

				By("Fetching objects owned by the Platform in the test namespace")
				cmd = exec.Command("kubectl", "get",
					"deployments,services,serviceaccounts,configmaps,secrets",
					"-n", platformTestNamespace,
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", platformName),
				)
				out, err = utils.Run(cmd)
				if err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Platform-owned objects:\n%s", out)
				}

				By("Fetching events in the Platform test namespace")
				cmd = exec.Command("kubectl", "get", "events", "-n", platformTestNamespace,
					"--sort-by=.lastTimestamp")
				out, err = utils.Run(cmd)
				if err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Platform ns events:\n%s", out)
				}
			}
		})

		It("should apply an external Platform and render all component objects with a Platform owner reference", func() {
			By("creating the external-mode Platform CR (DB + messaging external, edge disabled, utils enabled)")
			// NOTE: no spec.image is set, so components resolve to bundle coordinates only —
			// the pods will ImagePullBackOff, which is expected and irrelevant here (see the
			// image-tolerance note at the top of this file). The CEL validation on the CRD
			// requires host/name/credentials.secretRef for an external DB and
			// host/credentials.secretRef for external messaging, which this CR satisfies.
			platformYAML := fmt.Sprintf(`
apiVersion: otilm.com/v1alpha1
kind: Platform
metadata:
  name: %s
  namespace: %s
spec:
  database:
    mode: external
    host: postgres.example.com
    port: 5432
    name: ilmdb
    credentials:
      secretRef: %s
  messaging:
    mode: external
    brokerType: rabbitmq
    host: rabbitmq.example.com
    port: 5672
    virtualHost: ilm
    credentials:
      secretRef: %s
  utils:
    enabled: true
`, platformName, platformTestNamespace, dbSecretName, messagingSecretName)

			tmpFile := writeTempYAML(platformYAML)
			defer func() { _ = os.Remove(tmpFile) }()

			cmd := exec.Command("kubectl", "apply", "-f", tmpFile)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Platform CR")

			By("waiting for the operator to render a Deployment for every component, owner-ref'd to the Platform")
			Eventually(func(g Gomega) {
				for _, comp := range platformComponents {
					ownerKind := getOwnerKind(g, "deployment", comp)
					g.Expect(ownerKind).To(Equal("Platform"),
						"Deployment %q should be owned by the Platform, got owner kind: %q", comp, ownerKind)
				}
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying a Service and a ServiceAccount exist (and are Platform-owned) for every component")
			Eventually(func(g Gomega) {
				for _, comp := range platformComponents {
					svcOwner := getOwnerKind(g, "service", comp)
					g.Expect(svcOwner).To(Equal("Platform"),
						"Service %q should be owned by the Platform, got owner kind: %q", comp, svcOwner)
					saOwner := getOwnerKind(g, "serviceaccount", comp)
					g.Expect(saOwner).To(Equal("Platform"),
						"ServiceAccount %q should be owned by the Platform, got owner kind: %q", comp, saOwner)
				}
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the shared ConfigMaps exist and are Platform-owned (messaging-configmap, global-configmap, fe-administrator-configmap)")
			Eventually(func(g Gomega) {
				for _, cm := range []string{"messaging-configmap", "global-configmap", "fe-administrator-configmap"} {
					owner := getOwnerKind(g, "configmap", cm)
					g.Expect(owner).To(Equal("Platform"),
						"ConfigMap %q should be owned by the Platform, got owner kind: %q", cm, owner)
				}
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the gateway's global-configmap holds the declarative kong.yml")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", "global-configmap",
					"-n", platformTestNamespace,
					"-o", "jsonpath={.data}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("kong.yml"),
					"global-configmap should contain a kong.yml key")
				g.Expect(output).To(ContainSubstring("services"),
					"kong.yml should declare gateway services/routes")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the operator composed the auth-db Secret (the .NET connection string) and owns it")
			Eventually(func(g Gomega) {
				owner := getOwnerKind(g, "secret", "auth-db")
				g.Expect(owner).To(Equal("Platform"),
					"auth-db Secret should be owned by the Platform, got owner kind: %q", owner)

				// The composed Secret must carry the connection-string key. We assert the KEY
				// exists, never the value (a composed credential must never be logged/printed).
				cmd := exec.Command("kubectl", "get", "secret", "auth-db",
					"-n", platformTestNamespace,
					"-o", "jsonpath={.data.connection-string}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(),
					"auth-db Secret should hold a non-empty connection-string key")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should progress the Platform status (conditions present, phase Progressing or Running)", func() {
			// The operator's job is done once it has applied the desired state and reflected
			// the cluster's MEASURED readiness in status. Because the app pods can't pull/run
			// here, the platform stays Progressing (Available=False, Progressing=True) — which
			// is exactly the correct, honest status for this environment. We assert the status
			// MOVED OFF empty (conditions exist, phase is one the operator emits) rather than
			// pinning Running, so the spec is reliable regardless of image availability.
			By("waiting for the Platform to publish Available and Progressing conditions")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "platform", platformName,
					"-n", platformTestNamespace,
					"-o", "jsonpath={range .status.conditions[*]}{.type}={.status} {end}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Available="),
					"Platform should publish an Available condition, got: %q", output)
				g.Expect(output).To(ContainSubstring("Progressing="),
					"Platform should publish a Progressing condition, got: %q", output)
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the Platform phase is one the operator emits (Progressing or Running)")
			Eventually(func(g Gomega) {
				phase := getPlatformPhase(platformName)
				g.Expect(phase).To(Or(Equal("Progressing"), Equal("Running")),
					"Platform phase should be Progressing or Running (not Degraded/empty), got: %q", phase)
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying observedGeneration is set on the status (the controller observed the spec)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "platform", platformName,
					"-n", platformTestNamespace,
					"-o", "jsonpath={.status.observedGeneration}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).NotTo(BeEmpty(),
					"Platform status.observedGeneration should be set")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should prune the utils objects when the component is disabled", func() {
			// Validates the end-to-end prune on a real cluster: flipping utils.enabled
			// to false de-renders utils, and the post-apply prune (owner-ref + label
			// scoped) deletes its Deployment/Service/ServiceAccount.
			By("confirming the utils Deployment exists before the prune")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "utils",
					"-n", platformTestNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "utils Deployment should exist while enabled")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("patching the Platform to disable utils")
			patch := `{"spec":{"utils":{"enabled":false}}}`
			cmd := exec.Command("kubectl", "patch", "platform", platformName,
				"-n", platformTestNamespace,
				"--type=merge",
				"-p", patch,
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch Platform to disable utils")

			By("waiting for the utils Deployment to be pruned")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "utils",
					"-n", platformTestNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(),
					"utils Deployment should be pruned after disabling the component")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting for the utils Service to be pruned")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", "utils",
					"-n", platformTestNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(),
					"utils Service should be pruned after disabling the component")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting for the utils ServiceAccount to be pruned")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "serviceaccount", "utils",
					"-n", platformTestNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(),
					"utils ServiceAccount should be pruned after disabling the component")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the other components remain (the prune only removed the disabled one)")
			cmd = exec.Command("kubectl", "get", "deployment", "core",
				"-n", platformTestNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "core Deployment should remain after pruning utils")
		})

		It("should delete the Platform and clean up all owner-ref'd children", func() {
			By("recording the component Deployments that exist before deletion")
			// utils was pruned in the previous spec; the remaining components persist.
			remaining := []string{"core", "auth", "scheduler",
				"fe-administrator", "auth-opa-policies", "api-gateway"}

			By("deleting the Platform CR")
			cmd := exec.Command("kubectl", "delete", "platform", platformName,
				"-n", platformTestNamespace,
				"--timeout=120s",
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete Platform CR (finalizer should run and return)")

			By("verifying the Platform object is gone (finalizer ran and was removed)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "platform", platformName,
					"-n", platformTestNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Platform object should be removed after deletion")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying every component Deployment is garbage-collected (owner-ref cascade)")
			Eventually(func(g Gomega) {
				for _, comp := range remaining {
					cmd := exec.Command("kubectl", "get", "deployment", comp,
						"-n", platformTestNamespace)
					_, err := utils.Run(cmd)
					g.Expect(err).To(HaveOccurred(),
						"Deployment %q should be deleted after Platform deletion", comp)
				}
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the composed auth-db Secret is garbage-collected")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secret", "auth-db",
					"-n", platformTestNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(),
					"auth-db Secret should be deleted after Platform deletion")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying no Platform-owned objects remain in the namespace")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"deployments,services,serviceaccounts,configmaps",
					"-n", platformTestNamespace,
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", platformName),
					"-o", "name",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(BeEmpty(),
					"no Platform-owned objects should remain, got: %q", output)
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})
	})
}

// platformManagedDBNamespace is the namespace for the managed-database Platform e2e, kept
// isolated from the external-mode Platform namespace and the operator namespace.
const platformManagedDBNamespace = "ilm-platform-managed-db-e2e"

// -------------------------------------------------------------------------
// Platform E2E Tests (managed PostgreSQL via the REAL CloudNativePG operator)
//
// This is the FIRST managed-infra e2e — the VERIFY-validation gate. Unlike the external-
// mode specs (which need only the operator), these specs install the REAL CloudNativePG
// operator and apply a Platform with database.mode=managed, then assert the operator's
// rendered CloudNativePG Cluster is ACCEPTED by CNPG's admission webhook, RECONCILED to
// healthy, and that the platform converges its DatabaseReady condition + composes the
// auth-db Secret from the CNPG-generated credentials.
//
// WHY THIS MATTERS: the operator renders the CNPG Cluster as a preset-GVK unstructured
// object from field paths inferred against CNPG's documented API and flagged with
// `// VERIFY(cnpg)` comments. If any of those field paths/values is wrong, the real CNPG
// admission webhook rejects the apply (the Cluster never appears or carries no status) or
// the controller never reconciles it to healthy. So this test turns the doc-inferred
// guesses into operator-confirmed truth against CloudNativePG.
//
// SCOPING: messaging is EXTERNAL (a dummy host) and the edge + Keycloak are omitted, so
// the ONLY upstream prerequisite beyond the operator is CloudNativePG — keeping this test
// focused on the database. As with the external-mode specs we do NOT assert the ILM app
// pods reach Running (their images are private); we assert the operator's CR + readback +
// status. CNPG's own PostgreSQL operand image is PUBLIC, so the Cluster's PG pods DO run,
// which is exactly what lets us validate the rendered Cluster end-to-end.
// -------------------------------------------------------------------------

// platformManagedDatabaseSpecs registers the managed-database Platform e2e as a Context
// inside the top-level "Manager" Ordered Describe (see e2e_test.go), exactly like
// platformExternalModeSpecs — so it runs against the operator the "Manager" BeforeAll
// deploys (and before its AfterAll undeploys it). It installs CloudNativePG in its own
// BeforeAll (idempotent: skips if the CRDs are already present) so the spec is
// self-contained.
func platformManagedDatabaseSpecs() {
	Context("Platform managed-database reconcile (real CloudNativePG)", Ordered, Label("managed", "managed-postgres"), func() {
		const platformName = "ilm-mdb"
		const messagingSecretName = "ilm-mdb-messaging"
		// clusterName is the CNPG Cluster the operator renders for this Platform:
		// "<platform>-db" (ManagedDatabaseName). The CNPG-generated read-write Service is
		// "<cluster>-rw" and the app Secret "<cluster>-app".
		clusterName := platformName + "-db"
		appSecretName := clusterName + "-app"
		rwServiceName := clusterName + "-rw"

		// cnpgWasAlreadyInstalled records whether CloudNativePG was present before this spec
		// installed it, so AfterAll only uninstalls what this spec added (CI-friendly).
		var cnpgWasAlreadyInstalled bool

		BeforeAll(func() {
			By("installing the CloudNativePG operator (skipped if already present)")
			cnpgWasAlreadyInstalled = utils.IsCloudNativePGCRDsInstalled()
			if cnpgWasAlreadyInstalled {
				_, _ = fmt.Fprintf(GinkgoWriter, "CloudNativePG already installed; skipping install\n")
			} else {
				Expect(utils.InstallCloudNativePG()).To(Succeed(), "Failed to install CloudNativePG")
			}

			By("creating the managed-database Platform test namespace (idempotent — tolerate a reused cluster)")
			Expect(utils.CreateNamespaceIdempotent(platformManagedDBNamespace)).
				To(Succeed(), "Failed to create managed-DB test namespace")

			By("creating the external messaging credentials Secret (basic-auth: username/password)")
			// Only the messaging credential is created by hand — the DB credential is GENERATED
			// by CloudNativePG (the operator reads back the <cluster>-app Secret).
			Expect(utils.ApplyResource("secret", "generic", messagingSecretName,
				"-n", platformManagedDBNamespace,
				"--type=kubernetes.io/basic-auth",
				"--from-literal=username=ilm",
				"--from-literal=password=e2e-broker-password",
			)).To(Succeed(), "Failed to create messaging credentials Secret")
		})

		AfterAll(func() {
			By("deleting the Platform if it still exists")
			cmd := exec.Command("kubectl", "delete", "platform", platformName,
				"-n", platformManagedDBNamespace, "--ignore-not-found", "--timeout=120s")
			_, _ = utils.Run(cmd)

			By("deleting the CNPG Cluster left behind by the Retain deletion policy")
			// The default Retain policy deliberately leaves the Cluster intact when the
			// Platform is deleted (asserted below); clean it up here so the namespace can drain.
			cmd = exec.Command("kubectl", "delete", "cluster.postgresql.cnpg.io", clusterName,
				"-n", platformManagedDBNamespace, "--ignore-not-found", "--timeout=120s")
			_, _ = utils.Run(cmd)

			By("deleting the managed-database Platform test namespace")
			utils.DeleteNamespace(platformManagedDBNamespace)

			By("uninstalling CloudNativePG if this spec installed it")
			if !cnpgWasAlreadyInstalled {
				utils.UninstallCloudNativePG()
			}
		})

		AfterEach(func() {
			specReport := CurrentSpecReport()
			if specReport.Failed() {
				By("Fetching Platform object (yaml) for debugging")
				cmd := exec.Command("kubectl", "get", "platform", platformName,
					"-n", platformManagedDBNamespace, "-o", "yaml")
				if out, err := utils.Run(cmd); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Platform object:\n%s", out)
				}

				By("Fetching the CNPG Cluster (yaml) for debugging")
				cmd = exec.Command("kubectl", "get", "cluster.postgresql.cnpg.io", clusterName,
					"-n", platformManagedDBNamespace, "-o", "yaml")
				if out, err := utils.Run(cmd); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "CNPG Cluster:\n%s", out)
				}

				By("Fetching events in the managed-DB test namespace")
				cmd = exec.Command("kubectl", "get", "events", "-n", platformManagedDBNamespace,
					"--sort-by=.lastTimestamp")
				if out, err := utils.Run(cmd); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Managed-DB ns events:\n%s", out)
				}
			}
		})

		It("should apply a managed Platform and have CloudNativePG ACCEPT the rendered Cluster", func() {
			By("creating the managed-database Platform CR (DB managed via CNPG, messaging external)")
			// A small managed cluster: 1 instance, PostgreSQL 16, 1Gi storage — provisions
			// fast on a Kind node while still exercising the real CNPG admission + reconcile.
			// The PG version selects the CNPG community operand image (public), so CNPG's PG
			// pod actually runs and the Cluster reaches healthy. Messaging is external with a
			// dummy host (it satisfies the CEL rule and keeps this scoped to the database);
			// the edge and Keycloak are omitted.
			platformYAML := fmt.Sprintf(`
apiVersion: otilm.com/v1alpha1
kind: Platform
metadata:
  name: %s
  namespace: %s
spec:
  database:
    mode: managed
    managed:
      instances: 1
      version: "16"
      storage:
        size: 1Gi
  messaging:
    mode: external
    brokerType: rabbitmq
    host: rabbitmq.example.com
    port: 5672
    virtualHost: ilm
    credentials:
      secretRef: %s
`, platformName, platformManagedDBNamespace, messagingSecretName)

			tmpFile := writeTempYAML(platformYAML)
			defer func() { _ = os.Remove(tmpFile) }()

			cmd := exec.Command("kubectl", "apply", "-f", tmpFile)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create managed-database Platform CR")

			By("waiting for the operator to render a CNPG Cluster that CNPG accepted (it exists and CNPG set status on it)")
			// The core VERIFY assertion: if a Cluster spec field path the operator renders is
			// wrong, CNPG's admission webhook rejects the SSA apply (the operator's apply errors
			// and the Cluster never appears) OR the controller never reconciles it (no status).
			// Observing the Cluster WITH a status phase proves CNPG accepted AND is reconciling
			// the operator's rendered spec.
			Eventually(func(g Gomega) {
				phase := getCNPGClusterPhase(clusterName)
				g.Expect(phase).NotTo(BeEmpty(),
					"CNPG should set a status.phase on the operator's Cluster (accepted + reconciling)")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the operator's rendered Cluster carries the spec fields CNPG expects (instances, storage, imageName, bootstrap.initdb)")
			// Read the fields back THROUGH CNPG: a field CNPG ignored/renamed would be absent
			// here. This confirms the VERIFY(cnpg) field paths against the real CRD.
			Eventually(func(g Gomega) {
				instances := getCNPGClusterField(g, clusterName, "{.spec.instances}")
				g.Expect(instances).To(Equal("1"), "spec.instances should round-trip through CNPG")
				size := getCNPGClusterField(g, clusterName, "{.spec.storage.size}")
				g.Expect(size).To(Equal("1Gi"), "spec.storage.size should round-trip through CNPG")
				img := getCNPGClusterField(g, clusterName, "{.spec.imageName}")
				g.Expect(img).To(ContainSubstring(":16"), "spec.imageName should select the PG 16 operand image")
				db := getCNPGClusterField(g, clusterName, "{.spec.bootstrap.initdb.database}")
				g.Expect(db).To(Equal("ilm"), "spec.bootstrap.initdb.database is the readback-contract app DB")
				owner := getCNPGClusterField(g, clusterName, "{.spec.bootstrap.initdb.owner}")
				g.Expect(owner).To(Equal("ilm"), "spec.bootstrap.initdb.owner is the readback-contract owner")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should reach a healthy CNPG Cluster (the same Ready signal the operator's managedClusterReady probes)", func() {
			By("waiting for the CNPG Cluster to report healthy (status.phase 'Cluster in healthy state' and/or Ready condition True)")
			// This is the exact Ready signal internal/controller/platform/gate_database.go's
			// managedClusterReady checks. If the phase string or the Ready condition type were
			// wrong vs real CNPG, the operator would never see the Cluster as ready (and the
			// DatabaseReady assertion below would hang) — that would be a VERIFY bug to FIX.
			// Provisioning a real PG instance + PVC takes minutes, so the timeout is generous.
			Eventually(func(g Gomega) {
				g.Expect(cnpgClusterReady(g, clusterName)).To(BeTrue(),
					"CNPG Cluster should reach the healthy/Ready signal the operator probes")
			}, 8*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should produce the CNPG <cluster>-app Secret and read it back (the operator's credential source)", func() {
			By("verifying the CNPG-generated <cluster>-app Secret appears with username/password keys")
			// The operator's readback (db_connection.go) wires dependents to this CNPG-generated
			// Secret by reference. Its keys are the VERIFY(cnpg) bom.WiringProfile.DatabaseCred
			// names (username/password). We assert the KEYS exist, never the values.
			Eventually(func(g Gomega) {
				userKey := getSecretDataKey(g, platformManagedDBNamespace, appSecretName, "username")
				g.Expect(userKey).NotTo(BeEmpty(), "<cluster>-app Secret should carry a username key")
				passKey := getSecretDataKey(g, platformManagedDBNamespace, appSecretName, "password")
				g.Expect(passKey).NotTo(BeEmpty(), "<cluster>-app Secret should carry a password key")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the CNPG read-write Service <cluster>-rw exists (the host the operator wires dependents to)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", rwServiceName,
					"-n", platformManagedDBNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "CNPG should create the <cluster>-rw read-write Service")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should converge DatabaseReady=True and compose the auth-db Secret from the CNPG credentials", func() {
			By("waiting for the operator's DatabaseReady condition to flip to True")
			// DatabaseReady=True is the operator's signal that managedClusterReady returned true
			// AND the <cluster>-app Secret is present — the end-to-end proof that the managed
			// database converged through the real CNPG operator.
			Eventually(func(g Gomega) {
				status := getPlatformConditionStatus(g, platformName, "DatabaseReady")
				g.Expect(status).To(Equal(conditionStatusTrue),
					"DatabaseReady should be True once the managed CNPG cluster is healthy and its app Secret exists")
			}, 8*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the operator composed the auth-db Secret (the .NET connection string) and owns it")
			// The composed Secret proves the readback fed the mode-agnostic wiring: the operator
			// built auth-db FROM the CNPG-generated credentials. We assert the connection-
			// string KEY exists (a composed credential must never be logged/printed), never its value.
			Eventually(func(g Gomega) {
				owner := getOwnerKindInNS(g, platformManagedDBNamespace, "secret", "auth-db")
				g.Expect(owner).To(Equal("Platform"),
					"auth-db Secret should be owned by the Platform, got owner kind: %q", owner)
				connKey := getSecretDataKey(g, platformManagedDBNamespace, "auth-db", "connection-string")
				g.Expect(connKey).NotTo(BeEmpty(),
					"auth-db Secret should hold a non-empty connection-string key")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should LEAVE the CNPG Cluster intact when the Platform is deleted (default Retain policy)", func() {
			By("confirming the CNPG Cluster exists before deletion")
			cmd := exec.Command("kubectl", "get", "cluster.postgresql.cnpg.io", clusterName,
				"-n", platformManagedDBNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "CNPG Cluster should exist before the Platform is deleted")

			By("deleting the Platform CR (default deletionPolicy is Retain)")
			cmd = exec.Command("kubectl", "delete", "platform", platformName,
				"-n", platformManagedDBNamespace, "--timeout=120s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete Platform CR (finalizer should run and return)")

			By("verifying the Platform object is gone (finalizer ran and was removed)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "platform", platformName,
					"-n", platformManagedDBNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Platform object should be removed after deletion")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the CNPG Cluster STILL EXISTS (deletion-safety: Retain leaves managed infra intact)")
			// The managed Cluster carries NO controller owner reference and is prune-excluded, so
			// owner-ref GC must not collect it and the Retain deletion path must not delete it.
			// Re-check over a window so a late GC sweep would be caught.
			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cluster.postgresql.cnpg.io", clusterName,
					"-n", platformManagedDBNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(),
					"CNPG Cluster must remain after Platform deletion under the default Retain policy")
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})
	})
}

// platformManagedMQNamespace is the namespace for the managed-messaging Platform e2e, kept
// isolated from the other Platform namespaces and the operator namespace.
const platformManagedMQNamespace = "ilm-platform-managed-mq-e2e"

// conditionStatusTrue is the metav1.ConditionTrue string value, used by the readiness
// helpers that compare a status condition's status to "True" (the managed-DB and managed-
// messaging readiness probes + the operator's adjunct conditions).
const conditionStatusTrue = "True"

// -------------------------------------------------------------------------
// Platform E2E Tests (managed RabbitMQ via the REAL Cluster + Topology operators)
//
// This is the SECOND managed-infra e2e — the messaging VERIFY-validation gate, mirroring
// the managed-database (CloudNativePG) e2e. It installs the REAL RabbitMQ Cluster Operator
// AND Messaging Topology Operator and applies a Platform with messaging.mode=managed, then
// asserts the operator's rendered RabbitmqCluster is ACCEPTED by the Cluster Operator and
// RECONCILED to Ready, the FULL topology (Vhost + 5 Users + 5 Permissions + 2 Exchanges +
// 10 Queues + 9 Bindings) is accepted by the Topology Operator and reconciles, the per-user
// credential Secrets the readback expects appear, MessagingReady converges, and Retain
// leaves the broker on delete.
//
// WHY THIS MATTERS: the operator renders the RabbitmqCluster + topology as preset-GVK
// unstructured objects from field paths inferred against the documented rabbitmq.com APIs
// and flagged with `// VERIFY(rabbitmq)` comments. The Topology Operator's webhooks +
// controllers refute any wrong User/Permission/Binding/Exchange/Queue field shape (a
// rejected apply, or a CR that never reaches Ready), and a wrong generated-Secret name or
// cluster Ready signal would hang MessagingReady. So this test turns the doc-inferred
// guesses into operator-confirmed truth against the real operators.
//
// SCOPING: database is EXTERNAL (a dummy host) and the edge + Keycloak are omitted, so the
// ONLY upstream prerequisites beyond the operator are the two RabbitMQ operators (+ the
// cert-manager the suite already installs, which the Topology Operator's webhook needs). As
// with the other specs we do NOT assert the ILM app pods reach Running (their images are
// private); we assert the operator's CRs + readback + status. RabbitMQ's operand image is
// PUBLIC, so the broker's pod DOES run, which is what lets us validate the topology
// end-to-end (the Topology Operator really creates the vhost/users/etc. against it).
// -------------------------------------------------------------------------

// platformManagedMessagingSpecs registers the managed-messaging Platform e2e as a Context
// inside the top-level "Manager" Ordered Describe (see e2e_test.go), exactly like
// platformManagedDatabaseSpecs — so it runs against the operator the "Manager" BeforeAll
// deploys. It installs both RabbitMQ operators in its own BeforeAll (idempotent: skips
// whichever CRDs are already present) so the spec is self-contained.
func platformManagedMessagingSpecs() {
	Context("Platform managed-messaging reconcile (real RabbitMQ Cluster + Topology operators)", Ordered, Label("managed", "managed-rabbitmq"), func() {
		const platformName = "ilm-mmq"
		// clusterName is the RabbitmqCluster the operator renders for this Platform:
		// "<platform>-messaging" (ManagedMessagingName). The Cluster Operator names the AMQP
		// client Service after the cluster (suffix ""), so the broker Service is "<cluster>".
		clusterName := platformName + "-messaging"
		// The four platform users are "<cluster>-<role>"; the Topology Operator generates a
		// "<user>-user-credentials" Secret for each. The readback wires Core to the core-user
		// Secret and the provisioning flow to the provisioner-user Secret.
		userName := func(role string) string { return clusterName + "-" + role }
		coreUserSecret := userName("core") + "-user-credentials"
		provisionerUserSecret := userName("provisioner") + "-user-credentials"

		// Track whether each operator was pre-installed so AfterAll only uninstalls what this
		// spec added (CI-friendly).
		var clusterOpWasInstalled, topologyOpWasInstalled bool

		BeforeAll(func() {
			By("installing the RabbitMQ Cluster Operator (skipped if already present)")
			clusterOpWasInstalled = utils.IsRabbitMQClusterOperatorCRDsInstalled()
			if clusterOpWasInstalled {
				_, _ = fmt.Fprintf(GinkgoWriter, "RabbitMQ Cluster Operator already installed; skipping install\n")
			} else {
				Expect(utils.InstallRabbitMQClusterOperator()).To(Succeed(), "Failed to install RabbitMQ Cluster Operator")
			}

			By("installing the Messaging Topology Operator (skipped if already present)")
			topologyOpWasInstalled = utils.IsRabbitMQTopologyOperatorCRDsInstalled()
			if topologyOpWasInstalled {
				_, _ = fmt.Fprintf(GinkgoWriter, "Messaging Topology Operator already installed; skipping install\n")
			} else {
				Expect(utils.InstallRabbitMQTopologyOperator()).To(Succeed(), "Failed to install Messaging Topology Operator")
			}

			By("creating the managed-messaging Platform test namespace (idempotent — tolerate a reused cluster)")
			Expect(utils.CreateNamespaceIdempotent(platformManagedMQNamespace)).
				To(Succeed(), "Failed to create managed-messaging test namespace")

			By("creating the external database credentials Secret (basic-auth: username/password)")
			// Only the DB credential is created by hand — the BROKER credentials are GENERATED
			// by the Messaging Topology Operator (the operator reads back the per-user
			// <user>-user-credentials Secrets). Database is external here to keep this scoped to
			// messaging; its CEL rule requires host/name/credentials.secretRef.
			Expect(utils.ApplyResource("secret", "generic", "ilm-mmq-db",
				"-n", platformManagedMQNamespace,
				"--type=kubernetes.io/basic-auth",
				"--from-literal=username=ilm",
				"--from-literal=password=e2e-db-password",
			)).To(Succeed(), "Failed to create DB credentials Secret")
		})

		AfterAll(func() {
			By("deleting the Platform if it still exists")
			cmd := exec.Command("kubectl", "delete", "platform", platformName,
				"-n", platformManagedMQNamespace, "--ignore-not-found", "--timeout=120s")
			_, _ = utils.Run(cmd)

			By("deleting the RabbitmqCluster left behind by the Retain deletion policy")
			// The default Retain policy deliberately leaves the cluster intact when the Platform
			// is deleted (asserted below); clean it up here so the namespace can drain.
			cmd = exec.Command("kubectl", "delete", "rabbitmqcluster.rabbitmq.com", clusterName,
				"-n", platformManagedMQNamespace, "--ignore-not-found", "--timeout=120s")
			_, _ = utils.Run(cmd)

			By("deleting the managed-messaging Platform test namespace")
			utils.DeleteNamespace(platformManagedMQNamespace)

			By("uninstalling the RabbitMQ operators this spec installed")
			if !topologyOpWasInstalled {
				utils.UninstallRabbitMQTopologyOperator()
			}
			if !clusterOpWasInstalled {
				utils.UninstallRabbitMQClusterOperator()
			}
		})

		AfterEach(func() {
			specReport := CurrentSpecReport()
			if specReport.Failed() {
				By("Fetching Platform object (yaml) for debugging")
				cmd := exec.Command("kubectl", "get", "platform", platformName,
					"-n", platformManagedMQNamespace, "-o", "yaml")
				if out, err := utils.Run(cmd); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Platform object:\n%s", out)
				}

				By("Fetching the RabbitmqCluster (yaml) for debugging")
				cmd = exec.Command("kubectl", "get", "rabbitmqcluster.rabbitmq.com", clusterName,
					"-n", platformManagedMQNamespace, "-o", "yaml")
				if out, err := utils.Run(cmd); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "RabbitmqCluster:\n%s", out)
				}

				By("Fetching the topology CRs for debugging")
				cmd = exec.Command("kubectl", "get",
					"vhosts.rabbitmq.com,users.rabbitmq.com,permissions.rabbitmq.com,"+
						"exchanges.rabbitmq.com,queues.rabbitmq.com,bindings.rabbitmq.com",
					"-n", platformManagedMQNamespace, "-o", "wide")
				if out, err := utils.Run(cmd); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Topology CRs:\n%s", out)
				}

				By("Fetching events in the managed-messaging test namespace")
				cmd = exec.Command("kubectl", "get", "events", "-n", platformManagedMQNamespace,
					"--sort-by=.lastTimestamp")
				if out, err := utils.Run(cmd); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Managed-messaging ns events:\n%s", out)
				}
			}
		})

		It("should apply a managed Platform and have the Cluster Operator ACCEPT the rendered RabbitmqCluster", func() {
			By("creating the managed-messaging Platform CR (messaging managed via RabbitMQ, database external)")
			// A small managed cluster: 1 replica, a pinned RabbitMQ version, 1Gi storage —
			// provisions fast on a Kind node while still exercising the real Cluster Operator
			// admission + reconcile and the Topology Operator's reconcile against the live
			// broker. The RabbitMQ operand image is public, so the broker's pod actually runs.
			// Database is external with a dummy host (it satisfies the CEL rule and keeps this
			// scoped to messaging); the edge and Keycloak are omitted.
			platformYAML := fmt.Sprintf(`
apiVersion: otilm.com/v1alpha1
kind: Platform
metadata:
  name: %s
  namespace: %s
spec:
  database:
    mode: external
    host: postgres.example.com
    port: 5432
    name: ilmdb
    credentials:
      secretRef: ilm-mmq-db
  messaging:
    mode: managed
    brokerType: rabbitmq
    managed:
      replicas: 1
      version: "4.0"
      storage:
        size: 1Gi
`, platformName, platformManagedMQNamespace)

			tmpFile := writeTempYAML(platformYAML)
			defer func() { _ = os.Remove(tmpFile) }()

			cmd := exec.Command("kubectl", "apply", "-f", tmpFile)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create managed-messaging Platform CR")

			By("waiting for the operator to render a RabbitmqCluster the Cluster Operator accepted (it exists and has a status)")
			// The core VERIFY assertion for the cluster: if a RabbitmqCluster spec field path
			// the operator renders is wrong, the Cluster Operator's admission webhook rejects the
			// SSA apply (the cluster never appears) OR the controller never reconciles it (no
			// status). Observing the cluster WITH status conditions proves it was accepted AND is
			// being reconciled.
			Eventually(func(g Gomega) {
				conds := getRabbitMQClusterField(g, clusterName, "{.status.conditions}")
				g.Expect(conds).NotTo(BeEmpty(),
					"Cluster Operator should set status.conditions on the operator's RabbitmqCluster (accepted + reconciling)")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the rendered RabbitmqCluster carries the spec fields the Cluster Operator expects (replicas, persistence.storage, image)")
			// Read the fields back THROUGH the Cluster Operator: a field it ignored/renamed would
			// be absent here. This confirms the VERIFY(rabbitmq) field paths against the real CRD.
			Eventually(func(g Gomega) {
				replicas := getRabbitMQClusterField(g, clusterName, "{.spec.replicas}")
				g.Expect(replicas).To(Equal("1"), "spec.replicas should round-trip through the Cluster Operator")
				size := getRabbitMQClusterField(g, clusterName, "{.spec.persistence.storage}")
				g.Expect(size).To(Equal("1Gi"), "spec.persistence.storage should round-trip through the Cluster Operator")
				img := getRabbitMQClusterField(g, clusterName, "{.spec.image}")
				g.Expect(img).To(ContainSubstring("4.0"), "spec.image should select the requested RabbitMQ version")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should reach a Ready RabbitmqCluster (the same signal the operator's managedBrokerReady probes)", func() {
			By("waiting for the RabbitmqCluster to report Ready (AllReplicasReady and/or ClusterAvailable True)")
			// This is the exact Ready signal internal/controller/platform/gate_messaging.go's
			// managedBrokerReady checks. If the condition types were wrong vs the real Cluster
			// Operator, the operator would never see the cluster as Ready (and the MessagingReady
			// assertion below would hang) — that would be a VERIFY bug to FIX. Booting a real
			// broker + PVC takes minutes, so the timeout is generous.
			Eventually(func(g Gomega) {
				g.Expect(rabbitMQClusterReady(g, clusterName)).To(BeTrue(),
					"RabbitmqCluster should reach the Ready signal the operator probes (AllReplicasReady/ClusterAvailable)")
			}, 8*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should have the Topology Operator ACCEPT and reconcile the FULL topology (vhost, 5 users, 5 permissions, 2 exchanges, 10 queues, 9 bindings)", func() {
			// The core VERIFY validation for the topology. The Topology Operator's webhooks +
			// controllers refute any wrong field shape: a rejected apply leaves the CR absent, and
			// a CR whose spec the operator cannot apply to the broker never reaches its Ready
			// condition (Ready=True). Asserting EVERY rendered topology CR reaches Ready confirms
			// the User/Permission/Binding/Exchange/Queue shapes (spec.userReference,
			// spec.permissions.{configure,write,read}, binding source/destination/destinationType,
			// spec.rabbitmqClusterReference, etc.) against the real operator.
			By("listing every topology CR the operator should have rendered")
			// metadata.name of each rendered topology CR (see managed_messaging.go's naming):
			//   Vhost:       <cluster>-vhost
			//   User:        <cluster>-<role>
			//   Permission:  <cluster>-<role>-permission
			//   Exchange:    <cluster>-exchange-<sanitized name>
			//   Queue:       <cluster>-queue-<sanitized name>
			//   Binding:     <cluster>-binding-<sanitized source-destination>
			roles := []string{"administrator", "provisioner", "proxy", "core", "monitor"}
			users := make([]string, 0, len(roles))
			permissions := make([]string, 0, len(roles))
			for _, role := range roles {
				users = append(users, userName(role))
				permissions = append(permissions, userName(role)+"-permission")
			}
			exchanges := []string{
				clusterName + "-exchange-czertainly",
				clusterName + "-exchange-czertainly-proxy",
			}
			queues := []string{
				clusterName + "-queue-core",
				clusterName + "-queue-core-audit-logs",
				clusterName + "-queue-core-notifications",
				clusterName + "-queue-core-scheduler",
				clusterName + "-queue-core-actions",
				clusterName + "-queue-core-validation",
				clusterName + "-queue-core-events",
				clusterName + "-queue-time-quality-config",
				clusterName + "-queue-time-quality-config-request",
				clusterName + "-queue-time-quality-results",
			}
			bindings := []string{
				clusterName + "-binding-czertainly-core-audit-logs",
				clusterName + "-binding-czertainly-core-notifications",
				clusterName + "-binding-czertainly-core-actions",
				clusterName + "-binding-czertainly-core-scheduler",
				clusterName + "-binding-czertainly-core-validation",
				clusterName + "-binding-czertainly-core-events",
				clusterName + "-binding-czertainly-time-quality-config",
				clusterName + "-binding-czertainly-time-quality-config-request",
				clusterName + "-binding-czertainly-time-quality-results",
			}

			By("verifying the Vhost CR exists and reconciles to Ready")
			Eventually(func(g Gomega) {
				g.Expect(topologyObjectReady(g, "vhost", clusterName+"-vhost")).To(BeTrue(),
					"Vhost CR should be accepted and reach Ready (spec.name + spec.rabbitmqClusterReference)")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying all 5 User CRs exist and reconcile to Ready")
			Eventually(func(g Gomega) {
				for _, u := range users {
					g.Expect(topologyObjectReady(g, "user", u)).To(BeTrue(),
						"User CR %q should be accepted and reach Ready (spec.tags + spec.rabbitmqClusterReference)", u)
				}
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying all 5 Permission CRs exist and reconcile to Ready")
			Eventually(func(g Gomega) {
				for _, perm := range permissions {
					g.Expect(topologyObjectReady(g, "permission", perm)).To(BeTrue(),
						"Permission CR %q should be accepted and reach Ready (spec.userReference + spec.permissions.{configure,write,read})", perm)
				}
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying both Exchange CRs exist and reconcile to Ready")
			Eventually(func(g Gomega) {
				for _, ex := range exchanges {
					g.Expect(topologyObjectReady(g, "exchange", ex)).To(BeTrue(),
						"Exchange CR %q should be accepted and reach Ready (spec.{name,type,durable,vhost})", ex)
				}
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying all 10 Queue CRs exist and reconcile to Ready")
			Eventually(func(g Gomega) {
				for _, q := range queues {
					g.Expect(topologyObjectReady(g, "queue", q)).To(BeTrue(),
						"Queue CR %q should be accepted and reach Ready (spec.{name,durable,vhost})", q)
				}
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying all 9 Binding CRs exist and reconcile to Ready")
			Eventually(func(g Gomega) {
				for _, b := range bindings {
					g.Expect(topologyObjectReady(g, "binding", b)).To(BeTrue(),
						"Binding CR %q should be accepted and reach Ready (spec.{source,destination,destinationType,routingKey,vhost})", b)
				}
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should produce the per-user credential Secrets the readback expects (core + provisioner)", func() {
			By("verifying the Topology Operator generated the core-user credentials Secret with username/password keys")
			// The operator's readback (messaging_connection.go) wires Core/scheduler to this
			// Topology-generated Secret BY NAME (<user>-user-credentials). Its keys are the
			// VERIFY(rabbitmq) bom.WiringProfile.MessagingCred names (username/password). We
			// assert the KEYS exist, never the values. A different generated-Secret name pattern
			// would be a VERIFY bug in messaging_connection.go / the User builder.
			Eventually(func(g Gomega) {
				userKey := getSecretDataKey(g, platformManagedMQNamespace, coreUserSecret, "username")
				g.Expect(userKey).NotTo(BeEmpty(), "core-user Secret should carry a username key")
				passKey := getSecretDataKey(g, platformManagedMQNamespace, coreUserSecret, "password")
				g.Expect(passKey).NotTo(BeEmpty(), "core-user Secret should carry a password key")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the Topology Operator generated the provisioner-user credentials Secret")
			// The provisioning flow wires to the provisioner-user Secret by name.
			Eventually(func(g Gomega) {
				userKey := getSecretDataKey(g, platformManagedMQNamespace, provisionerUserSecret, "username")
				g.Expect(userKey).NotTo(BeEmpty(), "provisioner-user Secret should carry a username key")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the broker AMQP client Service <cluster> exists (the host the operator wires dependents to)")
			// The Cluster Operator names the AMQP client Service after the cluster (suffix ""),
			// which is exactly what the readback uses for the broker Host.
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", clusterName,
					"-n", platformManagedMQNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Cluster Operator should create the <cluster> AMQP client Service")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should converge MessagingReady=True and wire Core's BROKER_* to the core-user Secret", func() {
			By("waiting for the operator's MessagingReady condition to flip to True")
			// MessagingReady=True is the operator's signal that managedBrokerReady returned true
			// AND the core-user Secret is present — the end-to-end proof that the managed broker +
			// topology converged through the real operators.
			Eventually(func(g Gomega) {
				status := getPlatformConditionStatusInNS(g, platformManagedMQNamespace, platformName, "MessagingReady")
				g.Expect(status).To(Equal(conditionStatusTrue),
					"MessagingReady should be True once the managed broker is Ready and the core-user Secret exists")
			}, 8*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying Core's Deployment wires BROKER_USERNAME/BROKER_PASSWORD from the core-user Secret")
			// The operator wires Core's broker credentials via secretKeyRef into the Topology-
			// generated core-user Secret (never inlining the value). We assert the env var
			// references that Secret by NAME — the mode-agnostic wiring fed by the readback.
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "core",
					"-n", platformManagedMQNamespace,
					"-o", "jsonpath={.spec.template.spec.containers[*].env[?(@.name==\"BROKER_PASSWORD\")].valueFrom.secretKeyRef.name}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(Equal(coreUserSecret),
					"Core's BROKER_PASSWORD should be wired from the core-user credentials Secret by reference")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should LEAVE the RabbitmqCluster + topology intact when the Platform is deleted (default Retain policy)", func() {
			By("confirming the RabbitmqCluster exists before deletion")
			cmd := exec.Command("kubectl", "get", "rabbitmqcluster.rabbitmq.com", clusterName,
				"-n", platformManagedMQNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "RabbitmqCluster should exist before the Platform is deleted")

			By("deleting the Platform CR (default deletionPolicy is Retain)")
			cmd = exec.Command("kubectl", "delete", "platform", platformName,
				"-n", platformManagedMQNamespace, "--timeout=120s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete Platform CR (finalizer should run and return)")

			By("verifying the Platform object is gone (finalizer ran and was removed)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "platform", platformName,
					"-n", platformManagedMQNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Platform object should be removed after deletion")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the RabbitmqCluster + the Vhost STILL EXIST (deletion-safety: Retain leaves managed infra intact)")
			// The managed CRs carry NO controller owner reference and are prune-excluded, so
			// owner-ref GC must not collect them and the Retain deletion path must not delete
			// them. Re-check over a window so a late GC sweep would be caught.
			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "rabbitmqcluster.rabbitmq.com", clusterName,
					"-n", platformManagedMQNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(),
					"RabbitmqCluster must remain after Platform deletion under the default Retain policy")
				cmd = exec.Command("kubectl", "get", "vhost.rabbitmq.com", clusterName+"-vhost",
					"-n", platformManagedMQNamespace)
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(),
					"the Vhost topology CR must also remain after Platform deletion under Retain")
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})
	})
}

// -------------------------------------------------------------------------
// Platform E2E Tests (managed Keycloak via the REAL Keycloak Operator, sharing a managed
// CloudNativePG database)
//
// This is the THIRD and final managed-infra e2e — the OIDC/Keycloak VERIFY-validation gate,
// mirroring the managed-database (CloudNativePG) and managed-messaging (RabbitMQ) e2es. It
// installs the REAL Keycloak Operator AND CloudNativePG (Keycloak needs a working database to
// reach Ready, so the spec provisions one and exercises the managed-DB-sharing seam), applies
// a Platform with database.mode=managed + keycloak.mode=managed, then asserts the operator's
// rendered Keycloak CR is ACCEPTED by the Keycloak Operator and RECONCILED to Ready (Keycloak
// boots against the CNPG-provided database), the create-only KeycloakRealmImport is accepted +
// reconciles Done, KeycloakReady converges True, OIDCConfigured reaches True (the operator
// fetches the Keycloak-generated "ilm" client secret from the admin API and relays it into the
// operator-owned <platform>-oidc-client Secret — Core's image is private so Core never runs,
// but the in-pod rework means the OIDC wiring no longer depends on Core being up), and Retain
// leaves Keycloak on delete.
//
// WHY THIS MATTERS: the operator renders the Keycloak CR + KeycloakRealmImport as preset-GVK
// unstructured objects from field paths inferred against the documented k8s.keycloak.org API
// and flagged with `// VERIFY(keycloak)` comments (spec.db vendor/host/database/usernameSecret/
// passwordSecret from the shared-DB readback, spec.instances, spec.http, spec.hostname, the SCC
// spec.unsupported.podTemplate; the RealmImport spec.keycloakCRName/spec.realm; and the
// Keycloak CR Ready signal the gate probes). If any field path/value is wrong, the Keycloak
// Operator rejects the apply (the CR never appears or carries no status) or never reconciles
// it to Ready. So this test turns the doc-inferred guesses into operator-confirmed truth.
//
// NAMESPACE: the Keycloak Operator is NAMESPACE-SCOPED — it watches only the namespace it runs
// in (utils.KeycloakOperatorNamespace == "keycloak"), and its install manifest's
// ClusterRoleBinding subject hard-codes that namespace. So — unlike the CNPG/RabbitMQ specs,
// which use their own isolated namespace — this spec runs its Platform IN the Keycloak Operator
// namespace, so the operator reconciles the rendered Keycloak CR. CNPG and the ILM operator
// watch cluster-wide, so they handle resources in that namespace fine.
//
// SCOPING: messaging is EXTERNAL (a dummy host) and the edge is omitted, so the upstream
// prerequisites are the Keycloak Operator + CloudNativePG (for the shared DB). As with the
// other specs we do NOT assert the ILM app pods reach Running (their images are private); we
// assert the operator's CRs + readback + status. Keycloak's operand image is PUBLIC, so the
// Keycloak pod DOES run against the CNPG database — which is what lets us validate the rendered
// Keycloak CR + realm import end-to-end.
//
// OIDC WIRING SCOPE: the managed-Keycloak OIDC wiring (reconcileOIDCProvider) now FETCHES the
// Keycloak-generated "ilm" client secret from Keycloak's admin API and RELAYS it into the
// operator-owned <platform>-oidc-client Secret — it does NOT PUT Core (Core self-registers the
// provider IN-POD via a lifecycle.postStart hook, because Core's settings API is localhost-only).
// So this spec asserts OIDCConfigured=True even though Core's image is private and Core never
// runs: the wiring no longer depends on Core being up. The in-pod PUT itself (postStart →
// localhost) is proven end-to-end by the FULL managed spec, where Core's image is public.
// -------------------------------------------------------------------------

// platformManagedKeycloakSpecs registers the managed-Keycloak Platform e2e as a Context inside
// the top-level "Manager" Ordered Describe (see e2e_test.go), exactly like
// platformManagedDatabaseSpecs / platformManagedMessagingSpecs — so it runs against the
// operator the "Manager" BeforeAll deploys. It installs CloudNativePG (for the shared DB) and
// the Keycloak Operator in its own BeforeAll (idempotent: skips whichever CRDs are already
// present) so the spec is self-contained.
//
// It runs in utils.KeycloakOperatorNamespace because the Keycloak Operator only watches that
// namespace (see the file-level comment above).
func platformManagedKeycloakSpecs() {
	Context("Platform managed-Keycloak reconcile (real Keycloak Operator + CloudNativePG shared DB)", Ordered, Label("managed", "managed-keycloak"), func() {
		// The Platform runs in the Keycloak Operator's watched namespace (the operator is
		// namespace-scoped); CNPG + the ILM operator are cluster-wide so they reconcile here too.
		keycloakNS := utils.KeycloakOperatorNamespace
		const platformName = "ilm-mkc"
		const messagingSecretName = "ilm-mkc-messaging"
		const realmConfigMapName = "ilm-mkc-realm"
		// The realm representation lives under the operator's DEFAULT realm-import ConfigMap key
		// ("realm.json"); a minimal valid RealmRepresentation is enough to validate the import
		// shape end-to-end (the Keycloak Operator imports it into the running Keycloak).
		const realmConfigMapKey = "realm.json"
		const realmName = "ilm"

		// The operator-owned names for this Platform (see managed_database.go / managed_keycloak.go):
		//   CNPG Cluster:        "<platform>-db"          (the shared database)
		//   CNPG app Secret:     "<cluster>-app"          (the DB credential Keycloak references)
		//   CNPG read-write Svc:  "<cluster>-rw"           (CNPG's primary; created regardless of pooling)
		//   PgBouncer Pooler Svc: "<cluster>-pooler"       (DEFAULT-ON for a managed DB; the host every
		//                                                   component, Keycloak included, connects through)
		//   Keycloak CR:         "<platform>-keycloak"
		//   KeycloakRealmImport: "<platform>-keycloak-realm"
		clusterName := platformName + "-db"
		appSecretName := clusterName + "-app"
		keycloakCRName := platformName + "-keycloak"
		realmImportName := keycloakCRName + "-realm"

		// Track whether each operator was pre-installed so AfterAll only uninstalls what this
		// spec added (CI-friendly).
		var cnpgWasInstalled, keycloakOpWasInstalled bool

		BeforeAll(func() {
			By("installing CloudNativePG for the shared database (skipped if already present)")
			cnpgWasInstalled = utils.IsCloudNativePGCRDsInstalled()
			if cnpgWasInstalled {
				_, _ = fmt.Fprintf(GinkgoWriter, "CloudNativePG already installed; skipping install\n")
			} else {
				Expect(utils.InstallCloudNativePG()).To(Succeed(), "Failed to install CloudNativePG")
			}

			By("installing the Keycloak Operator (skipped if already present)")
			keycloakOpWasInstalled = utils.IsKeycloakOperatorCRDsInstalled()
			if keycloakOpWasInstalled {
				_, _ = fmt.Fprintf(GinkgoWriter, "Keycloak Operator already installed; skipping install\n")
			} else {
				Expect(utils.InstallKeycloakOperator()).To(Succeed(), "Failed to install Keycloak Operator")
			}

			By("ensuring the Keycloak Operator namespace exists (the Platform runs here)")
			// InstallKeycloakOperator creates it, but when the operator was pre-installed (skip
			// path) we still need it; create idempotently.
			Expect(utils.CreateNamespaceIdempotent(keycloakNS)).
				To(Succeed(), "Failed to ensure Keycloak Operator namespace")

			By("creating the external messaging credentials Secret (basic-auth: username/password)")
			// Only the messaging credential is created by hand — the DB credential is GENERATED by
			// CloudNativePG (the <cluster>-app Secret), which Keycloak references via spec.db.
			Expect(utils.ApplyResource("secret", "generic", messagingSecretName,
				"-n", keycloakNS,
				"--type=kubernetes.io/basic-auth",
				"--from-literal=username=ilm",
				"--from-literal=password=e2e-broker-password",
			)).To(Succeed(), "Failed to create messaging credentials Secret")

			By("creating the realm-import ConfigMap (a minimal valid RealmRepresentation)")
			// {"realm":"ilm","enabled":true} is a valid (minimal) RealmRepresentation — both keys
			// are real fields on the KeycloakRealmImport spec.realm schema. The reconciler reads
			// this ConfigMap, inlines it into the create-only KeycloakRealmImport's spec.realm, and
			// stamps the realm name; the Keycloak Operator then imports it into the running Keycloak.
			Expect(utils.ApplyResource("configmap", realmConfigMapName,
				"-n", keycloakNS,
				"--from-literal="+realmConfigMapKey+`={"realm":"`+realmName+`","enabled":true}`,
			)).To(Succeed(), "Failed to create realm-import ConfigMap")
		})

		AfterAll(func() {
			By("deleting the Platform if it still exists")
			cmd := exec.Command("kubectl", "delete", "platform", platformName,
				"-n", keycloakNS, "--ignore-not-found", "--timeout=120s")
			_, _ = utils.Run(cmd)

			By("deleting the Keycloak CR + realm import left behind by the Retain deletion policy")
			// The default Retain policy deliberately leaves them intact when the Platform is deleted
			// (asserted below); clean them up here so the namespace can drain.
			cmd = exec.Command("kubectl", "delete", "keycloak.k8s.keycloak.org", keycloakCRName,
				"-n", keycloakNS, "--ignore-not-found", "--timeout=120s")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "keycloakrealmimport.k8s.keycloak.org", realmImportName,
				"-n", keycloakNS, "--ignore-not-found", "--timeout=120s")
			_, _ = utils.Run(cmd)

			By("deleting the CNPG Cluster left behind by the Retain deletion policy")
			cmd = exec.Command("kubectl", "delete", "cluster.postgresql.cnpg.io", clusterName,
				"-n", keycloakNS, "--ignore-not-found", "--timeout=120s")
			_, _ = utils.Run(cmd)

			By("waiting for the Keycloak + CNPG workload pods to fully drain (free the node for the FULL block)")
			// keycloakNS is shared with the FULL managed block (both must run where the
			// namespace-scoped Keycloak Operator watches). Deleting the CRs above only triggers
			// async pod GC; block until every pod except the operator is gone so the FULL block's
			// stack never coexists with this block's StatefulSets on a single Kind node.
			utils.WaitForWorkloadsDrained(keycloakNS, "keycloak-operator")

			By("deleting the prerequisite Secret + ConfigMap from the Keycloak namespace")
			cmd = exec.Command("kubectl", "delete", "secret", messagingSecretName,
				"-n", keycloakNS, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "configmap", realmConfigMapName,
				"-n", keycloakNS, "--ignore-not-found")
			_, _ = utils.Run(cmd)

			By("uninstalling the operators this spec installed")
			if !keycloakOpWasInstalled {
				utils.UninstallKeycloakOperator()
			}
			if !cnpgWasInstalled {
				utils.UninstallCloudNativePG()
			}
		})

		AfterEach(func() {
			specReport := CurrentSpecReport()
			if specReport.Failed() {
				By("Fetching Platform object (yaml) for debugging")
				cmd := exec.Command("kubectl", "get", "platform", platformName,
					"-n", keycloakNS, "-o", "yaml")
				if out, err := utils.Run(cmd); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Platform object:\n%s", out)
				}

				By("Fetching the Keycloak CR + KeycloakRealmImport (yaml) for debugging")
				cmd = exec.Command("kubectl", "get",
					"keycloak.k8s.keycloak.org,keycloakrealmimport.k8s.keycloak.org",
					"-n", keycloakNS, "-o", "yaml")
				if out, err := utils.Run(cmd); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Keycloak CRs:\n%s", out)
				}

				By("Fetching the CNPG Cluster (yaml) for debugging")
				cmd = exec.Command("kubectl", "get", "cluster.postgresql.cnpg.io", clusterName,
					"-n", keycloakNS, "-o", "yaml")
				if out, err := utils.Run(cmd); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "CNPG Cluster:\n%s", out)
				}

				By("Fetching events in the Keycloak namespace")
				cmd = exec.Command("kubectl", "get", "events", "-n", keycloakNS,
					"--sort-by=.lastTimestamp")
				if out, err := utils.Run(cmd); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Keycloak ns events:\n%s", out)
				}
			}
		})

		It("should apply a managed Platform and have the Keycloak Operator ACCEPT the rendered Keycloak CR", func() {
			By("creating the managed Platform CR (database managed via CNPG, keycloak managed, messaging external)")
			// database.mode=managed gives Keycloak a REAL PostgreSQL (CNPG) to share — exercising
			// the managed-DB-sharing seam (ResolveDatabaseConnection feeds spec.db). keycloak.mode=
			// managed with a realm import. Messaging external (dummy host) satisfies the CEL rule and
			// keeps this scoped to Keycloak; the edge is omitted (so Keycloak's spec.hostname is
			// omitted and the Operator's hostname handling applies). The Keycloak version selects the
			// public quay.io/keycloak/keycloak operand image, so the Keycloak pod runs against the DB.
			platformYAML := fmt.Sprintf(`
apiVersion: otilm.com/v1alpha1
kind: Platform
metadata:
  name: %s
  namespace: %s
spec:
  database:
    mode: managed
    managed:
      instances: 1
      version: "16"
      storage:
        size: 1Gi
  messaging:
    mode: external
    brokerType: rabbitmq
    host: rabbitmq.example.com
    port: 5672
    virtualHost: ilm
    credentials:
      secretRef: %s
  keycloak:
    mode: managed
    realm: %s
    managed:
      instances: 1
      version: "%s"
      realmImport:
        configMapRef: %s
        key: %s
`, platformName, keycloakNS, messagingSecretName, realmName, utils.KeycloakOperatorVersion, realmConfigMapName, realmConfigMapKey)

			tmpFile := writeTempYAML(platformYAML)
			defer func() { _ = os.Remove(tmpFile) }()

			cmd := exec.Command("kubectl", "apply", "-f", tmpFile)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create managed-Keycloak Platform CR")

			By("waiting for the CNPG Cluster (the shared DB) to come up so Keycloak has a database")
			// Keycloak cannot reach Ready without a working DB; the shared CNPG cluster must
			// provision first. This reuses the validated managed-DB path (the <cluster>-app Secret +
			// <cluster>-rw Service feed Keycloak's spec.db).
			Eventually(func(g Gomega) {
				g.Expect(keycloakCNPGClusterReady(g, clusterName, keycloakNS)).To(BeTrue(),
					"the shared CNPG Cluster should reach healthy so Keycloak can connect")
			}, 8*time.Minute, 10*time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				userKey := getSecretDataKey(g, keycloakNS, appSecretName, "username")
				g.Expect(userKey).NotTo(BeEmpty(), "<cluster>-app Secret should carry a username key for Keycloak's spec.db")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting for the operator to render a Keycloak CR the Keycloak Operator accepted (it exists and has a status)")
			// The core VERIFY assertion: if a Keycloak CR spec field path the operator renders is
			// wrong, the Keycloak Operator rejects the SSA apply (the CR never appears) OR never
			// reconciles it (no status). Observing the CR WITH status conditions proves it was
			// accepted AND is being reconciled.
			Eventually(func(g Gomega) {
				conds := getKeycloakField(g, "keycloak", keycloakCRName, keycloakNS, "{.status.conditions}")
				g.Expect(conds).NotTo(BeEmpty(),
					"Keycloak Operator should set status.conditions on the operator's Keycloak CR (accepted + reconciling)")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the rendered Keycloak CR carries the spec fields the Keycloak Operator expects (instances, db, http, image, SCC podTemplate)")
			// Read the fields back THROUGH the Keycloak Operator: a field it ignored/renamed would
			// be absent here. This confirms the VERIFY(keycloak) field paths against the real CRD.
			Eventually(func(g Gomega) {
				instances := getKeycloakField(g, "keycloak", keycloakCRName, keycloakNS, "{.spec.instances}")
				g.Expect(instances).To(Equal("1"), "spec.instances should round-trip through the Keycloak Operator")

				// spec.db: vendor + the shared-DB coordinates + the credential Secret REFERENCES
				// (never inlined). These come from ResolveDatabaseConnection (the CNPG cluster).
				vendor := getKeycloakField(g, "keycloak", keycloakCRName, keycloakNS, "{.spec.db.vendor}")
				g.Expect(vendor).To(Equal("postgres"), "spec.db.vendor should be postgres")
				dbHost := getKeycloakField(g, "keycloak", keycloakCRName, keycloakNS, "{.spec.db.host}")
				g.Expect(dbHost).To(Equal(clusterName+"-pooler"), "spec.db.host should be the PgBouncer Pooler Service (default-on for a managed DB; Keycloak connects through the pooler like every other component, not directly to <cluster>-rw)")
				dbName := getKeycloakField(g, "keycloak", keycloakCRName, keycloakNS, "{.spec.db.database}")
				g.Expect(dbName).To(Equal("ilm"), "spec.db.database should be the shared app database")
				userSecret := getKeycloakField(g, "keycloak", keycloakCRName, keycloakNS, "{.spec.db.usernameSecret.name}")
				g.Expect(userSecret).To(Equal(appSecretName), "spec.db.usernameSecret.name should reference the CNPG app Secret")
				passSecret := getKeycloakField(g, "keycloak", keycloakCRName, keycloakNS, "{.spec.db.passwordSecret.name}")
				g.Expect(passSecret).To(Equal(appSecretName), "spec.db.passwordSecret.name should reference the CNPG app Secret")

				// spec.http.httpEnabled (Keycloak serves plain HTTP behind the gateway).
				httpEnabled := getKeycloakField(g, "keycloak", keycloakCRName, keycloakNS, "{.spec.http.httpEnabled}")
				g.Expect(httpEnabled).To(Equal("true"), "spec.http.httpEnabled should round-trip")

				// spec.image selects the requested operand version.
				img := getKeycloakField(g, "keycloak", keycloakCRName, keycloakNS, "{.spec.image}")
				g.Expect(img).To(ContainSubstring(utils.KeycloakOperatorVersion),
					"spec.image should select the requested Keycloak operand version")

				// The SCC-clean pod template (spec.unsupported.podTemplate): the pod-level
				// runAsNonRoot must round-trip through the Operator's documented pod-override path.
				runAsNonRoot := getKeycloakField(g, "keycloak", keycloakCRName, keycloakNS,
					"{.spec.unsupported.podTemplate.spec.securityContext.runAsNonRoot}")
				g.Expect(runAsNonRoot).To(Equal("true"),
					"spec.unsupported.podTemplate.spec.securityContext.runAsNonRoot should round-trip (the SCC pod template)")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should reach a Ready Keycloak CR (the same signal the operator's managedKeycloakReady probes)", func() {
			By("waiting for the Keycloak CR to report Ready (status condition type 'Ready', status 'True')")
			// This is the exact Ready signal internal/controller/platform/gate_keycloak.go's
			// managedKeycloakReady checks. If the condition type/status string were wrong vs the real
			// Keycloak Operator, the operator would never see Keycloak as Ready (and the KeycloakReady
			// assertion below would hang) — that would be a VERIFY(keycloak) bug to FIX. Keycloak
			// boots a JVM + connects to PG, so the timeout is generous.
			Eventually(func(g Gomega) {
				g.Expect(keycloakCRReady(g, keycloakCRName, keycloakNS)).To(BeTrue(),
					"Keycloak CR should reach the Ready signal the operator probes (status Ready=True)")
			}, 8*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should have the Keycloak Operator ACCEPT and reconcile the KeycloakRealmImport to Done", func() {
			By("verifying the KeycloakRealmImport CR exists and carries the operator-rendered shape")
			// The reconciler creates the import (create-only) once the Keycloak CR is Ready, reading
			// the realm ConfigMap and inlining spec.realm. Confirm the VERIFY(keycloak) RealmImport
			// shape: spec.keycloakCRName points at the Keycloak CR, and spec.realm.realm is the realm.
			Eventually(func(g Gomega) {
				crName := getKeycloakField(g, "keycloakrealmimport", realmImportName, keycloakNS, "{.spec.keycloakCRName}")
				g.Expect(crName).To(Equal(keycloakCRName),
					"KeycloakRealmImport spec.keycloakCRName should reference the Keycloak CR")
				realm := getKeycloakField(g, "keycloakrealmimport", realmImportName, keycloakNS, "{.spec.realm.realm}")
				g.Expect(realm).To(Equal(realmName),
					"KeycloakRealmImport spec.realm.realm should be the platform realm name")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the stored realm carries the confidential 'ilm' OIDC client (the operator ensures it)")
			// HARDENING (regression guard): the realm-import ConfigMap supplied above is the MINIMAL
			// {"realm":"ilm","enabled":true} — it omits the OIDC client. The operator MUST ensure the
			// "ilm" client is present in the imported realm (EnsureILMClient), or reconcileOIDCProvider
			// fetches a client that does not exist and OIDCConfigured never closes (the bug the gated
			// managed e2e caught). Reading the STORED object back (not just the operator's intent)
			// catches both an operator that drops the client AND any apiserver/CRD pruning of it.
			Eventually(func(g Gomega) {
				firstClientID := getKeycloakField(g, "keycloakrealmimport", realmImportName, keycloakNS,
					`{.spec.realm.clients[0].clientId}`)
				g.Expect(firstClientID).To(Equal("ilm"),
					"the stored realm must define the 'ilm' OIDC client so OIDCConfigured can close")
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting for the KeycloakRealmImport to reconcile to Done (status condition type 'Done', status 'True')")
			// The Keycloak Operator imports the realm into the running Keycloak and sets a Done
			// condition. A rejected/unappliable import never reaches it. This validates the
			// RealmImport shape end-to-end against the real operator + a live Keycloak.
			Eventually(func(g Gomega) {
				g.Expect(keycloakRealmImportDone(g, realmImportName, keycloakNS)).To(BeTrue(),
					"KeycloakRealmImport should reconcile to Done=True (the realm was imported into Keycloak)")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should converge KeycloakReady=True on the Platform", func() {
			By("waiting for the operator's KeycloakReady condition to flip to True")
			// KeycloakReady=True is the operator's signal that managedKeycloakReady returned true
			// (the Keycloak CR reports Ready) — the end-to-end proof that the managed Keycloak
			// converged through the real Keycloak Operator against the CNPG-provided database.
			Eventually(func(g Gomega) {
				status := getPlatformConditionStatusInNS(g, keycloakNS, platformName, "KeycloakReady")
				g.Expect(status).To(Equal(conditionStatusTrue),
					"KeycloakReady should be True once the managed Keycloak CR is Ready")
			}, 8*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should reach OIDCConfigured=True by fetching+relaying the ilm client secret (no dependence on Core)", func() {
			// SCOPE: the managed-Keycloak OIDC wiring now FETCHES the Keycloak-generated "ilm" client
			// secret from Keycloak's admin API and RELAYS it into the operator-owned
			// <platform>-oidc-client Secret — it does NOT PUT Core (Core self-registers the provider
			// in-pod via a postStart hook). So even though Core's image is private and Core never
			// runs here, OIDCConfigured must reach True: the wiring no longer depends on Core. This is
			// the live counterpart to the registration httptest unit tests — the real registrar
			// against a real Keycloak admin API + the real default-realm "ilm" client. A persistent
			// False with reason OIDCConfigFailed would be a real wiring finding to FIX.
			By("verifying OIDCConfigured reaches True (the operator fetched the ilm client secret and relayed it)")
			Eventually(func(g Gomega) {
				status := getPlatformConditionStatusInNS(g, keycloakNS, platformName, "OIDCConfigured")
				g.Expect(status).To(Equal(conditionStatusTrue),
					"OIDCConfigured should reach True once the operator fetches + relays the ilm client secret")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the operator-owned OIDC client Secret was populated with a non-empty clientSecret (by REFERENCE, never logged)")
			// Prove the relay actually happened: the Secret exists and carries a non-empty
			// clientSecret. We assert presence/non-emptiness only — never print the value.
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "secret", platformName+"-oidc-client",
					"-n", keycloakNS, "-o", "jsonpath={.data.clientSecret}"))
				g.Expect(err).NotTo(HaveOccurred(), "the operator-owned OIDC client Secret should exist")
				g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty(),
					"the OIDC client Secret must carry a non-empty clientSecret (the relayed Keycloak-generated secret)")
			}, 2*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the Platform did NOT go Degraded (OIDC is an adjunct, non-fatal signal)")
			// OIDCConfigured is an adjunct condition like DatabaseReady/KeycloakReady: it must never
			// flip the platform to Degraded. The phase stays Progressing (the app pods can't pull
			// their private images here) — never Degraded from the OIDC wiring.
			Consistently(func(g Gomega) {
				phase := getPlatformPhaseInNS(keycloakNS, platformName)
				g.Expect(phase).NotTo(Equal("Degraded"),
					"the Platform must not go Degraded because of the OIDC wiring, got phase: %q", phase)
			}, 20*time.Second, 5*time.Second).Should(Succeed())
		})

		It("should LEAVE the Keycloak CR intact when the Platform is deleted (default Retain policy)", func() {
			By("confirming the Keycloak CR exists before deletion")
			cmd := exec.Command("kubectl", "get", "keycloak.k8s.keycloak.org", keycloakCRName,
				"-n", keycloakNS)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Keycloak CR should exist before the Platform is deleted")

			By("deleting the Platform CR (default deletionPolicy is Retain)")
			cmd = exec.Command("kubectl", "delete", "platform", platformName,
				"-n", keycloakNS, "--timeout=120s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete Platform CR (finalizer should run and return)")

			By("verifying the Platform object is gone (finalizer ran and was removed)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "platform", platformName,
					"-n", keycloakNS)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Platform object should be removed after deletion")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the Keycloak CR STILL EXISTS (deletion-safety: Retain leaves managed infra intact)")
			// The managed Keycloak CR carries NO controller owner reference and is prune-excluded, so
			// owner-ref GC must not collect it and the Retain deletion path must not delete it.
			// Re-check over a window so a late GC sweep would be caught.
			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "keycloak.k8s.keycloak.org", keycloakCRName,
					"-n", keycloakNS)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(),
					"Keycloak CR must remain after Platform deletion under the default Retain policy")
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})
	})
}

// -------------------------------------------------------------------------
// Managed-Keycloak e2e helpers
// -------------------------------------------------------------------------

// getKeycloakField returns a single jsonpath-extracted field from a Keycloak Operator CR
// (kind "keycloak" or "keycloakrealmimport") in the given namespace, failing the surrounding
// Gomega assertion if the CR cannot be read. It lets a spec read a rendered spec field BACK
// through the Keycloak Operator (a field it ignored or renamed would be absent), confirming the
// VERIFY(keycloak) field paths.
func getKeycloakField(g Gomega, kind, name, ns, jsonpath string) string {
	cmd := exec.Command("kubectl", "get", kind+".k8s.keycloak.org", name,
		"-n", ns,
		"-o", "jsonpath="+jsonpath,
	)
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Keycloak %s %q should be readable for field %q", kind, name, jsonpath)
	return strings.TrimSpace(output)
}

// keycloakCRReady reports whether the Keycloak CR reports the SAME Ready signal the operator's
// managedKeycloakReady probes: a status condition of type "Ready" with status "True". Keeping
// this assertion identical to the operator's probe is the point — if the real signal differed
// (e.g. lowercase "true", or a different condition type), both this spec and the operator would
// fail to see readiness, surfacing a VERIFY(keycloak) bug to fix. The condition-status jsonpath
// is empty (not an error) while the CR is not yet Ready, so the caller's Eventually keeps
// polling; getKeycloakField still asserts the CR itself is readable (it exists by this point).
func keycloakCRReady(g Gomega, name, ns string) bool {
	status := getKeycloakField(g, "keycloak", name, ns,
		`{.status.conditions[?(@.type=="Ready")].status}`)
	return status == conditionStatusTrue
}

// keycloakRealmUserLookup returns the Keycloak admin-API users-lookup response body for the
// given username in the platform realm, queried THROUGH a real Keycloak from inside the Core
// pod. It mirrors the operator's registrar flow: authenticate to the master realm with the
// Operator-generated initial-admin credentials (admin-cli password grant), then GET
// /admin/realms/<realm>/users?username=<u>&exact=true with the bearer token. The caller asserts
// the returned representation carries the username + the superadmin group attribute.
//
// SECURITY/NO-LEAK: the admin credentials are passed to the pod ONLY as their base64 forms and
// decoded INSIDE the pod (base64 -d) — the plaintext admin password and the bearer token never
// enter the test process, its argv, or the Ginkgo log. The function returns only the users
// endpoint's JSON (which carries no secret — a user representation, not a credential).
func keycloakRealmUserLookup(g Gomega, ns, platformName, realm, username string) string {
	// The operator-owned names: the in-cluster Keycloak Service (port 8080) and the Keycloak
	// Operator-generated initial-admin Secret (keys username/password) — see managed_keycloak.go.
	// The managed Keycloak serves EVERY endpoint under /kc (http-relative-path, set by the
	// operator to match the gateway's /kc route — KeycloakRelativePath in managed_keycloak.go),
	// so the base URL must carry it: a root-path request gets Keycloak's HTML 404, not the API.
	kcURL := fmt.Sprintf("http://%s-keycloak-service.%s:8080/kc", platformName, ns)
	adminSecret := platformName + "-keycloak-initial-admin"
	// Fetch the admin creds as base64 (never decoded here); getSecretDataKey asserts presence.
	userB64 := getSecretDataKey(g, ns, adminSecret, "username")
	passB64 := getSecretDataKey(g, ns, adminSecret, "password")
	g.Expect(userB64).NotTo(BeEmpty(), "the Keycloak initial-admin Secret should carry a username")
	g.Expect(passB64).NotTo(BeEmpty(), "the Keycloak initial-admin Secret should carry a password")

	// Run the whole token + lookup flow inside the Core pod (curl present; the in-cluster Keycloak
	// Service is reachable). Decode the creds in-pod, obtain a master-realm admin token via the
	// admin-cli password grant, extract access_token without jq (grep/sed), then GET the users
	// endpoint with the bearer token. briefRepresentation=false makes the search return the FULL
	// UserRepresentation — the default brief form omits `attributes`, which callers assert on
	// (the superadmin groups attribute). The credentials are referenced via shell vars set from
	// the base64 inputs and never echoed.
	script := fmt.Sprintf(`set -e
KCU=$(printf %%s '%s' | base64 -d)
KCP=$(printf %%s '%s' | base64 -d)
TOKEN=$(curl -s --data-urlencode "client_id=admin-cli" \
  --data-urlencode "grant_type=password" \
  --data-urlencode "username=$KCU" \
  --data-urlencode "password=$KCP" \
  %s/realms/master/protocol/openid-connect/token \
  | grep -o '"access_token"[^,]*' | sed 's/.*:"//;s/"$//')
curl -s -H "Authorization: Bearer $TOKEN" \
  "%s/admin/realms/%s/users?username=%s&exact=true&briefRepresentation=false"
`, userB64, passB64, kcURL, kcURL, realm, username)

	out, err := utils.Run(exec.Command("kubectl", "exec", "deploy/core", "-c", "core", "-n", ns,
		"--", "sh", "-c", script))
	g.Expect(err).NotTo(HaveOccurred(), "exec of the Keycloak admin-API user lookup from the Core pod should succeed")
	return out
}

// keycloakRealmImportDone reports whether the KeycloakRealmImport reached its "Done" condition
// (type "Done", status "True") — the Keycloak Operator's signal that it imported the realm into
// the running Keycloak. A rejected/unappliable import never reaches it. The condition-status
// jsonpath is empty (not an error) while the import is in progress, so the caller's Eventually
// keeps polling; getKeycloakField still asserts the CR itself is readable.
func keycloakRealmImportDone(g Gomega, name, ns string) bool {
	status := getKeycloakField(g, "keycloakrealmimport", name, ns,
		`{.status.conditions[?(@.type=="Done")].status}`)
	return status == conditionStatusTrue
}

// keycloakCNPGClusterReady reports whether the shared CNPG Cluster (the database Keycloak
// connects to) reached the healthy/Ready signal, parameterized by namespace so the
// managed-Keycloak spec (which runs in the Keycloak Operator namespace) can reuse the
// validated managed-DB readiness check. Mirrors cnpgClusterReady.
func keycloakCNPGClusterReady(g Gomega, name, ns string) bool {
	phase := getCNPGClusterFieldInNS(g, name, ns, "{.status.phase}")
	if phase == "Cluster in healthy state" {
		return true
	}
	readyStatus := getCNPGClusterFieldInNS(g, name, ns,
		`{.status.conditions[?(@.type=="Ready")].status}`)
	return readyStatus == conditionStatusTrue
}

// getCNPGClusterFieldInNS is getCNPGClusterField parameterized by namespace, so the
// managed-Keycloak spec can read the shared CNPG Cluster in the Keycloak Operator namespace.
func getCNPGClusterFieldInNS(g Gomega, name, ns, jsonpath string) string {
	cmd := exec.Command("kubectl", "get", "cluster.postgresql.cnpg.io", name,
		"-n", ns,
		"-o", "jsonpath="+jsonpath,
	)
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "CNPG Cluster %q should be readable for field %q", name, jsonpath)
	return strings.TrimSpace(output)
}

// getPlatformPhaseInNS returns the Platform phase in the given namespace (empty if absent), so
// the managed-Keycloak spec can assert the phase outside the default Platform test namespace.
func getPlatformPhaseInNS(ns, name string) string {
	cmd := exec.Command("kubectl", "get", "platform", name,
		"-n", ns,
		"-o", "jsonpath={.status.phase}",
	)
	output, err := utils.Run(cmd)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

// -------------------------------------------------------------------------
// Managed-messaging (RabbitMQ) e2e helpers
// -------------------------------------------------------------------------

// getRabbitMQClusterField returns a single jsonpath-extracted field from the RabbitmqCluster
// in the managed-messaging namespace, failing the surrounding Gomega assertion if the
// cluster cannot be read. It lets a spec read a rendered spec field BACK through the Cluster
// Operator (a field it ignored or renamed would be absent), confirming the VERIFY(rabbitmq)
// field paths.
func getRabbitMQClusterField(g Gomega, name, jsonpath string) string {
	cmd := exec.Command("kubectl", "get", "rabbitmqcluster.rabbitmq.com", name,
		"-n", platformManagedMQNamespace,
		"-o", "jsonpath="+jsonpath,
	)
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "RabbitmqCluster %q should be readable for field %q", name, jsonpath)
	return strings.TrimSpace(output)
}

// rabbitMQClusterReady reports whether the RabbitmqCluster reports the SAME Ready signal the
// operator's managedBrokerReady probes: a status condition of type "AllReplicasReady" OR
// "ClusterAvailable" with status "True". Keeping this assertion identical to the operator's
// probe is the point — if the real signal differed, both this spec and the operator would
// fail to see readiness, surfacing a VERIFY(rabbitmq) bug to fix.
func rabbitMQClusterReady(g Gomega, name string) bool {
	for _, condType := range []string{"AllReplicasReady", "ClusterAvailable"} {
		status := getRabbitMQClusterField(g, name,
			fmt.Sprintf(`{.status.conditions[?(@.type==%q)].status}`, condType))
		if status == conditionStatusTrue {
			return true
		}
	}
	return false
}

// topologyObjectReady reports whether a Messaging Topology Operator CR (vhost/user/
// permission/exchange/queue/binding) reached its Ready condition (type "Ready", status
// "True"). The Topology Operator sets this once it has applied the CR's desired state to the
// broker; a rejected or unappliable CR never reaches it. A read error (e.g. the CR not yet
// created) reads as not-ready — not a hard failure — so the caller's Eventually keeps polling.
func topologyObjectReady(g Gomega, kind, name string) bool {
	return topologyObjectReadyInNS(g, platformManagedMQNamespace, kind, name)
}

// topologyObjectReadyInNS is topologyObjectReady parameterized by namespace, so the
// version-matrix specs (which run in the namespace-scoped Keycloak Operator's namespace)
// can reuse the topology readback.
func topologyObjectReadyInNS(g Gomega, ns, kind, name string) bool {
	cmd := exec.Command("kubectl", "get", kind+".rabbitmq.com", name,
		"-n", ns,
		"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`,
	)
	output, err := utils.Run(cmd)
	if err != nil {
		// Surface a clear message if the CR is missing entirely (a rejected apply), so the
		// eventual failure names the object rather than an opaque false.
		g.Expect(err).NotTo(HaveOccurred(),
			"%s %q should exist (so its Ready condition can be read)", kind, name)
		return false
	}
	return strings.TrimSpace(output) == conditionStatusTrue
}

// topologyObjectSpecName returns the BROKER-facing name a rabbitmq.com topology CR declares
// (.spec.name) in the given namespace. That — not the Kubernetes metadata.name — is what the
// messaging contract is about: the builder sanitizes every character outside [a-z0-9-] out of
// metadata.name (so "provider.status-poll" and "provider_status_poll" would share ONE object
// name) while spec.name carries the app-level name verbatim. It fails the surrounding Gomega
// assertion if the CR cannot be read, so callers can use it directly inside an Eventually.
func topologyObjectSpecName(g Gomega, ns, kind, name string) string {
	output, err := utils.Run(exec.Command("kubectl", "get", kind+".rabbitmq.com", name,
		"-n", ns, "-o", "jsonpath={.spec.name}"))
	g.Expect(err).NotTo(HaveOccurred(),
		"%s %q should exist (so its broker-facing spec.name can be read)", kind, name)
	return strings.TrimSpace(output)
}

// topologyObjectAbsent reports whether the named rabbitmq.com topology CR is ABSENT from ns.
// It distinguishes a real absence from an unreadable API: `--ignore-not-found -o name` exits 0
// with EMPTY output when the object is gone, so the lookup itself is required to succeed (an
// RBAC/discovery/API failure fails the assertion instead of masquerading as "the object is
// gone") and only the empty result counts as absence.
func topologyObjectAbsent(g Gomega, ns, kind, name string) bool {
	output, err := utils.Run(exec.Command("kubectl", "get", kind+".rabbitmq.com", name,
		"-n", ns, "--ignore-not-found", "-o", "name"))
	g.Expect(err).NotTo(HaveOccurred(),
		"the %s %q lookup must succeed (an error is not proof that the object is absent)", kind, name)
	return strings.TrimSpace(output) == ""
}

// -------------------------------------------------------------------------
// Platform helper functions
// -------------------------------------------------------------------------

// getPlatformPhase returns the current phase of the Platform CR in the Platform test
// namespace (empty string if the object or field is absent).
func getPlatformPhase(name string) string {
	cmd := exec.Command("kubectl", "get", "platform", name,
		"-n", platformTestNamespace,
		"-o", "jsonpath={.status.phase}",
	)
	output, err := utils.Run(cmd)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

// getOwnerKind returns the kind of the controller owner reference on the named object
// (of the given kind) in the Platform test namespace. It fails the surrounding Gomega
// assertion if the object cannot be read, so callers can use it directly inside an
// Eventually block to wait for the object to be created AND owned.
func getOwnerKind(g Gomega, kind, name string) string {
	return getOwnerKindInNS(g, platformTestNamespace, kind, name)
}

// getOwnerKindInNS is getOwnerKind parameterized by namespace, so the managed-database
// specs (which run in their own namespace) can reuse the owner-reference readback.
func getOwnerKindInNS(g Gomega, ns, kind, name string) string {
	cmd := exec.Command("kubectl", "get", kind, name,
		"-n", ns,
		"-o", "jsonpath={.metadata.ownerReferences[0].kind}",
	)
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(),
		"%s %q should exist (so its owner reference can be read)", kind, name)
	return strings.TrimSpace(output)
}

// -------------------------------------------------------------------------
// Managed-database (CloudNativePG) e2e helpers
// -------------------------------------------------------------------------

// getCNPGClusterPhase returns the CloudNativePG Cluster's status.phase in the managed-DB
// namespace (empty string if the Cluster or the field is absent). A non-empty phase means
// CNPG accepted the operator's Cluster and is reconciling it. A read error (e.g. the
// Cluster not yet created) reads as an empty phase — not a hard failure — so the caller's
// Eventually keeps polling until CNPG sets a phase.
func getCNPGClusterPhase(name string) string {
	cmd := exec.Command("kubectl", "get", "cluster.postgresql.cnpg.io", name,
		"-n", platformManagedDBNamespace,
		"-o", "jsonpath={.status.phase}",
	)
	output, err := utils.Run(cmd)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

// getCNPGClusterField returns a single jsonpath-extracted field from the CNPG Cluster in
// the managed-DB namespace, failing the surrounding Gomega assertion if the Cluster cannot
// be read. It lets a spec read a rendered spec field BACK through CNPG (a field CNPG
// ignored or renamed would be absent), confirming the VERIFY(cnpg) field paths.
func getCNPGClusterField(g Gomega, name, jsonpath string) string {
	cmd := exec.Command("kubectl", "get", "cluster.postgresql.cnpg.io", name,
		"-n", platformManagedDBNamespace,
		"-o", "jsonpath="+jsonpath,
	)
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "CNPG Cluster %q should be readable for field %q", name, jsonpath)
	return strings.TrimSpace(output)
}

// cnpgClusterReady reports whether the CNPG Cluster reports the SAME Ready signal the
// operator's managedClusterReady probes: status.phase == "Cluster in healthy state" OR a
// status condition of type "Ready" with status "True". Keeping this assertion identical to
// the operator's probe is the point — if the real CNPG signal differed, both this spec and
// the operator would fail to see readiness, surfacing a VERIFY(cnpg) bug to fix.
func cnpgClusterReady(g Gomega, name string) bool {
	phase := getCNPGClusterField(g, name, "{.status.phase}")
	if phase == "Cluster in healthy state" {
		return true
	}
	// status.conditions[?(@.type=="Ready")].status == "True"
	readyStatus := getCNPGClusterField(g, name,
		`{.status.conditions[?(@.type=="Ready")].status}`)
	return readyStatus == conditionStatusTrue
}

// getSecretDataKey returns the base64 data under a Secret's .data.<key> in the given
// namespace, failing the surrounding Gomega assertion if the Secret cannot be read. It is
// used to assert a KEY is present and non-empty WITHOUT ever exposing the decoded value
// (the raw base64 is treated as an opacity-preserving presence check).
func getSecretDataKey(g Gomega, ns, name, key string) string {
	cmd := exec.Command("kubectl", "get", "secret", name,
		"-n", ns,
		"-o", "jsonpath={.data."+key+"}",
	)
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Secret %q should be readable for key %q", name, key)
	return strings.TrimSpace(output)
}

// getPlatformConditionStatus returns the status ("True"/"False"/"Unknown") of the named
// status condition on the Platform in the managed-DB namespace, failing the surrounding
// Gomega assertion if the Platform cannot be read.
func getPlatformConditionStatus(g Gomega, name, condType string) string {
	return getPlatformConditionStatusInNS(g, platformManagedDBNamespace, name, condType)
}

// getPlatformConditionStatusInNS is getPlatformConditionStatus parameterized by namespace,
// so the managed-messaging specs (which run in their own namespace) can reuse the condition
// readback.
func getPlatformConditionStatusInNS(g Gomega, ns, name, condType string) string {
	cmd := exec.Command("kubectl", "get", "platform", name,
		"-n", ns,
		"-o", fmt.Sprintf("jsonpath={.status.conditions[?(@.type==%q)].status}", condType),
	)
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Platform %q should be readable for condition %q", name, condType)
	return strings.TrimSpace(output)
}

// -------------------------------------------------------------------------
// Platform E2E Tests (FULL managed platform — CNPG + RabbitMQ + Keycloak + the
// real ILM application images, all on Kind)
//
// This is the capstone managed-infra e2e: a single Platform with database, messaging,
// AND keycloak ALL mode=managed, the edge enabled with cert-manager (tls.source=internal),
// and NO user realm import — the operator imports its OWN version-bundled realm, which
// defines the confidential "ilm" OIDC client (Keycloak generates the secret). It brings up the
// real CloudNativePG + RabbitMQ (Cluster + Topology) + Keycloak operators (reusing the
// validated managed paths) AND — the new part — the real, PUBLIC ILM application images
// (hub.omnitrustregistry.com/ilm/{core,scheduler,auth,fe-administrator,
// utils,opa,kong,curl}). Because those images are public, the app pods actually
// PULL and RUN, which lets this spec close the two image-gated residuals end-to-end:
//
//   (a) Core <- Keycloak OIDC wiring: once Core + Keycloak are Ready, the operator fetches
//       the "ilm" client secret (Keycloak-generated, from the operator's OWN bundled realm)
//       from Keycloak's admin API and relays it into an operator-owned Secret Core reads
//       in-pod to self-register its internal OIDC provider, flipping OIDCConfigured to True
//       (the OIDC-wiring residual, closed live at the operator layer).
//   (b) read-only-root for the JVM/.NET app containers: the app pods run with a read-only
//       root filesystem (core/scheduler/utils/auth) and must not crash-loop (residual b).
//
// It ALSO validates the runtime DB/messaging wiring the unit/builder tests could not: Core
// connecting to the managed PostgreSQL + RabbitMQ with the readback-wired credentials,
// proving the image-default registry fix (the app images pull out of the box) at runtime.
//
// NAMESPACE: the Keycloak Operator is namespace-scoped to "keycloak" (its ClusterRoleBinding
// subject is hard-coded there), so this Platform — like platformManagedKeycloakSpecs — runs
// in utils.KeycloakOperatorNamespace. Only one Platform per namespace is active (the operator's
// singleton guard), so the BeforeAll first drains any leftover Platform from that namespace.
// -------------------------------------------------------------------------

// platformFullManagedSpecs registers the full managed-platform e2e as a Context inside the
// top-level "Manager" Ordered Describe (see e2e_test.go), exactly like the other managed
// specs — so it runs against the operator the "Manager" BeforeAll deploys. It installs CNPG +
// both RabbitMQ operators + the Keycloak Operator in its own BeforeAll (each idempotent: skips
// whichever CRDs are already present), then applies one all-managed Platform and asserts the
// whole system converges, including the OIDC PUT and read-only-root.
func platformFullManagedSpecs() {
	Context("Platform FULL managed reconcile (real CNPG + RabbitMQ + Keycloak + the real ILM app images)", Ordered, Label("managed", "full"), func() {
		// The Keycloak Operator is namespace-scoped to "keycloak"; CNPG + the RabbitMQ operators +
		// the ILM operator are cluster-wide and reconcile here too.
		ns := utils.KeycloakOperatorNamespace
		const platformName = "ilm-full"
		const realmName = "ilm"
		// provBootstrapSecret holds the provisioning service's API key + JWT signing key
		// (provisioning.mode=deploy), referenced by the Platform — never inlined.
		const provBootstrapSecret = "ilm-full-provisioning-bootstrap"
		// adminPasswordSecret holds the first-admin PASSWORD (key "password") for the
		// registerAdmin.password method — referenced by the Platform, never inlined, never
		// minted/logged by the operator. The operator hands it to Keycloak once to create the
		// realm user; this spec only asserts the user EXISTS (with the superadmin attribute) and
		// never reads the value back.
		const adminPasswordSecret = "ilm-full-admin-password"
		// adminUsername is the realm-user login name registerAdmin creates (also the Subject CN
		// for the cert method, which is disabled here). The spec looks the user up by it.
		const adminUsername = "admin"
		// The full platform's edge host. With the edge enabled the operator derives EdgeHost from
		// it, which (a) drives cert-manager issuance for the internal TLS and (b) populates Core's
		// browser-facing OIDC URLs. It need not resolve — Core reaches Keycloak back-channel via
		// the in-cluster Service; this host only shapes the browser URLs + the cert SAN.
		const edgeHost = "ilm-full.e2e.local"

		// Operator-owned names for this Platform (see managed_database.go / managed_messaging.go /
		// managed_keycloak.go):
		clusterName := platformName + "-db"                        // CNPG Cluster (shared DB)
		appSecretName := clusterName + "-app"                      // CNPG app Secret (DB creds)
		mqClusterName := platformName + "-messaging"               // RabbitmqCluster
		coreUserSecret := mqClusterName + "-core-user-credentials" // Topology core-user creds
		keycloakCRName := platformName + "-keycloak"               // Keycloak CR
		realmImportName := keycloakCRName + "-realm"               // KeycloakRealmImport

		// The seven ILM application component Deployments the operator renders (utils enabled).
		// These are the workloads whose pods must PULL their public images and (for the JVM/.NET
		// ones) run with a read-only root without crash-looping.
		appDeployments := []string{
			"core", "scheduler", "auth", "auth-opa-policies",
			"fe-administrator", "utils", "api-gateway",
		}

		// Track whether each operator was pre-installed so AfterAll only uninstalls what this
		// spec added (CI-friendly).
		var cnpgWasInstalled, clusterOpWasInstalled, topologyOpWasInstalled, keycloakOpWasInstalled bool

		BeforeAll(func() {
			By("installing CloudNativePG (skipped if already present)")
			cnpgWasInstalled = utils.IsCloudNativePGCRDsInstalled()
			if !cnpgWasInstalled {
				Expect(utils.InstallCloudNativePG()).To(Succeed(), "Failed to install CloudNativePG")
			}

			By("installing the RabbitMQ Cluster Operator (skipped if already present)")
			clusterOpWasInstalled = utils.IsRabbitMQClusterOperatorCRDsInstalled()
			if !clusterOpWasInstalled {
				Expect(utils.InstallRabbitMQClusterOperator()).To(Succeed(), "Failed to install RabbitMQ Cluster Operator")
			}

			By("installing the Messaging Topology Operator (skipped if already present)")
			topologyOpWasInstalled = utils.IsRabbitMQTopologyOperatorCRDsInstalled()
			if !topologyOpWasInstalled {
				Expect(utils.InstallRabbitMQTopologyOperator()).To(Succeed(), "Failed to install Messaging Topology Operator")
			}

			By("installing the Keycloak Operator (skipped if already present)")
			keycloakOpWasInstalled = utils.IsKeycloakOperatorCRDsInstalled()
			if !keycloakOpWasInstalled {
				Expect(utils.InstallKeycloakOperator()).To(Succeed(), "Failed to install Keycloak Operator")
			}

			By("ensuring the Keycloak Operator namespace exists (the Platform runs here)")
			Expect(utils.CreateNamespaceIdempotent(ns)).
				To(Succeed(), "Failed to ensure Keycloak Operator namespace")

			By("draining any leftover Platform from the namespace (one active Platform per namespace)")
			// platformManagedKeycloakSpecs runs earlier in the same namespace and deletes its
			// Platform in its final It; delete defensively + wait so the singleton guard never
			// parks ilm-full as AnotherPlatformExists behind a straggler.
			cmd := exec.Command("kubectl", "delete", "platform", "--all",
				"-n", ns, "--ignore-not-found", "--timeout=120s")
			_, _ = utils.Run(cmd)
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "platform",
					"-n", ns, "-o", "jsonpath={.items[*].metadata.name}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "namespace must have no Platform before applying ilm-full")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting for any leftover managed-Keycloak workload pods to drain before bringing up ilm-full")
			// The managed-Keycloak block ran its full Keycloak + CNPG stack in THIS namespace
			// (the only one its namespace-scoped operator watches) and its teardown deletes the
			// CRs but pod GC is async. Block until every pod except the operator is gone so the
			// ilm-full stack is not brought up alongside the previous block's StatefulSets on a
			// single Kind node — the node-exhaustion / containerd-OOM that fails this block.
			utils.WaitForWorkloadsDrained(ns, "keycloak-operator")

			// NO realm-import ConfigMap is created: this spec validates that the operator imports
			// its OWN version-bundled realm (DefaultKeycloakRealm) which DEFINES the confidential
			// "ilm" OIDC client out of the box. reconcileOIDCProvider then does GET /admin/realms/
			// ilm/clients?clientId=ilm + GET .../clients/<uuid>/client-secret against the realm the
			// operator imported — Keycloak GENERATES the secret (the operator never mints it), and
			// the operator reads it back and PUTs it to Core. This is the OIDC residual closed at
			// the operator layer.

			By("creating the provisioning bootstrap Secret (JWT signing key + API key, by reference)")
			// provisioning.mode=deploy REQUIRES a bootstrapSecretRef holding the service's API key
			// and JWT signing key (the operator wires both by secretKeyRef, never inlining them).
			// The signing key must be >= 32 chars. The broker provisioner/proxy credentials are
			// defaulted from the managed-messaging Topology Secrets, so only this bootstrap Secret
			// is user-supplied.
			Expect(utils.ApplyResource("secret", "generic", provBootstrapSecret,
				"-n", ns,
				"--from-literal=securityApiKey=e2e-provisioning-api-key",
				"--from-literal=tokenSigningKey=e2e-provisioning-token-signing-key-0123456789",
			)).To(Succeed(), "Failed to create provisioning bootstrap Secret")

			By("creating the first-admin password Secret (registerAdmin.password.secretRef, by reference)")
			// registerAdmin.password references this generic Secret (key "password"); the operator
			// reads it read-only and hands it to Keycloak once to create the realm user. The value
			// is never minted or logged by the operator; this spec never reads it back. Created
			// idempotently (ApplyResource) like every other prerequisite — never raw `kubectl create`.
			Expect(utils.ApplyResource("secret", "generic", adminPasswordSecret,
				"-n", ns,
				"--from-literal=password=e2e-admin-password",
			)).To(Succeed(), "Failed to create the first-admin password Secret")
		})

		AfterAll(func() {
			By("deleting the Platform if it still exists")
			_, _ = utils.Run(exec.Command("kubectl", "delete", "platform", platformName,
				"-n", ns, "--ignore-not-found", "--timeout=180s"))

			By("deleting the managed infra left behind by the Retain deletion policy")
			for _, kindName := range []string{
				"keycloak.k8s.keycloak.org/" + keycloakCRName,
				"keycloakrealmimport.k8s.keycloak.org/" + realmImportName,
				"rabbitmqcluster.rabbitmq.com/" + mqClusterName,
				"cluster.postgresql.cnpg.io/" + clusterName,
			} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", kindName,
					"-n", ns, "--ignore-not-found", "--timeout=120s"))
			}

			By("deleting the provisioning bootstrap + first-admin password Secrets")
			_, _ = utils.Run(exec.Command("kubectl", "delete", "secret", provBootstrapSecret,
				"-n", ns, "--ignore-not-found"))
			_, _ = utils.Run(exec.Command("kubectl", "delete", "secret", adminPasswordSecret,
				"-n", ns, "--ignore-not-found"))

			By("uninstalling the operators this spec installed")
			if !keycloakOpWasInstalled {
				utils.UninstallKeycloakOperator()
			}
			if !topologyOpWasInstalled {
				utils.UninstallRabbitMQTopologyOperator()
			}
			if !clusterOpWasInstalled {
				utils.UninstallRabbitMQClusterOperator()
			}
			if !cnpgWasInstalled {
				utils.UninstallCloudNativePG()
			}
		})

		AfterEach(func() {
			if !CurrentSpecReport().Failed() {
				return
			}
			By("dumping the Platform, managed CRs, app pods, and events for debugging")
			dump := func(args ...string) {
				if out, err := utils.Run(exec.Command("kubectl", args...)); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "$ kubectl %s\n%s\n", strings.Join(args, " "), out)
				}
			}
			dump("get", "platform", platformName, "-n", ns, "-o", "yaml")
			dump("get", "keycloak.k8s.keycloak.org,keycloakrealmimport.k8s.keycloak.org",
				"-n", ns, "-o", "wide")
			dump("get", "cluster.postgresql.cnpg.io,rabbitmqcluster.rabbitmq.com", "-n", ns, "-o", "wide")
			dump("get", "pods", "-n", ns, "-o", "wide")
			dump("describe", "pods", "-n", ns, "-l", "app.kubernetes.io/part-of=ilm")
			dump("get", "events", "-n", ns, "--sort-by=.lastTimestamp")
		})

		It("should apply an all-managed Platform and bring up the managed infra (CNPG + RabbitMQ + Keycloak)", func() {
			By("creating the full managed Platform CR (database + messaging + keycloak all managed, edge internal)")
			// All three stateful dependencies are managed; the edge is enabled with an internal
			// (cert-manager self-signed) issuer, which validates cert-manager issuance AND gives
			// the OIDC wiring a browser-facing host. NOTE: there is deliberately NO realmImport —
			// the operator imports its OWN version-bundled realm (with the confidential ilm client)
			// out of the box, which is what this spec validates (the OIDC residual closed at the
			// operator layer, not via a user-authored realm fixture).
			// Small footprint (1 instance/replica, 1Gi) so it provisions on a Kind node.
			platformYAML := fmt.Sprintf(`
apiVersion: otilm.com/v1alpha1
kind: Platform
metadata:
  name: %s
  namespace: %s
spec:
  utils:
    enabled: true
  core:
    # Per-component HPA + PDB validate the Phase-2 availability surface on a real cluster
    # WITHOUT the cluster-wide HA profile (which would double every stateless workload's
    # replicas and overload a single Kind node). With autoscaling set the operator OMITS
    # .spec.replicas so the HPA owns scaling (SSA co-ownership); min=1 means an idle Core
    # never scales up, so no extra pod pressure. The PDB (minAvailable=1) renders standalone.
    autoscaling:
      minReplicas: 1
      maxReplicas: 2
      targetCPUUtilization: 80
    podDisruptionBudget:
      enabled: true
      minAvailable: 1
  # provisioning.mode=deploy renders the bundled provisioning-rabbitmq service (a real,
  # public image) wired to the managed RabbitMQ provisioner/proxy users + Core's
  # PROVISIONING_API_URL. The JWT signing key + API key come from the bootstrap Secret.
  # Top-level spec.provisioning — a platform concern, not Core config.
  provisioning:
    mode: deploy
    deploy:
      bootstrapSecretRef: %s
  database:
    mode: managed
    managed:
      instances: 1
      version: "16"
      storage:
        size: 1Gi
  messaging:
    mode: managed
    brokerType: rabbitmq
    virtualHost: czertainly
    managed:
      replicas: 1
      version: "4.0"
      storage:
        size: 1Gi
  keycloak:
    mode: managed
    realm: %s
    managed:
      instances: 1
      version: "%s"
  # First-admin bootstrap by the PASSWORD method: the operator creates an idempotent Keycloak
  # realm user (superadmin attribute) with the password from adminPasswordSecret. The cert
  # method is disabled (password-only). Requires keycloak.mode=managed (satisfied above). The
  # password is referenced by name, never inlined; the operator never mints or logs it.
  registerAdmin:
    enabled: true
    username: %s
    name: Platform Administrator
    email: admin@example.com
    certificate:
      enabled: false
    password:
      enabled: true
      secretRef: %s
  edge:
    enabled: true
    host: %s
    tls:
      source: internal
`, platformName, ns, provBootstrapSecret, realmName, utils.KeycloakOperatorVersion, adminUsername, adminPasswordSecret, edgeHost)

			tmpFile := writeTempYAML(platformYAML)
			defer func() { _ = os.Remove(tmpFile) }()
			_, err := utils.Run(exec.Command("kubectl", "apply", "-f", tmpFile))
			Expect(err).NotTo(HaveOccurred(), "Failed to create the full managed Platform CR")

			By("waiting for the managed CNPG Cluster (the shared DB) to reach healthy")
			Eventually(func(g Gomega) {
				g.Expect(keycloakCNPGClusterReady(g, clusterName, ns)).To(BeTrue(),
					"the shared CNPG Cluster should reach healthy (Core + Keycloak both connect to it)")
			}, 10*time.Minute, 10*time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(getSecretDataKey(g, ns, appSecretName, "username")).NotTo(BeEmpty(),
					"the CNPG <cluster>-app Secret should carry credentials Core + Keycloak reference")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting for the managed RabbitmqCluster to reach Ready and the core-user Secret to appear")
			Eventually(func(g Gomega) {
				g.Expect(rabbitMQClusterReadyInNS(g, mqClusterName, ns)).To(BeTrue(),
					"the RabbitmqCluster should reach Ready (Core authenticates against it)")
			}, 10*time.Minute, 10*time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(getSecretDataKey(g, ns, coreUserSecret, "username")).NotTo(BeEmpty(),
					"the Topology core-user Secret should carry the credentials Core's BROKER_* wire to")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("waiting for MessagingReady=True and DatabaseReady=True on the Platform")
			Eventually(func(g Gomega) {
				g.Expect(getPlatformConditionStatusInNS(g, ns, platformName, "MessagingReady")).To(Equal(conditionStatusTrue))
				g.Expect(getPlatformConditionStatusInNS(g, ns, platformName, "DatabaseReady")).To(Equal(conditionStatusTrue))
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("waiting for the managed Keycloak CR to reach Ready and KeycloakReady=True")
			Eventually(func(g Gomega) {
				g.Expect(keycloakCRReady(g, keycloakCRName, ns)).To(BeTrue(),
					"the managed Keycloak CR should reach Ready (it shares the CNPG database)")
			}, 10*time.Minute, 10*time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(getPlatformConditionStatusInNS(g, ns, platformName, "KeycloakReady")).To(Equal(conditionStatusTrue))
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("waiting for the operator's default KeycloakRealmImport (with the ilm client) to reconcile to Done")
			// The operator renders this import from its OWN bundled realm (DefaultKeycloakRealm) —
			// no user ConfigMap. Its spec.realm.clients carries the confidential ilm client; verify
			// that before waiting for the import to land, so a regression in the default realm is
			// caught here (not just as a downstream OIDC timeout).
			Eventually(func(g Gomega) {
				clientID := getKeycloakField(g, "keycloakrealmimport", realmImportName, ns,
					`{.spec.realm.clients[0].clientId}`)
				g.Expect(clientID).To(Equal("ilm"),
					"the operator's default realm import must define the confidential ilm client")
			}, 2*time.Minute, 10*time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(keycloakRealmImportDone(g, realmImportName, ns)).To(BeTrue(),
					"the operator's bundled realm (with the confidential ilm client) should import so the OIDC secret-fetch can find it")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should PULL and START the ILM application pods (proving the image-default registry fix)", func() {
			By("verifying every ILM application Deployment has at least one pod that is NOT image-pull-blocked")
			// The image-default fix means an unset spec.image resolves to hub.omnitrustregistry.com
			// /ilm/<component>:<tag> (public). If the default were wrong the pods would sit in
			// ImagePullBackOff/ErrImagePull forever. We assert no app pod is stuck pulling AND each
			// Deployment observes its pods (containers created), proving the images pulled.
			Eventually(func(g Gomega) {
				for _, dep := range appDeployments {
					g.Expect(noImagePullErrorForApp(g, ns, dep)).To(BeTrue(),
						"Deployment %q pods must not be in ImagePullBackOff/ErrImagePull (image-default registry fix)", dep)
				}
			}, 8*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should reach a Ready Core connected to the managed PostgreSQL + RabbitMQ (runtime wiring)", func() {
			By("waiting for Core's Deployment to become Available")
			// Core Ready is the runtime proof of the readback wiring: Core connects to the managed
			// PG (JDBC_URL -> <cluster>-rw, creds from <cluster>-app) AND the managed RabbitMQ
			// (BROKER_* -> <mqcluster>, creds from the core-user Secret). If any wiring is wrong at
			// RUNTIME (a missing/incorrect env/config the operator should provide) Core never goes
			// Available — a real finding to FIX in the operator (a runtime check the unit/builder tests
			// cannot give). Core also boots its full init-container chain (wait-for-auth) first, so
			// the whole stateless set must be progressing for Core to start; hence a generous wait.
			Eventually(func(g Gomega) {
				g.Expect(deploymentAvailable(g, ns, "core")).To(BeTrue(),
					"Core should become Available connected to the managed PG + RabbitMQ")
			}, 15*time.Minute, 15*time.Second).Should(Succeed())
		})

		It("should hold read-only-root on the JVM/.NET app pods without crash-looping (residual b)", func() {
			By("verifying the JVM/.NET app containers run read-only-root and are not crash-looping")
			// The render sets readOnlyRootFilesystem:true on core/scheduler/utils/auth (+ a writable
			// /tmp, and TMPDIR=/tmp for the .NET auth container). This asserts the LIVE pods carry
			// that securityContext AND are not crash-looping on it (residual b). A crash-loop here
			// would mean a container needs more writable carve-out than /tmp — back THAT one off in
			// the builder with a documented reason rather than ship a crash.
			roChecks := []struct{ dep, container string }{
				{"core", "core"},
				{"scheduler", "scheduler"},
				{"utils", "utils"},
				{"auth", "auth"},
			}
			Eventually(func(g Gomega) {
				for _, c := range roChecks {
					g.Expect(mainContainerReadOnlyRoot(g, ns, c.dep, c.container)).To(BeTrue(),
						"%s/%s should run with readOnlyRootFilesystem=true (residual b)", c.dep, c.container)
					g.Expect(appPodCrashLooping(g, ns, c.dep)).To(BeFalse(),
						"%s must not be crash-looping under a read-only root", c.dep)
				}
			}, 10*time.Minute, 15*time.Second).Should(Succeed())
		})

		It("should complete the Core<-Keycloak OIDC wiring (OIDCConfigured=True, closing the Core-PUT residual)", func() {
			By("waiting for OIDCConfigured to flip to True")
			// THE OIDC RESIDUAL, CLOSED END-TO-END VIA THE IN-POD MECHANISM: with Keycloak Ready +
			// the operator's OWN bundled realm imported (defining the confidential ilm client, secret
			// GENERATED by Keycloak — not minted by the operator), reconcileOIDCProvider fetches that
			// client secret from Keycloak's admin API and RELAYS it into the operator-owned
			// <platform>-oidc-client Secret. Core reads it via $INTERNAL_OAUTH_SECRET and self-registers
			// its internal OIDC provider IN-POD through the lifecycle.postStart hook
			// (register-internal-keycloak.sh -> PUT http://localhost:<coreport>/...). The operator sets
			// OIDCConfigured=True once the Secret is relayed. A persistent False with reason
			// OIDCConfigFailed would be a real wiring finding to FIX in the operator.
			Eventually(func(g Gomega) {
				g.Expect(getPlatformConditionStatusInNS(g, ns, platformName, "OIDCConfigured")).To(Equal(conditionStatusTrue),
					"OIDCConfigured should reach True (the operator fetched the ilm client secret and relayed it for Core's in-pod registration)")
			}, 10*time.Minute, 15*time.Second).Should(Succeed())

			By("verifying the operator-owned OIDC client Secret was populated (relayed, by REFERENCE — never logged)")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "secret", platformName+"-oidc-client",
					"-n", ns, "-o", "jsonpath={.data.clientSecret}"))
				g.Expect(err).NotTo(HaveOccurred(), "the operator-owned OIDC client Secret should exist")
				g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty(),
					"the OIDC client Secret must carry a non-empty clientSecret (the relayed Keycloak-generated secret)")
			}, 2*time.Minute, 10*time.Second).Should(Succeed())

			By("PROVING the in-pod postStart actually registered the provider (GET Core's settings API from INSIDE the Core pod)")
			// The whole point of the rework: the postStart curls Core's LOCALHOST settings API. Prove
			// it landed by exec-ing into the Core pod and GETting the same endpoint from localhost — the
			// registered provider must report the ilm clientId. This goes beyond asserting the wiring
			// RENDERED (the prior specs) to asserting the in-pod PUT SUCCEEDED. A postStart hook is
			// fire-and-forget + the script waits for localhost, so allow generous time for the first
			// successful registration after Core finishes booting.
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "exec", "deploy/core", "-c", "core", "-n", ns,
					"--", "sh", "-c",
					"curl -s http://localhost:8080/api/v1/settings/authentication/oauth2Providers/internal"))
				g.Expect(err).NotTo(HaveOccurred(), "exec-curl of Core's localhost settings API should succeed")
				// The registered internal provider must report the ilm clientId — proof the postStart PUT
				// (with the relayed Keycloak-generated secret) actually registered the provider in Core.
				// Core re-serializes its own response, so match the field + value tolerantly of whitespace
				// rather than a fixed byte sequence.
				g.Expect(out).To(ContainSubstring("clientId"),
					"Core's internal OIDC provider response must include a clientId; got: %s", out)
				g.Expect(out).To(MatchRegexp(`"clientId"\s*:\s*"ilm"`),
					"Core's internal OIDC provider must report clientId=ilm (the in-pod postStart registered it); got: %s", out)
			}, 5*time.Minute, 15*time.Second).Should(Succeed())
		})

		It("should create the first-admin Keycloak realm user with the superadmin attribute (registerAdmin.password)", func() {
			// THE PASSWORD-ADMIN BOOTSTRAP, CLOSED END-TO-END: registerAdmin.password is enabled on
			// the Platform (cert method disabled). Once Keycloak is Ready the operator reads the admin
			// password Secret read-only and calls the Keycloak admin API to ensure an idempotent realm
			// user (username=admin) carrying the superadmin attribute (groups: ["superadmin"]) — the
			// same flow the registration httptest unit tests exercise, here against a REAL Keycloak.
			// It sets AdminUserReady=True. The operator NEVER mints or logs the password; this spec
			// asserts only that the USER exists with the superadmin attribute (never reading the value).
			By("waiting for AdminUserReady=True (the operator ensured the realm user via the Keycloak admin API)")
			Eventually(func(g Gomega) {
				g.Expect(getPlatformConditionStatusInNS(g, ns, platformName, "AdminUserReady")).To(Equal(conditionStatusTrue),
					"AdminUserReady should reach True once the operator ensures the password realm user")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the Platform did NOT go Degraded (the password admin is an adjunct, non-fatal signal)")
			// AdminUserReady is an adjunct condition like OIDCConfigured: a transient failure must never
			// flip the platform Degraded. Assert the phase is not Degraded over a short window.
			Consistently(func(g Gomega) {
				phase := getPlatformPhaseInNS(ns, platformName)
				g.Expect(phase).NotTo(Equal("Degraded"),
					"the Platform must not go Degraded because of the password-admin reconcile, got phase: %q", phase)
			}, 20*time.Second, 5*time.Second).Should(Succeed())

			By("PROVING the realm user EXISTS in Keycloak with the superadmin attribute (query the admin API)")
			// Beyond the condition: read the user back THROUGH the real Keycloak admin API to prove the
			// operator actually created it (not merely that it reported success). We authenticate with
			// the Keycloak Operator-generated initial-admin creds (master realm, admin-cli password
			// grant) and GET /admin/realms/ilm/users?username=admin&exact=true — the same path the
			// operator's registrar uses. The whole flow runs INSIDE the Core pod (curl present, the
			// in-cluster Keycloak Service reachable), mirroring the OIDC spec's exec-curl. The returned
			// user representation must carry the exact username AND the superadmin group attribute.
			Eventually(func(g Gomega) {
				body := keycloakRealmUserLookup(g, ns, platformName, realmName, adminUsername)
				g.Expect(body).To(MatchRegexp(`"username"\s*:\s*"`+adminUsername+`"`),
					"the realm user %q must exist in Keycloak; admin-API users response: %s", adminUsername, body)
				g.Expect(body).To(ContainSubstring("superadmin"),
					"the realm user must carry the superadmin attribute (groups: [\"superadmin\"]); admin-API users response: %s", body)
			}, 3*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should reach Platform Available=True (or surface precisely what blocks it)", func() {
			By("waiting for the Platform's Available condition to become True")
			// Available=True is the whole-system green light: every required component Deployment is
			// available. With all images public and the runtime wiring correct, the full platform
			// should converge. If a specific component blocks it, the AfterEach dump + this assertion's
			// failure name which one — documented in the report rather than hidden.
			Eventually(func(g Gomega) {
				g.Expect(getPlatformConditionStatusInNS(g, ns, platformName, "Available")).To(Equal(conditionStatusTrue),
					"the full managed Platform should reach Available=True")
			}, 10*time.Minute, 15*time.Second).Should(Succeed())
		})

		It("should converge to Available WITH the default-on NetworkPolicies applied (enforcement-safe)", func() {
			// HIGHEST-RISK PHASE-1 SURFACE: the platform's default-deny NetworkPolicies are ON by
			// default (opt-out). This spec proves they (a) exist and (b) do NOT break the platform —
			// the previous spec already reached Available=True WITH these policies in force, so on a
			// CNI that ENFORCES NetworkPolicy the intra-platform + edge allow-rules are demonstrably
			// sufficient (Core reached the managed DB/RabbitMQ/Keycloak AND the OIDC PUT succeeded
			// through them). NOTE on enforcement: Kind's default CNI is kindnet; recent kindnetd
			// (>= ~2024/2025 images, as on this cluster) ENFORCES NetworkPolicy, so convergence here
			// is a real enforcement result. On an OLDER kindnet (no NP controller) this asserts only
			// that the policies APPLY and the platform still converges — never that enforcement holds;
			// treat enforcement coverage as CNI-dependent.
			By("verifying all three default NetworkPolicies exist on the platform namespace")
			for _, np := range []string{"ilm-default-deny-ingress", "ilm-allow-edge-to-gateway", "ilm-allow-egress"} {
				Eventually(func(g Gomega) {
					out, err := utils.Run(exec.Command("kubectl", "get", "networkpolicy", np, "-n", ns,
						"-o", "jsonpath={.metadata.name}"))
					g.Expect(err).NotTo(HaveOccurred(), "NetworkPolicy %q should exist (default-on)", np)
					g.Expect(strings.TrimSpace(out)).To(Equal(np))
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
			}

			By("re-confirming Available=True is STABLE with the policies in force (no enforcement regression)")
			// Available was reached in the prior spec; assert it HOLDS over a window so a NetworkPolicy
			// enforcement regression (an allow-rule too tight) that intermittently severs intra-platform
			// traffic would be caught here rather than passing by luck.
			Consistently(func(g Gomega) {
				g.Expect(getPlatformConditionStatusInNS(g, ns, platformName, "Available")).To(Equal(conditionStatusTrue),
					"Available must stay True with the default-deny NetworkPolicies enforced")
			}, 30*time.Second, 10*time.Second).Should(Succeed())
		})

		It("should render the per-component PDB + HPA and OMIT the Deployment replicas (HPA-owned)", func() {
			// PHASE-2 AVAILABILITY SURFACE on a real cluster: Core carries autoscaling + a PDB. The
			// operator must (a) create the HPA targeting Core's Deployment, (b) create the PDB, and
			// (c) NOT send .spec.replicas on Core's Deployment (the HPA owns scaling under SSA
			// co-ownership). We assert the operator did not stamp itself as the replicas field manager
			// — the load-bearing SSA invariant — rather than a literal replica count (the apiserver/HPA
			// may write one).
			By("verifying the Core HorizontalPodAutoscaler targets the Core Deployment")
			Eventually(func(g Gomega) {
				kind := getCoreChildField(g, "hpa", ns, "{.spec.scaleTargetRef.kind}")
				g.Expect(kind).To(Equal("Deployment"))
				name := getCoreChildField(g, "hpa", ns, "{.spec.scaleTargetRef.name}")
				g.Expect(name).To(Equal("core"))
				maxR := getCoreChildField(g, "hpa", ns, "{.spec.maxReplicas}")
				g.Expect(maxR).To(Equal("2"), "the HPA carries the configured maxReplicas")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the Core PodDisruptionBudget exists with minAvailable=1")
			Eventually(func(g Gomega) {
				minAvail := getCoreChildField(g, "pdb", ns, "{.spec.minAvailable}")
				g.Expect(minAvail).To(Equal("1"), "the per-component PDB carries minAvailable=1")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the operator did NOT manage .spec.replicas on the Core Deployment (HPA owns it under SSA)")
			// With autoscaling set the operator omits .spec.replicas, so under SSA the operator's
			// field manager must NOT own f:spec.f:replicas. The HPA/apiserver may set the value, but
			// the OPERATOR not owning it is the invariant proving it ceded scaling to the HPA.
			Eventually(func(g Gomega) {
				g.Expect(deploymentReplicasUnmanagedByOperator(g, ns, "core")).To(BeTrue(),
					"the operator must not own .spec.replicas on an autoscaled Deployment (the HPA owns scaling)")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should render, APPLY, and run the provisioning-rabbitmq workload to Available (provisioning.mode=deploy)", func() {
			// PHASE-2 PROVISIONING SURFACE on a real cluster: provisioning.mode=deploy renders the
			// bundled provisioning-rabbitmq Deployment, wired to the managed RabbitMQ provisioner/proxy
			// users (defaulted from the Topology Secrets) + its bootstrap Secret, with Core's
			// PROVISIONING_API_URL pointing at its in-cluster Service. The 2.18.0 bundle pins the
			// provisioning image to the released 1.0.0 tag, so the pod actually PULLS and RUNS —
			// we therefore assert the workload reaches Available, not merely that it schedules. We
			// also keep the operator-side contract checks: the Deployment is rendered and the
			// bootstrap Secret is consumed by reference (never inlined).
			By("verifying the provisioning-rabbitmq Deployment exists")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "deployment", "provisioning-rabbitmq",
					"-n", ns, "-o", "jsonpath={.metadata.name}"))
				g.Expect(err).NotTo(HaveOccurred(), "provisioning.mode=deploy should render the provisioning-rabbitmq Deployment")
				g.Expect(strings.TrimSpace(out)).To(Equal("provisioning-rabbitmq"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the bootstrap Secret is consumed by REFERENCE (secretKeyRef), never inlined")
			Eventually(func(g Gomega) {
				refs, err := utils.Run(exec.Command("kubectl", "get", "deployment", "provisioning-rabbitmq",
					"-n", ns, "-o", "jsonpath={.spec.template.spec.containers[0].env[*].valueFrom.secretKeyRef.name}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(refs).To(ContainSubstring(provBootstrapSecret),
					"the provisioning service must reference the bootstrap Secret via secretKeyRef")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the provisioning-rabbitmq Deployment reaches Available (the image pulls + the service runs)")
			// The 1.0.0 image is pullable, so the deployed provisioning service must actually come
			// up: assert its Deployment reports an available replica. A persistent failure here
			// (ImagePullBackOff / CrashLoop) would be a real finding to FIX — the service can't run
			// against the managed RabbitMQ wiring.
			Eventually(func(g Gomega) {
				avail, err := utils.Run(exec.Command("kubectl", "get", "deployment", "provisioning-rabbitmq",
					"-n", ns, "-o", "jsonpath={.status.availableReplicas}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(avail)).To(Equal("1"),
					"the provisioning-rabbitmq Deployment should reach 1 available replica (the 1.0.0 image pulls and runs)")
			}, 5*time.Minute, 15*time.Second).Should(Succeed())
		})

		It("should LEAVE the managed infra intact when the Platform is deleted (default Retain policy)", func() {
			By("confirming the managed CRs exist before deletion")
			for _, kindName := range []string{
				"cluster.postgresql.cnpg.io/" + clusterName,
				"rabbitmqcluster.rabbitmq.com/" + mqClusterName,
				"keycloak.k8s.keycloak.org/" + keycloakCRName,
			} {
				_, err := utils.Run(exec.Command("kubectl", "get", kindName, "-n", ns))
				Expect(err).NotTo(HaveOccurred(), "%s should exist before the Platform is deleted", kindName)
			}

			By("deleting the Platform CR (default deletionPolicy is Retain)")
			_, err := utils.Run(exec.Command("kubectl", "delete", "platform", platformName,
				"-n", ns, "--timeout=180s"))
			Expect(err).NotTo(HaveOccurred(), "Failed to delete the full managed Platform (finalizer should run and return)")

			By("verifying the Platform object is gone (finalizer ran and was removed)")
			Eventually(func(g Gomega) {
				_, err := utils.Run(exec.Command("kubectl", "get", "platform", platformName, "-n", ns))
				g.Expect(err).To(HaveOccurred(), "Platform object should be removed after deletion")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the managed CNPG + RabbitMQ + Keycloak CRs STILL EXIST (Retain leaves managed infra intact)")
			// The managed CRs carry NO controller owner reference and are prune-excluded, so neither
			// owner-ref GC nor the Retain deletion path may collect them. Re-check over a window so a
			// late GC sweep would be caught.
			Consistently(func(g Gomega) {
				for _, kindName := range []string{
					"cluster.postgresql.cnpg.io/" + clusterName,
					"rabbitmqcluster.rabbitmq.com/" + mqClusterName,
					"keycloak.k8s.keycloak.org/" + keycloakCRName,
				} {
					_, err := utils.Run(exec.Command("kubectl", "get", kindName, "-n", ns))
					g.Expect(err).NotTo(HaveOccurred(),
						"%s must remain after Platform deletion under the default Retain policy", kindName)
				}
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})
	})
}

// -------------------------------------------------------------------------
// Full managed-platform e2e helpers
// -------------------------------------------------------------------------

// rabbitMQClusterReadyInNS reports whether the RabbitmqCluster in the given namespace reports
// Ready (the AllReplicasReady/ClusterAvailable signal the operator's managedBrokerReady probes).
// It is the namespace-parameterized sibling of rabbitMQClusterReady (which is pinned to the
// managed-messaging namespace) so the full-platform spec can reuse the readback in "keycloak".
func rabbitMQClusterReadyInNS(g Gomega, name, ns string) bool {
	for _, condType := range []string{"AllReplicasReady", "ClusterAvailable"} {
		out, err := utils.Run(exec.Command("kubectl", "get", "rabbitmqcluster.rabbitmq.com", name,
			"-n", ns, "-o", fmt.Sprintf("jsonpath={.status.conditions[?(@.type==%q)].status}", condType)))
		g.Expect(err).NotTo(HaveOccurred(), "RabbitmqCluster %q should be readable for condition %q", name, condType)
		if strings.TrimSpace(out) == "True" {
			return true
		}
	}
	return false
}

// deploymentAvailable reports whether the named Deployment in ns has its Available condition
// True (or at least one available replica) — the same readiness signal the operator's coreReady
// uses. Used to assert Core (and other components) reach Ready at RUNTIME.
func deploymentAvailable(g Gomega, ns, name string) bool {
	out, err := utils.Run(exec.Command("kubectl", "get", "deployment", name, "-n", ns,
		"-o", `jsonpath={.status.conditions[?(@.type=="Available")].status}`))
	g.Expect(err).NotTo(HaveOccurred(), "Deployment %q should be readable for the Available condition", name)
	return strings.TrimSpace(out) == "True"
}

// getCoreChildField returns a single jsonpath field off a Core-targeted platform child object
// (an hpa or pdb named "core") in the given namespace, failing the surrounding assertion on a
// get error. It is the sibling of getKeycloakField for the operator-owned availability children
// (the full-managed spec autoscales/PDBs only Core, so the object name is fixed).
func getCoreChildField(g Gomega, kind, ns, jsonpath string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", kind, "core", "-n", ns, "-o", "jsonpath="+jsonpath))
	g.Expect(err).NotTo(HaveOccurred(), "%s/core should be readable for %s", kind, jsonpath)
	return strings.TrimSpace(out)
}

// deploymentReplicasUnmanagedByOperator reports whether the operator's SSA field manager
// (ilm-operator) does NOT own .spec.replicas on the named Deployment — the load-bearing SSA
// invariant for an autoscaled component (the operator omits replicas so the HPA owns scaling).
// It reads the ilm-operator managedFields entry and checks its fieldsV1 has no f:spec→f:replicas.
// If the operator owned replicas, the entry's JSON would contain "f:replicas" nested under
// "f:spec"; we assert it does not. (The apiserver/HPA may still set the value under a different
// manager — that is fine; the invariant is that the OPERATOR ceded the field.)
func deploymentReplicasUnmanagedByOperator(g Gomega, ns, name string) bool {
	// Pull the fieldsV1 JSON for the manager whose name is the operator's SSA field owner.
	out, err := utils.Run(exec.Command("kubectl", "get", "deployment", name, "-n", ns,
		"-o", `jsonpath={.metadata.managedFields[?(@.manager=="ilm-operator")].fieldsV1}`))
	g.Expect(err).NotTo(HaveOccurred(), "Deployment %q managedFields should be readable", name)
	managed := strings.TrimSpace(out)
	g.Expect(managed).NotTo(BeEmpty(), "the operator (ilm-operator) must be a field manager on the Deployment")
	// The operator owns .spec (it applies the rest of the spec) but must NOT own f:replicas.
	return !strings.Contains(managed, `"f:replicas"`)
}

// mainContainerReadOnlyRoot reports whether the named container in the named Deployment's pod
// template has securityContext.readOnlyRootFilesystem=true (read off the LIVE Deployment, so it
// reflects what the operator actually applied). Used to assert residual b on the running pods.
func mainContainerReadOnlyRoot(g Gomega, ns, dep, container string) bool {
	jsonpath := fmt.Sprintf(
		`jsonpath={.spec.template.spec.containers[?(@.name==%q)].securityContext.readOnlyRootFilesystem}`, container)
	out, err := utils.Run(exec.Command("kubectl", "get", "deployment", dep, "-n", ns, "-o", jsonpath))
	g.Expect(err).NotTo(HaveOccurred(), "Deployment %q should be readable for container %q SCC", dep, container)
	return strings.TrimSpace(out) == "true"
}

// appPodCrashLooping reports whether ANY pod of the given component (selected by the app.k8s.io
// /name label) has a container in CrashLoopBackOff or with a non-zero restart count climbing.
// We treat CrashLoopBackOff as the crash signal (a transient restart during boot is not). Used to
// catch a component that genuinely cannot run with a read-only root (residual b back-off signal).
func appPodCrashLooping(g Gomega, ns, component string) bool {
	out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns,
		"-l", "app.kubernetes.io/name="+component,
		"-o", "jsonpath={.items[*].status.containerStatuses[*].state.waiting.reason}"))
	g.Expect(err).NotTo(HaveOccurred(), "pods for %q should be readable for waiting reasons", component)
	return strings.Contains(out, "CrashLoopBackOff")
}

// noImagePullErrorForApp reports whether the named Deployment's pods are FREE of image-pull
// errors (no container waiting with ImagePullBackOff/ErrImagePull) AND the Deployment has
// observed pods. A false result means the images are not pulling — the image-default registry
// fix would be wrong. It tolerates pods still being created (no containerStatuses yet) by
// requiring at least one pod to exist first.
func noImagePullErrorForApp(g Gomega, ns, dep string) bool {
	// At least one pod must exist for the Deployment (it has been scheduled).
	pods, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns,
		"-l", "app.kubernetes.io/name="+dep, "-o", "jsonpath={.items[*].metadata.name}"))
	g.Expect(err).NotTo(HaveOccurred(), "pods for %q should be listable", dep)
	if strings.TrimSpace(pods) == "" {
		return false
	}
	reasons, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns,
		"-l", "app.kubernetes.io/name="+dep,
		"-o", "jsonpath={.items[*].status.containerStatuses[*].state.waiting.reason}"+
			"{.items[*].status.initContainerStatuses[*].state.waiting.reason}"))
	g.Expect(err).NotTo(HaveOccurred(), "pods for %q should be readable for waiting reasons", dep)
	return !strings.Contains(reasons, "ImagePullBackOff") && !strings.Contains(reasons, "ErrImagePull")
}

// -------------------------------------------------------------------------
// VERSION-MATRIX e2e (split into two independently schedulable blocks)
//
// The version story used to live in ONE Ordered Context that brought a managed platform up
// twice — 2.17.0 → 2.18.0 → refusals, then a HARD teardown/drain barrier, then a fresh
// 2.19.0 — which made it the longest pole of the managed tier (two serial bring-ups on one
// node). It is now TWO Ordered Contexts, "matrix-upgrade" and "matrix-preview", each of which
// CI runs in its own job on its own fresh Kind cluster, in parallel. Because neither block
// shares a node with the other, the barrier that existed only to free that node is gone; the
// deletionPolicy=Delete reclaim it also asserted is kept, as the upgrade block's final spec.
// Both blocks keep the umbrella "matrix" label, so a local `make test-e2e-matrix` still runs
// the whole story (sequentially, in one cluster).
// -------------------------------------------------------------------------

// Version-matrix block identifiers. The upgrade block and the fresh-2.19.0 block each own a
// DISTINCT Platform name — and therefore distinct managed-infra and topology CR names — so
// neither can adopt or collide with the other's objects (the Topology Operator treats a Vhost's
// spec.name as immutable) even when both run in the same cluster locally.
const (
	matrixPlatformName  = "ilm-matrix"
	matrixMQClusterName = matrixPlatformName + "-messaging"
	matrixEdgeHost      = "ilm-matrix.e2e.local"

	previewPlatformName  = "ilm-preview"
	previewMQClusterName = previewPlatformName + "-messaging"
	previewEdgeHost      = "ilm-preview.e2e.local"

	// matrixRealm is the Keycloak realm both version-matrix blocks provision.
	matrixRealm = "ilm"
)

// matrixUpstreamOperators records which upstream operators a version-matrix block installed
// ITSELF (as opposed to finding already present), so its teardown uninstalls exactly those and
// leaves an installation that predates the block alone.
type matrixUpstreamOperators struct {
	cnpg       bool
	mqCluster  bool
	mqTopology bool
	keycloak   bool
}

// setupMatrixBlock installs the upstream operators a version-matrix block depends on
// (CloudNativePG + RabbitMQ Cluster/Topology + Keycloak, idempotently), ensures the
// namespace-scoped Keycloak Operator's namespace exists, and drains any leftover Platform and
// workloads from it. Shared by both blocks: in CI they run on SEPARATE clusters, so each has to
// stand its own dependencies up from scratch; locally they share one cluster and the installs
// are no-ops for whichever block runs second.
func setupMatrixBlock(ns string) matrixUpstreamOperators {
	var ops matrixUpstreamOperators

	By("installing CloudNativePG + RabbitMQ (Cluster + Topology) + Keycloak operators (idempotent)")
	ops.cnpg = !utils.IsCloudNativePGCRDsInstalled()
	if ops.cnpg {
		Expect(utils.InstallCloudNativePG()).To(Succeed(), "Failed to install CloudNativePG")
	}
	ops.mqCluster = !utils.IsRabbitMQClusterOperatorCRDsInstalled()
	if ops.mqCluster {
		Expect(utils.InstallRabbitMQClusterOperator()).To(Succeed(), "Failed to install RabbitMQ Cluster Operator")
	}
	ops.mqTopology = !utils.IsRabbitMQTopologyOperatorCRDsInstalled()
	if ops.mqTopology {
		Expect(utils.InstallRabbitMQTopologyOperator()).To(Succeed(), "Failed to install Messaging Topology Operator")
	}
	ops.keycloak = !utils.IsKeycloakOperatorCRDsInstalled()
	if ops.keycloak {
		Expect(utils.InstallKeycloakOperator()).To(Succeed(), "Failed to install Keycloak Operator")
	}

	By("ensuring the Keycloak Operator namespace + draining any leftover Platform/workloads")
	Expect(utils.CreateNamespaceIdempotent(ns)).To(Succeed(), "Failed to ensure Keycloak Operator namespace")
	_, _ = utils.Run(exec.Command("kubectl", "delete", "platform", "--all",
		"-n", ns, "--ignore-not-found", "--timeout=120s"))
	utils.WaitForWorkloadsDrained(ns, "keycloak-operator")

	return ops
}

// teardownMatrixBlock is the shared best-effort teardown for a version-matrix block, in the FULL
// block's PROVEN order: delete the Platform, then sweep the managed-infra CRs WHILE the upstream
// operators are still running (so their finalizers actually get cleared), and only then uninstall
// the operators. Uninstalling first deadlocks: each operator-manifest delete removes the CRDs,
// whose instance cascade blocks on retained-CR finalizers that only the just-deleted operator
// could clear — that hang burned the rest of the suite budget (~77m) in this block.
func teardownMatrixBlock(ns, platformName string, ops matrixUpstreamOperators) {
	By("best-effort deleting the Platform")
	_, _ = utils.Run(exec.Command("kubectl", "delete", "platform", platformName,
		"-n", ns, "--ignore-not-found", "--timeout=60s"))

	By("deleting the managed infra left behind by the Retain deletion policy")
	// A platform on the default Retain policy leaves its managed CRs behind; a
	// deletionPolicy=Delete one already reclaimed its own (these deletes are then no-ops via
	// --ignore-not-found). The Pooler is swept alongside the Cluster: a managed database renders
	// a PgBouncer Pooler by default (managed_database.go).
	for _, kindName := range matrixManagedCRRefs(platformName) {
		_, _ = utils.Run(exec.Command("kubectl", "delete", kindName,
			"-n", ns, "--ignore-not-found", "--timeout=120s"))
	}

	By("uninstalling the operators this block installed")
	if ops.keycloak {
		utils.UninstallKeycloakOperator()
	}
	if ops.mqTopology {
		utils.UninstallRabbitMQTopologyOperator()
	}
	if ops.mqCluster {
		utils.UninstallRabbitMQClusterOperator()
	}
	if ops.cnpg {
		utils.UninstallCloudNativePG()
	}
}

// dumpMatrixDiagnosticsOnFailure prints the Platform CR and the namespace events when the spec
// that just ran failed, so a red version-matrix run is diagnosable from the job log alone.
func dumpMatrixDiagnosticsOnFailure(ns, platformName string) {
	if !CurrentSpecReport().Failed() {
		return
	}
	if out, err := utils.Run(exec.Command("kubectl", "get", "platform", platformName,
		"-n", ns, "-o", "yaml")); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Platform %q:\n%s", platformName, out)
	}
	if out, err := utils.Run(exec.Command("kubectl", "get", "events",
		"-n", ns, "--sort-by=.lastTimestamp")); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Events:\n%s", out)
	}
}

// matrixManagedCRRefs returns the kubectl "kind/name" references for every POD-BEARING managed
// CR the version-matrix Platform shape renders, per the managed builders: the CNPG Cluster AND
// its PgBouncer Pooler (ResolveManagedDatabase renders the Pooler for every managed database
// unless pgBouncer.managed=false — it is DEFAULT-ON, and these CRs set no pgBouncer block), the
// RabbitmqCluster (ResolveManagedMessaging), and the Keycloak CR plus its realm import
// (ResolveManagedKeycloak). It is the single source of truth for both the reclaim assertion and
// the best-effort teardown sweep.
func matrixManagedCRRefs(platformName string) []string {
	return []string{
		"keycloak.k8s.keycloak.org/" + platformName + "-keycloak",
		"keycloakrealmimport.k8s.keycloak.org/" + platformName + "-keycloak-realm",
		"rabbitmqcluster.rabbitmq.com/" + platformName + "-messaging",
		"cluster.postgresql.cnpg.io/" + platformName + "-db",
		"pooler.postgresql.cnpg.io/" + platformName + "-db-pooler",
	}
}

// platformVersionMatrixUpgradeSpecs registers the UPGRADE half of the version-matrix e2e: a
// MANAGED Platform pinned to 2.17.0 (the pre-rebrand CZERTAINLY release) reaches Available with
// the 2.17.0 contract (core:2.17.0, RABBITMQ_* broker env, single-user managed topology), then
// UPGRADES in place to 2.18.0 (core:2.18.0, BROKER_* env, the five-user topology), then a
// DOWNGRADE is refused, then an upgrade onto the UNRELEASED 2.19.0 preview bundle is refused (the
// running 2.18.0 is preserved) and the restore re-converges, and finally the platform's
// deletionPolicy=Delete teardown is proven to RECLAIM every managed CR it rendered.
//
// Labelled "matrix-upgrade" so CI runs it in its OWN job on its OWN fresh cluster, in parallel
// with the fresh-2.19.0 block (platformVersionMatrixPreviewSpecs); the umbrella "matrix" label is
// kept so `make test-e2e-matrix` still runs both locally. Like the FULL block it runs in the
// Keycloak Operator's namespace (managed Keycloak is namespace-scoped) and installs its own
// upstream operators in BeforeAll.
func platformVersionMatrixUpgradeSpecs() {
	Context("Platform VERSION MATRIX — upgrades (managed 2.17.0 → 2.18.0 → downgrade refused → 2.19.0 preview refused → Delete reclaims)",
		Ordered, Label("managed", "matrix", "matrix-upgrade"), func() {
			ns := utils.KeycloakOperatorNamespace
			const coreUserSecret = "ilm-matrix-messaging-core-user-credentials"
			const provisionerUserSecret = "ilm-matrix-messaging-provisioner-user-credentials"
			var ops matrixUpstreamOperators

			BeforeAll(func() { ops = setupMatrixBlock(ns) })

			AfterAll(func() { teardownMatrixBlock(ns, matrixPlatformName, ops) })

			AfterEach(func() { dumpMatrixDiagnosticsOnFailure(ns, matrixPlatformName) })

			It("deploys a managed 2.17.0 Platform that reaches Available (core:2.17.0, RABBITMQ_*, single-user topology)", func() {
				// deletionPolicy: Delete (the CRD default is Retain) so the operator's own finalizer
				// reclaims every managed CR it rendered — the CNPG Cluster + Pooler, the RabbitmqCluster
				// + its topology, the Keycloak CR + its realm import — when this Platform is deleted.
				// The last spec of this block asserts exactly that reclaim.
				platformYAML := fmt.Sprintf(`
apiVersion: otilm.com/v1alpha1
kind: Platform
metadata:
  name: %s
  namespace: %s
spec:
  version: "2.17.0"
  deletionPolicy: Delete
  database:
    mode: managed
    managed:
      instances: 1
      version: "16"
      storage:
        size: 1Gi
  messaging:
    mode: managed
    brokerType: rabbitmq
    virtualHost: czertainly
    managed:
      replicas: 1
      version: "4.0"
      storage:
        size: 1Gi
  keycloak:
    mode: managed
    realm: %s
    managed:
      instances: 1
      version: "%s"
  edge:
    enabled: true
    host: %s
    tls:
      source: internal
`, matrixPlatformName, ns, matrixRealm, utils.KeycloakOperatorVersion, matrixEdgeHost)
				tmpFile := writeTempYAML(platformYAML)
				defer func() { _ = os.Remove(tmpFile) }()
				_, err := utils.Run(exec.Command("kubectl", "apply", "-f", tmpFile))
				Expect(err).NotTo(HaveOccurred(), "Failed to apply the 2.17.0 managed Platform")

				By("waiting for Available=True with observedVersion 2.17.0 (the full 2.17.0 stack converges)")
				Eventually(func(g Gomega) {
					ver, _ := utils.Run(exec.Command("kubectl", "get", "platform", matrixPlatformName, "-n", ns,
						"-o", "jsonpath={.status.observedVersion}"))
					g.Expect(strings.TrimSpace(ver)).To(Equal("2.17.0"), "observedVersion pins the requested 2.17.0")
					avail, _ := utils.Run(exec.Command("kubectl", "get", "platform", matrixPlatformName, "-n", ns,
						"-o", `jsonpath={.status.conditions[?(@.type=="Available")].status}`))
					g.Expect(strings.TrimSpace(avail)).To(Equal(conditionStatusTrue), "the 2.17.0 platform must reach Available")
				}, 20*time.Minute, 15*time.Second).Should(Succeed())

				By("verifying Core runs the 2.17.0 image and the RABBITMQ_* (not BROKER_*) broker env")
				img, err := utils.Run(exec.Command("kubectl", "get", "deployment", "core", "-n", ns,
					"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="core")].image}`))
				Expect(err).NotTo(HaveOccurred())
				Expect(img).To(ContainSubstring("/core:2.17.0"), "Core must run the 2.17.0 image")
				env, err := utils.Run(exec.Command("kubectl", "get", "deployment", "core", "-n", ns,
					"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="core")].env[*].name}`))
				Expect(err).NotTo(HaveOccurred())
				Expect(env).To(ContainSubstring("RABBITMQ_HOST"), "2.17.0 Core uses RABBITMQ_* broker env")
				Expect(env).NotTo(ContainSubstring("BROKER_HOST"), "2.17.0 Core must NOT use the 2.18.0 BROKER_* env")

				By("verifying the managed RabbitMQ has a SINGLE broker user (core user present, no provisioner user)")
				_, errCore := utils.Run(exec.Command("kubectl", "get", "secret", coreUserSecret, "-n", ns))
				Expect(errCore).NotTo(HaveOccurred(), "the single core-user credential Secret must exist")
				_, errProv := utils.Run(exec.Command("kubectl", "get", "secret", provisionerUserSecret, "-n", ns))
				Expect(errProv).To(HaveOccurred(), "2.17.0 is single-user: there is no provisioner-user Secret")
			})

			It("upgrades 2.17.0 → 2.18.0 in place and re-converges (core:2.18.0, BROKER_*, five-user topology)", func() {
				By("setting spec.version to 2.18.0")
				_, err := utils.Run(exec.Command("kubectl", "patch", "platform", matrixPlatformName, "-n", ns,
					"--type=merge", "-p", `{"spec":{"version":"2.18.0"}}`))
				Expect(err).NotTo(HaveOccurred(), "Failed to patch spec.version to 2.18.0")

				By("waiting for observedVersion 2.18.0 + Available=True + the core:2.18.0 image rolled out")
				Eventually(func(g Gomega) {
					ver, _ := utils.Run(exec.Command("kubectl", "get", "platform", matrixPlatformName, "-n", ns,
						"-o", "jsonpath={.status.observedVersion}"))
					g.Expect(strings.TrimSpace(ver)).To(Equal("2.18.0"), "observedVersion advances to 2.18.0")
					avail, _ := utils.Run(exec.Command("kubectl", "get", "platform", matrixPlatformName, "-n", ns,
						"-o", `jsonpath={.status.conditions[?(@.type=="Available")].status}`))
					g.Expect(strings.TrimSpace(avail)).To(Equal(conditionStatusTrue), "the platform must re-converge after the upgrade")
					img, _ := utils.Run(exec.Command("kubectl", "get", "deployment", "core", "-n", ns,
						"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="core")].image}`))
					g.Expect(img).To(ContainSubstring("/core:2.18.0"), "Core must roll to the 2.18.0 image")
				}, 20*time.Minute, 15*time.Second).Should(Succeed())

				By("verifying Core now uses BROKER_* env and the topology gained the provisioner/proxy/monitor users (five-user)")
				env, err := utils.Run(exec.Command("kubectl", "get", "deployment", "core", "-n", ns,
					"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="core")].env[*].name}`))
				Expect(err).NotTo(HaveOccurred())
				Expect(env).To(ContainSubstring("BROKER_HOST"), "2.18.0 Core uses BROKER_* broker env")
				Eventually(func(g Gomega) {
					_, err := utils.Run(exec.Command("kubectl", "get", "secret", provisionerUserSecret, "-n", ns))
					g.Expect(err).NotTo(HaveOccurred(), "the 2.18.0 five-user topology adds the provisioner user (a representative of the new users)")
				}, 5*time.Minute, 10*time.Second).Should(Succeed())
			})

			It("refuses a downgrade back to 2.17.0 (DowngradeForbidden; the running 2.18.0 is preserved)", func() {
				By("setting spec.version back to 2.17.0")
				_, err := utils.Run(exec.Command("kubectl", "patch", "platform", matrixPlatformName, "-n", ns,
					"--type=merge", "-p", `{"spec":{"version":"2.17.0"}}`))
				Expect(err).NotTo(HaveOccurred(), "Failed to patch spec.version to 2.17.0")

				By("verifying the platform goes Degraded/DowngradeForbidden and observedVersion stays 2.18.0")
				Eventually(func(g Gomega) {
					reason, _ := utils.Run(exec.Command("kubectl", "get", "platform", matrixPlatformName, "-n", ns,
						"-o", `jsonpath={.status.conditions[?(@.type=="Degraded")].reason}`))
					g.Expect(strings.TrimSpace(reason)).To(Equal("DowngradeForbidden"), "an older spec.version must be refused")
					ver, _ := utils.Run(exec.Command("kubectl", "get", "platform", matrixPlatformName, "-n", ns,
						"-o", "jsonpath={.status.observedVersion}"))
					g.Expect(strings.TrimSpace(ver)).To(Equal("2.18.0"), "the running version is NOT rolled back")
				}, 5*time.Minute, 10*time.Second).Should(Succeed())
			})

			It("refuses an upgrade onto the 2.19.0 preview bundle (PreviewVersionUpgradeBlocked; the running 2.18.0 is preserved)", func() {
				By("setting spec.version to the unreleased 2.19.0 preview bundle")
				_, err := utils.Run(exec.Command("kubectl", "patch", "platform", matrixPlatformName, "-n", ns,
					"--type=merge", "-p", `{"spec":{"version":"2.19.0"}}`))
				Expect(err).NotTo(HaveOccurred(), "Failed to patch spec.version to 2.19.0")

				By("verifying the platform goes Degraded/PreviewVersionUpgradeBlocked and observedVersion stays 2.18.0")
				Eventually(func(g Gomega) {
					reason, _ := utils.Run(exec.Command("kubectl", "get", "platform", matrixPlatformName, "-n", ns,
						"-o", `jsonpath={.status.conditions[?(@.type=="Degraded")].reason}`))
					g.Expect(strings.TrimSpace(reason)).To(Equal("PreviewVersionUpgradeBlocked"),
						"a live platform must not be upgraded onto an unreleased (preview) bundle")
					ver, _ := utils.Run(exec.Command("kubectl", "get", "platform", matrixPlatformName, "-n", ns,
						"-o", "jsonpath={.status.observedVersion}"))
					g.Expect(strings.TrimSpace(ver)).To(Equal("2.18.0"), "the running version is NOT advanced onto the preview")
				}, 5*time.Minute, 10*time.Second).Should(Succeed())

				By("verifying the running 2.18.0 workload + managed topology are left untouched (nothing 2.19.0 was rendered)")
				// The guard is a terminal steady state BEFORE the render/apply, so the live objects must
				// keep the 2.18.0 contract: Core stays on core:2.18.0 and the managed topology keeps the
				// 2.18.0 vhost with none of the 2.19.0-only objects (the renamed "ilm" exchange, the new
				// provider.status-poll queue) appearing alongside it.
				Consistently(func(g Gomega) {
					img, _ := utils.Run(exec.Command("kubectl", "get", "deployment", "core", "-n", ns,
						"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="core")].image}`))
					g.Expect(img).To(ContainSubstring("/core:2.18.0"), "Core must keep running the 2.18.0 image")
					g.Expect(topologyObjectSpecName(g, ns, "vhost", matrixMQClusterName+"-vhost")).To(Equal("czertainly"),
						"the managed vhost must stay the 2.18.0 one")
					g.Expect(topologyObjectAbsent(g, ns, "exchange", matrixMQClusterName+"-exchange-ilm")).To(BeTrue(),
						"the 2.19.0 \"ilm\" exchange must NOT be rendered")
					g.Expect(topologyObjectAbsent(g, ns, "queue", matrixMQClusterName+"-queue-provider-status-poll")).To(BeTrue(),
						"the 2.19.0 provider.status-poll queue must NOT be rendered")
				}, 30*time.Second, 5*time.Second).Should(Succeed())

				By("restoring spec.version to 2.18.0")
				_, err = utils.Run(exec.Command("kubectl", "patch", "platform", matrixPlatformName, "-n", ns,
					"--type=merge", "-p", `{"spec":{"version":"2.18.0"}}`))
				Expect(err).NotTo(HaveOccurred(), "Failed to patch spec.version back to 2.18.0")

				By("verifying the platform returns to Available with the blocked Degraded condition cleared")
				// The successful pass clears the stale Degraded=True (reason Reconciled), so the platform
				// stops advertising the refusal once the spec is corrected.
				Eventually(func(g Gomega) {
					avail, _ := utils.Run(exec.Command("kubectl", "get", "platform", matrixPlatformName, "-n", ns,
						"-o", `jsonpath={.status.conditions[?(@.type=="Available")].status}`))
					g.Expect(strings.TrimSpace(avail)).To(Equal(conditionStatusTrue), "the platform must be Available again")
					degraded, _ := utils.Run(exec.Command("kubectl", "get", "platform", matrixPlatformName, "-n", ns,
						"-o", `jsonpath={.status.conditions[?(@.type=="Degraded")].status}`))
					g.Expect(strings.TrimSpace(degraded)).To(Equal("False"), "the stale Degraded must be cleared once the spec is corrected")
					ver, _ := utils.Run(exec.Command("kubectl", "get", "platform", matrixPlatformName, "-n", ns,
						"-o", "jsonpath={.status.observedVersion}"))
					g.Expect(strings.TrimSpace(ver)).To(Equal("2.18.0"), "the restored version is the one that kept running")
				}, 10*time.Minute, 10*time.Second).Should(Succeed())
			})

			It("reclaims every managed CR when the deletionPolicy=Delete Platform is deleted", func() {
				// BEHAVIOURAL assertion of spec.deletionPolicy=Delete on a real cluster: the delete runs
				// the operator's own finalizer teardown, which must reclaim every managed CR it rendered
				// WHILE the upstream operators are still running (so their finalizers actually get
				// cleared). It is also the only place the teardown render path — which resolves against
				// the RUNNING version, not the requested one — is exercised end-to-end after a blocked
				// upgrade has left spec.version and status.observedVersion disagreeing.
				By("deleting the matrix Platform so its deletionPolicy=Delete teardown reclaims the managed infra")
				_, deleteErr := utils.Run(exec.Command("kubectl", "delete", "platform", matrixPlatformName,
					"-n", ns, "--ignore-not-found", "--timeout=180s"))
				Expect(deleteErr).NotTo(HaveOccurred(),
					"deleting the matrix Platform must SUCCEED (--ignore-not-found tolerates an already-deleted CR)")
				Eventually(func(g Gomega) {
					out, err := utils.Run(exec.Command("kubectl", "get", "platform",
						"-n", ns, "-o", "jsonpath={.items[*].metadata.name}"))
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(strings.TrimSpace(out)).To(BeEmpty(),
						"the Platform object itself must be gone once its finalizer teardown completes")
				}, 2*time.Minute, 5*time.Second).Should(Succeed())

				By("verifying every managed workload CR the matrix Platform provisioned is really gone")
				Eventually(func(g Gomega) {
					for _, kindName := range matrixManagedCRRefs(matrixPlatformName) {
						out, getErr := utils.Run(exec.Command("kubectl", "get", kindName, "-n", ns,
							"--ignore-not-found", "-o", "name"))
						g.Expect(getErr).NotTo(HaveOccurred(),
							"the %q lookup must succeed (an error is not proof that the CR is gone)", kindName)
						g.Expect(strings.TrimSpace(out)).To(BeEmpty(),
							"managed CR %q must be reclaimed by the deletionPolicy=Delete teardown", kindName)
					}
				}, 5*time.Minute, 10*time.Second).Should(Succeed())
			})
		})
}

// platformVersionMatrixPreviewSpecs registers the FRESH-INSTALL half of the version-matrix e2e:
// a brand-new managed Platform pinned to 2.19.0 comes up on the 2.19.0 contract — core:2.19.0
// (plus the auth/scheduler images of that bundle, each actually rolled out), the renamed
// LOGGING_LEVEL_COM_OTILM env, and, read back THROUGH the Messaging Topology Operator, the
// default "/" vhost, the renamed ilm / ilm-proxy exchanges and the new provider.status-poll
// queue. A fresh install MAY name an unreleased (preview) bundle; only UPGRADING a live platform
// onto one is refused (that refusal is the upgrade block's spec).
//
// Labelled "matrix-preview" so CI runs it in its OWN job on its OWN fresh cluster, in parallel
// with platformVersionMatrixUpgradeSpecs — the two bring-ups used to run serially in one Context
// with a teardown/drain barrier between them, which made this the managed tier's longest pole.
// The umbrella "matrix" label is kept so `make test-e2e-matrix` still runs both locally. Like the
// FULL block it runs in the Keycloak Operator's namespace (managed Keycloak is namespace-scoped)
// and installs its own upstream operators in BeforeAll.
func platformVersionMatrixPreviewSpecs() {
	Context("Platform VERSION MATRIX — fresh managed 2.19.0 install (core:2.19.0, vhost /, ilm exchanges, provider.status-poll)",
		Ordered, Label("managed", "matrix", "matrix-preview"), func() {
			ns := utils.KeycloakOperatorNamespace
			var ops matrixUpstreamOperators

			BeforeAll(func() { ops = setupMatrixBlock(ns) })

			AfterAll(func() { teardownMatrixBlock(ns, previewPlatformName, ops) })

			AfterEach(func() { dumpMatrixDiagnosticsOnFailure(ns, previewPlatformName) })

			It("deploys a FRESH managed 2.19.0 Platform that reaches Available (core:2.19.0, vhost /, ilm exchanges, provider.status-poll)", func() {
				By("creating a FRESH managed Platform pinned to 2.19.0 (a fresh install MAY name a preview bundle)")
				// The same shape as the upgrade block's CR — managed database + messaging + Keycloak,
				// edge enabled with an internal (cert-manager) issuer, small footprint — with its own
				// names and NO spec.messaging.virtualHost, so the vhost comes from the 2.19.0 bundle
				// ("/") rather than the 2.18.0 "czertainly" the upgrade block's CR pins. It keeps the
				// CRD-default Retain policy, so its managed CRs are swept by the block teardown.
				platformYAML := fmt.Sprintf(`
apiVersion: otilm.com/v1alpha1
kind: Platform
metadata:
  name: %s
  namespace: %s
spec:
  version: "2.19.0"
  database:
    mode: managed
    managed:
      instances: 1
      version: "16"
      storage:
        size: 1Gi
  messaging:
    mode: managed
    brokerType: rabbitmq
    managed:
      replicas: 1
      version: "4.0"
      storage:
        size: 1Gi
  keycloak:
    mode: managed
    realm: %s
    managed:
      instances: 1
      version: "%s"
  edge:
    enabled: true
    host: %s
    tls:
      source: internal
`, previewPlatformName, ns, matrixRealm, utils.KeycloakOperatorVersion, previewEdgeHost)
				tmpFile := writeTempYAML(platformYAML)
				defer func() { _ = os.Remove(tmpFile) }()
				_, err := utils.Run(exec.Command("kubectl", "apply", "-f", tmpFile))
				Expect(err).NotTo(HaveOccurred(), "Failed to apply the 2.19.0 managed Platform")

				By("waiting for Available=True with observedVersion 2.19.0 (the full 2.19.0 stack converges)")
				Eventually(func(g Gomega) {
					ver, _ := utils.Run(exec.Command("kubectl", "get", "platform", previewPlatformName, "-n", ns,
						"-o", "jsonpath={.status.observedVersion}"))
					g.Expect(strings.TrimSpace(ver)).To(Equal("2.19.0"), "observedVersion pins the explicitly requested preview")
					avail, _ := utils.Run(exec.Command("kubectl", "get", "platform", previewPlatformName, "-n", ns,
						"-o", `jsonpath={.status.conditions[?(@.type=="Available")].status}`))
					g.Expect(strings.TrimSpace(avail)).To(Equal(conditionStatusTrue), "the 2.19.0 platform must reach Available")
				}, 20*time.Minute, 15*time.Second).Should(Succeed())

				By("verifying the 2.19.0 image set RUNS (core:2.19.0, auth:1.7.0, scheduler:1.1.1 all Available)")
				// Each component's Deployment and its main container share the component name (see the
				// FULL block's read-only-root checks), so one jsonpath shape reads every image back. The
				// rendered image alone would be satisfied by a Deployment whose pods never start — the
				// Platform's Available gates only Core and auth, so a scheduler stuck in ImagePullBackOff
				// would slip through — hence each component must ALSO report Available. The image is
				// matched as an exact tag SUFFIX so no coincidental substring can satisfy it.
				for _, c := range []struct{ dep, image string }{
					{"core", "/core:2.19.0"},
					{"auth", "/auth:1.7.0"},
					{"scheduler", "/scheduler:1.1.1"},
				} {
					Eventually(func(g Gomega) {
						img, imgErr := utils.Run(exec.Command("kubectl", "get", "deployment", c.dep, "-n", ns,
							"-o", fmt.Sprintf(`jsonpath={.spec.template.spec.containers[?(@.name==%q)].image}`, c.dep)))
						g.Expect(imgErr).NotTo(HaveOccurred())
						g.Expect(strings.TrimSpace(img)).To(HaveSuffix(c.image),
							"%s must run the 2.19.0 bundle's image", c.dep)
						g.Expect(deploymentAvailable(g, ns, c.dep)).To(BeTrue(),
							"%s must actually roll out on that image, not merely be rendered with it", c.dep)
					}, 10*time.Minute, 15*time.Second).Should(Succeed())
				}

				By("verifying Core uses the renamed LOGGING_LEVEL_COM_OTILM env (not the CZERTAINLY one)")
				env, err := utils.Run(exec.Command("kubectl", "get", "deployment", "core", "-n", ns,
					"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="core")].env[*].name}`))
				Expect(err).NotTo(HaveOccurred())
				Expect(env).To(ContainSubstring("LOGGING_LEVEL_COM_OTILM"), "2.19.0 Core uses the OTILM logging env")
				Expect(env).NotTo(ContainSubstring("LOGGING_LEVEL_COM_CZERTAINLY"), "2.19.0 Core must NOT use the pre-rebrand logging env")

				By("verifying the managed topology is the 2.19.0 one at the BROKER level (vhost /, ilm + ilm-proxy exchanges, provider.status-poll)")
				// Read the topology back THROUGH the Messaging Topology Operator: each renamed/new CR
				// must exist AND reach Ready, i.e. the operator really applied the 2.19.0 topology to
				// the live broker (a rejected or unappliable CR never gets there). Readiness is asserted
				// on the CR, but the 2.19.0 CONTRACT is the broker-facing spec.name — the Kubernetes
				// metadata.name is sanitized ([a-z0-9-] only), so "provider.status-poll" and
				// "provider_status_poll" would share the object name "...-queue-provider-status-poll"
				// and only spec.name distinguishes them. Assert both.
				const previewVhostCR = previewMQClusterName + "-vhost"
				const previewStatusPollQueueCR = previewMQClusterName + "-queue-provider-status-poll"
				Eventually(func(g Gomega) {
					g.Expect(topologyObjectReadyInNS(g, ns, "vhost", previewVhostCR)).To(BeTrue(),
						"the Vhost CR should be accepted and reach Ready")
					g.Expect(topologyObjectSpecName(g, ns, "vhost", previewVhostCR)).To(Equal("/"),
						"2.19.0 provisions the default \"/\" vhost")
					for _, ex := range []struct{ cr, brokerName string }{
						{previewMQClusterName + "-exchange-ilm", "ilm"},
						{previewMQClusterName + "-exchange-ilm-proxy", "ilm-proxy"},
					} {
						g.Expect(topologyObjectReadyInNS(g, ns, "exchange", ex.cr)).To(BeTrue(),
							"Exchange CR %q should be accepted and reach Ready (2.19.0 renamed both exchanges)", ex.cr)
						g.Expect(topologyObjectSpecName(g, ns, "exchange", ex.cr)).To(Equal(ex.brokerName),
							"Exchange CR %q must declare the broker-facing name %q (the 2.19.0 rename)", ex.cr, ex.brokerName)
					}
					g.Expect(topologyObjectReadyInNS(g, ns, "queue", previewStatusPollQueueCR)).To(BeTrue(),
						"the 2.19.0-new provider.status-poll Queue CR should be accepted and reach Ready")
					g.Expect(topologyObjectSpecName(g, ns, "queue", previewStatusPollQueueCR)).To(Equal("provider.status-poll"),
						"the Queue CR must declare the broker-facing name \"provider.status-poll\" verbatim (dot, not dash)")
				}, 5*time.Minute, 10*time.Second).Should(Succeed())
			})
		})
}

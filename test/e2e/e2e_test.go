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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	"github.com/OmniTrustILM/operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "ilm-operator-system"

// serviceAccountName created for the project
const serviceAccountName = "ilm-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "ilm-operator-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "ilm-operator-metrics-binding"

// testNamespace is the namespace used for test Connector CRs to keep them isolated from the
// operator namespace.
const testNamespace = "ilm-e2e-test"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("creating test namespace for Connector CRs")
		cmd = exec.Command("kubectl", "create", "ns", testNamespace)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create test namespace")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("deleting test namespace")
		cmd = exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events (operator ns):\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching Kubernetes events in test namespace")
			cmd = exec.Command("kubectl", "get", "events", "-n", testNamespace, "--sort-by=.lastTimestamp")
			testEventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events (test ns):\n%s", testEventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get test ns events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=ilm-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccount": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			metricsOutput := getMetricsOutput()
			Expect(metricsOutput).To(ContainSubstring(
				"controller_runtime_reconcile_total",
			))
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	// -------------------------------------------------------------------------
	// Connector E2E Tests
	// -------------------------------------------------------------------------

	Context("Connector full lifecycle", Ordered, func() {
		const connName = "e2e-x509-lifecycle"

		AfterAll(func() {
			By("cleaning up Connector if it still exists")
			cmd := exec.Command("kubectl", "delete", "connector", connName,
				"-n", testNamespace, "--ignore-not-found", "--timeout=60s")
			_, _ = utils.Run(cmd)
		})

		It("should deploy a Connector CR and reach Running phase", func() {
			By("creating the Connector CR")
			connectorYAML := fmt.Sprintf(`
apiVersion: otilm.com/v1alpha1
kind: Connector
metadata:
  name: %s
  namespace: %s
spec:
  image:
    repository: docker.io/czertainly/czertainly-x509-compliance-provider
    tag: "2.13.0"
    pullPolicy: IfNotPresent
  service:
    port: 8080
    type: ClusterIP
  env:
    - name: SERVER_PORT
      value: "8080"
    - name: LOG_LEVEL
      value: "INFO"
    - name: E2E_TEST_VAR
      value: "initial"
`, connName, testNamespace)

			tmpFile := writeTempYAML(connectorYAML)
			defer func() { _ = os.Remove(tmpFile) }()

			cmd := exec.Command("kubectl", "apply", "-f", tmpFile)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Connector CR")

			By("waiting for Connector to reach Running phase (up to 5 minutes)")
			Eventually(func(g Gomega) {
				phase := getConnectorPhase(connName)
				g.Expect(phase).To(Equal("Running"),
					"Connector phase should be Running, got: %s", phase)
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should have a running pod backing the Connector", func() {
			By("verifying at least one pod is running for the Connector Deployment")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("otilm.com/connector=%s", connName),
					"-n", testNamespace,
					"-o", "jsonpath={.items[*].status.phase}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				phases := strings.Fields(output)
				g.Expect(phases).NotTo(BeEmpty(), "no pods found for Connector")
				for _, phase := range phases {
					g.Expect(phase).To(Equal("Running"), "pod phase should be Running, got: %s", phase)
				}
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should have a Service created for the Connector", func() {
			By("verifying the Service exists")
			cmd := exec.Command("kubectl", "get", "service", connName,
				"-n", testNamespace,
				"-o", "jsonpath={.spec.type}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Service should exist for Connector")
			Expect(output).To(Equal("ClusterIP"), "Service type should be ClusterIP")

			By("verifying the Service has the correct port")
			cmd = exec.Command("kubectl", "get", "service", connName,
				"-n", testNamespace,
				"-o", "jsonpath={.spec.ports[0].port}")
			portOutput, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(portOutput).To(Equal("8080"), "Service port should be 8080")

			By("verifying the Service has endpoints (pod is ready)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", connName,
					"-n", testNamespace,
					"-o", "jsonpath={.subsets}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(), "Service endpoints should have at least one address")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should update the Connector and reflect the change in the Deployment", func() {
			By("patching the Connector to change an env var")
			patch := `{"spec":{"env":[{"name":"SERVER_PORT","value":"8080"},{"name":"LOG_LEVEL","value":"DEBUG"},{"name":"E2E_TEST_VAR","value":"updated"}]}}`
			cmd := exec.Command("kubectl", "patch", "connector", connName,
				"-n", testNamespace,
				"--type=merge",
				"-p", patch,
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch Connector")

			By("waiting for the Deployment to reflect the updated env var")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", connName,
					"-n", testNamespace,
					"-o", "jsonpath={.spec.template.spec.containers[0].env[?(@.name==\"E2E_TEST_VAR\")].value}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("updated"),
					"Deployment env var E2E_TEST_VAR should be 'updated', got: %s", output)
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting for Connector to return to Running phase after the update")
			Eventually(func(g Gomega) {
				phase := getConnectorPhase(connName)
				g.Expect(phase).To(Equal("Running"),
					"Connector phase should return to Running after update, got: %s", phase)
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should delete the Connector and clean up all child resources", func() {
			By("deleting the Connector CR")
			cmd := exec.Command("kubectl", "delete", "connector", connName,
				"-n", testNamespace,
				"--timeout=120s",
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete Connector CR")

			By("verifying the Deployment is deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", connName,
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(),
					"Deployment should be deleted after Connector deletion")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the Service is deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", connName,
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(),
					"Service should be deleted after Connector deletion")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the ServiceAccount is deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "serviceaccount", connName,
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(),
					"ServiceAccount should be deleted after Connector deletion")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})
	})

	Context("Connector Secret rotation", Ordered, func() {
		const connName = "e2e-x509-secret-rotation"
		const secretName = "e2e-rotation-secret"

		AfterAll(func() {
			By("cleaning up Connector if it still exists")
			cmd := exec.Command("kubectl", "delete", "connector", connName,
				"-n", testNamespace, "--ignore-not-found", "--timeout=60s")
			_, _ = utils.Run(cmd)

			By("cleaning up Secret if it still exists")
			cmd = exec.Command("kubectl", "delete", "secret", secretName,
				"-n", testNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should trigger a rolling restart when the referenced Secret data changes", func() {
			By("creating a Secret to be referenced by the Connector")
			secretYAML := fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  api-key: "initial-api-key-value"
`, secretName, testNamespace)

			tmpSecret := writeTempYAML(secretYAML)
			defer func() { _ = os.Remove(tmpSecret) }()

			cmd := exec.Command("kubectl", "apply", "-f", tmpSecret)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test Secret")

			By("creating a Connector CR that references the Secret as an env var")
			connectorYAML := fmt.Sprintf(`
apiVersion: otilm.com/v1alpha1
kind: Connector
metadata:
  name: %s
  namespace: %s
spec:
  image:
    repository: docker.io/czertainly/czertainly-x509-compliance-provider
    tag: "2.13.0"
    pullPolicy: IfNotPresent
  service:
    port: 8080
    type: ClusterIP
  env:
    - name: SERVER_PORT
      value: "8080"
    - name: LOG_LEVEL
      value: "INFO"
  secretRefs:
    - name: %s
      type: env
      keys:
        - secretKey: api-key
          envVar: API_KEY
`, connName, testNamespace, secretName)

			tmpConn := writeTempYAML(connectorYAML)
			defer func() { _ = os.Remove(tmpConn) }()

			cmd = exec.Command("kubectl", "apply", "-f", tmpConn)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Connector CR with SecretRef")

			By("waiting for Connector to reach Running phase (up to 5 minutes)")
			Eventually(func(g Gomega) {
				phase := getConnectorPhase(connName)
				g.Expect(phase).To(Equal("Running"),
					"Connector phase should be Running, got: %s", phase)
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("recording current pod names before Secret rotation")
			initialPodNames := getPodNamesForConnector(connName, testNamespace)
			Expect(initialPodNames).NotTo(BeEmpty(), "expected at least one pod before rotation")
			_, _ = fmt.Fprintf(GinkgoWriter, "Initial pods: %v\n", initialPodNames)

			By("recording the current config checksum from the Connector status")
			initialChecksum := getConnectorConfigChecksum(connName, testNamespace)
			_, _ = fmt.Fprintf(GinkgoWriter, "Initial checksum: %s\n", initialChecksum)

			By("updating the Secret data to trigger a rolling restart")
			cmd = exec.Command("kubectl", "create", "secret", "generic", secretName,
				"-n", testNamespace,
				"--from-literal=api-key=rotated-api-key-value",
				"--dry-run=client", "-o", "yaml",
			)
			patchYAML, err := cmd.Output()
			Expect(err).NotTo(HaveOccurred())

			tmpPatch := writeTempYAML(string(patchYAML))
			defer func() { _ = os.Remove(tmpPatch) }()

			cmd = exec.Command("kubectl", "apply", "-f", tmpPatch)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to update Secret")

			By("waiting for the Connector config checksum to change (reconciler detected Secret change)")
			Eventually(func(g Gomega) {
				newChecksum := getConnectorConfigChecksum(connName, testNamespace)
				g.Expect(newChecksum).NotTo(BeEmpty())
				g.Expect(newChecksum).NotTo(Equal(initialChecksum),
					"config checksum should change after Secret rotation")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting for new pods to be scheduled (rolling restart triggered by checksum change)")
			Eventually(func(g Gomega) {
				currentPodNames := getPodNamesForConnector(connName, testNamespace)
				// After rolling restart we expect the pod set to differ from the initial set.
				// Either new pod names appeared or old ones are gone.
				changed := false
				initialSet := make(map[string]struct{}, len(initialPodNames))
				for _, n := range initialPodNames {
					initialSet[n] = struct{}{}
				}
				for _, n := range currentPodNames {
					if _, found := initialSet[n]; !found {
						changed = true
						break
					}
				}
				g.Expect(changed).To(BeTrue(),
					"expected new pods after Secret rotation, but pod names did not change. current: %v, initial: %v",
					currentPodNames, initialPodNames)
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("waiting for Connector to return to Running phase after the rolling restart")
			Eventually(func(g Gomega) {
				phase := getConnectorPhase(connName)
				g.Expect(phase).To(Equal("Running"),
					"Connector should be Running after Secret rotation, got: %s", phase)
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the new pods are running")
			newPodNames := getPodNamesForConnector(connName, testNamespace)
			Expect(newPodNames).NotTo(BeEmpty(), "expected running pods after rotation")
			for _, podName := range newPodNames {
				cmd := exec.Command("kubectl", "get", "pod", podName,
					"-n", testNamespace,
					"-o", "jsonpath={.status.phase}",
				)
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(output).To(Equal("Running"),
					"pod %s should be Running after rotation, got: %s", podName, output)
			}
		})
	})
})

// -------------------------------------------------------------------------
// Helper functions
// -------------------------------------------------------------------------

// getConnectorPhase returns the current phase of a Connector CR.
func getConnectorPhase(name string) string {
	cmd := exec.Command("kubectl", "get", "connector", name,
		"-n", testNamespace,
		"-o", "jsonpath={.status.phase}",
	)
	output, err := utils.Run(cmd)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

// getConnectorConfigChecksum returns the current configChecksum from the Connector status.
func getConnectorConfigChecksum(name, ns string) string {
	cmd := exec.Command("kubectl", "get", "connector", name,
		"-n", ns,
		"-o", "jsonpath={.status.configChecksum}",
	)
	output, err := utils.Run(cmd)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

// getPodNamesForConnector returns the names of non-terminating pods belonging to a Connector.
func getPodNamesForConnector(connName, ns string) []string {
	cmd := exec.Command("kubectl", "get", "pods",
		"-l", fmt.Sprintf("otilm.com/connector=%s", connName),
		"-n", ns,
		"-o", "go-template={{ range .items }}"+
			"{{ if not .metadata.deletionTimestamp }}"+
			"{{ .metadata.name }}{{ \"\\n\" }}"+
			"{{ end }}{{ end }}",
	)
	output, err := utils.Run(cmd)
	if err != nil {
		return nil
	}
	return utils.GetNonEmptyLines(output)
}

// writeTempYAML writes content to a temporary YAML file and returns its path.
func writeTempYAML(content string) string {
	f, err := os.CreateTemp("", "e2e-*.yaml")
	Expect(err).NotTo(HaveOccurred(), "Failed to create temp YAML file")
	_, err = f.WriteString(content)
	Expect(err).NotTo(HaveOccurred())
	Expect(f.Close()).To(Succeed())
	return f.Name()
}

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() string {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	metricsOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
	Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
	return metricsOutput
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

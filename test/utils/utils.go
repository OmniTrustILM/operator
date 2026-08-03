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

// Package utils provides test helper utilities for running e2e tests.
package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

const (
	prometheusOperatorVersion = "v0.77.1"
	prometheusOperatorURL     = "https://github.com/prometheus-operator/prometheus-operator/" +
		"releases/download/%s/bundle.yaml"

	certmanagerVersion = "v1.20.2"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	// cloudNativePGVersion is the pinned CloudNativePG operator release the managed-database
	// e2e installs. The release publishes a single self-contained install manifest
	// (cnpg-<ver>.yaml) carrying the operator Deployment, RBAC, and the postgresql.cnpg.io
	// CRDs (Cluster/Pooler). Pinned (not "latest") so the e2e is reproducible and the
	// VERIFY(cnpg) field-shape assertions are validated against a known CRD version.
	cloudNativePGVersion = "1.29.1"
	cloudNativePGURLTmpl = "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/" +
		"v%[1]s/cnpg-%[1]s.yaml"
	// cloudNativePGNamespace is the namespace the CNPG install manifest deploys the operator
	// into (CloudNativePG's documented default).
	cloudNativePGNamespace = "cnpg-system"
	// cloudNativePGDeployment is the CNPG controller Deployment created by the install manifest.
	cloudNativePGDeployment = "cnpg-controller-manager"

	// rabbitmqClusterOperatorVersion / rabbitmqClusterOperatorURLTmpl pin the RabbitMQ
	// Cluster Operator release the managed-messaging e2e installs. The release publishes a
	// single self-contained install manifest (cluster-operator.yml) carrying the operator
	// Deployment, RBAC, and the rabbitmqclusters.rabbitmq.com CRD. Pinned (not "latest") so
	// the e2e is reproducible and the VERIFY(rabbitmq) field-shape assertions are validated
	// against a known CRD version.
	rabbitmqClusterOperatorVersion = "v2.21.0"
	rabbitmqClusterOperatorURLTmpl = "https://github.com/rabbitmq/cluster-operator/releases/download/" +
		"%s/cluster-operator.yml"
	// rabbitmqClusterOperatorNamespace / rabbitmqClusterOperatorDeployment are the namespace
	// and controller Deployment the Cluster Operator install manifest creates.
	rabbitmqClusterOperatorNamespace  = "rabbitmq-system"
	rabbitmqClusterOperatorDeployment = "rabbitmq-cluster-operator"

	// rabbitmqTopologyOperatorVersion / rabbitmqTopologyOperatorURLTmpl pin the Messaging
	// Topology Operator release. The "-with-certmanager" manifest variant wires its serving
	// cert through cert-manager (which the suite already installs), so it is the right choice
	// here; it carries the operator Deployment, RBAC, and the topology CRDs (vhosts/users/
	// permissions/queues/exchanges/bindings.rabbitmq.com). Pinned for the same reproducibility
	// reason as the Cluster Operator.
	rabbitmqTopologyOperatorVersion = "v1.19.2"
	rabbitmqTopologyOperatorURLTmpl = "https://github.com/rabbitmq/messaging-topology-operator/releases/download/" +
		"%s/messaging-topology-operator-with-certmanager.yaml"
	// rabbitmqTopologyOperatorNamespace / rabbitmqTopologyOperatorDeployment are the namespace
	// and controller Deployment the Topology Operator install manifest creates.
	rabbitmqTopologyOperatorNamespace  = "rabbitmq-system"
	rabbitmqTopologyOperatorDeployment = "messaging-topology-operator"

	// keycloakOperatorVersion pins the Keycloak Operator release the managed-Keycloak e2e
	// installs. The keycloak-k8s-resources repo publishes, per release TAG, the two CRDs
	// (keycloaks / keycloakrealmimports.k8s.keycloak.org-v1.yml) and the operator install
	// manifest (kubernetes.yml). Pinned (not "nightly"/"latest") so the e2e is reproducible and
	// the VERIFY(keycloak) field-shape assertions are validated against a known CRD version.
	// The operand image (quay.io/keycloak/keycloak:<version>) the operator's builder composes
	// is PUBLIC, but Keycloak needs a working DATABASE to reach Ready — the managed-Keycloak
	// spec provides one via CloudNativePG.
	keycloakOperatorVersion  = "26.6.3"
	keycloakResourcesRawTmpl = "https://raw.githubusercontent.com/keycloak/keycloak-k8s-resources/" +
		"%[1]s/kubernetes/%[2]s"
	// keycloakOperatorNamespace is the namespace the Keycloak Operator MUST be installed into:
	// the operator is NAMESPACE-SCOPED (it watches only the namespace it runs in — see the
	// Keycloak Operator install docs), and its ClusterRoleBinding subject in kubernetes.yml
	// hard-codes the ServiceAccount in namespace "keycloak". So the operator is installed here
	// AND the managed-Keycloak Platform (with its Keycloak CR) must run here too, or the
	// operator never reconciles the CR.
	keycloakOperatorNamespace = "keycloak"
	// keycloakOperatorDeployment is the controller Deployment the kubernetes.yml manifest
	// creates in keycloakOperatorNamespace.
	keycloakOperatorDeployment = "keycloak-operator"
)

// KeycloakOperatorNamespace is the namespace the Keycloak Operator is installed into AND the
// only namespace it watches (see keycloakOperatorNamespace). The managed-Keycloak e2e runs its
// Platform here so the operator reconciles the rendered Keycloak CR. Exported for the spec.
const KeycloakOperatorNamespace = keycloakOperatorNamespace

// KeycloakOperatorVersion is the pinned Keycloak Operator release the managed-Keycloak e2e
// validates against, exported so the spec/report can name the version the VERIFY(keycloak)
// shapes were confirmed on.
const KeycloakOperatorVersion = keycloakOperatorVersion

// keycloakResourcesURL builds the raw URL for a keycloak-k8s-resources kubernetes/ file at the
// pinned operator version.
func keycloakResourcesURL(file string) string {
	return fmt.Sprintf(keycloakResourcesRawTmpl, keycloakOperatorVersion, file)
}

// keycloakManifestFiles are the keycloak-k8s-resources kubernetes/ files the Keycloak Operator
// install applies, in order: the two CRDs first (so the apiserver serves them), then the
// operator Deployment + RBAC.
var keycloakManifestFiles = []string{
	"keycloaks.k8s.keycloak.org-v1.yml",
	"keycloakrealmimports.k8s.keycloak.org-v1.yml",
	"kubernetes.yml",
}

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// CreateNamespaceIdempotent creates the named namespace in a way that is safe to re-run on a
// dirty cluster: a leftover namespace from a prior run is NOT an error. It pipes
// `kubectl create namespace <name> --dry-run=client -o yaml` into `kubectl apply -f -`, so an
// existing namespace is a no-op apply rather than the `AlreadyExists` failure that a bare
// `kubectl create ns` returns (which previously aborted whole e2e tiers at their BeforeAll).
// This is the single helper every e2e namespace setup should use so the idempotency cannot
// regress per-spec.
func CreateNamespaceIdempotent(name string) error {
	dir, _ := GetProjectDir()
	env := append(os.Environ(), "GO111MODULE=on")

	// `kubectl create ... --dry-run=client -o yaml` renders the Namespace manifest without
	// touching the cluster; `kubectl apply -f -` then creates-or-no-ops it.
	create := exec.Command("kubectl", "create", "namespace", name, //nolint:gosec // test utility; name is a hardcoded test constant
		"--dry-run=client", "-o", "yaml")
	apply := exec.Command("kubectl", "apply", "-f", "-")
	create.Dir, apply.Dir = dir, dir
	create.Env, apply.Env = env, env

	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q | %q\n",
		strings.Join(create.Args, " "), strings.Join(apply.Args, " "))

	pipe, err := create.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to wire create→apply pipe for namespace %q: %w", name, err)
	}
	apply.Stdin = pipe

	var applyOut bytes.Buffer
	apply.Stdout, apply.Stderr = &applyOut, &applyOut

	var createErr bytes.Buffer
	create.Stderr = &createErr

	if err := apply.Start(); err != nil {
		return fmt.Errorf("failed to start apply for namespace %q: %w", name, err)
	}
	if err := create.Run(); err != nil {
		return fmt.Errorf("failed to render namespace %q manifest: %q: %w", name, createErr.String(), err)
	}
	if err := apply.Wait(); err != nil {
		return fmt.Errorf("failed to apply namespace %q: %q: %w", name, applyOut.String(), err)
	}
	return nil
}

// ApplyResource creates a resource idempotently so e2e setup is safe to re-run on a dirty
// cluster: a leftover object from a prior run is NOT an error. It pipes
// `kubectl create <createArgs...> --dry-run=client -o yaml` into `kubectl apply -f -`, so an
// existing resource becomes a no-op apply rather than the `AlreadyExists` failure a bare
// `kubectl create` returns (which previously aborted whole e2e tiers at their BeforeAll).
//
// It is the generic sibling of CreateNamespaceIdempotent: callers pass exactly the arguments
// they would give `kubectl create`, e.g.
//
//	ApplyResource("secret", "generic", name, "-n", ns, "--from-literal=k=v")
//	ApplyResource("configmap", name, "-n", ns, "--from-literal=k=v")
//	ApplyResource("clusterrolebinding", name, "--clusterrole=r", "--serviceaccount=ns:sa")
//
// All e2e resource setup should route through this (or CreateNamespaceIdempotent) so the
// idempotency cannot regress per-spec.
func ApplyResource(createArgs ...string) error {
	dir, _ := GetProjectDir()
	env := append(os.Environ(), "GO111MODULE=on")

	// `kubectl create ... --dry-run=client -o yaml` renders the manifest without touching the
	// cluster; `kubectl apply -f -` then creates-or-no-ops it.
	args := append([]string{"create"}, createArgs...)
	args = append(args, "--dry-run=client", "-o", "yaml")
	create := exec.Command("kubectl", args...) //nolint:gosec // test utility; args are hardcoded test constants
	apply := exec.Command("kubectl", "apply", "-f", "-")
	create.Dir, apply.Dir = dir, dir
	create.Env, apply.Env = env, env

	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q | %q\n",
		strings.Join(create.Args, " "), strings.Join(apply.Args, " "))

	pipe, err := create.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to wire create→apply pipe for %q: %w", strings.Join(createArgs, " "), err)
	}
	apply.Stdin = pipe

	var applyOut bytes.Buffer
	apply.Stdout, apply.Stderr = &applyOut, &applyOut

	var createErr bytes.Buffer
	create.Stderr = &createErr

	if err := apply.Start(); err != nil {
		return fmt.Errorf("failed to start apply for %q: %w", strings.Join(createArgs, " "), err)
	}
	if err := create.Run(); err != nil {
		return fmt.Errorf("failed to render %q manifest: %q: %w",
			strings.Join(createArgs, " "), createErr.String(), err)
	}
	if err := apply.Wait(); err != nil {
		return fmt.Errorf("failed to apply %q: %q: %w",
			strings.Join(createArgs, " "), applyOut.String(), err)
	}
	return nil
}

// DeleteNamespace deletes the named namespace best-effort and idempotently: it passes
// `--ignore-not-found` (so a re-run on a cluster where teardown already removed the namespace
// is a no-op) and only warns on any other error, so suite/spec teardown never fails the run.
func DeleteNamespace(name string) {
	cmd := exec.Command("kubectl", "delete", "ns", name, "--ignore-not-found") //nolint:gosec // test utility; name is a hardcoded test constant
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// WaitForWorkloadsDrained blocks (up to 5 minutes) until every pod in ns whose name does not
// start with one of keepPrefixes has terminated. It exists because the Keycloak Operator is
// namespace-scoped: the managed-Keycloak and FULL managed e2e blocks both run their entire
// Keycloak (+ CloudNativePG + the ILM app) stack in the single namespace the operator watches,
// so — unlike the managed-database / managed-messaging blocks, which own a throwaway namespace
// and reclaim it with DeleteNamespace — they can neither isolate by namespace nor delete it (it
// holds the operator). Deleting a block's Platform + managed CRs only triggers ASYNCHRONOUS pod
// garbage collection, so without this barrier the next block's stack starts while the previous
// block's StatefulSet pods are still terminating; the two full stacks together exhaust a single
// Kind node, whose containerd then OOMs and fails every subsequent pod-sandbox creation. Callers
// invoke it after deleting a block's workloads (passing the operator's own Deployment name as a
// keepPrefix) so node resources are reclaimed before the next stack is brought up. It is
// best-effort: on a List error it retries, and on timeout it warns rather than failing teardown.
func WaitForWorkloadsDrained(ns string, keepPrefixes ...string) {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		remaining, err := RemainingWorkloadPods(ns, keepPrefixes...)
		if err == nil {
			if len(remaining) == 0 {
				return
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "waiting for %d workload pod(s) to drain from %q: %v\n",
				len(remaining), ns, remaining)
		}
		if time.Now().After(deadline) {
			warnError(fmt.Errorf("timed out waiting for workload pods to drain from namespace %q", ns))
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// RemainingWorkloadPods returns the names of the pods in ns whose name does not start with one
// of keepPrefixes — the pods still holding node resources. It is the single listing both drain
// paths share: WaitForWorkloadsDrained polls it best-effort (warning on timeout), while a spec
// that must NOT bring up its own stack until the node is free polls it inside an Eventually so
// the drain becomes a HARD barrier. The error is returned rather than swallowed so a caller can
// tell an unreadable API apart from a genuinely drained namespace.
func RemainingWorkloadPods(ns string, keepPrefixes ...string) ([]string, error) {
	out, err := Run(exec.Command("kubectl", "get", "pods", "-n", ns, //nolint:gosec // test utility; ns is a hardcoded test constant
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}"))
	if err != nil {
		return nil, err
	}
	var remaining []string
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || hasAnyPrefix(name, keepPrefixes) {
			continue
		}
		remaining = append(remaining, name)
	}
	return remaining, nil
}

// hasAnyPrefix reports whether s starts with any of the given prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// InstallPrometheusOperator installs the prometheus Operator to be used to export the enabled
// metrics. It applies (not creates) the bundle so a re-run on a reused cluster updates-or-no-ops
// the objects rather than failing AlreadyExists, mirroring the other Install* helpers.
func InstallPrometheusOperator() error {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	cmd := exec.Command("kubectl", "apply", "--server-side", "--force-conflicts", "-f", url) //nolint:gosec // test utility with trusted input
	_, err := Run(cmd)
	return err
}

// UninstallPrometheusOperator uninstalls the prometheus
func UninstallPrometheusOperator() {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url) //nolint:gosec // test utility with trusted input
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// IsPrometheusCRDsInstalled checks if any Prometheus CRDs are installed
// by verifying the existence of key CRDs related to Prometheus.
func IsPrometheusCRDsInstalled() bool {
	// List of common Prometheus CRDs
	prometheusCRDs := []string{
		"prometheuses.monitoring.coreos.com",
		"prometheusrules.monitoring.coreos.com",
		"prometheusagents.monitoring.coreos.com",
	}

	cmd := exec.Command("kubectl", "get", "crds", "-o", "custom-columns=NAME:.metadata.name")
	output, err := Run(cmd)
	if err != nil {
		return false
	}
	crdList := GetNonEmptyLines(output)
	for _, crd := range prometheusCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url) //nolint:gosec // test utility with trusted input
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url) //nolint:gosec // test utility with trusted input
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	_, err := Run(cmd)
	return err
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// UninstallCloudNativePG uninstalls the CloudNativePG operator installed by
// InstallCloudNativePG. Errors are warned (not failed) so suite teardown is best-effort,
// mirroring UninstallCertManager.
func UninstallCloudNativePG() {
	url := fmt.Sprintf(cloudNativePGURLTmpl, cloudNativePGVersion)
	// NOTE: `kubectl delete` does NOT accept --server-side (that flag is apply-only); passing
	// it made this teardown error out and skip the delete. A plain client-side delete removes
	// the objects regardless of how the install applied them (server-side apply only affects
	// field-management on CREATE/UPDATE, not DELETE).
	cmd := exec.Command("kubectl", "delete", "--ignore-not-found", "-f", url) //nolint:gosec // test utility with trusted, pinned input
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// InstallCloudNativePG installs the pinned CloudNativePG operator (it serves the
// postgresql.cnpg.io CRDs the operator's managed-database mode renders) and waits for it
// to be usable: the controller Deployment Available and the Cluster/Pooler CRDs
// Established. It mirrors InstallCertManager and is idempotent + CI-friendly:
//
//   - The install manifest is applied with --server-side --force-conflicts. CNPG's CRDs
//     carry very large schemas whose last-applied-configuration annotation exceeds the
//     client-side apply size limit, so server-side apply is required (and is what the
//     CNPG docs recommend); --force-conflicts lets a re-install on a dirty cluster take
//     ownership of fields a previous apply set.
//   - It then waits for the postgresql.cnpg.io CRDs to be Established (so the apiserver
//     serves them) and for the CNPG controller Deployment to be Available, so a Platform
//     applied immediately after finds both the CRDs and a running controller.
func InstallCloudNativePG() error {
	url := fmt.Sprintf(cloudNativePGURLTmpl, cloudNativePGVersion)
	cmd := exec.Command("kubectl", "apply", "--server-side", "--force-conflicts", "-f", url) //nolint:gosec // test utility with trusted, pinned input
	if _, err := Run(cmd); err != nil {
		return err
	}

	// Wait for the CRDs to be Established before the Deployment: a Platform applied right
	// after this helper returns immediately renders a Cluster, which needs the CRD served.
	for _, crd := range []string{"clusters.postgresql.cnpg.io", "poolers.postgresql.cnpg.io"} {
		cmd = exec.Command("kubectl", "wait", "--for=condition=Established", //nolint:gosec // test utility; crd is a hardcoded constant from the loop above
			"crd/"+crd, "--timeout=2m")
		if _, err := Run(cmd); err != nil {
			return err
		}
	}

	// Wait for the CNPG controller Deployment to be Available so it is ready to reconcile
	// the Cluster the operator applies.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/"+cloudNativePGDeployment,
		"--for", "condition=Available",
		"--namespace", cloudNativePGNamespace,
		"--timeout", "5m",
	)
	_, err := Run(cmd)
	return err
}

// IsCloudNativePGCRDsInstalled reports whether the CloudNativePG CRDs are already present
// on the cluster, so the suite can skip (re)installing the operator (mirrors
// IsCertManagerCRDsInstalled). It keys off the Cluster + Pooler CRDs the managed-database
// mode depends on.
func IsCloudNativePGCRDsInstalled() bool {
	cnpgCRDs := []string{
		"clusters.postgresql.cnpg.io",
		"poolers.postgresql.cnpg.io",
	}
	cmd := exec.Command("kubectl", "get", "crds", "-o", "custom-columns=NAME:.metadata.name")
	output, err := Run(cmd)
	if err != nil {
		return false
	}
	crdList := GetNonEmptyLines(output)
	for _, crd := range cnpgCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}
	return false
}

// UninstallRabbitMQClusterOperator uninstalls the RabbitMQ Cluster Operator installed by
// InstallRabbitMQClusterOperator. Errors are warned (not failed) so suite teardown is
// best-effort, mirroring UninstallCloudNativePG.
func UninstallRabbitMQClusterOperator() {
	url := fmt.Sprintf(rabbitmqClusterOperatorURLTmpl, rabbitmqClusterOperatorVersion)
	cmd := exec.Command("kubectl", "delete", "--ignore-not-found", "-f", url) //nolint:gosec // test utility with trusted, pinned input
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// InstallRabbitMQClusterOperator installs the pinned RabbitMQ Cluster Operator (it serves
// the rabbitmqclusters.rabbitmq.com CRD the operator's managed-messaging mode renders) and
// waits for it to be usable: the rabbitmqclusters CRD Established and the controller
// Deployment Available. It mirrors InstallCloudNativePG and is idempotent + CI-friendly.
//
// The Cluster Operator's RabbitMQ operand image (the rabbitmq broker) is PUBLIC, so the
// broker pods actually start on a stock Kind node — which is what lets the e2e validate the
// rendered RabbitmqCluster (and the Topology Operator's vhost/users/etc.) end-to-end.
func InstallRabbitMQClusterOperator() error {
	url := fmt.Sprintf(rabbitmqClusterOperatorURLTmpl, rabbitmqClusterOperatorVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url) //nolint:gosec // test utility with trusted, pinned input
	if _, err := Run(cmd); err != nil {
		return err
	}

	// Wait for the CRD to be Established before the Deployment: a Platform applied right
	// after this helper returns immediately renders a RabbitmqCluster, which needs the CRD.
	cmd = exec.Command("kubectl", "wait", "--for=condition=Established",
		"crd/rabbitmqclusters.rabbitmq.com", "--timeout=2m")
	if _, err := Run(cmd); err != nil {
		return err
	}

	// Wait for the Cluster Operator controller Deployment to be Available so it is ready to
	// reconcile the RabbitmqCluster the operator applies.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/"+rabbitmqClusterOperatorDeployment,
		"--for", "condition=Available",
		"--namespace", rabbitmqClusterOperatorNamespace,
		"--timeout", "5m",
	)
	_, err := Run(cmd)
	return err
}

// IsRabbitMQClusterOperatorCRDsInstalled reports whether the RabbitMQ Cluster Operator CRD
// is already present on the cluster, so the suite can skip (re)installing it (mirrors
// IsCloudNativePGCRDsInstalled). It keys off the rabbitmqclusters CRD the managed-messaging
// mode depends on.
func IsRabbitMQClusterOperatorCRDsInstalled() bool {
	return crdsPresent([]string{"rabbitmqclusters.rabbitmq.com"})
}

// UninstallRabbitMQTopologyOperator uninstalls the Messaging Topology Operator installed by
// InstallRabbitMQTopologyOperator. Errors are warned (not failed) so suite teardown is
// best-effort.
func UninstallRabbitMQTopologyOperator() {
	url := fmt.Sprintf(rabbitmqTopologyOperatorURLTmpl, rabbitmqTopologyOperatorVersion)
	// Free any retained Messaging-Topology CRs first: deleting the operator together with its CRDs in one
	// `kubectl delete -f` would otherwise deadlock on finalizers only the (being-deleted) operator could
	// clear, blocking this delete until the Ginkgo suite timeout (~85m). Stripping them first lets the
	// CRD deletion complete promptly AND fully, so a later block can cleanly re-install the operator.
	stripRabbitMQTopologyFinalizers()
	cmd := exec.Command("kubectl", "delete", "--ignore-not-found", "-f", url) //nolint:gosec // test utility with trusted, pinned input
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// stripRabbitMQTopologyFinalizers force-removes finalizers from any retained Messaging Topology CRs
// (queues/exchanges/bindings/users/permissions/vhosts/policies/...). With deletionPolicy Retain the
// managed broker's topology objects are intentionally kept; deleting the Topology Operator and its CRDs
// together leaves no controller to run the deletion.finalizers.*.rabbitmq.com finalizers, which hangs the
// delete. The Kind cluster is torn down right after the suite, so force-clearing here is safe and
// best-effort (a kind whose CRD is absent, or a transient error, is skipped).
func stripRabbitMQTopologyFinalizers() {
	kinds := []string{
		"bindings", "exchanges", "federations", "operatorpolicies", "permissions",
		"policies", "queues", "schemareplications", "shovels", "superstreams",
		"topicpermissions", "users", "vhosts",
	}
	for _, kind := range kinds {
		res := kind + ".rabbitmq.com"
		out, err := Run(exec.Command("kubectl", "get", res, "--all-namespaces", //nolint:gosec // test utility; kind is a hardcoded constant
			"-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}{\"\\n\"}{end}"))
		if err != nil {
			continue // the CRD may not be installed for this operator version
		}
		for _, ref := range strings.Fields(out) {
			ns, name, ok := strings.Cut(ref, "/")
			if !ok {
				continue
			}
			_, _ = Run(exec.Command("kubectl", "patch", res, name, "-n", ns, //nolint:gosec // test utility; identifiers come from the cluster
				"--type=merge", "-p", `{"metadata":{"finalizers":null}}`))
		}
	}
}

// InstallRabbitMQTopologyOperator installs the pinned Messaging Topology Operator (the
// cert-manager-backed manifest variant — the suite already installs cert-manager) and waits
// for it to be usable: the topology CRDs (vhosts/users/permissions/queues/exchanges/
// bindings) Established and the controller Deployment Available. The Topology Operator
// reconciles the operator's Vhost/User/Permission/Exchange/Queue/Binding CRs against the
// running broker and GENERATES each User's <user>-user-credentials Secret.
//
// PREREQUISITE: the RabbitMQ Cluster Operator AND cert-manager must already be installed
// (the topology CRs reference a RabbitmqCluster, and this manifest's serving cert is issued
// by cert-manager). The suite installs both before this helper runs.
func InstallRabbitMQTopologyOperator() error {
	url := fmt.Sprintf(rabbitmqTopologyOperatorURLTmpl, rabbitmqTopologyOperatorVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url) //nolint:gosec // test utility with trusted, pinned input
	if _, err := Run(cmd); err != nil {
		return err
	}

	// Wait for every topology CRD the operator renders to be Established before the
	// Deployment, so a Platform applied immediately after finds them served.
	for _, crd := range []string{
		"vhosts.rabbitmq.com",
		"users.rabbitmq.com",
		"permissions.rabbitmq.com",
		"exchanges.rabbitmq.com",
		"queues.rabbitmq.com",
		"bindings.rabbitmq.com",
	} {
		cmd = exec.Command("kubectl", "wait", "--for=condition=Established", //nolint:gosec // test utility; crd is a hardcoded constant from the loop above
			"crd/"+crd, "--timeout=2m")
		if _, err := Run(cmd); err != nil {
			return err
		}
	}

	// Wait for the Topology Operator controller Deployment to be Available.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/"+rabbitmqTopologyOperatorDeployment,
		"--for", "condition=Available",
		"--namespace", rabbitmqTopologyOperatorNamespace,
		"--timeout", "5m",
	)
	_, err := Run(cmd)
	return err
}

// IsRabbitMQTopologyOperatorCRDsInstalled reports whether the Messaging Topology Operator
// CRDs are already present on the cluster, so the suite can skip (re)installing it. It keys
// off the full set of topology CRDs the managed-messaging mode renders.
func IsRabbitMQTopologyOperatorCRDsInstalled() bool {
	return crdsPresent([]string{
		"vhosts.rabbitmq.com",
		"users.rabbitmq.com",
		"permissions.rabbitmq.com",
		"exchanges.rabbitmq.com",
		"queues.rabbitmq.com",
		"bindings.rabbitmq.com",
	})
}

// UninstallKeycloakOperator uninstalls the Keycloak Operator installed by
// InstallKeycloakOperator (the CRDs + the operator Deployment/RBAC). Errors are warned (not
// failed) so suite teardown is best-effort, mirroring UninstallCloudNativePG. The operator
// manifest is applied with -n keycloak, so it is deleted the same way; the (cluster-scoped)
// CRDs ignore the namespace flag.
func UninstallKeycloakOperator() {
	for i := len(keycloakManifestFiles) - 1; i >= 0; i-- {
		url := keycloakResourcesURL(keycloakManifestFiles[i])
		cmd := exec.Command("kubectl", "delete", "-n", keycloakOperatorNamespace, //nolint:gosec // test utility with trusted, pinned input
			"--ignore-not-found", "-f", url)
		if _, err := Run(cmd); err != nil {
			warnError(err)
		}
	}
}

// InstallKeycloakOperator installs the pinned Keycloak Operator (it serves the k8s.keycloak.org
// CRDs — Keycloak + KeycloakRealmImport — the operator's managed-Keycloak mode renders) and
// waits for it to be usable: the two CRDs Established and the controller Deployment Available.
// It mirrors InstallCloudNativePG / InstallRabbitMQ* and is idempotent + CI-friendly.
//
// NAMESPACE-SCOPED: unlike CNPG/RabbitMQ (which ship their own namespace and watch
// cluster-wide), the Keycloak Operator watches ONLY the namespace it runs in, and its
// kubernetes.yml ClusterRoleBinding subject hard-codes the ServiceAccount in namespace
// "keycloak". So this helper creates that namespace and applies the operator manifest into it
// with -n; the managed-Keycloak Platform (carrying the Keycloak CR) must run in the SAME
// namespace, or the operator never reconciles the CR. The CRDs are cluster-scoped (the -n flag
// is ignored for them).
//
// The Keycloak operand image (quay.io/keycloak/keycloak) is PUBLIC, so the Keycloak pod starts
// on a stock Kind node — which (with a working database) is what lets the e2e validate the
// rendered Keycloak CR + KeycloakRealmImport end-to-end.
func InstallKeycloakOperator() error {
	// Ensure the operator's hard-coded namespace exists (idempotent: a leftover namespace from
	// a prior run is a no-op, not an AlreadyExists failure).
	if err := CreateNamespaceIdempotent(keycloakOperatorNamespace); err != nil {
		return err
	}

	// Apply the CRDs first, then the operator Deployment + RBAC, all into the operator
	// namespace (the manifest's namespaced RoleBindings have no namespace of their own, so -n
	// places them in the operator namespace alongside the hard-coded ClusterRoleBinding
	// subject; the cluster-scoped CRDs ignore -n).
	for _, file := range keycloakManifestFiles {
		url := keycloakResourcesURL(file)
		cmd := exec.Command("kubectl", "apply", "-n", keycloakOperatorNamespace, "-f", url) //nolint:gosec // test utility with trusted, pinned input
		if _, err := Run(cmd); err != nil {
			return err
		}
	}

	// Wait for both CRDs to be Established before the Deployment: a Platform applied right
	// after this helper returns immediately renders a Keycloak CR (and, when a realm import is
	// configured, a KeycloakRealmImport), which need the CRDs served.
	for _, crd := range []string{"keycloaks.k8s.keycloak.org", "keycloakrealmimports.k8s.keycloak.org"} {
		cmd := exec.Command("kubectl", "wait", "--for=condition=Established", //nolint:gosec // test utility; crd is a hardcoded constant from the loop above
			"crd/"+crd, "--timeout=2m")
		if _, err := Run(cmd); err != nil {
			return err
		}
	}

	// Wait for the Keycloak Operator controller Deployment to be Available so it is ready to
	// reconcile the Keycloak CR the operator applies.
	cmd := exec.Command("kubectl", "wait", "deployment.apps/"+keycloakOperatorDeployment,
		"--for", "condition=Available",
		"--namespace", keycloakOperatorNamespace,
		"--timeout", "5m",
	)
	_, err := Run(cmd)
	return err
}

// IsKeycloakOperatorCRDsInstalled reports whether the Keycloak Operator CRDs are already
// present on the cluster, so the suite can skip (re)installing it (mirrors
// IsCloudNativePGCRDsInstalled). It keys off both CRDs the managed-Keycloak mode depends on.
func IsKeycloakOperatorCRDsInstalled() bool {
	return crdsPresent([]string{
		"keycloaks.k8s.keycloak.org",
		"keycloakrealmimports.k8s.keycloak.org",
	})
}

// crdsPresent reports whether ALL of the named CRDs are present on the cluster. It is the
// shared idempotency probe behind the Is...CRDsInstalled helpers: a managed-infra install
// helper skips its (re)install only when every CRD it depends on is already served.
func crdsPresent(want []string) bool {
	cmd := exec.Command("kubectl", "get", "crds", "-o", "custom-columns=NAME:.metadata.name")
	output, err := Run(cmd)
	if err != nil {
		return false
	}
	have := make(map[string]struct{})
	for _, line := range GetNonEmptyLines(output) {
		have[strings.TrimSpace(line)] = struct{}{}
	}
	for _, crd := range want {
		if _, ok := have[crd]; !ok {
			return false
		}
	}
	return true
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string) error {
	cluster := "kind"
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	cmd := exec.Command(kindBinary(), kindOptions...) //nolint:gosec // test utility with trusted input
	_, err := Run(cmd)
	return err
}

// kindBinary resolves the kind executable the e2e suite should invoke, keeping the
// suite aligned with the Makefile (which manages a pinned kind under bin/). It prefers
// an explicit KIND override (set by the test-e2e target to the Makefile's $(KIND)),
// then the project-local bin/kind, and finally a bare "kind" on PATH. Without this the
// suite shells out to a bare "kind" that is absent when the only kind is the
// Makefile-managed bin/kind, failing the BeforeSuite image load.
func kindBinary() string {
	if v := os.Getenv("KIND"); v != "" {
		return v
	}
	if dir, err := GetProjectDir(); err == nil {
		local := filepath.Join(dir, "bin", "kind")
		if info, statErr := os.Stat(local); statErr == nil && !info.IsDir() {
			return local
		}
	}
	return "kind"
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}

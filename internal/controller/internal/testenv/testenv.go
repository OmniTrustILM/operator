/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

// Package testenv provides a reusable envtest bootstrap shared by the per-kind
// controller test suites (Connector, Platform, ...). It resolves the project
// root from this file's own location so the helper works regardless of the
// calling package's depth in the tree.
package testenv

import (
	"os"
	"path/filepath"
	"runtime"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// Start boots an envtest API server with the project CRDs and registers the
// otilm.com and Prometheus monitoring types onto scheme.Scheme. Returns the
// rest.Config and the Environment (call Stop() in AfterSuite).
func Start() (*rest.Config, *envtest.Environment, error) {
	if err := otilmv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return nil, nil, err
	}
	if err := monitoringv1.AddToScheme(scheme.Scheme); err != nil {
		return nil, nil, err
	}
	root := repoRoot()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if bin := firstEnvTestBinaryDir(root); bin != "" {
		env.BinaryAssetsDirectory = bin
	}
	// Widen the API server's Service ClusterIP range well beyond the 10.0.0.0/24
	// (254-IP) default. Each Platform a spec creates renders ~8 Services, and the
	// suites deliberately keep every created Platform alive (no per-spec teardown;
	// isolation is by namespace), so a full run allocates well over 254 ClusterIPs.
	// On the default range that exhausts mid-suite and surfaces as random,
	// order-dependent "failed to allocate a serviceIP: range is full" reconcile
	// errors — whichever specs happen to need a NEW Service after exhaustion fail.
	// A /16 (65 534 IPs) removes the ceiling for any realistic suite size.
	env.ControlPlane.GetAPIServer().Configure().Set("service-cluster-ip-range", "10.0.0.0/16")
	cfg, err := env.Start()
	if err != nil {
		return nil, nil, err
	}
	return cfg, env, nil
}

// repoRoot derives the module root from this file's location
// (internal/controller/internal/testenv/testenv.go → up 4).
func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..")
}

// firstEnvTestBinaryDir locates the first envtest binary directory under
// bin/k8s, mirroring the behaviour of the KUBEBUILDER_ASSETS env var so tests
// can run from an IDE after `make setup-envtest`. Returns "" when none exists.
func firstEnvTestBinaryDir(root string) string {
	base := filepath.Join(root, "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

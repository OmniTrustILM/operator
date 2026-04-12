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

// Package main is the entrypoint for the ILM Connector Operator manager.
package main

import (
	"crypto/tls"
	"flag"
	"os"
	"path/filepath"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	otilmcomv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/controller"

	// Import monitoring package for Prometheus metrics registration side effects.
	_ "github.com/OmniTrustILM/operator/internal/monitoring"
	"github.com/OmniTrustILM/operator/internal/version"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(otilmcomv1alpha1.AddToScheme(scheme))
	utilruntime.Must(monitoringv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// managerConfig holds parsed command-line flags.
type managerConfig struct {
	metricsAddr          string
	metricsCertPath      string
	metricsCertName      string
	metricsCertKey       string
	webhookCertPath      string
	webhookCertName      string
	webhookCertKey       string
	enableLeaderElection bool
	probeAddr            string
	secureMetrics        bool
	enableHTTP2          bool
}

// parseFlags parses command-line flags and returns the configuration.
func parseFlags() managerConfig {
	var cfg managerConfig
	flag.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&cfg.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&cfg.secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&cfg.webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&cfg.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&cfg.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&cfg.metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&cfg.metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&cfg.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&cfg.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	return cfg
}

// buildTLSOpts returns the base TLS options, disabling HTTP/2 when not enabled.
func buildTLSOpts(enableHTTP2 bool) []func(*tls.Config) {
	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	if enableHTTP2 {
		return nil
	}
	return []func(*tls.Config){
		func(c *tls.Config) {
			setupLog.Info("disabling http/2")
			c.NextProtos = []string{"http/1.1"}
		},
	}
}

// setupCertWatcher creates a certificate watcher for the given paths.
// Returns nil if certPath is empty.
func setupCertWatcher(certPath, certName, certKey, description string) (*certwatcher.CertWatcher, error) {
	if len(certPath) == 0 {
		return nil, nil
	}
	setupLog.Info("Initializing "+description+" certificate watcher using provided certificates",
		description+"-cert-path", certPath, description+"-cert-name", certName, description+"-cert-key", certKey)

	watcher, err := certwatcher.New(
		filepath.Join(certPath, certName),
		filepath.Join(certPath, certKey),
	)
	if err != nil {
		setupLog.Error(err, "Failed to initialize "+description+" certificate watcher")
		return nil, err
	}
	return watcher, nil
}

// setupWebhookServer creates the webhook server with TLS configuration.
func setupWebhookServer(tlsOpts []func(*tls.Config), watcher *certwatcher.CertWatcher) webhook.Server {
	webhookTLSOpts := tlsOpts
	if watcher != nil {
		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = watcher.GetCertificate
		})
	}
	return webhook.NewServer(webhook.Options{TLSOpts: webhookTLSOpts})
}

// setupMetricsOptions builds metrics server options from configuration.
func setupMetricsOptions(cfg managerConfig, tlsOpts []func(*tls.Config), watcher *certwatcher.CertWatcher) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   cfg.metricsAddr,
		SecureServing: cfg.secureMetrics,
		TLSOpts:       tlsOpts,
	}
	if cfg.secureMetrics {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if watcher != nil {
		opts.TLSOpts = append(opts.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = watcher.GetCertificate
		})
	}
	return opts
}

// addCertWatcherToManager adds a non-nil cert watcher to the manager.
func addCertWatcherToManager(mgr ctrl.Manager, watcher *certwatcher.CertWatcher, description string) {
	if watcher == nil {
		return
	}
	setupLog.Info("Adding " + description + " certificate watcher to manager")
	if err := mgr.Add(watcher); err != nil {
		setupLog.Error(err, "unable to add "+description+" certificate watcher to manager")
		os.Exit(1)
	}
}

func main() {
	cfg := parseFlags()
	setupLog.Info("starting ilm-operator", "version", version.Version, "commit", version.GitCommit)

	tlsOpts := buildTLSOpts(cfg.enableHTTP2)

	webhookCertWatcher, err := setupCertWatcher(cfg.webhookCertPath, cfg.webhookCertName, cfg.webhookCertKey, "webhook")
	if err != nil {
		os.Exit(1)
	}

	metricsCertWatcher, err := setupCertWatcher(cfg.metricsCertPath, cfg.metricsCertName, cfg.metricsCertKey, "metrics")
	if err != nil {
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                setupMetricsOptions(cfg, tlsOpts, metricsCertWatcher),
		WebhookServer:          setupWebhookServer(tlsOpts, webhookCertWatcher),
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.enableLeaderElection,
		LeaderElectionID:       "af2ba91e.otilm.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.ConnectorReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("ilm-operator"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Connector")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	addCertWatcherToManager(mgr, metricsCertWatcher, "metrics")
	addCertWatcherToManager(mgr, webhookCertWatcher, "webhook")

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

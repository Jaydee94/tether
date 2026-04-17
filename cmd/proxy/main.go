package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Jaydee94/tether/pkg/audit"
	"github.com/Jaydee94/tether/pkg/proxy"
)

func main() {
	var (
		listenAddr         string
		metricsAddr        string
		targetAddr         string
		upstreamKubeconfig string
		auditDir           string
		auditMaxFileSize   int64
		tlsSkipVerify      bool
		tlsCertFile        string
		tlsKeyFile         string
		tokenNamespace     string
		devToken           string
		devSessionID       string
		devMode            bool
	)

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.StringVar(&listenAddr, "listen", ":8443", "Address to listen on.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":9090", "Address to serve Prometheus metrics on.")
	flag.StringVar(&targetAddr, "target", "https://kubernetes.default.svc", "Kubernetes API server URL.")
	flag.StringVar(&upstreamKubeconfig, "upstream-kubeconfig", defaultKubeconfig(), "Path to kubeconfig used for upstream API auth.")
	flag.StringVar(&auditDir, "audit-dir", "/var/tether/audit", "Directory for local audit recordings.")
	flag.Int64Var(&auditMaxFileSize, "audit-max-file-size", 0, "Maximum size in bytes for a single audit .cast file before rotation (0 = disabled).")
	flag.BoolVar(&tlsSkipVerify, "tls-skip-verify", false, "Skip TLS verification of the upstream API server (dev only).")
	flag.StringVar(&tlsCertFile, "tls-cert", "", "Path to TLS certificate file for the proxy listener.")
	flag.StringVar(&tlsKeyFile, "tls-key", "", "Path to TLS key file for the proxy listener.")
	flag.StringVar(&tokenNamespace, "token-namespace", "tether-system", "Namespace where session-token Secrets are stored.")
	// Dev-mode flags: if both are set, fall back to a static single-token validator (useful in CI/testing).
	flag.StringVar(&devToken, "dev-token", os.Getenv("TETHER_TOKEN"), "Dev-only static token (overrides k8s-backed validation).")
	flag.StringVar(&devSessionID, "dev-session-id", os.Getenv("TETHER_SESSION_ID"), "Session ID paired with --dev-token.")
	flag.BoolVar(&devMode, "dev-mode", false, "Disable TLS on the proxy listener (development only; never use in production).")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("proxy")

	backend, err := audit.NewLocalBackendWithConfig(auditDir, audit.LocalBackendConfig{
		MaxFileSizeBytes: auditMaxFileSize,
	})
	if err != nil {
		log.Error(err, "Failed to create audit backend")
		os.Exit(1)
	}

	upstreamTransport, err := buildUpstreamTransport(targetAddr, upstreamKubeconfig, tlsSkipVerify)
	if err != nil {
		log.Error(err, "Failed to configure upstream API authentication")
		os.Exit(1)
	}

	var validator proxy.SessionValidator
	if devToken != "" && devSessionID != "" {
		// Dev / CI mode: bypass k8s Secret lookup.
		log.Info("Using static dev token validator (dev-only mode)", "sessionID", devSessionID)
		validator = proxy.NewStaticValidator(map[string]string{devToken: devSessionID})
	} else {
		// Production mode: validate tokens against k8s Secrets and live TetherLease status.
		restCfg, cfgErr := clientcmd.BuildConfigFromFlags("", upstreamKubeconfig)
		if cfgErr != nil {
			log.Error(cfgErr, "Failed to load kubeconfig for token validator")
			os.Exit(1)
		}
		kubeClient, k8sErr := kubernetes.NewForConfig(restCfg)
		if k8sErr != nil {
			log.Error(k8sErr, "Failed to create Kubernetes client for token validator")
			os.Exit(1)
		}
		dynClient, dynErr := dynamic.NewForConfig(restCfg)
		if dynErr != nil {
			log.Error(dynErr, "Failed to create dynamic client for token validator")
			os.Exit(1)
		}
		log.Info("Using Kubernetes-backed lease validator", "tokenNamespace", tokenNamespace)
		validator = proxy.NewKubernetesLeaseValidator(kubeClient, dynClient, tokenNamespace)
	}

	p, err := proxy.NewTetherProxy(targetAddr, backend, validator, tlsSkipVerify, upstreamTransport)
	if err != nil {
		log.Error(err, "Failed to create proxy")
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           p.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}

	ctx := ctrl.SetupSignalHandler()

	// Start metrics server with health checks
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	metricsMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	metricsSrv := &http.Server{
		Addr:              metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("Metrics server starting", "addr", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error(err, "Metrics server error")
		}
	}()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutCtx)
		_ = srv.Shutdown(shutCtx)
	}()

	// Enforce TLS in production mode.
	if !devMode && (tlsCertFile == "" || tlsKeyFile == "") {
		fmt.Fprintln(os.Stderr, "TLS cert and key are required in production mode. Use --dev-mode to disable TLS (development only).")
		os.Exit(1)
	}

	log.Info("Tether proxy starting", "listen", listenAddr, "target", targetAddr, "devMode", devMode)
	var srvErr error
	if tlsCertFile != "" && tlsKeyFile != "" {
		srvErr = srv.ListenAndServeTLS(tlsCertFile, tlsKeyFile)
	} else {
		log.Info("TLS cert/key not provided, starting HTTP (--dev-mode enabled)")
		srvErr = srv.ListenAndServe()
	}
	if srvErr != nil && srvErr != http.ErrServerClosed {
		log.Error(srvErr, "Server error")
		os.Exit(1)
	}
}

func buildUpstreamTransport(targetAddr, kubeconfig string, tlsSkipVerify bool) (http.RoundTripper, error) {
	var (
		cfg          *rest.Config
		loadErr      error
		inClusterErr error
	)

	if kubeconfig != "" {
		cfg, loadErr = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if loadErr != nil || cfg == nil {
		cfg, inClusterErr = rest.InClusterConfig()
		if inClusterErr != nil {
			return nil, fmt.Errorf("loading upstream kubernetes config: kubeconfig=%q err=%v, incluster=%w", kubeconfig, loadErr, inClusterErr)
		}
	}

	if targetAddr != "" {
		cfg.Host = targetAddr
	}
	cfg.Insecure = tlsSkipVerify
	if tlsSkipVerify {
		cfg.TLSClientConfig.CAFile = ""
		cfg.TLSClientConfig.CAData = nil
	}

	rt, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("building upstream transport: %w", err)
	}
	return rt, nil
}

func defaultKubeconfig() string {
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		return kc
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube", "config")
}

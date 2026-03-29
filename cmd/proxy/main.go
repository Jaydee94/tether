package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Jaydee94/tether/pkg/audit"
	"github.com/Jaydee94/tether/pkg/proxy"
)

func main() {
	var (
		listenAddr    string
		targetAddr    string
		auditDir      string
		tlsSkipVerify bool
		tlsCertFile   string
		tlsKeyFile    string
	)

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.StringVar(&listenAddr, "listen", ":8443", "Address to listen on.")
	flag.StringVar(&targetAddr, "target", "https://kubernetes.default.svc", "Kubernetes API server URL.")
	flag.StringVar(&auditDir, "audit-dir", "/var/tether/audit", "Directory for local audit recordings.")
	flag.BoolVar(&tlsSkipVerify, "tls-skip-verify", false, "Skip TLS verification of the upstream API server (dev only).")
	flag.StringVar(&tlsCertFile, "tls-cert", "", "Path to TLS certificate file for the proxy listener.")
	flag.StringVar(&tlsKeyFile, "tls-key", "", "Path to TLS key file for the proxy listener.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("proxy")

	backend, err := audit.NewLocalBackend(auditDir)
	if err != nil {
		log.Error(err, "Failed to create audit backend")
		os.Exit(1)
	}

	// In production, replace StaticValidator with a real token store backed by k8s secrets.
	token := os.Getenv("TETHER_TOKEN")
	sessionID := os.Getenv("TETHER_SESSION_ID")
	tokens := map[string]string{}
	if token != "" && sessionID != "" {
		tokens[token] = sessionID
	}
	validator := proxy.NewStaticValidator(tokens)

	p, err := proxy.NewTetherProxy(targetAddr, backend, validator, tlsSkipVerify)
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

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Info("Tether proxy starting", "listen", listenAddr, "target", targetAddr)
	var srvErr error
	if tlsCertFile != "" && tlsKeyFile != "" {
		srvErr = srv.ListenAndServeTLS(tlsCertFile, tlsKeyFile)
	} else {
		log.Info("TLS cert/key not provided, starting HTTP (dev mode)")
		srvErr = srv.ListenAndServe()
	}
	if srvErr != nil && srvErr != http.ErrServerClosed {
		log.Error(srvErr, "Server error")
		os.Exit(1)
	}
}

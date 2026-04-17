package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	tetherv1alpha1 "github.com/Jaydee94/tether/pkg/api/v1alpha1"
	"github.com/Jaydee94/tether/pkg/audit"
)

// ClusterConfig holds configuration for routing to a specific cluster.
type ClusterConfig struct {
	Name      string
	APIServer string
}

// ClusterResolver resolves cluster configurations by name.
type ClusterResolver interface {
	GetCluster(ctx context.Context, name string) (*ClusterConfig, error)
	GetDefaultCluster(ctx context.Context) (*ClusterConfig, error)
}

// MultiClusterProxy routes requests to different Kubernetes clusters based on TetherLease configuration.
type MultiClusterProxy struct {
	controlPlaneClient client.Client
	clusterResolver    ClusterResolver
	backend            audit.Backend
	validator          SessionValidator
	tlsSkipVerify      bool
	upstreamTransport  http.RoundTripper

	mu                 sync.RWMutex
	recorders          map[string]*Recorder
	proxies            map[string]*httputil.ReverseProxy // clusterName -> proxy
	clusterCache       map[string]string                 // sessionID -> clusterName
	clusterCacheExpiry map[string]time.Time              // sessionID -> expiry time
}

// NewMultiClusterProxy creates a proxy that routes to multiple clusters.
func NewMultiClusterProxy(
	controlPlaneClient client.Client,
	clusterResolver ClusterResolver,
	backend audit.Backend,
	validator SessionValidator,
	tlsSkipVerify bool,
	upstreamTransport http.RoundTripper,
) *MultiClusterProxy {
	return &MultiClusterProxy{
		controlPlaneClient: controlPlaneClient,
		clusterResolver:    clusterResolver,
		backend:            backend,
		validator:          validator,
		tlsSkipVerify:      tlsSkipVerify,
		upstreamTransport:  upstreamTransport,
		recorders:          make(map[string]*Recorder),
		proxies:            make(map[string]*httputil.ReverseProxy),
		clusterCache:       make(map[string]string),
		clusterCacheExpiry: make(map[string]time.Time),
	}
}

// StartSessionReaper launches a background goroutine that periodically checks
// all active session recorders against the provided SessionFinalizer.
func (p *MultiClusterProxy) StartSessionReaper(ctx context.Context, finalizer SessionFinalizer, interval time.Duration) {
	if finalizer == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.reapSessions(ctx, finalizer)
			}
		}
	}()
}

// reapSessions checks each active session and finalizes any that are no longer active.
func (p *MultiClusterProxy) reapSessions(ctx context.Context, finalizer SessionFinalizer) {
	p.mu.RLock()
	ids := make([]string, 0, len(p.recorders))
	for id := range p.recorders {
		ids = append(ids, id)
	}
	p.mu.RUnlock()

	for _, id := range ids {
		if !finalizer.IsActive(ctx, id) {
			p.FinishSession(ctx, id)
		}
	}
}

// FinishSession flushes the recorder for sessionID to the backend.
func (p *MultiClusterProxy) FinishSession(ctx context.Context, sessionID string) {
	p.mu.Lock()
	rec, ok := p.recorders[sessionID]
	if ok {
		delete(p.recorders, sessionID)
	}
	p.mu.Unlock()

	if !ok {
		return
	}

	logger := log.FromContext(ctx)
	if err := rec.Finish(ctx); err != nil {
		logger.Error(err, "Failed to flush session recording", "sessionID", sessionID)
	} else {
		logger.Info("Session recording flushed", "sessionID", sessionID)
	}
}

// ServeHTTP implements http.Handler and routes requests to the appropriate cluster.
func (p *MultiClusterProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := log.FromContext(ctx)

	// Extract token
	token := r.Header.Get("X-Tether-Token")
	if token == "" {
		authz := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
			token = strings.TrimSpace(authz[len("Bearer "):])
		}
	}
	if token == "" {
		http.Error(w, "X-Tether-Token or Authorization Bearer token required", http.StatusUnauthorized)
		return
	}

	// Validate token
	sessionID, err := p.validator.Validate(ctx, token)
	if err != nil {
		logger.Info("Token validation failed", "error", err)
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return
	}

	r.Header.Del("X-Tether-Token")
	r.Header.Del("Authorization")

	// Resolve target cluster for this session
	clusterName, err := p.resolveCluster(ctx, sessionID)
	if err != nil {
		logger.Error(err, "Failed to resolve cluster for session", "sessionID", sessionID)
		http.Error(w, "cluster configuration error", http.StatusBadGateway)
		return
	}

	// Get or create the proxy for the target cluster
	proxy, err := p.getProxyForCluster(ctx, clusterName)
	if err != nil {
		logger.Error(err, "Failed to get proxy for cluster", "cluster", clusterName)
		http.Error(w, "cluster not available", http.StatusBadGateway)
		return
	}

	// Serve with recording
	p.serveWithRecording(w, r, sessionID, clusterName, proxy)
}

// resolveCluster determines which cluster this session should route to.
// It caches the result to avoid fetching the lease on every request.
func (p *MultiClusterProxy) resolveCluster(ctx context.Context, sessionID string) (string, error) {
	// Check cache first
	p.mu.RLock()
	if clusterName, ok := p.clusterCache[sessionID]; ok {
		if expiry, hasExpiry := p.clusterCacheExpiry[sessionID]; hasExpiry && time.Now().Before(expiry) {
			p.mu.RUnlock()
			return clusterName, nil
		}
	}
	p.mu.RUnlock()

	// Cache miss or expired - fetch the lease
	lease := &tetherv1alpha1.TetherLease{}
	if err := p.controlPlaneClient.Get(ctx, types.NamespacedName{Name: sessionID}, lease); err != nil {
		return "", fmt.Errorf("fetching lease %q: %w", sessionID, err)
	}

	clusterName := lease.Status.Cluster
	if clusterName == "" {
		clusterName = "local"
	}

	// Cache the result with TTL (10 seconds is reasonable)
	p.mu.Lock()
	p.clusterCache[sessionID] = clusterName
	p.clusterCacheExpiry[sessionID] = time.Now().Add(10 * time.Second)
	p.mu.Unlock()

	return clusterName, nil
}

// getProxyForCluster returns a reverse proxy for the given cluster, creating it if needed.
func (p *MultiClusterProxy) getProxyForCluster(ctx context.Context, clusterName string) (*httputil.ReverseProxy, error) {
	p.mu.RLock()
	if proxy, ok := p.proxies[clusterName]; ok {
		p.mu.RUnlock()
		return proxy, nil
	}
	p.mu.RUnlock()

	// Create new proxy
	clusterCfg, err := p.clusterResolver.GetCluster(ctx, clusterName)
	if err != nil {
		return nil, fmt.Errorf("resolving cluster %q: %w", clusterName, err)
	}

	target, err := url.Parse(clusterCfg.APIServer)
	if err != nil {
		return nil, fmt.Errorf("parsing API server URL %q: %w", clusterCfg.APIServer, err)
	}

	transport := p.upstreamTransport
	if transport == nil {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: p.tlsSkipVerify, //nolint:gosec
			},
		}
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport

	// Cache the proxy
	p.mu.Lock()
	p.proxies[clusterName] = proxy
	p.mu.Unlock()

	return proxy, nil
}

func (p *MultiClusterProxy) serveWithRecording(w http.ResponseWriter, r *http.Request, sessionID, clusterName string, proxy *httputil.ReverseProxy) {
	ctx := r.Context()
	logger := log.FromContext(ctx)

	recorder, err := p.recorderForSession(sessionID, clusterName)
	if err != nil {
		logger.Error(err, "Failed to initialize recorder")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	recorder.RecordOutput([]byte(fmt.Sprintf("\n$ [cluster=%s] %s\n", clusterName, kubectlLikeCommand(r))))

	rw := &recordingResponseWriter{
		ResponseWriter: w,
		recorder:       recorder,
	}

	proxy.ServeHTTP(rw, r)
}

func (p *MultiClusterProxy) recorderForSession(sessionID, clusterName string) (*Recorder, error) {
	// Fast path: check if recorder exists
	p.mu.RLock()
	rec, ok := p.recorders[sessionID]
	p.mu.RUnlock()

	if ok {
		return rec, nil
	}

	// Create new recorder with cluster name in title
	title := fmt.Sprintf("cluster=%s tether/%s kubernetes api session", clusterName, sessionID)
	newRec := NewRecorder(sessionID, p.backend, title)
	if err := newRec.Start(); err != nil {
		return nil, err
	}

	// Store the recorder, check for race condition
	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, ok := p.recorders[sessionID]; ok {
		return existing, nil
	}

	p.recorders[sessionID] = newRec
	return newRec, nil
}

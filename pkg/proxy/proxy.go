package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Jaydee94/tether/pkg/audit"
)

// SessionFinalizer is implemented by validators that can report when a session
// is no longer active (expired or revoked). The proxy uses this to decide when
// to flush and discard a session recorder.
type SessionFinalizer interface {
	// IsActive returns true if the session identified by sessionID is still
	// considered valid. Returning false causes the proxy to call Finish() on
	// the recorder and remove it from the active map.
	IsActive(ctx context.Context, sessionID string) bool
}

// SessionValidator validates X-Tether-Token values.
type SessionValidator interface {
	Validate(ctx context.Context, token string) (sessionID string, err error)
}

// TetherProxy is the reverse proxy that sits in front of the Kubernetes API server.
type TetherProxy struct {
	target    *url.URL
	proxy     *httputil.ReverseProxy
	backend   audit.Backend
	validator SessionValidator

	mu        sync.Mutex
	recorders map[string]*Recorder
}

// FinishSession flushes the recorder for sessionID to the backend and removes
// it from the active session map. It is a no-op if no recorder exists for the
// session. Safe to call concurrently.
func (p *TetherProxy) FinishSession(ctx context.Context, sessionID string) {
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

// StartSessionReaper launches a background goroutine that periodically checks
// all active session recorders against the provided SessionFinalizer. Any
// session that is no longer active (expired or revoked) is flushed and
// discarded. The goroutine exits when ctx is cancelled.
//
// Call this once after constructing TetherProxy when you have a finalizer
// available (e.g. the CachedLeaseValidator). If finalizer is nil the reaper is
// not started.
func (p *TetherProxy) StartSessionReaper(ctx context.Context, finalizer SessionFinalizer, interval time.Duration) {
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

// reapSessions checks each active session and finalises any that are no longer
// active according to finalizer.
func (p *TetherProxy) reapSessions(ctx context.Context, finalizer SessionFinalizer) {
	p.mu.Lock()
	ids := make([]string, 0, len(p.recorders))
	for id := range p.recorders {
		ids = append(ids, id)
	}
	p.mu.Unlock()

	for _, id := range ids {
		if !finalizer.IsActive(ctx, id) {
			p.FinishSession(ctx, id)
		}
	}
}

// NewTetherProxy creates a proxy that forwards to target.
func NewTetherProxy(target string, backend audit.Backend, validator SessionValidator, tlsSkipVerify bool, upstreamTransport http.RoundTripper) (*TetherProxy, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("parsing target URL %q: %w", target, err)
	}

	transport := upstreamTransport
	if transport == nil {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: tlsSkipVerify, //nolint:gosec
			},
		}
	}

	rp := httputil.NewSingleHostReverseProxy(u)
	rp.Transport = transport

	return &TetherProxy{
		target:    u,
		proxy:     rp,
		backend:   backend,
		validator: validator,
		recorders: map[string]*Recorder{},
	}, nil
}

// ServeHTTP implements http.Handler.
func (p *TetherProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := log.FromContext(ctx)
	start := time.Now()

	token := r.Header.Get("X-Tether-Token")
	if token == "" {
		authz := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
			token = strings.TrimSpace(authz[len("Bearer "):])
		}
	}
	if token == "" {
		proxyRequestsTotal.WithLabelValues("401").Inc()
		proxyRequestDuration.Observe(time.Since(start).Seconds())
		http.Error(w, "X-Tether-Token or Authorization Bearer token required", http.StatusUnauthorized)
		return
	}

	sessionID, err := p.validator.Validate(ctx, token)
	if err != nil {
		logger.Info("Token validation failed", "error", err)
		tokenValidationErrors.Inc()
		proxyRequestsTotal.WithLabelValues("401").Inc()
		proxyRequestDuration.Observe(time.Since(start).Seconds())
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return
	}

	r.Header.Del("X-Tether-Token")
	r.Header.Del("Authorization")

	p.serveWithRecording(w, r, sessionID, start)
}

func (p *TetherProxy) serveWithRecording(w http.ResponseWriter, r *http.Request, sessionID string, start time.Time) {
	ctx := r.Context()
	logger := log.FromContext(ctx)

	recorder, err := p.recorderForSession(sessionID)
	if err != nil {
		logger.Error(err, "Failed to initialize recorder")
		proxyRequestsTotal.WithLabelValues("500").Inc()
		proxyRequestDuration.Observe(time.Since(start).Seconds())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	recorder.RecordOutput([]byte("\n$ " + kubectlLikeCommand(r) + "\n"))

	rw := &recordingResponseWriter{
		ResponseWriter: w,
		recorder:       recorder,
		statusCode:     http.StatusOK,
	}

	p.proxy.ServeHTTP(rw, r)

	proxyRequestsTotal.WithLabelValues(strconv.Itoa(rw.statusCode)).Inc()
	proxyRequestDuration.Observe(time.Since(start).Seconds())
}

func (p *TetherProxy) recorderForSession(sessionID string) (*Recorder, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if rec, ok := p.recorders[sessionID]; ok {
		return rec, nil
	}

	rec := NewRecorder(sessionID, p.backend, fmt.Sprintf("tether/%s kubernetes api session", sessionID))
	if err := rec.Start(); err != nil {
		return nil, err
	}
	p.recorders[sessionID] = rec
	return rec, nil
}

func kubectlLikeCommand(r *http.Request) string {
	path := r.URL.Path
	parts := strings.Split(strings.Trim(path, "/"), "/")

	// /api/v1/namespaces/{ns}/pods/{pod}/log
	if len(parts) >= 7 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "namespaces" && parts[4] == "pods" && parts[6] == "log" {
		ns := parts[3]
		pod := parts[5]
		q := r.URL.Query()
		cmd := fmt.Sprintf("kubectl logs -n %s pod/%s", ns, pod)
		if container := q.Get("container"); container != "" {
			cmd += " -c " + container
		}
		if q.Get("follow") == "true" {
			cmd += " -f"
		}
		if prev := q.Get("previous"); prev == "true" {
			cmd += " --previous"
		}
		if tail := q.Get("tailLines"); tail != "" {
			cmd += " --tail=" + tail
		}
		return cmd
	}

	// /api/v1/namespaces/{ns}/pods/{pod}/exec?command=...&container=...&stdin=...&tty=...
	if len(parts) >= 7 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "namespaces" && parts[4] == "pods" && parts[6] == "exec" {
		ns := parts[3]
		pod := parts[5]
		q := r.URL.Query()
		cmd := fmt.Sprintf("kubectl exec -n %s pod/%s", ns, pod)
		if container := q.Get("container"); container != "" {
			cmd += " -c " + container
		}
		if q.Get("stdin") == "true" {
			cmd += " -i"
		}
		if q.Get("tty") == "true" {
			cmd += " -t"
		}
		argv := q["command"]
		if len(argv) > 0 {
			cmd += " -- " + strings.Join(argv, " ")
		}
		return cmd
	}

	if r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "namespaces" {
		return "kubectl get namespaces"
	}

	if r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "namespaces" {
		return "kubectl get namespace " + parts[3]
	}

	return fmt.Sprintf("%s %s", r.Method, path)
}

type recordingResponseWriter struct {
	http.ResponseWriter
	recorder   *Recorder
	statusCode int
}

func (rw *recordingResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recordingResponseWriter) Write(p []byte) (int, error) {
	rw.recorder.RecordOutput(p)
	return rw.ResponseWriter.Write(p)
}

// Handler returns the http.Handler for the proxy.
func (p *TetherProxy) Handler() http.Handler {
	return p
}

// StaticValidator is a simple validator that accepts a fixed set of tokens.
type StaticValidator struct {
	tokens map[string]string
}

// NewStaticValidator creates a StaticValidator from a token map.
func NewStaticValidator(tokens map[string]string) *StaticValidator {
	return &StaticValidator{tokens: tokens}
}

func (v *StaticValidator) Validate(_ context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if sessionID, ok := v.tokens[token]; ok {
		return sessionID, nil
	}
	return "", fmt.Errorf("unknown token")
}

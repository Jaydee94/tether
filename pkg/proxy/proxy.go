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

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Jaydee94/tether/pkg/audit"
)

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

	sessionID, err := p.validator.Validate(ctx, token)
	if err != nil {
		logger.Info("Token validation failed", "error", err)
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return
	}

	r.Header.Del("X-Tether-Token")
	r.Header.Del("Authorization")

	p.serveWithRecording(w, r, sessionID)
}

func (p *TetherProxy) serveWithRecording(w http.ResponseWriter, r *http.Request, sessionID string) {
	ctx := r.Context()
	logger := log.FromContext(ctx)

	recorder, err := p.recorderForSession(sessionID)
	if err != nil {
		logger.Error(err, "Failed to initialize recorder")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	recorder.RecordOutput([]byte("\n$ " + kubectlLikeCommand(r) + "\n"))

	rw := &recordingResponseWriter{
		ResponseWriter: w,
		recorder:       recorder,
	}

	p.proxy.ServeHTTP(rw, r)

	go func() {
		saveCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := recorder.Finish(saveCtx); err != nil {
			logger.Error(err, "Failed to save session recording", "sessionID", sessionID)
		} else {
			logger.Info("Session recording saved", "sessionID", sessionID)
		}
	}()
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
	recorder *Recorder
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

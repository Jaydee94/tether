package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Jaydee94/tether/pkg/audit"
)

var (
	execPattern = regexp.MustCompile(`^/api/v1/namespaces/[^/]+/pods/[^/]+/exec$`)
	logPattern  = regexp.MustCompile(`^/api/v1/namespaces/[^/]+/pods/[^/]+/log$`)
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
}

// NewTetherProxy creates a proxy that forwards to target.
func NewTetherProxy(target string, backend audit.Backend, validator SessionValidator, tlsSkipVerify bool) (*TetherProxy, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("parsing target URL %q: %w", target, err)
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: tlsSkipVerify, //nolint:gosec
		},
	}

	rp := httputil.NewSingleHostReverseProxy(u)
	rp.Transport = transport

	return &TetherProxy{
		target:    u,
		proxy:     rp,
		backend:   backend,
		validator: validator,
	}, nil
}

// ServeHTTP implements http.Handler.
func (p *TetherProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := log.FromContext(ctx)

	token := r.Header.Get("X-Tether-Token")
	if token == "" {
		http.Error(w, "X-Tether-Token header required", http.StatusUnauthorized)
		return
	}

	sessionID, err := p.validator.Validate(ctx, token)
	if err != nil {
		logger.Info("Token validation failed", "error", err)
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return
	}

	r.Header.Del("X-Tether-Token")

	path := r.URL.Path
	if execPattern.MatchString(path) || logPattern.MatchString(path) {
		p.serveWithRecording(w, r, sessionID)
		return
	}

	p.proxy.ServeHTTP(w, r)
}

func (p *TetherProxy) serveWithRecording(w http.ResponseWriter, r *http.Request, sessionID string) {
	ctx := r.Context()
	logger := log.FromContext(ctx)

	title := fmt.Sprintf("tether/%s %s %s", sessionID, r.Method, r.URL.Path)
	recorder := NewRecorder(sessionID, p.backend, title)

	if err := recorder.Start(); err != nil {
		logger.Error(err, "Failed to start recorder")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

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

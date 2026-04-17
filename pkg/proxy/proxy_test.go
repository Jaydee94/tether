package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Jaydee94/tether/pkg/audit"
)

func TestStaticValidator(t *testing.T) {
	v := NewStaticValidator(map[string]string{"tok1": "sess1"})

	sid, err := v.Validate(context.TODO(), "tok1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sid != "sess1" {
		t.Errorf("expected sess1, got %q", sid)
	}

	_, err = v.Validate(context.TODO(), "bad-token")
	if err == nil {
		t.Error("expected error for unknown token")
	}
}

func TestProxy_MissingToken(t *testing.T) {
	backend, _ := audit.NewLocalBackend(t.TempDir())
	validator := NewStaticValidator(map[string]string{"good": "session"})

	p, err := NewTetherProxy("http://localhost:9999", backend, validator, true, nil)
	if err != nil {
		t.Fatalf("NewTetherProxy: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestProxy_InvalidToken(t *testing.T) {
	backend, _ := audit.NewLocalBackend(t.TempDir())
	validator := NewStaticValidator(map[string]string{"good": "session"})

	p, err := NewTetherProxy("http://localhost:9999", backend, validator, true, nil)
	if err != nil {
		t.Fatalf("NewTetherProxy: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil)
	req.Header.Set("X-Tether-Token", "bad-token")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestProxy_InvalidBearerToken(t *testing.T) {
	backend, _ := audit.NewLocalBackend(t.TempDir())
	validator := NewStaticValidator(map[string]string{"good": "session"})

	p, err := NewTetherProxy("http://localhost:9999", backend, validator, true, nil)
	if err != nil {
		t.Fatalf("NewTetherProxy: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestRecorder_AsciinemaFormat(t *testing.T) {
	dir := t.TempDir()
	backend, err := audit.NewLocalBackend(dir)
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}

	rec := NewRecorder("test-session", backend, "test recording")
	if err := rec.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	rec.RecordOutput([]byte("hello world\r\n"))
	rec.RecordOutput([]byte("second line\r\n"))

	ctx := t.Context()
	if err := rec.Finish(ctx); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	data, err := backend.Read(ctx, "test-session")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	lines := splitRecordingLines(data)
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines, got %d", len(lines))
	}
}

func splitRecordingLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

func TestKubectlLikeCommandForLogs(t *testing.T) {
	u, err := url.Parse("https://localhost:8443/api/v1/namespaces/kube-system/pods/coredns-123/log?container=coredns&follow=true")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	r := &http.Request{Method: http.MethodGet, URL: u}

	got := kubectlLikeCommand(r)
	want := "kubectl logs -n kube-system pod/coredns-123 -c coredns -f"
	if got != want {
		t.Fatalf("unexpected command:\n got: %q\nwant: %q", got, want)
	}
}

func TestKubectlLikeCommandForExec(t *testing.T) {
	u, err := url.Parse("https://localhost:8443/api/v1/namespaces/default/pods/web/exec?container=app&stdin=true&tty=true&command=sh&command=-lc&command=id")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	r := &http.Request{Method: http.MethodPost, URL: u}

	got := kubectlLikeCommand(r)
	want := "kubectl exec -n default pod/web -c app -i -t -- sh -lc id"
	if got != want {
		t.Fatalf("unexpected command:\n got: %q\nwant: %q", got, want)
	}
}

func TestKubectlLikeCommandForGetNamespaces(t *testing.T) {
	u, err := url.Parse("https://localhost:8443/api/v1/namespaces")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	r := &http.Request{Method: http.MethodGet, URL: u}

	got := kubectlLikeCommand(r)
	want := "kubectl get namespaces"
	if got != want {
		t.Fatalf("unexpected command:\n got: %q\nwant: %q", got, want)
	}
}

func TestProxy_RecordsAllAuthenticatedRequests(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok:" + r.URL.Path + "\n"))
	}))
	defer target.Close()

	backend, err := audit.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}

	p, err := NewTetherProxy(target.URL, backend, NewStaticValidator(map[string]string{"tok": "sess-all"}), true, nil)
	if err != nil {
		t.Fatalf("NewTetherProxy: %v", err)
	}

	for _, path := range []string{"/api/v1/namespaces", "/api/v1/namespaces/default"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tether-Token", "tok")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for %s, got %d", path, rr.Code)
		}
	}

	// Explicitly finish the session - Finish() is no longer called per-request.
	ctx := context.Background()
	p.FinishSession(ctx, "sess-all")

	data, err := backend.Read(ctx, "sess-all")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Contains(data, []byte("$ kubectl get namespaces")) {
		t.Errorf("recording missing 'kubectl get namespaces' command")
	}
	if !bytes.Contains(data, []byte("$ kubectl get namespace default")) {
		t.Errorf("recording missing 'kubectl get namespace default' command")
	}
}

// TestProxy_MultiRequestSessionNotTruncated verifies that multiple requests
// within the same session accumulate in the recording — the core regression
// guarded by this test (previously Finish() after each request could cause a
// race where later events overwrote earlier ones).
func TestProxy_MultiRequestSessionNotTruncated(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("resp:" + r.URL.Path + "\n"))
	}))
	defer target.Close()

	backend, err := audit.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}

	p, err := NewTetherProxy(target.URL, backend, NewStaticValidator(map[string]string{"tok": "sess-multi"}), true, nil)
	if err != nil {
		t.Fatalf("NewTetherProxy: %v", err)
	}

	paths := []string{
		"/api/v1/namespaces",
		"/api/v1/namespaces/default",
		"/api/v1/namespaces",
	}
	for _, path := range paths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tether-Token", "tok")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for %s, got %d", path, rr.Code)
		}
	}

	ctx := context.Background()
	p.FinishSession(ctx, "sess-multi")

	data, err := backend.Read(ctx, "sess-multi")
	if err != nil {
		t.Fatalf("Read after FinishSession: %v", err)
	}

	lines := splitRecordingLines(data)
	// Expect: 1 header line + at least 3 command events + 3 response events
	if len(lines) < 7 {
		t.Errorf("expected at least 7 lines in recording (header + 3 cmds + 3 responses), got %d\n%s", len(lines), data)
	}

	// All three command invocations must be present.
	occurrences := bytes.Count(data, []byte("$ kubectl get namespaces"))
	if occurrences < 2 {
		t.Errorf("expected 'kubectl get namespaces' to appear at least twice, got %d occurrences", occurrences)
	}
}

// TestProxy_FinishSessionIdempotent verifies that calling FinishSession twice
// does not panic or write a second time to the backend.
func TestProxy_FinishSessionIdempotent(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	backend, err := audit.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}

	p, err := NewTetherProxy(target.URL, backend, NewStaticValidator(map[string]string{"tok": "sess-idem"}), true, nil)
	if err != nil {
		t.Fatalf("NewTetherProxy: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	req.Header.Set("X-Tether-Token", "tok")
	p.ServeHTTP(httptest.NewRecorder(), req)

	ctx := context.Background()
	p.FinishSession(ctx, "sess-idem")
	// Second call must not panic.
	p.FinishSession(ctx, "sess-idem")
}

// TestProxy_SessionReaper verifies that the reaper goroutine calls FinishSession
// for sessions that are no longer active.
func TestProxy_SessionReaper(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	backend, err := audit.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}

	p, err := NewTetherProxy(target.URL, backend, NewStaticValidator(map[string]string{"tok": "sess-reap"}), true, nil)
	if err != nil {
		t.Fatalf("NewTetherProxy: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	req.Header.Set("X-Tether-Token", "tok")
	p.ServeHTTP(httptest.NewRecorder(), req)

	// Use an always-inactive finalizer to simulate expired session.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	p.StartSessionReaper(ctx, alwaysInactiveFinalizer{}, 20*time.Millisecond)

	// Wait until the recording appears (reaper flushed it).
	for {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for reaper to flush session")
		default:
		}
		_, readErr := backend.Read(context.Background(), "sess-reap")
		if readErr == nil {
			return // recording was written — test passes
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// alwaysInactiveFinalizer reports every session as inactive.
type alwaysInactiveFinalizer struct{}

func (alwaysInactiveFinalizer) IsActive(_ context.Context, _ string) bool { return false }

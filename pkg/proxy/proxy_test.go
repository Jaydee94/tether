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

	sid, err := v.Validate(nil, "tok1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sid != "sess1" {
		t.Errorf("expected sess1, got %q", sid)
	}

	_, err = v.Validate(nil, "bad-token")
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var data []byte
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for recording to flush")
		default:
		}

		data, err = backend.Read(context.Background(), "sess-all")
		if err == nil && bytes.Contains(data, []byte("$ kubectl get namespaces")) && bytes.Contains(data, []byte("$ kubectl get namespace default")) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

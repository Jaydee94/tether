package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

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

	p, err := NewTetherProxy("http://localhost:9999", backend, validator, true)
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

	p, err := NewTetherProxy("http://localhost:9999", backend, validator, true)
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

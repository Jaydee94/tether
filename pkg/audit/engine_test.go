package audit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// LocalBackend tests
// ---------------------------------------------------------------------------

func TestLocalBackend_WriteRead(t *testing.T) {
	dir := t.TempDir()
	b, err := NewLocalBackend(dir)
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}

	ctx := context.Background()
	data := []byte(`{"version":2,"width":220,"height":50,"timestamp":1704067200,"title":"test"}` + "\n" +
		`[0.1,"o","hello\r\n"]` + "\n")

	if err := b.Write(ctx, "sess-001", data); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := b.Read(ctx, "sess-001")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if string(got) != string(data) {
		t.Errorf("data mismatch\ngot:  %q\nwant: %q", got, data)
	}
}

func TestLocalBackend_ReadNotFound(t *testing.T) {
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	_, err = b.Read(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestLocalBackend_InvalidDir(t *testing.T) {
	// Use a path we can't create (file in place of dir).
	f, err := os.CreateTemp("", "audit-file-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	// Try to create a backend whose dir is a file — MkdirAll should fail or
	// Write should fail.
	b, err := NewLocalBackend(filepath.Join(f.Name(), "sub"))
	if err == nil {
		// On some OSes MkdirAll may succeed oddly; ensure Write fails.
		werr := b.Write(context.Background(), "s", []byte("data"))
		if werr == nil {
			t.Error("expected Write to fail with invalid dir")
		}
	}
	// err != nil is the expected happy path here — just verify we don't panic.
}

func TestLocalBackend_Rotation(t *testing.T) {
	dir := t.TempDir()
	// max 10 bytes per file
	b, err := NewLocalBackendWithConfig(dir, LocalBackendConfig{MaxFileSizeBytes: 10})
	if err != nil {
		t.Fatalf("NewLocalBackendWithConfig: %v", err)
	}

	ctx := context.Background()
	session := "rot-sess"

	// First write: 8 bytes — fits in the limit.
	if err := b.Write(ctx, session, []byte("12345678")); err != nil {
		t.Fatalf("Write 1: %v", err)
	}

	activePath := filepath.Join(dir, session+".cast")
	info, err := os.Stat(activePath)
	if err != nil {
		t.Fatalf("active file should exist: %v", err)
	}
	if info.Size() != 8 {
		t.Errorf("expected 8 bytes, got %d", info.Size())
	}

	// Second write: 8 bytes — combined 16 > 10, should trigger rotation.
	if err := b.Write(ctx, session, []byte("abcdefgh")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}

	// The rotated file should now exist.
	rotated := filepath.Join(dir, session+"-1.cast")
	if _, err := os.Stat(rotated); err != nil {
		t.Errorf("rotated file %q should exist: %v", rotated, err)
	}

	// The active file should contain the second write.
	got, err := b.Read(ctx, session)
	if err != nil {
		t.Fatalf("Read after rotation: %v", err)
	}
	if string(got) != "abcdefgh" {
		t.Errorf("active file content wrong: got %q", got)
	}
}

func TestLocalBackend_RotationDisabled(t *testing.T) {
	dir := t.TempDir()
	// MaxFileSizeBytes == 0 means disabled.
	b, err := NewLocalBackendWithConfig(dir, LocalBackendConfig{MaxFileSizeBytes: 0})
	if err != nil {
		t.Fatalf("NewLocalBackendWithConfig: %v", err)
	}

	ctx := context.Background()
	session := "no-rot"
	big := strings.Repeat("x", 1000)

	if err := b.Write(ctx, session, []byte(big)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := b.Write(ctx, session, []byte(big)); err != nil {
		t.Fatalf("Write 2: %v", err)
	}

	// No rotated file should exist.
	rotated := filepath.Join(dir, session+"-1.cast")
	if _, err := os.Stat(rotated); err == nil {
		t.Error("rotation should be disabled but rotated file exists")
	}
}

// ---------------------------------------------------------------------------
// ElasticBackend tests
// ---------------------------------------------------------------------------

func newElasticTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestElasticBackend_WriteSuccess(t *testing.T) {
	called := false
	srv := newElasticTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		w.WriteHeader(http.StatusCreated)
	})

	b := NewElasticBackend(srv.URL, "audit")
	if err := b.Write(context.Background(), "sess-e1", []byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !called {
		t.Error("handler not called")
	}
}

func TestElasticBackend_WriteError(t *testing.T) {
	srv := newElasticTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})

	b := NewElasticBackend(srv.URL, "audit")
	if err := b.Write(context.Background(), "sess-e2", []byte("data")); err == nil {
		t.Error("expected error on non-2xx response")
	}
}

func TestElasticBackend_APIKeyAuth(t *testing.T) {
	srv := newElasticTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "ApiKey my-key" {
			http.Error(w, fmt.Sprintf("bad auth: %q", auth), http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	b := NewElasticBackendWithConfig(srv.URL, "audit", ElasticBackendConfig{
		Auth: ElasticAuthConfig{APIKey: "my-key"},
	})
	if err := b.Write(context.Background(), "sess-auth", []byte("data")); err != nil {
		t.Fatalf("Write with API key: %v", err)
	}
}

func TestElasticBackend_BasicAuth(t *testing.T) {
	srv := newElasticTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "elastic" || pass != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	b := NewElasticBackendWithConfig(srv.URL, "audit", ElasticBackendConfig{
		Auth: ElasticAuthConfig{Username: "elastic", Password: "secret"},
	})
	if err := b.Write(context.Background(), "sess-basic", []byte("data")); err != nil {
		t.Fatalf("Write with basic auth: %v", err)
	}
}

func TestElasticBackend_APIKeyTakesPrecedenceOverBasicAuth(t *testing.T) {
	srv := newElasticTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "ApiKey ") {
			http.Error(w, fmt.Sprintf("expected ApiKey header, got %q", auth), http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	b := NewElasticBackendWithConfig(srv.URL, "audit", ElasticBackendConfig{
		Auth: ElasticAuthConfig{
			APIKey:   "my-key",
			Username: "elastic",
			Password: "secret",
		},
	})
	if err := b.Write(context.Background(), "sess-prec", []byte("data")); err != nil {
		t.Fatalf("APIKey should take precedence: %v", err)
	}
}

func TestElasticBackend_ReadNotFound(t *testing.T) {
	srv := newElasticTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	b := NewElasticBackend(srv.URL, "audit")
	_, err := b.Read(context.Background(), "missing-session")
	if err == nil {
		t.Error("expected error for 404")
	}
}

func TestElasticBackend_ReadSuccess(t *testing.T) {
	srv := newElasticTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"_source":{"cast":"hello"}}`)
	})

	b := NewElasticBackend(srv.URL, "audit")
	data, err := b.Read(context.Background(), "sess-e3")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty response body")
	}
}

func TestElasticBackend_NoAuth(t *testing.T) {
	srv := newElasticTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "unexpected auth header", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})

	b := NewElasticBackend(srv.URL, "audit")
	if err := b.Write(context.Background(), "sess-noauth", []byte("data")); err != nil {
		t.Fatalf("Write without auth: %v", err)
	}
}

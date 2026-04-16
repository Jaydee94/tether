package proxy

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// mockValidator counts how many times Validate is called.
type mockValidator struct {
	calls     int64
	sessionID string
	err       error
}

func (m *mockValidator) Validate(_ context.Context, _ string) (string, error) {
	atomic.AddInt64(&m.calls, 1)
	return m.sessionID, m.err
}

func newTestMetrics() (prometheus.Counter, prometheus.Counter) {
	hits := prometheus.NewCounter(prometheus.CounterOpts{
		Name: fmt.Sprintf("test_cache_hits_%d", time.Now().UnixNano()),
	})
	misses := prometheus.NewCounter(prometheus.CounterOpts{
		Name: fmt.Sprintf("test_cache_misses_%d", time.Now().UnixNano()),
	})
	return hits, misses
}

func TestCachedLeaseValidator_CacheHit(t *testing.T) {
	mock := &mockValidator{sessionID: "lease-1"}
	hits, misses := newTestMetrics()
	v := NewCachedLeaseValidatorWithMetrics(mock, time.Minute, hits, misses)

	// First call: cache miss → inner called.
	id, err := v.Validate(context.Background(), "token-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "lease-1" {
		t.Fatalf("expected lease-1, got %q", id)
	}
	if atomic.LoadInt64(&mock.calls) != 1 {
		t.Errorf("expected 1 inner call, got %d", mock.calls)
	}

	// Second call: cache hit → inner NOT called again.
	id2, err := v.Validate(context.Background(), "token-abc")
	if err != nil {
		t.Fatalf("unexpected error on cache hit: %v", err)
	}
	if id2 != "lease-1" {
		t.Fatalf("expected lease-1, got %q", id2)
	}
	if atomic.LoadInt64(&mock.calls) != 1 {
		t.Errorf("expected still 1 inner call after cache hit, got %d", mock.calls)
	}
}

func TestCachedLeaseValidator_CacheExpiry(t *testing.T) {
	mock := &mockValidator{sessionID: "lease-exp"}
	hits, misses := newTestMetrics()
	v := NewCachedLeaseValidatorWithMetrics(mock, 10*time.Millisecond, hits, misses)

	_, err := v.Validate(context.Background(), "token-exp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt64(&mock.calls) != 1 {
		t.Errorf("expected 1 inner call, got %d", mock.calls)
	}

	// Wait for TTL to expire.
	time.Sleep(20 * time.Millisecond)

	_, err = v.Validate(context.Background(), "token-exp")
	if err != nil {
		t.Fatalf("unexpected error after expiry: %v", err)
	}
	if atomic.LoadInt64(&mock.calls) != 2 {
		t.Errorf("expected 2 inner calls after TTL expiry, got %d", mock.calls)
	}
}

func TestCachedLeaseValidator_InvalidationOnRevoke(t *testing.T) {
	mock := &mockValidator{sessionID: "lease-rev"}
	hits, misses := newTestMetrics()
	v := NewCachedLeaseValidatorWithMetrics(mock, time.Minute, hits, misses)

	_, err := v.Validate(context.Background(), "token-rev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Simulate revocation.
	v.NotifyRevoked("lease-rev")

	// Entry should be gone; inner called again.
	_, err = v.Validate(context.Background(), "token-rev")
	if err != nil {
		t.Fatalf("unexpected error after revocation: %v", err)
	}
	if atomic.LoadInt64(&mock.calls) != 2 {
		t.Errorf("expected 2 inner calls after revoke, got %d", mock.calls)
	}
}

func TestCachedLeaseValidator_InvalidationOnExpired(t *testing.T) {
	mock := &mockValidator{sessionID: "lease-done"}
	hits, misses := newTestMetrics()
	v := NewCachedLeaseValidatorWithMetrics(mock, time.Minute, hits, misses)

	_, err := v.Validate(context.Background(), "token-done")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v.NotifyExpired("lease-done")

	_, _ = v.Validate(context.Background(), "token-done")
	if atomic.LoadInt64(&mock.calls) != 2 {
		t.Errorf("expected 2 inner calls after NotifyExpired, got %d", mock.calls)
	}
}

func TestCachedLeaseValidator_InnerError_NotCached(t *testing.T) {
	mock := &mockValidator{err: fmt.Errorf("k8s unavailable")}
	hits, misses := newTestMetrics()
	v := NewCachedLeaseValidatorWithMetrics(mock, time.Minute, hits, misses)

	_, err := v.Validate(context.Background(), "bad-token")
	if err == nil {
		t.Fatal("expected error from inner validator")
	}

	// Call again — inner must be called again (error not cached).
	mock.err = nil
	mock.sessionID = "lease-ok"
	_, err = v.Validate(context.Background(), "bad-token")
	if err != nil {
		t.Fatalf("unexpected error on retry: %v", err)
	}
	if atomic.LoadInt64(&mock.calls) != 2 {
		t.Errorf("expected 2 inner calls (error not cached), got %d", mock.calls)
	}
}

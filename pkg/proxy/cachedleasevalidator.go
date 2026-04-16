package proxy

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// tokenCacheEntry stores a resolved sessionID and its expiration time.
type tokenCacheEntry struct {
	sessionID string
	expiresAt time.Time
}

// CachedLeaseValidator wraps a TokenValidator with an in-memory TTL cache.
// Cache entries expire after ttl and are immediately invalidated when
// NotifyRevoked or NotifyExpired is called (e.g. from a Kubernetes watch).
type CachedLeaseValidator struct {
	inner TokenValidator
	ttl   time.Duration

	mu    sync.RWMutex
	cache map[string]tokenCacheEntry // token → entry

	cacheHits   prometheus.Counter
	cacheMisses prometheus.Counter
}

// TokenValidator is the interface satisfied by KubernetesLeaseValidator.
type TokenValidator interface {
	Validate(ctx context.Context, token string) (string, error)
}

var (
	defaultCacheHits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tether_proxy_token_cache_hits_total",
		Help: "Total number of token validation cache hits.",
	})
	defaultCacheMisses = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tether_proxy_token_cache_misses_total",
		Help: "Total number of token validation cache misses.",
	})
)

// NewCachedLeaseValidator wraps inner with a TTL cache of the given duration.
// Pass ttl=0 to use the default of 10 seconds.
// The returned validator is safe for concurrent use.
func NewCachedLeaseValidator(inner TokenValidator, ttl time.Duration) *CachedLeaseValidator {
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return &CachedLeaseValidator{
		inner:       inner,
		ttl:         ttl,
		cache:       make(map[string]tokenCacheEntry),
		cacheHits:   defaultCacheHits,
		cacheMisses: defaultCacheMisses,
	}
}

// NewCachedLeaseValidatorWithMetrics is like NewCachedLeaseValidator but accepts
// custom Prometheus counters, useful for testing without global metric conflicts.
func NewCachedLeaseValidatorWithMetrics(inner TokenValidator, ttl time.Duration, hits, misses prometheus.Counter) *CachedLeaseValidator {
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return &CachedLeaseValidator{
		inner:       inner,
		ttl:         ttl,
		cache:       make(map[string]tokenCacheEntry),
		cacheHits:   hits,
		cacheMisses: misses,
	}
}

// Validate returns the cached sessionID if the entry is still valid, otherwise
// delegates to the inner validator and caches the result.
func (c *CachedLeaseValidator) Validate(ctx context.Context, token string) (string, error) {
	// Fast path: check cache under read lock.
	c.mu.RLock()
	entry, ok := c.cache[token]
	c.mu.RUnlock()

	if ok && time.Now().Before(entry.expiresAt) {
		c.cacheHits.Inc()
		return entry.sessionID, nil
	}

	c.cacheMisses.Inc()

	// Slow path: validate against k8s API.
	sessionID, err := c.inner.Validate(ctx, token)
	if err != nil {
		// Evict any stale entry so a subsequent request re-validates.
		c.mu.Lock()
		delete(c.cache, token)
		c.mu.Unlock()
		return "", err
	}

	c.mu.Lock()
	c.cache[token] = tokenCacheEntry{
		sessionID: sessionID,
		expiresAt: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()

	return sessionID, nil
}

// NotifyRevoked immediately removes the cache entry for all tokens associated
// with leaseName. This should be called when a TetherLease transitions to
// Revoked so that the next request re-validates instead of using stale data.
func (c *CachedLeaseValidator) NotifyRevoked(leaseName string) {
	c.evictByLease(leaseName)
}

// NotifyExpired immediately removes the cache entry for all tokens associated
// with leaseName. This should be called when a TetherLease transitions to
// Expired.
func (c *CachedLeaseValidator) NotifyExpired(leaseName string) {
	c.evictByLease(leaseName)
}

// evictByLease removes all cache entries whose sessionID matches leaseName.
func (c *CachedLeaseValidator) evictByLease(leaseName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for token, entry := range c.cache {
		if entry.sessionID == leaseName {
			delete(c.cache, token)
		}
	}
}

package proxy

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// proxyRequestsTotal counts proxy requests by HTTP status code class.
	proxyRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxy_requests_total",
		Help: "Total number of proxy requests, labeled by HTTP status code.",
	}, []string{"status"})

	// proxyRequestDuration observes the latency of proxied requests in seconds.
	proxyRequestDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "proxy_request_duration_seconds",
		Help:    "Histogram of proxied request durations in seconds.",
		Buckets: prometheus.DefBuckets,
	})

	// tokenValidationErrors counts failed token validation attempts.
	tokenValidationErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "token_validation_errors_total",
		Help: "Total number of token validation failures.",
	})
)

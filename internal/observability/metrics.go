// Package observability provides Prometheus metrics, health/readiness endpoints,
// structured logging, and OpenTelemetry tracing for EdgeQuota.
package observability

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds both Prometheus gauges/counters and atomic counters for
// fast-path access in the middleware hot path.
type Metrics struct {
	// Atomic counters for hot-path (no mutex, no allocation).
	allowed          int64
	limited          int64
	redisErrors      int64
	fallbackUsed     int64
	readOnlyRetries  int64
	keyExtractErrors int64
	authErrors       int64
	authDenied       int64

	// Prometheus counters for scraping.
	promAllowed      prometheus.Counter
	promLimited      prometheus.Counter
	promRedisErrors  prometheus.Counter
	promFallbackUsed prometheus.Counter
	promAuthErrors   prometheus.Counter
	promAuthDenied   prometheus.Counter
	promKeyErrors    prometheus.Counter

	// Prometheus histograms.
	PromRequestDuration *prometheus.HistogramVec

	// Rate limit remaining tokens distribution (histogram, not per-key gauge
	// — avoids unbounded cardinality from high-cardinality keys like IPs).
	PromRLRemaining prometheus.Histogram

	// Per-tenant counters. Tenants are bounded entities (unlike IPs), so
	// using a label is safe from cardinality explosions.
	promTenantAllowed *prometheus.CounterVec
	promTenantLimited *prometheus.CounterVec
}

// NewMetrics creates and registers Prometheus metrics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	factory := promauto.With(reg)

	m := &Metrics{
		promAllowed: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "requests_allowed_total",
			Help:      "Total number of requests that passed rate limiting.",
		}),
		promLimited: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "requests_limited_total",
			Help:      "Total number of requests rejected by rate limiting.",
		}),
		promRedisErrors: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "redis_errors_total",
			Help:      "Total number of Redis errors encountered.",
		}),
		promFallbackUsed: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "fallback_used_total",
			Help:      "Total number of requests handled by in-memory fallback.",
		}),
		promAuthErrors: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "auth_errors_total",
			Help:      "Total number of auth service errors.",
		}),
		promAuthDenied: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "auth_denied_total",
			Help:      "Total number of requests denied by auth service.",
		}),
		promKeyErrors: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "key_extract_errors_total",
			Help:      "Total number of key extraction errors.",
		}),
		PromRequestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "edgequota",
			Name:      "request_duration_seconds",
			Help:      "Request duration in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method", "status_code"}),
		PromRLRemaining: factory.NewHistogram(prometheus.HistogramOpts{
			Namespace: "edgequota",
			Name:      "ratelimit_remaining_tokens",
			Help:      "Distribution of remaining tokens across rate-limit checks.",
			Buckets:   []float64{0, 1, 5, 10, 25, 50, 100, 250, 500, 1000},
		}),
		promTenantAllowed: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "tenant_requests_allowed_total",
			Help:      "Total requests allowed per tenant.",
		}, []string{"tenant"}),
		promTenantLimited: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "tenant_requests_limited_total",
			Help:      "Total requests rate-limited per tenant.",
		}, []string{"tenant"}),
	}

	return m
}

// IncAllowed increments the allowed requests counter.
func (m *Metrics) IncAllowed() {
	atomic.AddInt64(&m.allowed, 1)
	m.promAllowed.Inc()
}

// IncLimited increments the rate-limited requests counter.
func (m *Metrics) IncLimited() {
	atomic.AddInt64(&m.limited, 1)
	m.promLimited.Inc()
}

// IncRedisErrors increments the Redis error counter.
func (m *Metrics) IncRedisErrors() {
	atomic.AddInt64(&m.redisErrors, 1)
	m.promRedisErrors.Inc()
}

// IncFallbackUsed increments the fallback usage counter.
func (m *Metrics) IncFallbackUsed() {
	atomic.AddInt64(&m.fallbackUsed, 1)
	m.promFallbackUsed.Inc()
}

// IncReadOnlyRetries increments the READONLY retry counter.
func (m *Metrics) IncReadOnlyRetries() {
	atomic.AddInt64(&m.readOnlyRetries, 1)
}

// IncKeyExtractErrors increments the key extraction error counter.
func (m *Metrics) IncKeyExtractErrors() {
	atomic.AddInt64(&m.keyExtractErrors, 1)
	m.promKeyErrors.Inc()
}

// IncAuthErrors increments the auth service error counter.
func (m *Metrics) IncAuthErrors() {
	atomic.AddInt64(&m.authErrors, 1)
	m.promAuthErrors.Inc()
}

// IncAuthDenied increments the auth denied counter.
func (m *Metrics) IncAuthDenied() {
	atomic.AddInt64(&m.authDenied, 1)
	m.promAuthDenied.Inc()
}

// IncTenantAllowed increments the per-tenant allowed counter.
func (m *Metrics) IncTenantAllowed(tenant string) {
	m.promTenantAllowed.WithLabelValues(tenant).Inc()
}

// IncTenantLimited increments the per-tenant rate-limited counter.
func (m *Metrics) IncTenantLimited(tenant string) {
	m.promTenantLimited.WithLabelValues(tenant).Inc()
}

// ObserveRemaining records the remaining tokens as a histogram observation.
// This provides distribution visibility without per-key cardinality.
func (m *Metrics) ObserveRemaining(remaining int64) {
	m.PromRLRemaining.Observe(float64(remaining))
}

// MetricsSnapshot holds a point-in-time copy of all atomic counters.
type MetricsSnapshot struct {
	Allowed          int64
	Limited          int64
	RedisErrors      int64
	FallbackUsed     int64
	ReadOnlyRetries  int64
	KeyExtractErrors int64
	AuthErrors       int64
	AuthDenied       int64
}

// Snapshot returns the current counter values.
func (m *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		Allowed:          atomic.LoadInt64(&m.allowed),
		Limited:          atomic.LoadInt64(&m.limited),
		RedisErrors:      atomic.LoadInt64(&m.redisErrors),
		FallbackUsed:     atomic.LoadInt64(&m.fallbackUsed),
		ReadOnlyRetries:  atomic.LoadInt64(&m.readOnlyRetries),
		KeyExtractErrors: atomic.LoadInt64(&m.keyExtractErrors),
		AuthErrors:       atomic.LoadInt64(&m.authErrors),
		AuthDenied:       atomic.LoadInt64(&m.authDenied),
	}
}

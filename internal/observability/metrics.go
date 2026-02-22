// Package observability provides Prometheus metrics, health/readiness endpoints,
// structured logging, and OpenTelemetry tracing for EdgeQuota.
package observability

import (
	"sync"
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

	// Per-stage latency histograms for identifying bottlenecks.
	PromAuthDuration       prometheus.Histogram
	PromExternalRLDuration prometheus.Histogram
	PromBackendDuration    prometheus.Histogram

	// Rate limit remaining tokens distribution (histogram, not per-key gauge
	// — avoids unbounded cardinality from high-cardinality keys like IPs).
	PromRLRemaining prometheus.Histogram

	// Per-tenant counters. Tenants are bounded entities (unlike IPs), so
	// using a label is safe from cardinality explosions.
	promTenantAllowed *prometheus.CounterVec
	promTenantLimited *prometheus.CounterVec

	// Redis health gauge (0 = unhealthy, 1 = healthy).
	PromRedisHealthy prometheus.Gauge

	// Tenant label cardinality tracking.
	tenantLabels       sync.Map // map[string]struct{}
	tenantLabelCount   atomic.Int64
	maxTenantLabels    int64
	promTenantOverflow prometheus.Counter
	promEventsDropped  prometheus.Counter

	// External service response validation.
	tenantKeyRejected    int64
	promTenantKeyRejects prometheus.Counter

	// External RL concurrency metrics.
	PromExtRLSemaphoreRejected  prometheus.Counter
	PromExtRLSingleflightShared prometheus.Counter

	// Global concurrency limiter.
	PromConcurrencyRejected prometheus.Counter

	// Events emitter failures.
	PromEventsSendFailures prometheus.Counter
}

// NewMetrics creates and registers Prometheus metrics. maxTenantLabels caps
// the number of distinct tenant label values in per-tenant counters to prevent
// cardinality explosions. 0 uses the default of 1000.
func NewMetrics(reg prometheus.Registerer, maxTenantLabels int64) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	if maxTenantLabels <= 0 {
		maxTenantLabels = 1000
	}

	factory := promauto.With(reg)

	m := &Metrics{
		maxTenantLabels: maxTenantLabels,
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
			// Edge-proxy-tuned buckets with sub-millisecond granularity.
			Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5, 10},
		}, []string{"method", "status_code"}),
		PromAuthDuration: factory.NewHistogram(prometheus.HistogramOpts{
			Namespace: "edgequota",
			Name:      "auth_duration_seconds",
			Help:      "Latency of external auth checks.",
			Buckets:   []float64{0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5},
		}),
		PromExternalRLDuration: factory.NewHistogram(prometheus.HistogramOpts{
			Namespace: "edgequota",
			Name:      "external_rl_duration_seconds",
			Help:      "Latency of external rate limit service calls.",
			Buckets:   []float64{0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5},
		}),
		PromBackendDuration: factory.NewHistogram(prometheus.HistogramOpts{
			Namespace: "edgequota",
			Name:      "backend_duration_seconds",
			Help:      "Latency of backend proxy calls.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5, 10, 30},
		}),
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
		PromRedisHealthy: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: "edgequota",
			Name:      "redis_healthy",
			Help:      "Whether Redis is reachable (1 = healthy, 0 = unhealthy).",
		}),
		promTenantOverflow: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "tenant_label_overflow_total",
			Help:      "Number of requests bucketed under __overflow__ due to tenant label cap.",
		}),
		promEventsDropped: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "events_dropped_total",
			Help:      "Number of usage events dropped due to buffer overflow.",
		}),
		promTenantKeyRejects: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "tenant_key_rejected_total",
			Help:      "Number of tenant keys rejected due to validation failure (length or charset).",
		}),
		PromExtRLSemaphoreRejected: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "external_rl_semaphore_rejected_total",
			Help:      "Number of external RL requests rejected due to concurrency semaphore.",
		}),
		PromExtRLSingleflightShared: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "external_rl_singleflight_shared_total",
			Help:      "Number of external RL requests that shared a singleflight result.",
		}),
		PromConcurrencyRejected: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "concurrent_requests_rejected_total",
			Help:      "Number of requests rejected because max_concurrent_requests was reached.",
		}),
		PromEventsSendFailures: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "edgequota",
			Name:      "events_send_failures_total",
			Help:      "Number of event batches that failed to send after all retries.",
		}),
	}

	// Start as healthy (set to 1). The middleware chain will set to 0 on
	// connectivity failure and back to 1 on recovery.
	m.PromRedisHealthy.Set(1)

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

// IncTenantKeyRejected increments the counter for tenant keys rejected by
// validation. This fires when the external rate-limit service returns a
// tenant_key that violates length or charset constraints.
func (m *Metrics) IncTenantKeyRejected() {
	atomic.AddInt64(&m.tenantKeyRejected, 1)
	m.promTenantKeyRejects.Inc()
}

// tenantOverflowLabel is the sentinel label used when the tenant cardinality
// cap is reached. All excess tenants are aggregated under this label.
const tenantOverflowLabel = "__overflow__"

// resolveTenantLabel returns the tenant label to use for metrics, applying the
// cardinality cap. Known tenants pass through; new tenants beyond the cap are
// bucketed under __overflow__.
func (m *Metrics) resolveTenantLabel(tenant string) string {
	// Fast path: already known tenant.
	if _, ok := m.tenantLabels.Load(tenant); ok {
		return tenant
	}

	// Check if we have room for a new tenant.
	if m.tenantLabelCount.Load() >= m.maxTenantLabels {
		m.promTenantOverflow.Inc()
		return tenantOverflowLabel
	}

	// Try to register the tenant. A benign race may cause the count to
	// slightly exceed maxTenantLabels, but it is bounded and transient.
	if _, loaded := m.tenantLabels.LoadOrStore(tenant, struct{}{}); !loaded {
		m.tenantLabelCount.Add(1)
	}
	return tenant
}

// IncTenantAllowed increments the per-tenant allowed counter, respecting
// the tenant label cardinality cap.
func (m *Metrics) IncTenantAllowed(tenant string) {
	m.promTenantAllowed.WithLabelValues(m.resolveTenantLabel(tenant)).Inc()
}

// IncTenantLimited increments the per-tenant rate-limited counter, respecting
// the tenant label cardinality cap.
func (m *Metrics) IncTenantLimited(tenant string) {
	m.promTenantLimited.WithLabelValues(m.resolveTenantLabel(tenant)).Inc()
}

// IncEventsDropped increments the events dropped counter.
func (m *Metrics) IncEventsDropped() {
	m.promEventsDropped.Inc()
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

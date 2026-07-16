// Package observability provides OpenTelemetry metrics, health/readiness
// endpoints, structured logging, and tracing for EdgeQuota.
package observability

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// meterName is the instrumentation scope for EdgeQuota's first-party metrics.
// Service identity is carried by the service.name resource attribute (set in
// InitMetrics), so metric names are domain-scoped ("edgequota.*"), not
// service-prefixed.
const meterName = "github.com/edgequota/edgequota/internal/observability"

// Metric names (portable OTel semconv: dotted, lowercase, no unit suffix — the
// unit is a separate instrument field). Kept as consts so emission sites and the
// bucket Views can't drift on a typo.
const (
	metricRequests                = "edgequota.requests"
	metricActiveRequests          = "http.server.active_requests"
	metricRatelimitDecisions      = "edgequota.ratelimit.decisions"
	metricRatelimitFallbackUsed   = "edgequota.ratelimit.fallback_used"
	metricRatelimitRemaining      = "edgequota.ratelimit.remaining_tokens"
	metricConcurrencyRejected     = "edgequota.concurrency.rejected"
	metricKeyExtractErrors        = "edgequota.key_extract.errors"
	metricTenantKeyRejected       = "edgequota.tenant_key.rejected"
	metricAuthOutcomes            = "edgequota.auth.outcomes"
	metricAuthDuration            = "edgequota.auth.duration"
	metricExtRLDuration           = "edgequota.external_ratelimit.duration"
	metricExtRLCacheLookups       = "edgequota.external_ratelimit.cache.lookups"
	metricExtRLSemaphoreRejected  = "edgequota.external_ratelimit.semaphore_rejected"
	metricExtRLSingleflightShared = "edgequota.external_ratelimit.singleflight_shared"
	metricBackendDuration         = "edgequota.backend.duration"
	metricRespCacheLookups        = "edgequota.response_cache.lookups"
	metricRespCacheOperations     = "edgequota.response_cache.operations"
	metricRespCacheBodySize       = "edgequota.response_cache.body.size"
	metricRedisErrors             = "edgequota.redis.errors"
	metricRedisHealthy            = "edgequota.redis.healthy"
	metricRedisReadonlyRetries    = "edgequota.redis.readonly_retries"
	metricConnections             = "edgequota.connections"
	metricStreamingConnections    = "edgequota.streaming.connections"
	metricStreamingActive         = "edgequota.streaming.active_connections"
	metricStreamingDuration       = "edgequota.streaming.duration"
	metricEventsDropped           = "edgequota.events.dropped"
	metricEventsSendFailures      = "edgequota.events.send_failures"
	metricReloadTimestamp         = "edgequota.reload.last_success.timestamp"
	metricCORSPreflightBypassed   = "edgequota.cors.preflight_bypassed"
)

// Metric attribute keys (OTel dotted convention; Dash0 surfaces these with dots
// replaced by underscores in PromQL). Constants so emission sites can't drift.
const (
	attrHTTPMethod         = "http.request.method"
	attrStatusClass        = "edgequota.status_class"
	attrNetProtocolName    = "network.protocol.name"
	attrNetProtocolVersion = "network.protocol.version"
	attrTLSMode            = "edgequota.tls.mode"
	attrRatelimitDecision  = "edgequota.ratelimit.decision"
	attrRedisPool          = "edgequota.redis.pool"
	attrAuthOutcome        = "edgequota.auth.outcome"
	attrCacheResult        = "edgequota.cache.result"
	attrCacheOperation     = "edgequota.cache.operation"
	attrStreamProtocol     = "edgequota.stream.protocol"
	attrReloadTarget       = "edgequota.reload.target"
)

// Attribute values.
const (
	decisionAllowed = "allowed"
	decisionLimited = "limited"

	authOutcomeError    = "error"
	authOutcomeDenied   = "denied"
	authOutcomeCanceled = "canceled"

	cacheResultHit   = "hit"
	cacheResultMiss  = "miss"
	cacheResultStale = "stale"
	// A response the cache was never eligible to serve (not 200/301, or no
	// positive max-age). Kept distinct from "miss" so hit rate can be measured
	// over cache-eligible traffic only — mixing the two makes the ratio track
	// traffic composition rather than cache health.
	cacheResultUncacheable = "uncacheable"

	cacheOpStore = "store"
	cacheOpSkip  = "skip"
	cacheOpPurge = "purge"

	reloadTargetConfig = "config"
	reloadTargetTLS    = "tls"
	reloadTargetMTLSCA = "mtls_ca"

	netProtocolName = "http"
)

// Redis pool attribute values.
const (
	poolRateLimit       = "ratelimit"
	poolExternalRLCache = "external_rl_cache"
	poolResponseCache   = "response_cache"
)

// Precomputed measurement options for static single-attribute records on the hot
// path — built once so per-request increments allocate no attribute slice.
var (
	optDecisionAllowed = metric.WithAttributes(attribute.String(attrRatelimitDecision, decisionAllowed))
	optDecisionLimited = metric.WithAttributes(attribute.String(attrRatelimitDecision, decisionLimited))

	optAuthError    = metric.WithAttributes(attribute.String(attrAuthOutcome, authOutcomeError))
	optAuthDenied   = metric.WithAttributes(attribute.String(attrAuthOutcome, authOutcomeDenied))
	optAuthCanceled = metric.WithAttributes(attribute.String(attrAuthOutcome, authOutcomeCanceled))

	optCacheHit         = metric.WithAttributes(attribute.String(attrCacheResult, cacheResultHit))
	optCacheMiss        = metric.WithAttributes(attribute.String(attrCacheResult, cacheResultMiss))
	optCacheStale       = metric.WithAttributes(attribute.String(attrCacheResult, cacheResultStale))
	optCacheUncacheable = metric.WithAttributes(attribute.String(attrCacheResult, cacheResultUncacheable))

	optRespOpStore = metric.WithAttributes(attribute.String(attrCacheOperation, cacheOpStore))
	optRespOpSkip  = metric.WithAttributes(attribute.String(attrCacheOperation, cacheOpSkip))
	optRespOpPurge = metric.WithAttributes(attribute.String(attrCacheOperation, cacheOpPurge))
)

// Histogram bucket boundaries (seconds unless noted). Preserved from the prior
// Prometheus histograms so quantiles stay accurate and dashboards' by(le)
// queries keep working across the name/attribute migration.
var (
	latencyShortBuckets   = []float64{0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5}
	backendBuckets        = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5, 10, 30}
	streamingBuckets      = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}
	remainingTokenBuckets = []float64{0, 1, 5, 10, 25, 50, 100, 250, 500, 1000}
	bodySizeBuckets       = []float64{256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304}
)

// metricViews sets explicit-bucket boundaries for EdgeQuota's histogram
// instruments (the default OTel boundaries are tuned for milliseconds and would
// collapse these second-scale distributions). Applied to every MeterProvider via
// meterProviderOptions (used by InitMetrics and the tests). otelhttp's
// http.server.request.duration is intentionally not listed — it carries its own
// seconds-appropriate defaults.
func metricViews() []sdkmetric.View {
	hist := func(name string, bounds []float64) sdkmetric.View {
		return sdkmetric.NewView(
			sdkmetric.Instrument{Name: name},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: bounds}},
		)
	}
	return []sdkmetric.View{
		hist(metricAuthDuration, latencyShortBuckets),
		hist(metricExtRLDuration, latencyShortBuckets),
		hist(metricBackendDuration, backendBuckets),
		hist(metricStreamingDuration, streamingBuckets),
		hist(metricRatelimitRemaining, remainingTokenBuckets),
		hist(metricRespCacheBodySize, bodySizeBuckets),
	}
}

// Metrics holds EdgeQuota's OpenTelemetry instruments plus a small set of atomic
// mirror counters read by the /v1/stats endpoint.
//
// Instruments are built from the global otel.Meter during construction (in
// server.New, before InitMetrics installs the real MeterProvider). OpenTelemetry
// late-binds these global-delegate instruments — including the observable-gauge
// callbacks — onto the real provider when it is set, so they start recording
// without re-registration. When metrics are disabled the global provider stays
// no-op and every instrument is inert.
type Metrics struct {
	// Atomic mirror counters read by /v1/stats Snapshot(). Decoupled from the
	// OTel pipeline so the stats endpoint works even when metrics export is off.
	allowed          int64
	limited          int64
	redisErrors      int64
	fallbackUsed     int64
	readOnlyRetries  int64
	keyExtractErrors int64
	authErrors       int64
	authDenied       int64
	authCanceled     int64

	// Counters.
	requests                metric.Int64Counter
	ratelimitDecisions      metric.Int64Counter
	ratelimitFallbackUsed   metric.Int64Counter
	concurrencyRejected     metric.Int64Counter
	keyExtractErrorsCtr     metric.Int64Counter
	tenantKeyRejected       metric.Int64Counter
	authOutcomes            metric.Int64Counter
	extRLCacheLookups       metric.Int64Counter
	extRLSemaphoreRejected  metric.Int64Counter
	extRLSingleflightShared metric.Int64Counter
	respCacheLookups        metric.Int64Counter
	respCacheOperations     metric.Int64Counter
	redisErrorsCtr          metric.Int64Counter
	redisReadonlyRetries    metric.Int64Counter
	connections             metric.Int64Counter
	streamingConnections    metric.Int64Counter
	eventsDropped           metric.Int64Counter
	eventsSendFailures      metric.Int64Counter
	corsPreflightBypassed   metric.Int64Counter

	// Up/down counters.
	activeRequests  metric.Int64UpDownCounter
	streamingActive metric.Int64UpDownCounter

	// Histograms. The *.duration histograms carry exemplars linking a data point
	// back to the recording trace (via the request context threaded into Record).
	authDuration      metric.Float64Histogram
	extRLDuration     metric.Float64Histogram
	backendDuration   metric.Float64Histogram
	streamingDuration metric.Float64Histogram
	remainingTokens   metric.Int64Histogram
	respCacheBodySize metric.Int64Histogram

	// Observable-gauge backing state, sampled by the async callbacks.
	redisHealthy   map[string]*atomic.Int64 // pool -> 0/1; map is created once, never mutated
	configReloadTS atomic.Int64             // unix seconds; 0 = never reloaded
	tlsReloadTS    atomic.Int64
	mtlsCAReloadTS atomic.Int64
}

// NewMetrics builds the OpenTelemetry instruments from the global meter. Safe to
// call before InitMetrics installs the real MeterProvider: instruments bind to a
// no-op until then and late-bind afterwards. Instrument construction errors are
// logged and tolerated (the OTel API returns a usable no-op instrument alongside
// any error), so this never fails.
func NewMetrics(logger *slog.Logger) *Metrics {
	m := otel.Meter(meterName)

	mx := &Metrics{
		redisHealthy: map[string]*atomic.Int64{
			poolRateLimit:       new(atomic.Int64),
			poolExternalRLCache: new(atomic.Int64),
			poolResponseCache:   new(atomic.Int64),
		},

		requests:                int64Counter(m, logger, metricRequests, "Total requests served, by method, status class, and protocol version. Counts every terminal path including streaming and preflight.", "{request}"),
		ratelimitDecisions:      int64Counter(m, logger, metricRatelimitDecisions, "Rate-limit decisions by outcome (allowed/limited).", "{request}"),
		ratelimitFallbackUsed:   int64Counter(m, logger, metricRatelimitFallbackUsed, "Requests handled by the in-memory fallback limiter, by pool.", "{request}"),
		concurrencyRejected:     int64Counter(m, logger, metricConcurrencyRejected, "Requests rejected because max_concurrent_requests was reached.", "{request}"),
		keyExtractErrorsCtr:     int64Counter(m, logger, metricKeyExtractErrors, "Rate-limit key extraction errors.", ""),
		tenantKeyRejected:       int64Counter(m, logger, metricTenantKeyRejected, "Tenant keys rejected by validation (length or charset).", ""),
		authOutcomes:            int64Counter(m, logger, metricAuthOutcomes, "External auth outcomes by type (error/denied/canceled). Cancellations are client disconnects, not auth-service failures.", "{request}"),
		extRLCacheLookups:       int64Counter(m, logger, metricExtRLCacheLookups, "External rate-limit cache lookups by result (hit/miss/stale).", "{lookup}"),
		extRLSemaphoreRejected:  int64Counter(m, logger, metricExtRLSemaphoreRejected, "External rate-limit requests rejected by the concurrency semaphore.", ""),
		extRLSingleflightShared: int64Counter(m, logger, metricExtRLSingleflightShared, "External rate-limit requests that shared a singleflight result.", ""),
		respCacheLookups:        int64Counter(m, logger, metricRespCacheLookups, "Response-cache lookups by result (hit/miss/uncacheable). A miss is a response the cache could have served but had not stored; uncacheable responses were never eligible, so hit rate is hit/(hit+miss).", "{lookup}"),
		respCacheOperations:     int64Counter(m, logger, metricRespCacheOperations, "Response-cache operations by type (store/skip/purge).", "{operation}"),
		redisErrorsCtr:          int64Counter(m, logger, metricRedisErrors, "Redis errors encountered, by pool.", ""),
		redisReadonlyRetries:    int64Counter(m, logger, metricRedisReadonlyRetries, "Redis READONLY retries (replica failover).", ""),
		connections:             int64Counter(m, logger, metricConnections, "Requests by network protocol and TLS mode.", "{connection}"),
		streamingConnections:    int64Counter(m, logger, metricStreamingConnections, "Streaming connections opened, by protocol.", "{connection}"),
		eventsDropped:           int64Counter(m, logger, metricEventsDropped, "Usage events dropped due to buffer overflow.", "{event}"),
		eventsSendFailures:      int64Counter(m, logger, metricEventsSendFailures, "Event batches that failed to send after all retries.", ""),
		corsPreflightBypassed:   int64Counter(m, logger, metricCORSPreflightBypassed, "CORS preflight requests that bypassed auth and rate limiting.", "{request}"),

		activeRequests:  int64UpDownCounter(m, logger, metricActiveRequests, "In-flight HTTP requests.", "{request}"),
		streamingActive: int64UpDownCounter(m, logger, metricStreamingActive, "Active streaming connections, by protocol.", "{connection}"),

		authDuration:      float64Histogram(m, logger, metricAuthDuration, "External auth check latency."),
		extRLDuration:     float64Histogram(m, logger, metricExtRLDuration, "External rate-limit service call latency."),
		backendDuration:   float64Histogram(m, logger, metricBackendDuration, "Backend proxy call latency."),
		streamingDuration: float64Histogram(m, logger, metricStreamingDuration, "Streaming connection duration, by protocol."),
		remainingTokens:   int64Histogram(m, logger, metricRatelimitRemaining, "Distribution of remaining tokens across rate-limit checks.", "{token}"),
		respCacheBodySize: int64Histogram(m, logger, metricRespCacheBodySize, "Distribution of cached response body sizes.", "By"),
	}

	// All pools start healthy; the middleware flips a pool to 0 on connectivity
	// failure and back to 1 on recovery.
	for _, v := range mx.redisHealthy {
		v.Store(1)
	}

	mx.registerRedisHealthyGauge(m, logger)
	mx.registerReloadTimestampGauge(m, logger)

	return mx
}

// registerRedisHealthyGauge wires the async gauge that reports each Redis pool's
// reachability (1 = healthy, 0 = unhealthy) from the atomic state.
func (m *Metrics) registerRedisHealthyGauge(meter metric.Meter, logger *slog.Logger) {
	_, err := meter.Int64ObservableGauge(
		metricRedisHealthy,
		metric.WithDescription("Whether each Redis pool is reachable (1 = healthy, 0 = unhealthy)."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for pool, v := range m.redisHealthy {
				o.Observe(v.Load(), metric.WithAttributes(attribute.String(attrRedisPool, pool)))
			}
			return nil
		}),
	)
	if err != nil {
		logger.Warn("create observable gauge failed", "metric", metricRedisHealthy, "error", err)
	}
}

// registerReloadTimestampGauge wires the async gauge that reports the unix
// timestamp of the last successful reload for each target (config/tls/mtls_ca).
func (m *Metrics) registerReloadTimestampGauge(meter metric.Meter, logger *slog.Logger) {
	_, err := meter.Float64ObservableGauge(
		metricReloadTimestamp,
		metric.WithDescription("Unix timestamp (seconds) of the last successful reload, by target. 0 until the first reload."),
		metric.WithUnit("s"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			o.Observe(float64(m.configReloadTS.Load()), metric.WithAttributes(attribute.String(attrReloadTarget, reloadTargetConfig)))
			o.Observe(float64(m.tlsReloadTS.Load()), metric.WithAttributes(attribute.String(attrReloadTarget, reloadTargetTLS)))
			o.Observe(float64(m.mtlsCAReloadTS.Load()), metric.WithAttributes(attribute.String(attrReloadTarget, reloadTargetMTLSCA)))
			return nil
		}),
	)
	if err != nil {
		logger.Warn("create observable gauge failed", "metric", metricReloadTimestamp, "error", err)
	}
}

// --- Tolerant instrument builders. The OTel API returns a usable (no-op)
// instrument alongside any construction error, so callers can always record. ---

func int64Counter(m metric.Meter, logger *slog.Logger, name, desc, unit string) metric.Int64Counter {
	opts := []metric.Int64CounterOption{metric.WithDescription(desc)}
	if unit != "" {
		opts = append(opts, metric.WithUnit(unit))
	}
	c, err := m.Int64Counter(name, opts...)
	if err != nil {
		logger.Warn("create counter failed", "metric", name, "error", err)
	}
	return c
}

func int64UpDownCounter(m metric.Meter, logger *slog.Logger, name, desc, unit string) metric.Int64UpDownCounter {
	opts := []metric.Int64UpDownCounterOption{metric.WithDescription(desc)}
	if unit != "" {
		opts = append(opts, metric.WithUnit(unit))
	}
	c, err := m.Int64UpDownCounter(name, opts...)
	if err != nil {
		logger.Warn("create updowncounter failed", "metric", name, "error", err)
	}
	return c
}

// float64Histogram builds a seconds-valued latency histogram. All of
// EdgeQuota's Float64 histograms measure durations, so the unit is fixed at "s".
func float64Histogram(m metric.Meter, logger *slog.Logger, name, desc string) metric.Float64Histogram {
	h, err := m.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit("s"))
	if err != nil {
		logger.Warn("create histogram failed", "metric", name, "error", err)
	}
	return h
}

func int64Histogram(m metric.Meter, logger *slog.Logger, name, desc, unit string) metric.Int64Histogram {
	opts := []metric.Int64HistogramOption{metric.WithDescription(desc)}
	if unit != "" {
		opts = append(opts, metric.WithUnit(unit))
	}
	h, err := m.Int64Histogram(name, opts...)
	if err != nil {
		logger.Warn("create histogram failed", "metric", name, "error", err)
	}
	return h
}

// protocolVersion strips the "HTTP/" prefix from a request's Proto ("HTTP/1.1"
// -> "1.1") for the network.protocol.version attribute.
func protocolVersion(proto string) string {
	return strings.TrimPrefix(proto, "HTTP/")
}

// --- Rate-limit decisions (mirror atomics + OTel). ---

// IncAllowed increments the allowed requests counter.
func (m *Metrics) IncAllowed() {
	atomic.AddInt64(&m.allowed, 1)
	m.ratelimitDecisions.Add(context.Background(), 1, optDecisionAllowed)
}

// IncLimited increments the rate-limited requests counter.
func (m *Metrics) IncLimited() {
	atomic.AddInt64(&m.limited, 1)
	m.ratelimitDecisions.Add(context.Background(), 1, optDecisionLimited)
}

// IncRedisErrors increments the Redis error counter for the given pool.
func (m *Metrics) IncRedisErrors(pool string) {
	atomic.AddInt64(&m.redisErrors, 1)
	m.redisErrorsCtr.Add(context.Background(), 1, metric.WithAttributes(attribute.String(attrRedisPool, pool)))
}

// IncFallbackUsed increments the fallback usage counter for the given pool.
func (m *Metrics) IncFallbackUsed(pool string) {
	atomic.AddInt64(&m.fallbackUsed, 1)
	m.ratelimitFallbackUsed.Add(context.Background(), 1, metric.WithAttributes(attribute.String(attrRedisPool, pool)))
}

// IncReadOnlyRetries increments the READONLY retry counter.
func (m *Metrics) IncReadOnlyRetries() {
	atomic.AddInt64(&m.readOnlyRetries, 1)
	m.redisReadonlyRetries.Add(context.Background(), 1)
}

// IncKeyExtractErrors increments the key extraction error counter.
func (m *Metrics) IncKeyExtractErrors() {
	atomic.AddInt64(&m.keyExtractErrors, 1)
	m.keyExtractErrorsCtr.Add(context.Background(), 1)
}

// --- Auth outcomes (mirror atomics + OTel, folded onto one counter). ---

// IncAuthErrors increments the auth service error counter.
func (m *Metrics) IncAuthErrors() {
	atomic.AddInt64(&m.authErrors, 1)
	m.authOutcomes.Add(context.Background(), 1, optAuthError)
}

// IncAuthDenied increments the auth denied counter.
func (m *Metrics) IncAuthDenied() {
	atomic.AddInt64(&m.authDenied, 1)
	m.authOutcomes.Add(context.Background(), 1, optAuthDenied)
}

// IncAuthCanceled increments the counter of auth checks aborted by client
// cancellation (client disconnect / shutdown). These are not auth-service
// failures, so they are tracked under outcome=canceled, not error.
func (m *Metrics) IncAuthCanceled() {
	atomic.AddInt64(&m.authCanceled, 1)
	m.authOutcomes.Add(context.Background(), 1, optAuthCanceled)
}

// --- Other counters (OTel only). ---

// IncTenantKeyRejected increments the counter for tenant keys rejected by
// validation.
func (m *Metrics) IncTenantKeyRejected() {
	m.tenantKeyRejected.Add(context.Background(), 1)
}

// IncEventsDropped increments the dropped-events counter.
func (m *Metrics) IncEventsDropped() {
	m.eventsDropped.Add(context.Background(), 1)
}

// IncEventsSendFailures increments the event-send-failure counter.
func (m *Metrics) IncEventsSendFailures() {
	m.eventsSendFailures.Add(context.Background(), 1)
}

// IncPreflightBypassed increments the CORS-preflight-bypass counter.
func (m *Metrics) IncPreflightBypassed() {
	m.corsPreflightBypassed.Add(context.Background(), 1)
}

// IncConcurrencyRejected increments the concurrency-rejection counter. These
// 503s return before the terminal defer, so this counter is their only home.
func (m *Metrics) IncConcurrencyRejected() {
	m.concurrencyRejected.Add(context.Background(), 1)
}

// IncConnection records a request by network protocol version and TLS mode.
func (m *Metrics) IncConnection(proto, tlsMode string) {
	m.connections.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String(attrNetProtocolName, netProtocolName),
		attribute.String(attrNetProtocolVersion, protocolVersion(proto)),
		attribute.String(attrTLSMode, tlsMode),
	))
}

// --- Active-request gauge (up/down). ---

// IncRequestsInFlight increments the in-flight request gauge.
func (m *Metrics) IncRequestsInFlight() {
	m.activeRequests.Add(context.Background(), 1)
}

// DecRequestsInFlight decrements the in-flight request gauge.
func (m *Metrics) DecRequestsInFlight() {
	m.activeRequests.Add(context.Background(), -1)
}

// --- Streaming connections. ---

// IncStreamingConn records a newly opened streaming connection for the protocol
// (bumps both the total counter and the active gauge).
func (m *Metrics) IncStreamingConn(proto string) {
	opt := metric.WithAttributes(attribute.String(attrStreamProtocol, proto))
	m.streamingConnections.Add(context.Background(), 1, opt)
	m.streamingActive.Add(context.Background(), 1, opt)
}

// DecStreamingActive decrements the active streaming-connection gauge for the
// protocol.
func (m *Metrics) DecStreamingActive(proto string) {
	m.streamingActive.Add(context.Background(), -1, metric.WithAttributes(attribute.String(attrStreamProtocol, proto)))
}

// ObserveStreamingDuration records a streaming connection's lifetime. The
// context carries the request span so the observation gets an exemplar.
func (m *Metrics) ObserveStreamingDuration(ctx context.Context, proto string, seconds float64) {
	m.streamingDuration.Record(ctx, seconds, metric.WithAttributes(attribute.String(attrStreamProtocol, proto)))
}

// --- Latency histograms (exemplar-bearing; ctx carries the request span). ---

// RecordRequest records a terminal request in the traffic counter with its
// method, status class, and protocol version.
func (m *Metrics) RecordRequest(ctx context.Context, method string, code int, proto string) {
	m.requests.Add(ctx, 1, metric.WithAttributes(
		attribute.String(attrHTTPMethod, method),
		attribute.String(attrStatusClass, StatusFamily(code)),
		attribute.String(attrNetProtocolVersion, protocolVersion(proto)),
	))
}

// ObserveAuthDuration records external auth check latency.
func (m *Metrics) ObserveAuthDuration(ctx context.Context, seconds float64) {
	m.authDuration.Record(ctx, seconds)
}

// ObserveExternalRLDuration records external rate-limit call latency.
func (m *Metrics) ObserveExternalRLDuration(ctx context.Context, seconds float64) {
	m.extRLDuration.Record(ctx, seconds)
}

// ObserveBackendDuration records backend proxy latency.
func (m *Metrics) ObserveBackendDuration(ctx context.Context, seconds float64) {
	m.backendDuration.Record(ctx, seconds)
}

// ObserveRemaining records the remaining tokens distribution.
func (m *Metrics) ObserveRemaining(remaining int64) {
	m.remainingTokens.Record(context.Background(), remaining)
}

// --- Response-cache hooks (func()-shaped so they bind directly to cache.Store
// callbacks). ---

// IncRespCacheHit records a fresh response-cache hit.
func (m *Metrics) IncRespCacheHit() {
	m.respCacheLookups.Add(context.Background(), 1, optCacheHit)
}

// IncRespCacheMiss records a response-cache miss: the response was cache
// eligible but was not already cached.
func (m *Metrics) IncRespCacheMiss() {
	m.respCacheLookups.Add(context.Background(), 1, optCacheMiss)
}

// IncRespCacheUncacheable records a lookup whose response the cache was never
// eligible to serve. These can never become hits, so they are excluded from
// hit rate and reported separately.
func (m *Metrics) IncRespCacheUncacheable() {
	m.respCacheLookups.Add(context.Background(), 1, optCacheUncacheable)
}

// IncRespCacheStore records a response stored in the cache.
func (m *Metrics) IncRespCacheStore() {
	m.respCacheOperations.Add(context.Background(), 1, optRespOpStore)
}

// IncRespCacheSkip records a response skipped for caching.
func (m *Metrics) IncRespCacheSkip() {
	m.respCacheOperations.Add(context.Background(), 1, optRespOpSkip)
}

// IncRespCachePurge records a response-cache purge operation.
func (m *Metrics) IncRespCachePurge() {
	m.respCacheOperations.Add(context.Background(), 1, optRespOpPurge)
}

// ObserveRespCacheBodySize records a cached body size in bytes.
func (m *Metrics) ObserveRespCacheBodySize(size float64) {
	m.respCacheBodySize.Record(context.Background(), int64(size))
}

// --- External rate-limit hooks (func()-shaped for ExternalClient callbacks). ---

// IncExtRLSemaphoreRejected records an external RL request rejected by the
// concurrency semaphore.
func (m *Metrics) IncExtRLSemaphoreRejected() {
	m.extRLSemaphoreRejected.Add(context.Background(), 1)
}

// IncExtRLSingleflightShared records an external RL request that shared a
// singleflight result.
func (m *Metrics) IncExtRLSingleflightShared() {
	m.extRLSingleflightShared.Add(context.Background(), 1)
}

// IncExtRLCacheHit records a fresh external RL cache hit.
func (m *Metrics) IncExtRLCacheHit() {
	m.extRLCacheLookups.Add(context.Background(), 1, optCacheHit)
}

// IncExtRLCacheMiss records an external RL cache miss.
func (m *Metrics) IncExtRLCacheMiss() {
	m.extRLCacheLookups.Add(context.Background(), 1, optCacheMiss)
}

// IncExtRLCacheStaleHit records a stale external RL cache hit.
func (m *Metrics) IncExtRLCacheStaleHit() {
	m.extRLCacheLookups.Add(context.Background(), 1, optCacheStale)
}

// --- Observable-gauge state setters. ---

// SetRedisHealthy sets the reachability state (reported by the async gauge) for
// a pool. Unknown pools are ignored.
func (m *Metrics) SetRedisHealthy(pool string, healthy bool) {
	v, ok := m.redisHealthy[pool]
	if !ok {
		return
	}
	if healthy {
		v.Store(1)
	} else {
		v.Store(0)
	}
}

// SetConfigReloadTimestamp records now as the last successful config reload.
func (m *Metrics) SetConfigReloadTimestamp() {
	m.configReloadTS.Store(time.Now().Unix())
}

// SetTLSReloadTimestamp records now as the last successful TLS certificate reload.
func (m *Metrics) SetTLSReloadTimestamp() {
	m.tlsReloadTS.Store(time.Now().Unix())
}

// SetMTLSCAReloadTimestamp records now as the last successful mTLS CA reload.
func (m *Metrics) SetMTLSCAReloadTimestamp() {
	m.mtlsCAReloadTS.Store(time.Now().Unix())
}

// MetricsSnapshot holds a point-in-time copy of all atomic counters. Served as
// JSON by the /v1/stats admin endpoint.
type MetricsSnapshot struct {
	Allowed          int64 `json:"allowed"`
	Limited          int64 `json:"limited"`
	RedisErrors      int64 `json:"redis_errors"`
	FallbackUsed     int64 `json:"fallback_used"`
	ReadOnlyRetries  int64 `json:"readonly_retries"`
	KeyExtractErrors int64 `json:"key_extract_errors"`
	AuthErrors       int64 `json:"auth_errors"`
	AuthDenied       int64 `json:"auth_denied"`
	AuthCanceled     int64 `json:"auth_canceled"`
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
		AuthCanceled:     atomic.LoadInt64(&m.authCanceled),
	}
}

// StatusFamily collapses an HTTP status code to its family label
// ("1xx".."5xx"). Codes outside the standard 100-599 range are bucketed as
// "unknown" so the attribute stays bounded even when an upstream returns a
// garbage code.
func StatusFamily(code int) string {
	switch {
	case code >= 100 && code < 200:
		return "1xx"
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		return "unknown"
	}
}

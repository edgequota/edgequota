package observability

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestMetrics installs a MeterProvider backed by a manual reader and returns
// the Metrics plus a collect function so tests can assert on the actual emitted
// data points. The provider is installed before NewMetrics so every instrument
// binds to it directly.
func newTestMetrics(t *testing.T) (*Metrics, func() metricdata.ResourceMetrics) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	// Build the provider through the SAME meterProviderOptions() the production
	// InitMetrics uses (views + exemplar filter), so this harness can't mask a
	// missing option — only the reader differs (manual vs periodic-OTLP).
	mp := sdkmetric.NewMeterProvider(append(
		[]sdkmetric.Option{sdkmetric.WithReader(reader)},
		meterProviderOptions()...,
	)...)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetMeterProvider(noop.NewMeterProvider())
		_ = mp.Shutdown(context.Background())
	})

	m := NewMetrics(discardLogger())
	collect := func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		return rm
	}
	return m, collect
}

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name == name {
				return md
			}
		}
	}
	t.Fatalf("metric %q not found in collected data", name)
	return metricdata.Metrics{}
}

func sumInt64(t *testing.T, rm metricdata.ResourceMetrics, name string) int64 {
	t.Helper()
	md := findMetric(t, rm, name)
	sum, ok := md.Data.(metricdata.Sum[int64])
	require.Truef(t, ok, "metric %q is not an Int64 sum (%T)", name, md.Data)
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}

func TestSnapshot(t *testing.T) {
	t.Run("mirror atomics are independent of the OTel pipeline", func(t *testing.T) {
		// No MeterProvider installed: OTel instruments are no-ops, but the atomic
		// mirror counters that /v1/stats reads must still work.
		otel.SetMeterProvider(noop.NewMeterProvider())
		m := NewMetrics(discardLogger())

		m.IncAllowed()
		m.IncAllowed()
		m.IncLimited()
		m.IncRedisErrors("ratelimit")
		m.IncFallbackUsed("ratelimit")
		m.IncReadOnlyRetries()
		m.IncKeyExtractErrors()
		m.IncAuthErrors()
		m.IncAuthDenied()
		m.IncAuthCanceled()

		snap := m.Snapshot()
		assert.Equal(t, int64(2), snap.Allowed)
		assert.Equal(t, int64(1), snap.Limited)
		assert.Equal(t, int64(1), snap.RedisErrors)
		assert.Equal(t, int64(1), snap.FallbackUsed)
		assert.Equal(t, int64(1), snap.ReadOnlyRetries)
		assert.Equal(t, int64(1), snap.KeyExtractErrors)
		assert.Equal(t, int64(1), snap.AuthErrors)
		assert.Equal(t, int64(1), snap.AuthDenied)
		assert.Equal(t, int64(1), snap.AuthCanceled)
	})

	t.Run("auth canceled does not touch auth errors", func(t *testing.T) {
		otel.SetMeterProvider(noop.NewMeterProvider())
		m := NewMetrics(discardLogger())
		m.IncAuthCanceled()
		snap := m.Snapshot()
		assert.Equal(t, int64(1), snap.AuthCanceled)
		assert.Equal(t, int64(0), snap.AuthErrors)
	})
}

func TestRequestCounter(t *testing.T) {
	m, collect := newTestMetrics(t)

	ctx := context.Background()
	m.RecordRequest(ctx, "GET", 200, "HTTP/2.0")
	m.RecordRequest(ctx, "GET", 200, "HTTP/2.0")
	m.RecordRequest(ctx, "POST", 503, "HTTP/1.1")

	rm := collect()
	assert.Equal(t, int64(3), sumInt64(t, rm, metricRequests))

	// Status class is derived from the raw code, and protocol version is stripped
	// of its "HTTP/" prefix.
	md := findMetric(t, rm, metricRequests)
	sum := md.Data.(metricdata.Sum[int64])
	var saw5xx bool
	for _, dp := range sum.DataPoints {
		if class, ok := dp.Attributes.Value(attrStatusClass); ok && class.AsString() == "5xx" {
			saw5xx = true
			ver, _ := dp.Attributes.Value(attrNetProtocolVersion)
			assert.Equal(t, "1.1", ver.AsString())
		}
	}
	assert.True(t, saw5xx, "expected a 5xx data point")
}

func TestDecisionAndAuthOutcomeCounters(t *testing.T) {
	m, collect := newTestMetrics(t)

	m.IncAllowed()
	m.IncAllowed()
	m.IncLimited()
	m.IncAuthErrors()
	m.IncAuthDenied()
	m.IncAuthCanceled()

	rm := collect()
	assert.Equal(t, int64(3), sumInt64(t, rm, metricRatelimitDecisions))
	assert.Equal(t, int64(3), sumInt64(t, rm, metricAuthOutcomes))
}

func TestActiveAndStreamingGauges(t *testing.T) {
	m, collect := newTestMetrics(t)

	m.IncRequestsInFlight()
	m.IncRequestsInFlight()
	m.DecRequestsInFlight()

	m.IncStreamingConn("sse")
	m.IncStreamingConn("ws")
	m.DecStreamingActive("sse")

	rm := collect()

	active := findMetric(t, rm, metricActiveRequests).Data.(metricdata.Sum[int64])
	var activeTotal int64
	for _, dp := range active.DataPoints {
		activeTotal += dp.Value
	}
	assert.Equal(t, int64(1), activeTotal, "in-flight gauge should net to 1")

	// streaming.connections is a monotonic counter (2 opens); active nets to 1.
	assert.Equal(t, int64(2), sumInt64(t, rm, metricStreamingConnections))
	streamActive := findMetric(t, rm, metricStreamingActive).Data.(metricdata.Sum[int64])
	var streamTotal int64
	for _, dp := range streamActive.DataPoints {
		streamTotal += dp.Value
	}
	assert.Equal(t, int64(1), streamTotal)
}

func TestHistogramsUseExplicitBuckets(t *testing.T) {
	m, collect := newTestMetrics(t)

	ctx := context.Background()
	m.ObserveBackendDuration(ctx, 0.03)
	m.ObserveAuthDuration(ctx, 0.002)
	m.ObserveExternalRLDuration(ctx, 0.5)
	m.ObserveStreamingDuration(ctx, "sse", 42)
	m.ObserveRemaining(17)
	m.ObserveRespCacheBodySize(2048)

	rm := collect()

	backend := findMetric(t, rm, metricBackendDuration).Data.(metricdata.Histogram[float64])
	require.Len(t, backend.DataPoints, 1)
	assert.Equal(t, uint64(1), backend.DataPoints[0].Count)
	// The View pinned the backend boundaries; the default OTel boundaries would
	// differ, so this both proves the View applied and guards the bucket model.
	assert.Equal(t, backendBuckets, backend.DataPoints[0].Bounds)

	tokens := findMetric(t, rm, metricRatelimitRemaining).Data.(metricdata.Histogram[int64])
	require.Len(t, tokens.DataPoints, 1)
	assert.Equal(t, remainingTokenBuckets, tokens.DataPoints[0].Bounds)
}

func TestExemplarsLinkDurationToTrace(t *testing.T) {
	// The Observe*Duration methods take a ctx specifically so the recorded point
	// carries an exemplar linking it to the trace. With a sampled tracer and the
	// TraceBasedFilter (installed via meterProviderOptions), recording on a
	// span-carrying context must attach an exemplar with that span's trace ID.
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(nooptrace.NewTracerProvider())
		_ = tp.Shutdown(context.Background())
	})

	m, collect := newTestMetrics(t)

	ctx, span := tp.Tracer("test").Start(context.Background(), "unit")
	m.ObserveBackendDuration(ctx, 0.02)
	span.End()

	rm := collect()
	backend := findMetric(t, rm, metricBackendDuration).Data.(metricdata.Histogram[float64])
	require.Len(t, backend.DataPoints, 1)
	require.NotEmpty(t, backend.DataPoints[0].Exemplars,
		"a duration recorded on a sampled span must carry an exemplar")

	wantTrace := span.SpanContext().TraceID()
	assert.Equal(t, wantTrace[:], backend.DataPoints[0].Exemplars[0].TraceID,
		"exemplar must link to the recording trace")
}

func TestObservableGauges(t *testing.T) {
	m, collect := newTestMetrics(t)

	// Pools start healthy; flip one to unhealthy.
	m.SetRedisHealthy(poolRateLimit, false)
	m.SetRedisHealthy("nonexistent-pool", false) // ignored, must not panic
	m.SetConfigReloadTimestamp()

	rm := collect()

	healthy := findMetric(t, rm, metricRedisHealthy).Data.(metricdata.Gauge[int64])
	byPool := map[string]int64{}
	for _, dp := range healthy.DataPoints {
		pool, _ := dp.Attributes.Value(attrRedisPool)
		byPool[pool.AsString()] = dp.Value
	}
	assert.Equal(t, int64(0), byPool[poolRateLimit], "ratelimit pool flipped unhealthy")
	assert.Equal(t, int64(1), byPool[poolResponseCache], "other pools stay healthy")

	reload := findMetric(t, rm, metricReloadTimestamp).Data.(metricdata.Gauge[float64])
	var configTS float64
	for _, dp := range reload.DataPoints {
		if target, _ := dp.Attributes.Value(attrReloadTarget); target.AsString() == reloadTargetConfig {
			configTS = dp.Value
		}
	}
	assert.Greater(t, configTS, float64(0), "config reload timestamp should be set")
}

// TestRecordAllInstruments exercises every remaining record path once, both for
// coverage and to prove no method panics against a live provider.
func TestRecordAllInstruments(t *testing.T) {
	m, collect := newTestMetrics(t)
	ctx := context.Background()

	m.IncRedisErrors("ratelimit")
	m.IncFallbackUsed("ratelimit")
	m.IncReadOnlyRetries()
	m.IncKeyExtractErrors()
	m.IncTenantKeyRejected()
	m.IncEventsDropped()
	m.IncEventsSendFailures()
	m.IncPreflightBypassed()
	m.IncConcurrencyRejected()
	m.IncConnection("HTTP/1.1", "mtls")
	m.ObserveRemaining(5)
	m.IncRespCacheHit()
	m.IncRespCacheMiss()
	m.IncRespCacheUncacheable()
	m.IncRespCacheStore()
	m.IncRespCacheSkip()
	m.IncRespCachePurge()
	m.ObserveRespCacheBodySize(1024)
	m.IncExtRLSemaphoreRejected()
	m.IncExtRLSingleflightShared()
	m.IncExtRLCacheHit()
	m.IncExtRLCacheMiss()
	m.IncExtRLCacheStaleHit()
	m.ObserveAuthDuration(ctx, 0.01)
	m.ObserveExternalRLDuration(ctx, 0.01)
	m.SetTLSReloadTimestamp()
	m.SetMTLSCAReloadTimestamp()

	rm := collect()
	assert.Equal(t, int64(3), sumInt64(t, rm, metricRespCacheLookups))
	assert.Equal(t, int64(3), sumInt64(t, rm, metricRespCacheOperations))
	assert.Equal(t, int64(3), sumInt64(t, rm, metricExtRLCacheLookups))
	assert.Equal(t, int64(1), sumInt64(t, rm, metricConnections))
}

func TestStatusFamily(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{0, "unknown"},
		{99, "unknown"},
		{100, "1xx"},
		{199, "1xx"},
		{200, "2xx"},
		{299, "2xx"},
		{300, "3xx"},
		{399, "3xx"},
		{400, "4xx"},
		{499, "4xx"},
		{500, "5xx"},
		{599, "5xx"},
		{600, "unknown"},
		{-1, "unknown"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, StatusFamily(c.code), "code=%d", c.code)
	}
}

func TestProtocolVersion(t *testing.T) {
	assert.Equal(t, "1.1", protocolVersion("HTTP/1.1"))
	assert.Equal(t, "2.0", protocolVersion("HTTP/2.0"))
	assert.Equal(t, "", protocolVersion("HTTP/"))
}

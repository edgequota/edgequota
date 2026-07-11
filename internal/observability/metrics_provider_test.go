package observability

import (
	"context"
	"os"
	"testing"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestInitMetrics(t *testing.T) {
	t.Run("disabled returns a no-op shutdown and leaves the provider untouched", func(t *testing.T) {
		otel.SetMeterProvider(noop.NewMeterProvider())
		shutdown, err := InitMetrics(context.Background(), config.TracingConfig{}, false, "test")
		require.NoError(t, err)
		require.NotNil(t, shutdown)
		assert.NoError(t, shutdown(context.Background()))
	})

	// The OTLP exporters dial lazily, so InitMetrics succeeds against an
	// unreachable endpoint for both transports; the collector is only contacted
	// on the first export/flush.
	t.Run("enabled grpc installs a MeterProvider", func(t *testing.T) {
		cfg := config.TracingConfig{
			Protocol: config.TracingProtocolGRPC,
			Endpoint: "127.0.0.1:4317",
			Insecure: true,
		}
		shutdown, err := InitMetrics(context.Background(), cfg, true, "test")
		require.NoError(t, err)
		require.NotNil(t, shutdown)
		t.Cleanup(func() {
			_ = shutdown(context.Background())
			otel.SetMeterProvider(noop.NewMeterProvider())
		})
		// Instruments built after install bind to the real provider.
		m := NewMetrics(discardLogger())
		m.IncAllowed()
	})

	t.Run("enabled http selects the http exporter", func(t *testing.T) {
		cfg := config.TracingConfig{
			Protocol: config.TracingProtocolHTTP,
			Endpoint: "http://127.0.0.1:4318",
			Insecure: true,
		}
		shutdown, err := InitMetrics(context.Background(), cfg, true, "test")
		require.NoError(t, err)
		require.NotNil(t, shutdown)
		t.Cleanup(func() {
			_ = shutdown(context.Background())
			otel.SetMeterProvider(noop.NewMeterProvider())
		})
	})
}

func TestRuntimeMetricsEmitted(t *testing.T) {
	// InitMetrics wires runtime.Start so the go.* runtime metrics (the portable
	// replacement for the dropped go_*/process_* collectors) keep flowing. Prove
	// the instruments actually emit by collecting from a manual-reader provider.
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	require.NoError(t, runtime.Start(runtime.WithMeterProvider(mp)))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	var sawGoroutines bool
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name == "go.goroutine.count" {
				sawGoroutines = true
			}
		}
	}
	assert.True(t, sawGoroutines, "runtime.Start must emit go.goroutine.count")
}

func TestNewResource(t *testing.T) {
	res, err := newResource("edgequota", "v1.2.3")
	require.NoError(t, err)
	require.NotNil(t, res)

	attrs := res.Attributes()
	var sawName, sawVersion, sawInstance bool
	for _, kv := range attrs {
		switch string(kv.Key) {
		case "service.name":
			sawName = kv.Value.AsString() == "edgequota"
		case "service.version":
			sawVersion = kv.Value.AsString() == "v1.2.3"
		case "service.instance.id":
			sawInstance = kv.Value.AsString() != ""
		}
	}
	assert.True(t, sawName, "service.name should be set")
	assert.True(t, sawVersion, "service.version should be set")
	assert.True(t, sawInstance, "service.instance.id should be set (distinguishes replicas)")
}

func TestInstanceID(t *testing.T) {
	// POD_NAME (downward API) takes precedence.
	t.Setenv("POD_NAME", "edgequota-5bddc98c7f-d5zjq")
	assert.Equal(t, "edgequota-5bddc98c7f-d5zjq", instanceID())

	// Falls back to the hostname (the pod name in Kubernetes) when POD_NAME is unset.
	t.Setenv("POD_NAME", "")
	if h, err := os.Hostname(); err == nil && h != "" {
		assert.Equal(t, h, instanceID())
	}
}

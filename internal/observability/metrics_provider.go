package observability

import (
	"context"
	"fmt"
	"os"

	"github.com/edgequota/edgequota/internal/config"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"google.golang.org/grpc/credentials/insecure"
)

// InitMetrics sets up a portable OpenTelemetry MeterProvider with an OTLP push
// exporter and installs it as the global provider, so that every instrument
// built from the global otel.Meter (in NewMetrics) — plus the otelhttp
// middleware's http.server.request.duration histogram — starts recording.
//
// Enablement is independent of tracing: the transport (endpoint, protocol,
// insecure) is borrowed from the tracing config, but this only initializes when
// the caller passes enabled=true (config.Metrics.Enabled). When disabled, the
// global provider is left as OpenTelemetry's no-op and every instrument is inert
// — no exporter, no goroutine, no push.
//
// Returns a shutdown function that should be called on application exit; it is a
// no-op when metrics are disabled.
func InitMetrics(ctx context.Context, cfg config.TracingConfig, enabled bool, version string) (func(context.Context) error, error) {
	if !enabled {
		return func(_ context.Context) error { return nil }, nil
	}

	exporter, err := newMetricExporter(ctx, cfg)
	if err != nil {
		return nil, err
	}

	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = "edgequota"
	}

	res, err := newResource(serviceName, version)
	if err != nil {
		return nil, err
	}

	opts := append([]sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	}, meterProviderOptions()...)
	mp := sdkmetric.NewMeterProvider(opts...)

	otel.SetMeterProvider(mp)

	// Go runtime metrics (go.goroutine.count, go.memory.*, go.gc.*) — the
	// portable OTel replacement for the go_*/process_* Prometheus collectors the
	// removed /metrics surface exposed, so goroutine-leak / heap-growth alerting
	// survives the migration. Registers async instruments on mp (no goroutine to
	// stop; collected on export). Explicitly best-effort: Start errors only on
	// invalid option config, which we never pass — and a failure must not drop
	// the first-party instruments or the provider shutdown, so it is ignored.
	_ = runtime.Start(runtime.WithMeterProvider(mp))

	return mp.Shutdown, nil
}

// meterProviderOptions returns the MeterProvider options that must be identical
// between the production provider (InitMetrics) and tests: the exemplar filter
// and the explicit-bucket histogram Views. Keeping them in one place means a
// test that builds a provider via this helper genuinely exercises the same
// aggregation the running binary uses — a missing View can't slip through green.
// Callers supply their own resource and reader.
func meterProviderOptions() []sdkmetric.Option {
	return []sdkmetric.Option{
		// Explicit-bucket boundaries for the first-party histograms. Without
		// this the SDK falls back to its millisecond-scale defaults, which would
		// collapse EdgeQuota's second/byte-scale distributions into one bucket.
		sdkmetric.WithView(metricViews()...),
		// Exemplars link a metric data point back to the trace that recorded it,
		// so a latency/error spike jumps straight to the offending span. The
		// TraceBasedFilter attaches an exemplar only when the measurement is
		// recorded on a context carrying a sampled span. This is the SDK default
		// — pinned here to make the posture explicit and survive a future
		// default change.
		sdkmetric.WithExemplarFilter(exemplar.TraceBasedFilter),
	}
}

// newMetricExporter creates an OTLP metric exporter for the configured
// transport, mirroring the tracing exporter's grpc/http selection so metrics
// and traces travel the same path to the collector.
//
//   - grpc (default): expects a bare host:port endpoint (e.g. "collector:4317").
//     When cfg.Insecure is true, plaintext gRPC is used.
//   - http: expects a full URL with scheme (e.g. "http://collector:4318").
//
// Cumulative temporality is pinned on both transports: the downstream (Dash0's
// OTLP→PromQL translation and any future rate() scalers) assumes monotonic
// cumulative series; delta temporality would silently break them. This is
// already the exporter default, but it is set explicitly so it can't be flipped
// by the OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE env var.
func newMetricExporter(ctx context.Context, cfg config.TracingConfig) (sdkmetric.Exporter, error) {
	switch cfg.ResolvedProtocol() {
	case config.TracingProtocolHTTP:
		opts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpointURL(cfg.Endpoint),
			otlpmetrichttp.WithTemporalitySelector(cumulativeTemporality),
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		exp, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("create otlp http metric exporter: %w", err)
		}
		return exp, nil

	default: // grpc
		opts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
			otlpmetricgrpc.WithTemporalitySelector(cumulativeTemporality),
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetricgrpc.WithTLSCredentials(insecure.NewCredentials()))
		}
		exp, err := otlpmetricgrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("create otlp grpc metric exporter: %w", err)
		}
		return exp, nil
	}
}

// cumulativeTemporality reports cumulative temporality for every instrument
// kind. Used to pin the OTLP metric exporter regardless of environment.
func cumulativeTemporality(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}

// newResource builds the OpenTelemetry resource carrying service identity. It is
// shared by the tracer and meter providers so traces and metrics report the same
// service.name / service.version / service.instance.id.
//
// service.instance.id distinguishes replicas: without it, every pod reports an
// identical resource, so an OTLP backend collapses their metric streams into one
// series (a merged cumulative counter mis-computes rates). The instance id is the
// pod name (POD_NAME downward-API env if set, else the hostname, which equals the
// pod name in Kubernetes).
func newResource(serviceName, version string) (*resource.Resource, error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version),
			semconv.ServiceInstanceID(instanceID()),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}
	return res, nil
}

// instanceID returns a per-replica identifier for service.instance.id: the
// POD_NAME env (Kubernetes downward API) when set, otherwise the hostname (which
// is the pod name in Kubernetes). Falls back to "unknown" only if both are
// unavailable, which does not happen in a running container.
func instanceID() string {
	if v := os.Getenv("POD_NAME"); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

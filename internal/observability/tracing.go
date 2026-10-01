package observability

import (
	"context"
	"fmt"
	"net/url"

	"github.com/edgequota/edgequota/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/credentials/insecure"
)

// InitTracing sets up OpenTelemetry tracing with an OTLP exporter.
// The transport protocol (gRPC or HTTP) is selected via cfg.Protocol;
// gRPC is the default when unset.
//
// The W3C TraceContext + Baggage propagator is always registered so that
// incoming traceparent/tracestate headers pass through even when export
// is disabled.
//
// Returns a shutdown function that should be called on application exit.
func InitTracing(ctx context.Context, cfg config.TracingConfig, version string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	)

	if !cfg.Enabled {
		return func(_ context.Context) error { return nil }, nil
	}

	exporter, err := newExporter(ctx, cfg)
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

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

// newExporter creates an OTLP span exporter for the configured protocol.
//
//   - grpc (default): expects a bare host:port endpoint (e.g. "collector:4317").
//     When cfg.Insecure is true, plaintext gRPC is used.
//   - http: expects a full URL with scheme (e.g. "http://collector:4318").
//     A URL without a path posts to /v1/traces (see endpointURLHasNoPath).
func newExporter(ctx context.Context, cfg config.TracingConfig) (*otlptrace.Exporter, error) {
	switch cfg.ResolvedProtocol() {
	case config.TracingProtocolHTTP:
		opts := []otlptracehttp.Option{
			otlptracehttp.WithEndpointURL(cfg.Endpoint),
		}
		if endpointURLHasNoPath(cfg.Endpoint) {
			opts = append(opts, otlptracehttp.WithURLPath(otlpTracesPath))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exp, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("create otlp http exporter: %w", err)
		}
		return exp, nil

	default: // grpc
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
		}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithTLSCredentials(insecure.NewCredentials()))
		}
		exp, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("create otlp grpc exporter: %w", err)
		}
		return exp, nil
	}
}

// Default OTLP/HTTP signal paths for an endpoint URL that has no path.
const (
	otlpTracesPath  = "/v1/traces"
	otlpMetricsPath = "/v1/metrics"
)

// endpointURLHasNoPath reports whether an OTLP/HTTP endpoint URL has an empty
// path, as in the documented form "http://collector:4318" that traces and
// metrics share. OpenTelemetry's HTTP exporters appended the signal path
// (/v1/traces, /v1/metrics) to such a URL up to v1.44.0; from v1.45.0 they
// post to "/" instead, so the callers set the signal path explicitly. A URL
// with any path, including "/", is used as given, as before. An unparsable
// URL is left to the exporter, which reports it.
func endpointURLHasNoPath(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.Path == ""
}

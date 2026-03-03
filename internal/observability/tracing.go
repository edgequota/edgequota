package observability

import (
	"context"
	"fmt"

	"github.com/edgequota/edgequota/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
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

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
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
func newExporter(ctx context.Context, cfg config.TracingConfig) (*otlptrace.Exporter, error) {
	switch cfg.ResolvedProtocol() {
	case config.TracingProtocolHTTP:
		opts := []otlptracehttp.Option{
			otlptracehttp.WithEndpointURL(cfg.Endpoint),
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

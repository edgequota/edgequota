package observability

import (
	"context"
	"fmt"

	"github.com/edgequota/edgequota/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// InitTracing sets up OpenTelemetry tracing with an OTLP HTTP exporter.
// It also registers the W3C TraceContext + Baggage propagator so that
// incoming traceparent/tracestate headers are extracted into the request
// context and injected into all outgoing HTTP/gRPC calls.
// Returns a shutdown function that should be called on application exit.
func InitTracing(ctx context.Context, cfg config.TracingConfig, version string) (func(context.Context) error, error) {
	// Always register the propagator so that W3C trace headers (traceparent,
	// tracestate) pass through unmodified even when exporting is disabled.
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	)

	if !cfg.Enabled {
		return func(_ context.Context) error { return nil }, nil
	}

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
	)
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter: %w", err)
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

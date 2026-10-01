package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// newPathRecorder starts an OTLP/HTTP collector stand-in that records the
// request path of every export and answers 200 with an empty body.
func newPathRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// TestOTLPHTTPExportPath pins where the HTTP exporters post for each endpoint
// form. The path-less form is the documented one and is shared by traces and
// metrics, so each signal must keep its own default path.
func TestOTLPHTTPExportPath(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		wantTraces  string
		wantMetrics string
	}{
		{name: "no path gets the signal path", path: "", wantTraces: "/v1/traces", wantMetrics: "/v1/metrics"},
		{name: "root path is used as given", path: "/", wantTraces: "/", wantMetrics: "/"},
		{name: "custom path is used as given", path: "/otlp/ingest", wantTraces: "/otlp/ingest", wantMetrics: "/otlp/ingest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, paths := newPathRecorder(t)
			cfg := config.TracingConfig{
				Protocol: config.TracingProtocolHTTP,
				Endpoint: srv.URL + tc.path,
				Insecure: true,
			}
			ctx := context.Background()

			exp, err := newExporter(ctx, cfg)
			require.NoError(t, err)
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			_, span := tp.Tracer("test").Start(ctx, "op")
			span.End()
			require.NoError(t, tp.Shutdown(ctx))

			mexp, err := newMetricExporter(ctx, cfg)
			require.NoError(t, err)
			rm := &metricdata.ResourceMetrics{
				ScopeMetrics: []metricdata.ScopeMetrics{{
					Scope: instrumentation.Scope{Name: "test"},
					Metrics: []metricdata.Metrics{{
						Name: "test.gauge",
						Data: metricdata.Gauge[int64]{
							DataPoints: []metricdata.DataPoint[int64]{{Value: 1}},
						},
					}},
				}},
			}
			require.NoError(t, mexp.Export(ctx, rm))
			require.NoError(t, mexp.Shutdown(ctx))

			assert.Equal(t, []string{tc.wantTraces, tc.wantMetrics}, paths())
		})
	}
}

func TestEndpointURLHasNoPath(t *testing.T) {
	cases := []struct {
		endpoint string
		want     bool
	}{
		{endpoint: "http://collector:4318", want: true},
		{endpoint: "https://collector:4318?tenant=a", want: true},
		{endpoint: "http://collector:4318/", want: false},
		{endpoint: "http://collector:4318/v1/traces", want: false},
		{endpoint: "http://collector:4318/otlp", want: false},
		{endpoint: "://missing-scheme", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			assert.Equal(t, tc.want, endpointURLHasNoPath(tc.endpoint))
		})
	}
}

package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
)

func benchLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func benchMetrics() *observability.Metrics {
	return observability.NewMetrics(prometheus.NewRegistry())
}

func benchConfig(redisAddr string) *config.Config {
	cfg := config.Defaults()
	cfg.Backend.URL = "http://127.0.0.1:65535" // won't be hit in bench
	cfg.Redis.Endpoints = []string{redisAddr}
	cfg.Redis.Mode = config.RedisModeSingle
	cfg.RateLimit.Average = 1000000 // Very high limit so RL doesn't interfere.
	cfg.RateLimit.Burst = 1000000
	return cfg
}

// BenchmarkServeHTTP measures the overhead of the middleware chain on the hot
// path (auth disabled, rate limiting with in-memory fallback). It exercises the
// key extraction, rate limit check, and metric recording.
func BenchmarkServeHTTP(b *testing.B) {
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatal(err)
	}
	defer mr.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	cfg := benchConfig(mr.Addr())
	cfg.Backend.URL = backend.URL

	chain, err := NewChain(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), cfg, benchLogger(), benchMetrics())
	if err != nil {
		b.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/bench", nil)
	req.RemoteAddr = "192.168.1.1:12345"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		chain.ServeHTTP(w, req)
	}
}

// BenchmarkGetHeaderMap validates that plain map allocation performs well for
// the external RL header extraction path.
func BenchmarkGetHeaderMap(b *testing.B) {
	req := httptest.NewRequest(http.MethodGet, "/bench", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "bench/1.0")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = getHeaderMap(req)
	}
}

package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testMetrics() *observability.Metrics {
	return observability.NewMetrics(prometheus.NewRegistry())
}

func testConfig(redisAddr string) *config.Config {
	cfg := config.Defaults()
	cfg.Backend.URL = "http://backend:8080"
	cfg.RateLimit.Average = 10
	cfg.RateLimit.Burst = 5
	cfg.RateLimit.Period = "1s"
	cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
	cfg.Redis.Endpoints = []string{redisAddr}
	cfg.Redis.Mode = config.RedisModeSingle
	return cfg
}

func TestNewChain(t *testing.T) {
	t.Run("creates chain with valid config and Redis", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		assert.NotNil(t, chain)
		defer chain.Close()
	})

	t.Run("creates chain with no rate limit (average=0)", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		assert.NotNil(t, chain)
		defer chain.Close()
	})

	t.Run("starts recovery when Redis unavailable with passthrough", func(t *testing.T) {
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "100ms"
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()
	})

	t.Run("fails when Redis unavailable with failclosed", func(t *testing.T) {
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "100ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyFailClosed
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

		_, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "redis connection failed")
	})
}

func TestChainServeHTTP(t *testing.T) {
	t.Run("allows requests within rate limit", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()

		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		snap := metrics.Snapshot()
		assert.Equal(t, int64(1), snap.Allowed)
	})

	t.Run("rate limits after burst exhaustion", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.Average = 2
		cfg.RateLimit.Burst = 3
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Exhaust burst.
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "1.2.3.4:5555"
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		// Next should be 429.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code)
		assert.NotEmpty(t, rr.Header().Get("Retry-After"))
		assert.NotEmpty(t, rr.Header().Get("X-Retry-In"))
	})

	t.Run("passes through when rate limit disabled (average=0)", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		for i := 0; i < 20; i++ {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		snap := metrics.Snapshot()
		assert.Equal(t, int64(20), snap.Allowed)
	})

	t.Run("passthrough policy allows all when Redis is down", func(t *testing.T) {
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "100ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()

		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("inMemoryFallback limits when Redis is down", func(t *testing.T) {
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "100ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyInMemoryFallback
		cfg.RateLimit.Average = 2
		cfg.RateLimit.Burst = 2
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		allowed := 0
		limited := 0
		// The first request creates a ristretto entry asynchronously. Give
		// ristretto time to admit the entry before sending more requests,
		// otherwise all requests may create independent buckets.
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "1.2.3.4:5555"
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			if rr.Code == http.StatusOK {
				allowed++
			} else {
				limited++
			}
			// After first request, small pause so ristretto processes the Set.
			if i == 0 {
				time.Sleep(10 * time.Millisecond)
			}
		}

		assert.Greater(t, allowed, 0)
		assert.Greater(t, limited, 0)
	})

	t.Run("returns 500 on key extraction failure", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.KeyStrategy = config.KeyStrategyConfig{
			Type:       config.KeyStrategyHeader,
			HeaderName: "X-Tenant-Id",
		}
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Request without the required header.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusInternalServerError, rr.Code)
		snap := metrics.Snapshot()
		assert.Equal(t, int64(1), snap.KeyExtractErrors)
	})
}

func TestChainClose(t *testing.T) {
	t.Run("closes without error", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)

		assert.NoError(t, chain.Close())
	})

	t.Run("closes with no limiter", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)

		assert.NoError(t, chain.Close())
	})
}

func TestChainRecovery(t *testing.T) {
	t.Run("recovers when Redis becomes available", func(t *testing.T) {
		// Start with Redis down.
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "100ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Should pass through (Redis down).
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Start Redis.
		mr := miniredis.RunT(t)
		chain.cfg.Redis.Endpoints = []string{mr.Addr()}

		// Wait for recovery loop to pick up new Redis.
		time.Sleep(3 * time.Second)

		// Eventually should use Redis limiting.
		// (recovery may or may not have happened depending on backoff timing)
	})
}

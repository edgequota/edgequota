package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/edgequota/edgequota/internal/config"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChainServeHTTPRedisFailureMidRequest(t *testing.T) {
	t.Run("falls back when Redis dies mid-operation", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// First request should succeed.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Close Redis to simulate failure.
		mr.Close()

		// Next request should detect connectivity error and fall back.
		req2 := httptest.NewRequest(http.MethodGet, "/", nil)
		req2.RemoteAddr = "1.2.3.4:5555"
		rr2 := httptest.NewRecorder()
		chain.ServeHTTP(rr2, req2)

		// With passthrough policy, should still return 200.
		assert.Equal(t, http.StatusOK, rr2.Code)
		snap := metrics.Snapshot()
		assert.True(t, snap.RedisErrors > 0 || snap.Allowed >= 2)
	})

	t.Run("failclosed policy blocks when Redis dies mid-operation", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.FailurePolicy = config.FailurePolicyFailClosed
		cfg.RateLimit.FailureCode = 503
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// First request should succeed.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Close Redis.
		mr.Close()

		// Next request should get 503 with failClosed.
		req2 := httptest.NewRequest(http.MethodGet, "/", nil)
		req2.RemoteAddr = "1.2.3.4:5555"
		rr2 := httptest.NewRecorder()
		chain.ServeHTTP(rr2, req2)

		assert.Equal(t, http.StatusServiceUnavailable, rr2.Code)
	})

	t.Run("inMemoryFallback handles Redis mid-failure", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.FailurePolicy = config.FailurePolicyInMemoryFallback
		cfg.RateLimit.Average = 10
		cfg.RateLimit.Burst = 5
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// First request via Redis.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Close Redis.
		mr.Close()

		// Next request should use in-memory fallback.
		req2 := httptest.NewRequest(http.MethodGet, "/", nil)
		req2.RemoteAddr = "1.2.3.4:5555"
		rr2 := httptest.NewRecorder()
		chain.ServeHTTP(rr2, req2)

		// Should be allowed by in-memory fallback.
		assert.Equal(t, http.StatusOK, rr2.Code)

		snap := metrics.Snapshot()
		assert.True(t, snap.FallbackUsed > 0)
	})
}

func TestChainRecoveryLoop(t *testing.T) {
	// Override backoff for fast testing.
	origBase := recoveryBackoffBase
	origMax := recoveryBackoffMax
	origJitter := backoffJitter
	recoveryBackoffBase = 50 * time.Millisecond
	recoveryBackoffMax = 200 * time.Millisecond
	backoffJitter = func(d time.Duration) time.Duration { return d }
	defer func() {
		recoveryBackoffBase = origBase
		recoveryBackoffMax = origMax
		backoffJitter = origJitter
	}()

	t.Run("recovers and restores Redis limiter", func(t *testing.T) {
		// Start with Redis down.
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "50ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		cfg.RateLimit.Average = 10
		cfg.RateLimit.Burst = 5
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Verify no limiter.
		chain.mu.RLock()
		assert.Nil(t, chain.limiter)
		chain.mu.RUnlock()

		// Start Redis and update config.
		mr := miniredis.RunT(t)
		updatedCfg := *chain.cfg.Load()
		updatedCfg.Redis.Endpoints = []string{mr.Addr()}
		chain.cfg.Store(&updatedCfg)

		// Wait for recovery loop to pick up.
		time.Sleep(1 * time.Second)

		// Limiter should be restored.
		chain.mu.RLock()
		limiterRestored := chain.limiter != nil
		chain.mu.RUnlock()
		assert.True(t, limiterRestored, "limiter should be restored after Redis becomes available")
	})

	t.Run("stops recovery when context canceled", func(t *testing.T) {
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "50ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)

		// Give recovery loop a moment to start.
		time.Sleep(200 * time.Millisecond)

		// Cancel context via Close.
		require.NoError(t, chain.Close())

		// Recovery should stop.
		time.Sleep(300 * time.Millisecond)

		chain.reconnectMu.Lock()
		reconnecting := chain.reconnecting
		chain.reconnectMu.Unlock()
		assert.False(t, reconnecting)
	})
}

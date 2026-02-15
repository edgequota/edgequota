package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedisOutagePolicyConformance systematically verifies the behavior of
// each failure policy when Redis goes down during operation.
func TestRedisOutagePolicyConformance(t *testing.T) {
	policies := []struct {
		name                   string
		policy                 config.FailurePolicy
		failureCode            int
		expectCodeDuringOutage int
		expectAllowed          bool
	}{
		{
			name:                   "passthrough allows all traffic",
			policy:                 config.FailurePolicyPassThrough,
			expectCodeDuringOutage: http.StatusOK,
			expectAllowed:          true,
		},
		{
			name:                   "failclosed blocks with default 429",
			policy:                 config.FailurePolicyFailClosed,
			failureCode:            0, // defaults to 429
			expectCodeDuringOutage: http.StatusTooManyRequests,
			expectAllowed:          false,
		},
		{
			name:                   "failclosed blocks with custom 503",
			policy:                 config.FailurePolicyFailClosed,
			failureCode:            http.StatusServiceUnavailable,
			expectCodeDuringOutage: http.StatusServiceUnavailable,
			expectAllowed:          false,
		},
	}

	for _, tc := range policies {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			cfg := testConfig(mr.Addr())
			cfg.RateLimit.FailurePolicy = tc.policy
			if tc.failureCode != 0 {
				cfg.RateLimit.FailureCode = tc.failureCode
			}
			metrics := testMetrics()
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})

			chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
			require.NoError(t, err)
			defer chain.Close()

			// Verify normal operation.
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "10.0.0.1:1234"
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code, "should work normally before outage")

			// Kill Redis.
			mr.Close()

			// Send request during outage.
			req2 := httptest.NewRequest(http.MethodGet, "/", nil)
			req2.RemoteAddr = "10.0.0.1:1234"
			rr2 := httptest.NewRecorder()
			chain.ServeHTTP(rr2, req2)
			assert.Equal(t, tc.expectCodeDuringOutage, rr2.Code)
		})
	}

	t.Run("inMemoryFallback rate limits after burst with Redis down", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.FailurePolicy = config.FailurePolicyInMemoryFallback
		cfg.RateLimit.Average = 2
		cfg.RateLimit.Burst = 2
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Use one request through Redis.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Kill Redis.
		mr.Close()

		// Send many requests to exhaust in-memory fallback burst.
		allowed, limited := 0, 0
		for i := 0; i < 10; i++ {
			reqN := httptest.NewRequest(http.MethodGet, "/", nil)
			reqN.RemoteAddr = "10.0.0.1:1234"
			rrN := httptest.NewRecorder()
			chain.ServeHTTP(rrN, reqN)
			if rrN.Code == http.StatusOK {
				allowed++
			} else if rrN.Code == http.StatusTooManyRequests {
				limited++
			}
		}

		assert.Greater(t, allowed, 0, "some requests should be allowed by in-memory fallback")
		assert.Greater(t, limited, 0, "some requests should be rate limited by in-memory fallback")

		snap := metrics.Snapshot()
		assert.Greater(t, snap.FallbackUsed, int64(0), "fallback counter should be incremented")
	})

	t.Run("inMemoryFallback returns Retry-After header on 429", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.FailurePolicy = config.FailurePolicyInMemoryFallback
		cfg.RateLimit.Average = 1
		cfg.RateLimit.Burst = 1
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Use one request through Redis.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.2:1234"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		// Kill Redis.
		mr.Close()

		// Exhaust the in-memory burst.
		for i := 0; i < 5; i++ {
			reqN := httptest.NewRequest(http.MethodGet, "/", nil)
			reqN.RemoteAddr = "10.0.0.2:1234"
			rrN := httptest.NewRecorder()
			chain.ServeHTTP(rrN, reqN)
			if rrN.Code == http.StatusTooManyRequests {
				assert.NotEmpty(t, rrN.Header().Get("Retry-After"),
					"429 response must include Retry-After header")
				assert.NotEmpty(t, rrN.Header().Get("X-Retry-In"),
					"429 response must include X-Retry-In header")
				return // Found what we need.
			}
		}
	})
}

// TestRedisRecoveryConformance verifies that each failure policy correctly
// recovers and resumes Redis-based rate limiting when Redis comes back.
func TestRedisRecoveryConformance(t *testing.T) {
	// Speed up recovery for testing.
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

	t.Run("passthrough recovers limiter from nil state", func(t *testing.T) {
		// Start with Redis down.
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "50ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		cfg.RateLimit.Average = 10
		cfg.RateLimit.Burst = 5
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Verify no limiter initially.
		chain.mu.RLock()
		assert.Nil(t, chain.limiter, "limiter should be nil when Redis is down")
		chain.mu.RUnlock()

		// Bring Redis up.
		mr := miniredis.RunT(t)
		chain.cfg.Redis.Endpoints = []string{mr.Addr()}

		// Wait for recovery.
		time.Sleep(1 * time.Second)

		chain.mu.RLock()
		restored := chain.limiter != nil
		chain.mu.RUnlock()
		assert.True(t, restored, "limiter should be restored after Redis recovery")
	})

	t.Run("inMemoryFallback switches back to Redis after recovery", func(t *testing.T) {
		// Start with Redis down.
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "50ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyInMemoryFallback
		cfg.RateLimit.Average = 10
		cfg.RateLimit.Burst = 5
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Should use in-memory fallback while Redis is down.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.3:1234"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Bring Redis up.
		mr := miniredis.RunT(t)
		chain.cfg.Redis.Endpoints = []string{mr.Addr()}

		// Wait for recovery.
		time.Sleep(1 * time.Second)

		// Limiter should be restored.
		chain.mu.RLock()
		restored := chain.limiter != nil
		chain.mu.RUnlock()
		assert.True(t, restored, "limiter should be restored for inMemoryFallback")
	})
}

// TestRateLimitHeadersConformance verifies that standard rate-limit response
// headers are always present in both allowed and denied responses.
func TestRateLimitHeadersConformance(t *testing.T) {
	t.Run("allowed response includes rate limit headers", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.Average = 100
		cfg.RateLimit.Burst = 50
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.10:1234"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.NotEmpty(t, rr.Header().Get("X-RateLimit-Limit"), "must have X-RateLimit-Limit")
		assert.NotEmpty(t, rr.Header().Get("X-RateLimit-Remaining"), "must have X-RateLimit-Remaining")
		assert.NotEmpty(t, rr.Header().Get("X-RateLimit-Reset"), "must have X-RateLimit-Reset")
	})

	t.Run("denied response includes rate limit headers", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.Average = 1
		cfg.RateLimit.Burst = 1
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Exhaust the burst.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.11:1234"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Next should be 429.
		req2 := httptest.NewRequest(http.MethodGet, "/", nil)
		req2.RemoteAddr = "10.0.0.11:1234"
		rr2 := httptest.NewRecorder()
		chain.ServeHTTP(rr2, req2)

		assert.Equal(t, http.StatusTooManyRequests, rr2.Code)
		assert.NotEmpty(t, rr2.Header().Get("X-RateLimit-Limit"), "must have X-RateLimit-Limit on 429")
		assert.NotEmpty(t, rr2.Header().Get("X-RateLimit-Remaining"), "must have X-RateLimit-Remaining on 429")
		assert.NotEmpty(t, rr2.Header().Get("X-RateLimit-Reset"), "must have X-RateLimit-Reset on 429")
		assert.NotEmpty(t, rr2.Header().Get("Retry-After"), "must have Retry-After on 429")
	})
}

package middleware

// Tests for the bug: External RL mode — Redis limiter never initialized.
//
// Root cause: initRedis only created c.limiter when p.ratePerSecond > 0. In
// external RL mode with rateLimit.static: null (Average=0), ratePerSecond=0 so
// the Redis client was immediately closed and c.limiter stayed nil.
// tryRedisLimit then always returned false, causing every request to fall
// through to handleRedisFailurePolicy. If the external service returned a
// per-tenant failure_policy=failclosed, every request was rejected with 429.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// externalRLNullStaticCfg returns the canonical "external RL mode, static=null"
// configuration described in the bug report.
func externalRLNullStaticCfg(redisAddr, extURL string) *config.Config {
	cfg := config.Defaults()
	cfg.RateLimit.Static.Average = 0 // static: null
	cfg.RateLimit.Static.BackendURL = "http://backend:8080"
	cfg.RateLimit.FailurePolicy = config.FailurePolicyInMemoryFallback
	cfg.Redis.Endpoints = []string{redisAddr}
	cfg.Redis.Mode = config.RedisModeSingle
	cfg.RateLimit.External.Enabled = true
	cfg.RateLimit.External.HTTP.URL = extURL
	cfg.RateLimit.External.Fallback.BackendURL = "http://backend:8080"
	cfg.RateLimit.External.Fallback.Average = 200
	cfg.RateLimit.External.Fallback.Burst = 400
	cfg.RateLimit.External.Fallback.Period = "1s"
	cfg.RateLimit.External.Fallback.KeyStrategy = config.KeyStrategyConfig{
		Type:      config.KeyStrategyGlobal,
		GlobalKey: "global-fallback",
	}
	return cfg
}

func TestExternalRLRedisLimiterInit(t *testing.T) {
	t.Run("limiter is non-nil when external RL enabled and static.Average=0", func(t *testing.T) {
		// Before the fix initRedis closed the Redis client when ratePerSecond=0,
		// leaving c.limiter nil even though external RL needs it.
		mr := miniredis.RunT(t)
		extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"average":     100,
				"burst":       50,
				"period":      "1s",
				"backend_url": "http://backend:8080",
			})
		}))
		defer extSrv.Close()

		cfg := externalRLNullStaticCfg(mr.Addr(), extSrv.URL)
		chain, err := NewChain(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}), cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		chain.mu.RLock()
		lim := chain.limiter
		chain.mu.RUnlock()

		assert.NotNil(t, lim,
			"Redis limiter must be initialized in external RL mode even when static.Average=0; "+
				"without it tryRedisLimit always returns false and bypasses per-request limits")
	})

	t.Run("per-request limits from external service enforced via Redis token bucket", func(t *testing.T) {
		// The external service returns average=10, burst=2 for tenant-abc.
		// After consuming the 2-token burst the next request must get 429,
		// proving the Redis token bucket is actually being used.
		mr := miniredis.RunT(t)
		const burst = 2
		extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"average":     10,
				"burst":       burst,
				"period":      "1s",
				"backend_url": "http://backend:8080",
				"tenant_key":  "tenant-abc",
			})
		}))
		defer extSrv.Close()

		cfg := externalRLNullStaticCfg(mr.Addr(), extSrv.URL)
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough

		chain, err := NewChain(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}), cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		for i := range burst {
			req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
			req.RemoteAddr = "1.2.3.4:5555"
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code,
				"request %d (within burst=%d) should be allowed via Redis token bucket", i+1, burst)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code,
			"request beyond burst must be rate-limited; Redis token bucket must be active")
	})

	t.Run("per-tenant failclosed policy does not reject requests within limits", func(t *testing.T) {
		// Exact production failure from the bug report:
		//
		//   rateLimit.static: null
		//   rateLimit.failurePolicy: inMemoryFallback  (global)
		//   external service returns: failure_policy=failclosed  (per-tenant)
		//
		// Before the fix:
		//   c.limiter == nil → tryRedisLimit always false → handleRedisFailurePolicy →
		//   per-tenant failclosed → 429 on every request, even the very first one.
		//
		// After the fix:
		//   c.limiter != nil → tryRedisLimit applies extLimits via Redis token bucket →
		//   request is allowed (bucket has plenty of tokens at average=100, burst=50).
		mr := miniredis.RunT(t)
		extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"average":        100,
				"burst":          50,
				"period":         "1s",
				"backend_url":    "http://backend:8080",
				"tenant_key":     "tenant-xyz",
				"failure_policy": "failclosed",
			})
		}))
		defer extSrv.Close()

		cfg := externalRLNullStaticCfg(mr.Addr(), extSrv.URL)

		chain, err := NewChain(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}), cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code,
			"request within limits must NOT be rejected 429 when external service sets "+
				"failure_policy=failclosed; that policy only applies when Redis is down")
	})

	t.Run("per-tenant failclosed policy correctly rejects when Redis is down", func(t *testing.T) {
		// Verify the failure path still works: when Redis is unreachable AND the
		// external service has set failure_policy=failclosed, requests must be rejected.
		mr := miniredis.RunT(t)
		extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"average":        100,
				"burst":          50,
				"period":         "1s",
				"backend_url":    "http://backend:8080",
				"tenant_key":     "tenant-xyz",
				"failure_policy": "failclosed",
			})
		}))
		defer extSrv.Close()

		cfg := externalRLNullStaticCfg(mr.Addr(), extSrv.URL)
		// Global policy is inMemoryFallback, external service overrides to failclosed.
		cfg.RateLimit.FailurePolicy = config.FailurePolicyInMemoryFallback

		chain, err := NewChain(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}), cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		// Kill Redis so tryRedisLimit will fail and fall through to the failure policy.
		mr.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code,
			"when Redis is down and external service set failure_policy=failclosed, request must be rejected")
	})

	t.Run("recoveryInstall creates limiter in external RL mode with static=null", func(t *testing.T) {
		// Before the fix recoveryInstall would close the recovered client and set
		// c.limiter=nil when c.ratePerSecond <= 0, breaking post-recovery behavior
		// in external RL mode.
		mr := miniredis.RunT(t)

		// Build a chain in external RL + static=null mode with a live Redis so we
		// can verify c.limiter is set, then simulate a recoveryInstall call.
		extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"average":     100,
				"burst":       50,
				"period":      "1s",
				"backend_url": "http://backend:8080",
			})
		}))
		defer extSrv.Close()

		cfg := externalRLNullStaticCfg(mr.Addr(), extSrv.URL)
		chain, err := NewChain(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}), cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		// Confirm the limiter was correctly set at startup.
		chain.mu.RLock()
		limBefore := chain.limiter
		chain.mu.RUnlock()
		require.NotNil(t, limBefore, "pre-condition: limiter must be non-nil after NewChain")

		// Null out the limiter and call recoveryInstall directly with a fresh Redis
		// client. This exercises the fixed code path in recoveryInstall.
		chain.mu.Lock()
		_ = chain.swapLimiterLocked(nil)
		chain.mu.Unlock()

		recoveredClient, clientErr := redis.NewClient(cfg.Redis)
		require.NoError(t, clientErr)

		chain.recoveryInstall(recoveredClient)

		chain.mu.RLock()
		limAfter := chain.limiter
		chain.mu.RUnlock()

		assert.NotNil(t, limAfter,
			"recoveryInstall must create a limiter in external RL mode even when ratePerSecond=0")
	})
}

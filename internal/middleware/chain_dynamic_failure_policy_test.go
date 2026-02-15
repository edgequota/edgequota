package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/ratelimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDynamicFailurePolicyOverride verifies that the external rate-limit service
// can dynamically override the static failure policy on a per-request basis.
// This enables use cases like:
//   - Planned maintenance: override "failclosed" → "passthrough" to let users through.
//   - Active attack: override "passthrough" → "failclosed" to block everyone.
func TestDynamicFailurePolicyOverride(t *testing.T) {
	t.Run("external service overrides failclosed to passthrough (maintenance mode)", func(t *testing.T) {
		// Static config is failclosed. External service says "passthrough" because
		// Redis is down for planned maintenance and we want to let users through.
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.FailurePolicy = config.FailurePolicyFailClosed
		cfg.RateLimit.FailureCode = 503
		cfg.RateLimit.External.Enabled = true

		extRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"average":        100,
				"burst":          50,
				"period":         "1s",
				"failure_policy": "passthrough",
			})
		}))
		defer extRL.Close()
		cfg.RateLimit.External.HTTP.URL = extRL.URL

		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Kill Redis to trigger failure policy.
		mr.Close()

		// Send a request — should be allowed because external service said passthrough.
		req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		// Without the override, failclosed would have returned 503.
		// With the override, passthrough lets the request through.
		assert.Equal(t, http.StatusOK, rr.Code,
			"external service overrode failclosed→passthrough, request should be allowed")
	})

	t.Run("external service overrides passthrough to failclosed (attack mode)", func(t *testing.T) {
		// Static config is passthrough. External service says "failclosed" because
		// it detected an attack and wants to deny all traffic while Redis is down.
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		cfg.RateLimit.External.Enabled = true

		extRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"average":        100,
				"burst":          50,
				"period":         "1s",
				"failure_policy": "failclosed",
				"failure_code":   429,
			})
		}))
		defer extRL.Close()
		cfg.RateLimit.External.HTTP.URL = extRL.URL

		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Kill Redis.
		mr.Close()

		// Send a request — should be blocked because external service said failclosed.
		req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		req.RemoteAddr = "10.0.0.2:1234"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		// Without the override, passthrough would have returned 200.
		// With the override, failclosed blocks with 429.
		assert.Equal(t, http.StatusTooManyRequests, rr.Code,
			"external service overrode passthrough→failclosed, request should be blocked")
	})

	t.Run("external service overrides failure code", func(t *testing.T) {
		// Static config uses failclosed with code 429. External service
		// overrides the code to 503 (Service Unavailable).
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.FailurePolicy = config.FailurePolicyFailClosed
		cfg.RateLimit.FailureCode = 429
		cfg.RateLimit.External.Enabled = true

		extRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"average":      100,
				"burst":        50,
				"period":       "1s",
				"failure_code": 503,
			})
		}))
		defer extRL.Close()
		cfg.RateLimit.External.HTTP.URL = extRL.URL

		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Kill Redis.
		mr.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		req.RemoteAddr = "10.0.0.3:1234"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		// Should be 503, not 429.
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
			"external service overrode failure code 429→503")
	})

	t.Run("invalid failure_policy from external service is ignored", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.FailurePolicy = config.FailurePolicyFailClosed
		cfg.RateLimit.FailureCode = 503
		cfg.RateLimit.External.Enabled = true

		extRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"average":        100,
				"burst":          50,
				"period":         "1s",
				"failure_policy": "totally_invalid_policy",
			})
		}))
		defer extRL.Close()
		cfg.RateLimit.External.HTTP.URL = extRL.URL

		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Kill Redis.
		mr.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		req.RemoteAddr = "10.0.0.4:1234"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		// Invalid policy should be ignored — static failclosed/503 applies.
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
			"invalid failure_policy should be ignored, static config should apply")
	})

	t.Run("empty override fields use static config", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		cfg.RateLimit.External.Enabled = true

		extRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// No failure_policy or failure_code — static config should apply.
			json.NewEncoder(w).Encode(map[string]any{
				"average": 100,
				"burst":   50,
				"period":  "1s",
			})
		}))
		defer extRL.Close()
		cfg.RateLimit.External.HTTP.URL = extRL.URL

		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Kill Redis.
		mr.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		req.RemoteAddr = "10.0.0.5:1234"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		// Static passthrough should apply — 200.
		assert.Equal(t, http.StatusOK, rr.Code,
			"empty override should use static passthrough config")
	})

	t.Run("override to inmemoryfallback works", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.FailurePolicy = config.FailurePolicyFailClosed
		cfg.RateLimit.FailureCode = 503
		cfg.RateLimit.Average = 2
		cfg.RateLimit.Burst = 2
		cfg.RateLimit.External.Enabled = true

		extRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"average":        100,
				"burst":          50,
				"period":         "1s",
				"failure_policy": "inmemoryfallback",
			})
		}))
		defer extRL.Close()
		cfg.RateLimit.External.HTTP.URL = extRL.URL

		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Kill Redis.
		mr.Close()

		// Should use in-memory fallback instead of failclosed.
		req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		req.RemoteAddr = "10.0.0.6:1234"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		// In-memory fallback with burst=2 should allow the first request.
		assert.Equal(t, http.StatusOK, rr.Code,
			"override to inmemoryfallback should allow requests within burst")

		snap := metrics.Snapshot()
		assert.Greater(t, snap.FallbackUsed, int64(0),
			"in-memory fallback counter should be incremented")
	})
}

// TestResolveFailurePolicy unit-tests the policy resolution logic directly.
func TestResolveFailurePolicy(t *testing.T) {
	makeChain := func(policy config.FailurePolicy, code int) *Chain {
		return &Chain{
			failurePolicy: policy,
			failureCode:   code,
			logger:        testLogger(),
		}
	}

	t.Run("nil extLimits returns static config", func(t *testing.T) {
		c := makeChain(config.FailurePolicyFailClosed, 503)
		fp, fc := c.resolveFailurePolicy(nil)
		assert.Equal(t, config.FailurePolicyFailClosed, fp)
		assert.Equal(t, 503, fc)
	})

	t.Run("empty override returns static config", func(t *testing.T) {
		c := makeChain(config.FailurePolicyFailClosed, 503)
		fp, fc := c.resolveFailurePolicy(&ratelimit.ExternalLimits{})
		assert.Equal(t, config.FailurePolicyFailClosed, fp)
		assert.Equal(t, 503, fc)
	})

	t.Run("valid policy override replaces static", func(t *testing.T) {
		c := makeChain(config.FailurePolicyFailClosed, 503)
		fp, fc := c.resolveFailurePolicy(&ratelimit.ExternalLimits{
			FailurePolicy: config.FailurePolicyPassThrough,
		})
		assert.Equal(t, config.FailurePolicyPassThrough, fp)
		assert.Equal(t, 503, fc) // Code not overridden.
	})

	t.Run("valid code override replaces static", func(t *testing.T) {
		c := makeChain(config.FailurePolicyFailClosed, 503)
		fp, fc := c.resolveFailurePolicy(&ratelimit.ExternalLimits{
			FailureCode: 429,
		})
		assert.Equal(t, config.FailurePolicyFailClosed, fp) // Policy not overridden.
		assert.Equal(t, 429, fc)
	})

	t.Run("both overrides replace static", func(t *testing.T) {
		c := makeChain(config.FailurePolicyFailClosed, 503)
		fp, fc := c.resolveFailurePolicy(&ratelimit.ExternalLimits{
			FailurePolicy: config.FailurePolicyPassThrough,
			FailureCode:   200,
		})
		assert.Equal(t, config.FailurePolicyPassThrough, fp)
		assert.Equal(t, 200, fc)
	})

	t.Run("invalid policy is ignored", func(t *testing.T) {
		c := makeChain(config.FailurePolicyFailClosed, 503)
		fp, fc := c.resolveFailurePolicy(&ratelimit.ExternalLimits{
			FailurePolicy: config.FailurePolicy("garbage"),
			FailureCode:   418,
		})
		assert.Equal(t, config.FailurePolicyFailClosed, fp) // Ignored.
		assert.Equal(t, 418, fc)                            // Code still applies.
	})
}

package ratelimit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Circuit Breaker Conformance Tests
// ---------------------------------------------------------------------------

func TestCircuitBreakerConformance(t *testing.T) {
	t.Run("opens after threshold failures and returns stale cache", func(t *testing.T) {
		redisClient, _ := newTestCacheRedis(t)
		var calls atomic.Int32

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := calls.Add(1)
			if n <= 1 {
				// First call succeeds — seeds the cache.
				json.NewEncoder(w).Encode(ExternalLimits{
					Average: 100,
					Burst:   50,
					Period:  "1s",
				})
				return
			}
			// Subsequent calls fail.
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			cacheTTL:       0, // no primary cache (force re-fetch)
			redisClient:    redisClient,
			headerFilter:   config.NewHeaderFilter(config.HeaderFilterConfig{}),
			cbThreshold:    3,
			cbResetTimeout: 5 * time.Second,
		}

		// First call succeeds, seeding stale cache.
		limits, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "cb-test"})
		require.NoError(t, err)
		assert.Equal(t, int64(100), limits.Average)

		// Trigger failures to open the circuit breaker.
		for i := 0; i < 3; i++ {
			_, _ = c.GetLimits(context.Background(), &ExternalRequest{Key: "cb-fail"})
		}

		// Circuit should be open now.
		assert.True(t, c.isCircuitOpen(), "circuit breaker should be open after threshold failures")

		// With circuit open, stale cache should be returned for the seeded key.
		staleLimits, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "cb-test"})
		require.NoError(t, err)
		assert.Equal(t, int64(100), staleLimits.Average)
	})

	t.Run("returns error when circuit open and no stale cache", func(t *testing.T) {
		c := &ExternalClient{
			httpURL:        "http://127.0.0.1:1",
			httpClient:     http.DefaultClient,
			timeout:        1e9,
			headerFilter:   config.NewHeaderFilter(config.HeaderFilterConfig{}),
			cbThreshold:    2,
			cbResetTimeout: 30 * time.Second,
		}

		// Trip the circuit breaker.
		for i := 0; i < 3; i++ {
			_, _ = c.GetLimits(context.Background(), &ExternalRequest{Key: "no-stale"})
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "no-stale"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "circuit breaker open")
	})

	t.Run("resets to closed after successful probe", func(t *testing.T) {
		var calls atomic.Int32

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := calls.Add(1)
			if n <= 3 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(ExternalLimits{
				Average: 200,
				Burst:   100,
				Period:  "1s",
			})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			cacheTTL:       0,
			headerFilter:   config.NewHeaderFilter(config.HeaderFilterConfig{}),
			cbThreshold:    3,
			cbResetTimeout: 10 * time.Millisecond, // Short timeout for testing.
		}

		// Trip the circuit breaker.
		for i := 0; i < 3; i++ {
			_, _ = c.GetLimits(context.Background(), &ExternalRequest{Key: "reset-test"})
		}
		assert.True(t, c.isCircuitOpen())

		// Wait for circuit to enter half-open state.
		time.Sleep(20 * time.Millisecond)

		// Next call should probe and succeed, resetting the circuit.
		limits, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "reset-test"})
		require.NoError(t, err)
		assert.Equal(t, int64(200), limits.Average)

		// Circuit should be closed now.
		assert.False(t, c.isCircuitOpen())
	})
}

// ---------------------------------------------------------------------------
// Stale-While-Revalidate Conformance Tests
// ---------------------------------------------------------------------------

func TestStaleWhileRevalidateConformance(t *testing.T) {
	t.Run("serves stale data on fetch failure after successful seed", func(t *testing.T) {
		redisClient, _ := newTestCacheRedis(t)
		var calls atomic.Int32

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := calls.Add(1)
			if n == 1 {
				w.Header().Set("Cache-Control", "max-age=1") // Very short TTL.
				json.NewEncoder(w).Encode(ExternalLimits{
					Average: 500,
					Burst:   250,
					Period:  "1s",
				})
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			cacheTTL:       60e9,
			headerFilter:   config.NewHeaderFilter(config.HeaderFilterConfig{}),
			redisClient:    redisClient,
			cbThreshold:    100, // High threshold so circuit stays closed.
			cbResetTimeout: 30 * time.Second,
		}

		// Seed the cache.
		limits, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "swr-test"})
		require.NoError(t, err)
		assert.Equal(t, int64(500), limits.Average)

		// Wait for primary cache to expire.
		time.Sleep(2 * time.Second)

		// Fetch should fail, but stale cache should return the seeded value.
		staleLimits, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "swr-test"})
		require.NoError(t, err)
		assert.Equal(t, int64(500), staleLimits.Average)
	})

	t.Run("stale cache has longer TTL than primary cache", func(t *testing.T) {
		redisClient, mr := newTestCacheRedis(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "max-age=10")
			json.NewEncoder(w).Encode(ExternalLimits{
				Average: 100,
				Burst:   50,
				Period:  "1s",
			})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			cacheTTL:       10e9,
			headerFilter:   config.NewHeaderFilter(config.HeaderFilterConfig{}),
			redisClient:    redisClient,
			cbThreshold:    5,
			cbResetTimeout: 30 * time.Second,
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "ttl-test"})
		require.NoError(t, err)

		// Primary cache TTL should be ~10s (from Cache-Control header).
		primaryTTL := mr.TTL(cacheKeyPrefix + "ttl-test")
		// Stale cache TTL should be ~5m (staleCacheTTL constant).
		staleTTL := mr.TTL(staleCachePrefix + "ttl-test")

		assert.True(t, primaryTTL.Seconds() <= 10, "primary TTL should be ~10s, got %v", primaryTTL)
		assert.True(t, staleTTL.Seconds() > 60, "stale TTL should be ~5m, got %v", staleTTL)
	})
}

// ---------------------------------------------------------------------------
// Cache Isolation Conformance Tests
// ---------------------------------------------------------------------------

func TestCacheIsolationConformance(t *testing.T) {
	t.Run("tenant_key provides cache isolation", func(t *testing.T) {
		redisClient, _ := newTestCacheRedis(t)
		var calls atomic.Int32

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req ExternalRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			calls.Add(1)

			resp := ExternalLimits{
				Average:   100,
				Burst:     50,
				Period:    "1s",
				TenantKey: "tenant:" + req.Key,
			}
			w.Header().Set("Cache-Control", "max-age=300")
			json.NewEncoder(w).Encode(resp)
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			cacheTTL:       60e9,
			headerFilter:   config.NewHeaderFilter(config.HeaderFilterConfig{}),
			redisClient:    redisClient,
			cbThreshold:    5,
			cbResetTimeout: 30 * time.Second,
		}

		// Request for tenant A.
		limitsA, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "tenant-a"})
		require.NoError(t, err)
		assert.Equal(t, "tenant:tenant-a", limitsA.TenantKey)

		// Request for tenant B.
		limitsB, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "tenant-b"})
		require.NoError(t, err)
		assert.Equal(t, "tenant:tenant-b", limitsB.TenantKey)

		// Both calls should have hit the server (different keys).
		assert.Equal(t, int32(2), calls.Load())

		// Repeat for tenant A — should come from cache.
		limitsA2, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "tenant-a"})
		require.NoError(t, err)
		assert.Equal(t, "tenant:tenant-a", limitsA2.TenantKey)
		assert.Equal(t, int32(2), calls.Load()) // No extra server call.
	})
}

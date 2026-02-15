package ratelimit

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

func newTestCacheRedis(t *testing.T) (redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := redis.NewClient(config.RedisConfig{
		Endpoints: []string{mr.Addr()},
		Mode:      config.RedisModeSingle,
	})
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	return client, mr
}

func int64Ptr(v int64) *int64 { return &v }

func TestNewExternalClient(t *testing.T) {
	t.Run("creates client with HTTP URL", func(t *testing.T) {
		cfg := config.ExternalRLConfig{
			Timeout:  "5s",
			CacheTTL: "30s",
			HTTP:     config.ExternalHTTPConfig{URL: "http://rl-service:8080/limits"},
		}
		c, err := NewExternalClient(cfg, nil)
		require.NoError(t, err)
		assert.NotNil(t, c)
		assert.Equal(t, "http://rl-service:8080/limits", c.httpURL)
		require.NoError(t, c.Close())
	})

	t.Run("defaults timeout for invalid value", func(t *testing.T) {
		cfg := config.ExternalRLConfig{
			Timeout:  "invalid",
			CacheTTL: "invalid",
			HTTP:     config.ExternalHTTPConfig{URL: "http://rl:8080"},
		}
		c, err := NewExternalClient(cfg, nil)
		require.NoError(t, err)
		assert.Equal(t, "5s", c.timeout.String())
		assert.Equal(t, "1m0s", c.cacheTTL.String())
		require.NoError(t, c.Close())
	})
}

func TestExternalClientGetLimitsHTTP(t *testing.T) {
	t.Run("returns limits from external service", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req ExternalRequest
			err := json.NewDecoder(r.Body).Decode(&req)
			require.NoError(t, err)
			assert.Equal(t, "tenant-1", req.Key)
			assert.Equal(t, "GET", req.Method)

			json.NewEncoder(w).Encode(ExternalLimits{
				Average: 100,
				Burst:   50,
				Period:  "1s",
			})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:    srv.URL,
			httpClient: http.DefaultClient,
			timeout:    5e9,
			cacheTTL:   0,
		}

		limits, err := c.GetLimits(context.Background(), &ExternalRequest{
			Key:    "tenant-1",
			Method: "GET",
			Path:   "/api/v1",
		})
		require.NoError(t, err)
		assert.Equal(t, int64(100), limits.Average)
		assert.Equal(t, int64(50), limits.Burst)
		assert.Equal(t, "1s", limits.Period)
	})

	t.Run("returns error for non-200 status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:    srv.URL,
			httpClient: http.DefaultClient,
			timeout:    5e9,
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "test"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "status 500")
	})

	t.Run("returns error for unreachable service", func(t *testing.T) {
		c := &ExternalClient{
			httpURL:    "http://127.0.0.1:1/limits",
			httpClient: http.DefaultClient,
			timeout:    5e9,
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "test"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "http request")
	})

	t.Run("returns error when no service configured", func(t *testing.T) {
		c := &ExternalClient{}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "test"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no external rate limit service")
	})
}

func TestExternalClientRedisCaching(t *testing.T) {
	t.Run("caches result in Redis on second call", func(t *testing.T) {
		redisClient, _ := newTestCacheRedis(t)
		callCount := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			callCount++
			w.Header().Set("Cache-Control", "max-age=300")
			json.NewEncoder(w).Encode(ExternalLimits{
				Average: 200,
				Burst:   100,
				Period:  "1s",
			})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:     srv.URL,
			httpClient:  http.DefaultClient,
			timeout:     5e9,
			cacheTTL:    60e9,
			redisClient: redisClient,
		}

		limits1, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "cached-key"})
		require.NoError(t, err)
		assert.Equal(t, int64(200), limits1.Average)
		assert.Equal(t, 1, callCount)

		limits2, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "cached-key"})
		require.NoError(t, err)
		assert.Equal(t, int64(200), limits2.Average)
		assert.Equal(t, 1, callCount)
	})

	t.Run("different keys get different cache entries", func(t *testing.T) {
		redisClient, _ := newTestCacheRedis(t)
		callCount := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			callCount++
			w.Header().Set("Cache-Control", "max-age=60")
			json.NewEncoder(w).Encode(ExternalLimits{
				Average: int64(100 * callCount),
				Burst:   50,
				Period:  "1s",
			})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:     srv.URL,
			httpClient:  http.DefaultClient,
			timeout:     5e9,
			cacheTTL:    60e9,
			redisClient: redisClient,
		}

		limits1, _ := c.GetLimits(context.Background(), &ExternalRequest{Key: "key-a"})
		limits2, _ := c.GetLimits(context.Background(), &ExternalRequest{Key: "key-b"})

		assert.Equal(t, int64(100), limits1.Average)
		assert.Equal(t, int64(200), limits2.Average)
		assert.Equal(t, 2, callCount)
	})

	t.Run("works without Redis client (no caching)", func(t *testing.T) {
		callCount := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			callCount++
			json.NewEncoder(w).Encode(ExternalLimits{Average: 50, Burst: 10, Period: "1s"})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:    srv.URL,
			httpClient: http.DefaultClient,
			timeout:    5e9,
			cacheTTL:   60e9,
		}

		_, _ = c.GetLimits(context.Background(), &ExternalRequest{Key: "k"})
		_, _ = c.GetLimits(context.Background(), &ExternalRequest{Key: "k"})
		assert.Equal(t, 2, callCount)
	})
}

func TestResolveHTTPCacheTTL(t *testing.T) {
	ec := &ExternalClient{cacheTTL: 60e9}

	t.Run("Cache-Control max-age takes highest priority", func(t *testing.T) {
		h := http.Header{}
		h.Set("Cache-Control", "max-age=300")
		limits := &ExternalLimits{CacheMaxAgeSec: int64Ptr(10)}
		assert.Equal(t, 300*1e9, float64(ec.resolveHTTPCacheTTL(h, limits)))
	})

	t.Run("parses max-age with other directives", func(t *testing.T) {
		h := http.Header{}
		h.Set("Cache-Control", "public, max-age=120, must-revalidate")
		assert.Equal(t, 120*1e9, float64(ec.resolveHTTPCacheTTL(h, &ExternalLimits{})))
	})

	t.Run("no-cache returns zero TTL regardless of body", func(t *testing.T) {
		h := http.Header{}
		h.Set("Cache-Control", "no-cache")
		limits := &ExternalLimits{CacheMaxAgeSec: int64Ptr(600)}
		assert.Equal(t, int64(0), int64(ec.resolveHTTPCacheTTL(h, limits)))
	})

	t.Run("no-store returns zero TTL regardless of body", func(t *testing.T) {
		h := http.Header{}
		h.Set("Cache-Control", "no-store")
		limits := &ExternalLimits{CacheMaxAgeSec: int64Ptr(600)}
		assert.Equal(t, int64(0), int64(ec.resolveHTTPCacheTTL(h, limits)))
	})

	t.Run("Expires header used when no Cache-Control", func(t *testing.T) {
		h := http.Header{}
		h.Set("Expires", "Thu, 01 Dec 2099 16:00:00 GMT")
		ttl := ec.resolveHTTPCacheTTL(h, &ExternalLimits{})
		assert.True(t, ttl > 0, "expected positive TTL from Expires header")
	})

	t.Run("expired Expires header returns zero TTL", func(t *testing.T) {
		h := http.Header{}
		h.Set("Expires", "Thu, 01 Jan 2000 00:00:00 GMT")
		assert.Equal(t, int64(0), int64(ec.resolveHTTPCacheTTL(h, &ExternalLimits{})))
	})

	t.Run("no headers falls back to body cache_max_age_seconds", func(t *testing.T) {
		h := http.Header{}
		limits := &ExternalLimits{CacheMaxAgeSec: int64Ptr(180)}
		assert.Equal(t, 180*1e9, float64(ec.resolveHTTPCacheTTL(h, limits)))
	})

	t.Run("no headers with body cache_no_store returns zero TTL", func(t *testing.T) {
		h := http.Header{}
		limits := &ExternalLimits{CacheNoStore: true}
		assert.Equal(t, int64(0), int64(ec.resolveHTTPCacheTTL(h, limits)))
	})

	t.Run("no headers and no body fields returns default TTL", func(t *testing.T) {
		h := http.Header{}
		assert.Equal(t, 60*1e9, float64(ec.resolveHTTPCacheTTL(h, &ExternalLimits{})))
	})
}

func TestResolveBodyCacheTTL(t *testing.T) {
	ec := &ExternalClient{cacheTTL: 60e9}

	t.Run("cache_no_store returns zero TTL", func(t *testing.T) {
		limits := &ExternalLimits{CacheNoStore: true, CacheMaxAgeSec: int64Ptr(300)}
		assert.Equal(t, int64(0), int64(ec.resolveBodyCacheTTL(limits)))
	})

	t.Run("cache_max_age_seconds sets TTL", func(t *testing.T) {
		limits := &ExternalLimits{CacheMaxAgeSec: int64Ptr(120)}
		assert.Equal(t, 120*1e9, float64(ec.resolveBodyCacheTTL(limits)))
	})

	t.Run("nil cache_max_age_seconds returns default TTL", func(t *testing.T) {
		limits := &ExternalLimits{}
		assert.Equal(t, 60*1e9, float64(ec.resolveBodyCacheTTL(limits)))
	})

	t.Run("zero cache_max_age_seconds treated as not set", func(t *testing.T) {
		limits := &ExternalLimits{CacheMaxAgeSec: int64Ptr(0)}
		assert.Equal(t, 60*1e9, float64(ec.resolveBodyCacheTTL(limits)))
	})
}

func TestHTTPBodyCacheFieldsEndToEnd(t *testing.T) {
	t.Run("body cache_max_age_seconds used when no HTTP headers", func(t *testing.T) {
		redisClient, mr := newTestCacheRedis(t)
		callCount := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			callCount++
			// No Cache-Control header — body fields should drive TTL.
			json.NewEncoder(w).Encode(ExternalLimits{
				Average:        100,
				Burst:          50,
				Period:         "1s",
				CacheMaxAgeSec: int64Ptr(600),
			})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:     srv.URL,
			httpClient:  http.DefaultClient,
			timeout:     5e9,
			cacheTTL:    10e9,
			redisClient: redisClient,
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "body-ttl"})
		require.NoError(t, err)
		assert.Equal(t, 1, callCount)

		// Verify the Redis key TTL is ~600s (from body), not 10s (default).
		ttl := mr.TTL(cacheKeyPrefix + "body-ttl")
		assert.True(t, ttl.Seconds() > 500, "expected TTL > 500s from body field, got %v", ttl)
	})

	t.Run("body cache_no_store prevents caching even without headers", func(t *testing.T) {
		redisClient, _ := newTestCacheRedis(t)
		callCount := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			callCount++
			json.NewEncoder(w).Encode(ExternalLimits{
				Average:      100,
				Burst:        50,
				Period:       "1s",
				CacheNoStore: true,
			})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:     srv.URL,
			httpClient:  http.DefaultClient,
			timeout:     5e9,
			cacheTTL:    60e9,
			redisClient: redisClient,
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "no-store"})
		require.NoError(t, err)
		_, err = c.GetLimits(context.Background(), &ExternalRequest{Key: "no-store"})
		require.NoError(t, err)
		assert.Equal(t, 2, callCount) // Both calls hit the server — not cached.
	})

	t.Run("HTTP header takes precedence over body field", func(t *testing.T) {
		redisClient, mr := newTestCacheRedis(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "max-age=30")
			json.NewEncoder(w).Encode(ExternalLimits{
				Average:        100,
				Burst:          50,
				Period:         "1s",
				CacheMaxAgeSec: int64Ptr(3600), // body says 1h
			})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:     srv.URL,
			httpClient:  http.DefaultClient,
			timeout:     5e9,
			cacheTTL:    60e9,
			redisClient: redisClient,
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Key: "header-wins"})
		require.NoError(t, err)

		// Header said 30s, body said 3600s — header should win.
		ttl := mr.TTL(cacheKeyPrefix + "header-wins")
		assert.True(t, ttl.Seconds() <= 30, "expected TTL ~30s from header, got %v", ttl)
	})
}

func TestExternalClientClose(t *testing.T) {
	t.Run("close with no grpc conn returns nil", func(t *testing.T) {
		c := &ExternalClient{}
		assert.NoError(t, c.Close())
	})
}

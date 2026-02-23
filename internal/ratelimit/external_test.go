package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"
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
			Timeout: "5s",
			HTTP:    config.ExternalHTTPConfig{URL: "http://rl-service:8080/limits"},
		}
		c, err := NewExternalClient(cfg, nil, nil)
		require.NoError(t, err)
		assert.NotNil(t, c)
		assert.Equal(t, "http://rl-service:8080/limits", c.httpURL)
		require.NoError(t, c.Close())
	})

	t.Run("defaults timeout for invalid value", func(t *testing.T) {
		cfg := config.ExternalRLConfig{
			Timeout: "invalid",
			HTTP:    config.ExternalHTTPConfig{URL: "http://rl:8080"},
		}
		c, err := NewExternalClient(cfg, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, "5s", c.timeout.String())
		require.NoError(t, c.Close())
	})
}

func TestExternalClientGetLimitsHTTP(t *testing.T) {
	t.Run("returns limits from external service", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req ExternalRequest
			err := json.NewDecoder(r.Body).Decode(&req)
			require.NoError(t, err)
			assert.Equal(t, "GET", req.Method)
			assert.Equal(t, "/api/v1", req.Path)

			json.NewEncoder(w).Encode(ExternalLimits{
				Average:    100,
				Burst:      50,
				Period:     "1s",
				BackendURL: "http://backend:8080",
			})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			fetchSem:       semaphore.NewWeighted(defaultMaxConcurrentFetches),
			maxBreakers:    defaultMaxCircuitBreakers,
			cbThreshold:    defaultCBThreshold,
			cbResetTimeout: defaultCBResetTimeout,
			done:           make(chan struct{}),
		}

		limits, err := c.GetLimits(context.Background(), &ExternalRequest{
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
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			fetchSem:       semaphore.NewWeighted(defaultMaxConcurrentFetches),
			maxBreakers:    defaultMaxCircuitBreakers,
			cbThreshold:    defaultCBThreshold,
			cbResetTimeout: defaultCBResetTimeout,
			done:           make(chan struct{}),
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/test"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "status 500")
	})

	t.Run("returns error for unreachable service", func(t *testing.T) {
		c := &ExternalClient{
			httpURL:        "http://127.0.0.1:1/limits",
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			fetchSem:       semaphore.NewWeighted(defaultMaxConcurrentFetches),
			maxBreakers:    defaultMaxCircuitBreakers,
			cbThreshold:    defaultCBThreshold,
			cbResetTimeout: defaultCBResetTimeout,
			done:           make(chan struct{}),
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/test"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "http request")
	})

	t.Run("returns error when no service configured", func(t *testing.T) {
		c := &ExternalClient{
			fetchSem:       semaphore.NewWeighted(defaultMaxConcurrentFetches),
			maxBreakers:    defaultMaxCircuitBreakers,
			cbThreshold:    defaultCBThreshold,
			cbResetTimeout: defaultCBResetTimeout,
			done:           make(chan struct{}),
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/test"})
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
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			redisClient:    redisClient,
			fetchSem:       semaphore.NewWeighted(defaultMaxConcurrentFetches),
			maxBreakers:    defaultMaxCircuitBreakers,
			cbThreshold:    defaultCBThreshold,
			cbResetTimeout: defaultCBResetTimeout,
			done:           make(chan struct{}),
		}

		limits1, err := c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/cached-key"})
		require.NoError(t, err)
		assert.Equal(t, int64(200), limits1.Average)
		assert.Equal(t, 1, callCount)

		limits2, err := c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/cached-key"})
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
				Average:    int64(100 * callCount),
				Burst:      50,
				Period:     "1s",
				BackendURL: "http://backend:8080",
			})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			redisClient:    redisClient,
			fetchSem:       semaphore.NewWeighted(defaultMaxConcurrentFetches),
			maxBreakers:    defaultMaxCircuitBreakers,
			cbThreshold:    defaultCBThreshold,
			cbResetTimeout: defaultCBResetTimeout,
			done:           make(chan struct{}),
		}

		limits1, _ := c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/key-a"})
		limits2, _ := c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/key-b"})

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
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			fetchSem:       semaphore.NewWeighted(defaultMaxConcurrentFetches),
			maxBreakers:    defaultMaxCircuitBreakers,
			cbThreshold:    defaultCBThreshold,
			cbResetTimeout: defaultCBResetTimeout,
			done:           make(chan struct{}),
		}

		_, _ = c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/k"})
		_, _ = c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/k"})
		assert.Equal(t, 2, callCount)
	})
}

func TestResolveHTTPCacheTTL(t *testing.T) {
	ec := &ExternalClient{
		maxBreakers: defaultMaxCircuitBreakers,
		done:        make(chan struct{}),
	}

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

	t.Run("no headers and no body fields returns zero TTL", func(t *testing.T) {
		h := http.Header{}
		assert.Equal(t, int64(0), int64(ec.resolveHTTPCacheTTL(h, &ExternalLimits{})))
	})
}

func TestResolveBodyCacheTTL(t *testing.T) {
	ec := &ExternalClient{
		maxBreakers: defaultMaxCircuitBreakers,
		done:        make(chan struct{}),
	}

	t.Run("cache_no_store returns zero TTL", func(t *testing.T) {
		limits := &ExternalLimits{CacheNoStore: true, CacheMaxAgeSec: int64Ptr(300)}
		assert.Equal(t, int64(0), int64(ec.resolveBodyCacheTTL(limits)))
	})

	t.Run("cache_max_age_seconds sets TTL", func(t *testing.T) {
		limits := &ExternalLimits{CacheMaxAgeSec: int64Ptr(120)}
		assert.Equal(t, 120*1e9, float64(ec.resolveBodyCacheTTL(limits)))
	})

	t.Run("nil cache_max_age_seconds returns zero TTL", func(t *testing.T) {
		limits := &ExternalLimits{}
		assert.Equal(t, int64(0), int64(ec.resolveBodyCacheTTL(limits)))
	})

	t.Run("zero cache_max_age_seconds treated as not set", func(t *testing.T) {
		limits := &ExternalLimits{CacheMaxAgeSec: int64Ptr(0)}
		assert.Equal(t, int64(0), int64(ec.resolveBodyCacheTTL(limits)))
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
				BackendURL:     "http://backend:8080",
				CacheMaxAgeSec: int64Ptr(600),
			})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			redisClient:    redisClient,
			fetchSem:       semaphore.NewWeighted(defaultMaxConcurrentFetches),
			maxBreakers:    defaultMaxCircuitBreakers,
			cbThreshold:    defaultCBThreshold,
			cbResetTimeout: defaultCBResetTimeout,
			done:           make(chan struct{}),
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/body-ttl"})
		require.NoError(t, err)
		assert.Equal(t, 1, callCount)

		// Verify the Redis key TTL is ~600s (from body), not 10s (default).
		ttl := mr.TTL(cacheKeyPrefix + "GET|/body-ttl")
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
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			redisClient:    redisClient,
			fetchSem:       semaphore.NewWeighted(defaultMaxConcurrentFetches),
			maxBreakers:    defaultMaxCircuitBreakers,
			cbThreshold:    defaultCBThreshold,
			cbResetTimeout: defaultCBResetTimeout,
			done:           make(chan struct{}),
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/no-store"})
		require.NoError(t, err)
		_, err = c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/no-store"})
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
				BackendURL:     "http://backend:8080",
				CacheMaxAgeSec: int64Ptr(3600), // body says 1h
			})
		}))
		defer srv.Close()

		c := &ExternalClient{
			httpURL:        srv.URL,
			httpClient:     http.DefaultClient,
			timeout:        5e9,
			redisClient:    redisClient,
			fetchSem:       semaphore.NewWeighted(defaultMaxConcurrentFetches),
			maxBreakers:    defaultMaxCircuitBreakers,
			cbThreshold:    defaultCBThreshold,
			cbResetTimeout: defaultCBResetTimeout,
			done:           make(chan struct{}),
		}

		_, err := c.GetLimits(context.Background(), &ExternalRequest{Method: "GET", Path: "/header-wins"})
		require.NoError(t, err)

		// Header said 30s, body said 3600s — header should win.
		ttl := mr.TTL(cacheKeyPrefix + "GET|/header-wins")
		assert.True(t, ttl.Seconds() <= 30, "expected TTL ~30s from header, got %v", ttl)
	})
}

func TestExternalClientClose(t *testing.T) {
	t.Run("close with no grpc conn returns nil", func(t *testing.T) {
		c := &ExternalClient{
			maxBreakers: defaultMaxCircuitBreakers,
			done:        make(chan struct{}),
		}
		assert.NoError(t, c.Close())
	})
}

func TestCircuitBreakerCap(t *testing.T) {
	c := &ExternalClient{
		httpURL:        "http://127.0.0.1:1",
		httpClient:     http.DefaultClient,
		timeout:        1e9,
		headerFilter:   config.NewHeaderFilter(config.HeaderFilterConfig{}),
		fetchSem:       semaphore.NewWeighted(defaultMaxConcurrentFetches),
		maxBreakers:    3, // Very low cap for testing.
		cbThreshold:    5,
		cbResetTimeout: 30 * time.Second,
		done:           make(chan struct{}),
	}
	defer c.Close()

	// Create 3 breakers (at cap).
	for i := range 3 {
		key := fmt.Sprintf("tenant-%d", i)
		cb := c.getOrCreateCB(key)
		assert.NotEqual(t, closedBreakerStub, cb, "breaker %d should be a real breaker", i)
	}
	assert.Equal(t, int64(3), c.breakerCount.Load())

	// 4th key should get the passthrough stub.
	cb := c.getOrCreateCB("overflow-tenant")
	assert.Equal(t, closedBreakerStub, cb, "excess key should get closedBreakerStub")

	// Existing keys should still return their real breakers.
	cb = c.getOrCreateCB("tenant-0")
	assert.NotEqual(t, closedBreakerStub, cb, "existing key should still get real breaker")
}

func TestExternalClientGoroutineLeak(t *testing.T) {
	// Create and close multiple clients, verifying no goroutine leak.
	before := runtime.NumGoroutine()

	for range 5 {
		c := &ExternalClient{
			httpURL:        "http://127.0.0.1:1",
			httpClient:     http.DefaultClient,
			timeout:        1e9,
			headerFilter:   config.NewHeaderFilter(config.HeaderFilterConfig{}),
			fetchSem:       semaphore.NewWeighted(1),
			maxBreakers:    10,
			cbThreshold:    5,
			cbResetTimeout: 100 * time.Millisecond,
			done:           make(chan struct{}),
		}
		// Start the eviction goroutine like the constructor does.
		go func() {
			ticker := time.NewTicker(c.cbResetTimeout)
			defer ticker.Stop()
			for {
				select {
				case <-c.done:
					return
				case <-ticker.C:
					c.evictStaleBreakers()
				}
			}
		}()
		_ = c.Close()
	}

	// Give goroutines time to exit.
	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()

	// Allow some slack for test infrastructure goroutines.
	assert.LessOrEqual(t, after, before+2,
		"goroutine count should not grow after closing clients (before=%d, after=%d)", before, after)
}

func TestSetMetricHooks(t *testing.T) {
	c := &ExternalClient{done: make(chan struct{})}

	var semCalled, sfCalled bool
	c.SetMetricHooks(func() { semCalled = true }, func() { sfCalled = true })

	c.onSemaphoreReject()
	c.onSingleflightShare()
	assert.True(t, semCalled)
	assert.True(t, sfCalled)
}

func TestSetCacheMetricHooks(t *testing.T) {
	c := &ExternalClient{done: make(chan struct{})}

	var hitCalled, missCalled, staleCalled bool
	c.SetCacheMetricHooks(
		func() { hitCalled = true },
		func() { missCalled = true },
		func() { staleCalled = true },
	)

	c.onCacheHit()
	c.onCacheMiss()
	c.onCacheStaleHit()
	assert.True(t, hitCalled)
	assert.True(t, missCalled)
	assert.True(t, staleCalled)
}

func TestTenantCBIsStale(t *testing.T) {
	cb := newTenantCB(5, 30*time.Second)
	now := time.Now()

	t.Run("not stale when recently accessed", func(t *testing.T) {
		assert.False(t, cb.isStale(now, time.Minute))
	})

	t.Run("stale when idle exceeds threshold", func(t *testing.T) {
		future := now.Add(2 * time.Minute)
		assert.True(t, cb.isStale(future, time.Minute))
	})
}

func TestEvictStaleBreakers(t *testing.T) {
	c := &ExternalClient{
		cbThreshold:    5,
		cbResetTimeout: 50 * time.Millisecond,
		maxBreakers:    100,
		done:           make(chan struct{}),
	}

	cb := newTenantCB(5, 50*time.Millisecond)
	cb.mu.Lock()
	cb.lastAccess = time.Now().Add(-10 * time.Minute)
	cb.mu.Unlock()
	c.breakers.Store("stale-tenant", cb)
	c.breakerCount.Store(1)

	fresh := newTenantCB(5, 50*time.Millisecond)
	c.breakers.Store("fresh-tenant", fresh)
	c.breakerCount.Add(1)

	c.evictStaleBreakers()

	_, staleFound := c.breakers.Load("stale-tenant")
	_, freshFound := c.breakers.Load("fresh-tenant")
	assert.False(t, staleFound, "stale breaker should be evicted")
	assert.True(t, freshFound, "fresh breaker should remain")
	assert.Equal(t, int64(1), c.breakerCount.Load())
}

func TestFailurePolicyFromProto(t *testing.T) {
	assert.Equal(t, config.FailurePolicyPassThrough, failurePolicyFromProto(1))
	assert.Equal(t, config.FailurePolicyFailClosed, failurePolicyFromProto(2))
	assert.Equal(t, config.FailurePolicyInMemoryFallback, failurePolicyFromProto(3))
	assert.Equal(t, config.FailurePolicy(""), failurePolicyFromProto(0))
	assert.Equal(t, config.FailurePolicy(""), failurePolicyFromProto(99))
}

func TestExternalClientFilterHeaders(t *testing.T) {
	c := &ExternalClient{
		headerFilter: config.NewHeaderFilter(config.HeaderFilterConfig{
			DenyList: []string{"Authorization", "Cookie"},
		}),
		done: make(chan struct{}),
	}

	headers := map[string]string{
		"Authorization": "Bearer tok",
		"Content-Type":  "application/json",
		"Cookie":        "session=abc",
		"X-Request-Id":  "123",
	}
	c.FilterHeaders(headers)

	assert.NotContains(t, headers, "Authorization")
	assert.NotContains(t, headers, "Cookie")
	assert.Equal(t, "application/json", headers["Content-Type"])
	assert.Equal(t, "123", headers["X-Request-Id"])
}

func TestExternalClientBuildFilteredHeaders(t *testing.T) {
	c := &ExternalClient{
		headerFilter: config.NewHeaderFilter(config.HeaderFilterConfig{
			AllowList: []string{"X-Request-Id", "Content-Type"},
		}),
		done: make(chan struct{}),
	}

	h := http.Header{
		"Authorization": {"Bearer tok"},
		"Content-Type":  {"application/json"},
		"X-Request-Id":  {"456"},
	}
	got := c.BuildFilteredHeaders(h)

	assert.Equal(t, "application/json", got["Content-Type"])
	assert.Equal(t, "456", got["X-Request-Id"])
	assert.NotContains(t, got, "Authorization")
}

// --- lookupKey tests ---

func newLookupKeyClient(t *testing.T) *ExternalClient {
	t.Helper()
	cfg := config.ExternalRLConfig{
		Timeout: "5s",
		HTTP:    config.ExternalHTTPConfig{URL: "http://rl:8080"},
	}
	c, err := NewExternalClient(cfg, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	return c
}

func TestLookupKey_EphemeralHeadersExcludedByDefault(t *testing.T) {
	c := newLookupKeyClient(t)

	base := &ExternalRequest{
		Method:  "GET",
		Path:    "/api/v1/data",
		Headers: map[string]string{"Host": "api.example.com", "X-Tenant-Id": "acme"},
	}
	baseKey := c.LookupKey(base)

	ephemeralHeaders := []string{
		"X-Request-Id", "X-Correlation-Id", "Request-Id",
		"Traceparent", "Tracestate",
		"X-B3-Traceid", "X-B3-Spanid", "X-B3-Parentspanid", "X-B3-Sampled",
		"Uber-Trace-Id", "X-Amzn-Trace-Id", "X-Cloud-Trace-Context",
		"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host",
		"X-Real-Ip", "Forwarded", "Via",
		"X-Envoy-Attempt-Count", "X-Envoy-External-Address",
		"Cf-Ray", "Cdn-Loop", "X-Request-Start",
		"True-Client-Ip", "B3",
	}

	for _, eh := range ephemeralHeaders {
		t.Run(eh, func(t *testing.T) {
			withEphemeral := &ExternalRequest{
				Method: "GET",
				Path:   "/api/v1/data",
				Headers: map[string]string{
					"Host":        "api.example.com",
					"X-Tenant-Id": "acme",
					eh:            "unique-value-per-request-" + eh,
				},
			}
			assert.Equal(t, baseKey, c.LookupKey(withEphemeral),
				"ephemeral header %q should not affect lookup key", eh)
		})
	}
}

func TestLookupKey_StableHeadersIncluded(t *testing.T) {
	c := newLookupKeyClient(t)

	req1 := &ExternalRequest{
		Method:  "GET",
		Path:    "/api",
		Headers: map[string]string{"X-Tenant-Id": "acme"},
	}
	req2 := &ExternalRequest{
		Method:  "GET",
		Path:    "/api",
		Headers: map[string]string{"X-Tenant-Id": "globex"},
	}
	assert.NotEqual(t, c.LookupKey(req1), c.LookupKey(req2),
		"different stable headers must produce different keys")
}

func TestLookupKey_MethodAndPathAffectKey(t *testing.T) {
	c := newLookupKeyClient(t)

	headers := map[string]string{"Host": "api.example.com"}

	get := c.LookupKey(&ExternalRequest{Method: "GET", Path: "/api", Headers: headers})
	post := c.LookupKey(&ExternalRequest{Method: "POST", Path: "/api", Headers: headers})
	assert.NotEqual(t, get, post, "different methods must produce different keys")

	pathA := c.LookupKey(&ExternalRequest{Method: "GET", Path: "/api/a", Headers: headers})
	pathB := c.LookupKey(&ExternalRequest{Method: "GET", Path: "/api/b", Headers: headers})
	assert.NotEqual(t, pathA, pathB, "different paths must produce different keys")
}

func TestLookupKey_Deterministic(t *testing.T) {
	c := newLookupKeyClient(t)

	req := &ExternalRequest{
		Method: "POST",
		Path:   "/submit",
		Headers: map[string]string{
			"Host":        "api.example.com",
			"X-Tenant-Id": "acme",
			"X-Plan":      "enterprise",
		},
	}
	k1 := c.LookupKey(req)
	k2 := c.LookupKey(req)
	assert.Equal(t, k1, k2, "same input must always produce same key")
}

func TestLookupKey_EmptyHeaders(t *testing.T) {
	c := newLookupKeyClient(t)

	req := &ExternalRequest{
		Method:  "GET",
		Path:    "/health",
		Headers: map[string]string{},
	}
	key := c.LookupKey(req)
	assert.Equal(t, "GET|/health", key)
}

func TestLookupKey_OnlyEphemeralHeaders(t *testing.T) {
	c := newLookupKeyClient(t)

	req := &ExternalRequest{
		Method: "GET",
		Path:   "/api",
		Headers: map[string]string{
			"X-Request-Id":  "req-1",
			"Traceparent":   "00-trace1-span1-01",
			"X-B3-Traceid":  "abc",
			"Uber-Trace-Id": "def",
		},
	}
	assert.Equal(t, "GET|/api", c.LookupKey(req),
		"request with only ephemeral headers should have key based on method+path only")
}

func TestLookupKey_CaseInsensitiveEphemeralMatch(t *testing.T) {
	c := newLookupKeyClient(t)

	base := &ExternalRequest{
		Method:  "GET",
		Path:    "/api",
		Headers: map[string]string{"X-Tenant-Id": "acme"},
	}

	withMixedCase := &ExternalRequest{
		Method: "GET",
		Path:   "/api",
		Headers: map[string]string{
			"X-Tenant-Id":  "acme",
			"x-request-id": "lower-case",
			"TRACEPARENT":  "upper-case",
			"X-B3-TraceId": "mixed-case",
		},
	}

	assert.Equal(t, c.LookupKey(base), c.LookupKey(withMixedCase),
		"ephemeral header matching must be case-insensitive")
}

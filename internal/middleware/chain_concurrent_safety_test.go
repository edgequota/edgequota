package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConcurrent_AuthCacheRedisAtomicReadWrite guards H1 directly:
// authCacheRedis must be safe to read concurrently with writes. Several
// readers and writers run for a fixed iteration count and the race
// detector validates the absence of torn interface headers.
//
// A full ServeHTTP-vs-Reload test is intentionally out of scope here —
// pre-existing races on primitive scalar fields (c.average, etc.) trip
// the race detector independently and need their own dedicated fix.
func TestConcurrent_AuthCacheRedisAtomicReadWrite(t *testing.T) {
	mr := miniredis.RunT(t)
	clientA, err := redis.NewClient(config.RedisConfig{
		Endpoints: []string{mr.Addr()},
		Mode:      config.RedisModeSingle,
	})
	require.NoError(t, err)
	defer clientA.Close()
	clientB, err := redis.NewClient(config.RedisConfig{
		Endpoints: []string{mr.Addr()},
		Mode:      config.RedisModeSingle,
	})
	require.NoError(t, err)
	defer clientB.Close()

	c := &Chain{}

	const goroutines = 4
	const iterations = 1000

	var wg sync.WaitGroup
	for r := range goroutines {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := range iterations {
				if seed%2 == 0 {
					if got := c.loadAuthCacheRedis(); got != nil {
						_ = got
					}
				} else {
					switch i % 3 {
					case 0:
						c.storeAuthCacheRedis(clientA)
					case 1:
						c.storeAuthCacheRedis(clientB)
					case 2:
						c.storeAuthCacheRedis(nil)
					}
				}
			}
		}(r)
	}
	wg.Wait()
}

// TestRecovery_RebindsBorrowedCacheAlias guards H2: when the limiter is
// rebuilt by recoveryInstall on Redis recovery, c.cacheRedis was
// previously left aliased to the (now-closed) old limiter client. After
// the fix, the borrowed alias is rebound to the new limiter's client,
// and authCacheRedis + externalRL pick up the fresh client too.
func TestRecovery_RebindsBorrowedCacheAlias(t *testing.T) {
	mr := miniredis.RunT(t)
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer authSrv.Close()
	extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"average":     100,
			"burst":       50,
			"period":      "1s",
			"backend_url": "http://backend:8080",
		})
	}))
	defer extSrv.Close()

	cfg := config.Defaults()
	cfg.Redis.Endpoints = []string{mr.Addr()}
	cfg.Redis.Mode = config.RedisModeSingle
	cfg.RateLimit.Static.BackendURL = "http://backend:8080"
	cfg.RateLimit.Static.Average = 0
	cfg.RateLimit.External.Enabled = true
	cfg.RateLimit.External.HTTP.URL = extSrv.URL
	cfg.RateLimit.External.Fallback.BackendURL = "http://backend:8080"
	cfg.Auth.Enabled = true
	cfg.Auth.HTTP.URL = authSrv.URL
	// Intentionally NO dedicated cache_redis — exercises the borrowed-alias path.

	chain, err := NewChain(context.Background(),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		cfg, testLogger(), testMetrics())
	require.NoError(t, err)
	defer chain.Close()

	chain.mu.RLock()
	require.False(t, chain.cacheRedisOwned, "precondition: cacheRedis must be borrowed")
	staleClient := chain.cacheRedis
	chain.mu.RUnlock()
	require.NotNil(t, staleClient)
	assert.True(t, staleClient == chain.loadAuthCacheRedis(),
		"precondition: authCacheRedis is the same borrowed alias")

	// Simulate Redis recovery: build a fresh client and hand it to
	// recoveryInstall, which is what the recovery loop normally does.
	freshClient, err := redis.NewClient(config.RedisConfig{
		Endpoints: []string{mr.Addr()},
		Mode:      config.RedisModeSingle,
	})
	require.NoError(t, err)
	chain.recoveryInstall(freshClient)

	chain.mu.RLock()
	rebound := chain.cacheRedis
	chain.mu.RUnlock()

	assert.False(t, staleClient == rebound,
		"borrowed cacheRedis must be rebound to the new limiter's client after recovery")
	assert.True(t, rebound == chain.loadAuthCacheRedis(),
		"authCacheRedis must follow cacheRedis after recovery")
	require.NoError(t, rebound.Ping(context.Background()).Err(),
		"rebound cache client must be live")
}

// TestRecovery_DoesNotRebindOwnedCache is the negative control for H2:
// when cache_redis is a dedicated (owned) client, recovery must not
// touch it.
func TestRecovery_DoesNotRebindOwnedCache(t *testing.T) {
	mr := miniredis.RunT(t)
	cacheMr := miniredis.RunT(t)
	extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"average":     100,
			"burst":       50,
			"period":      "1s",
			"backend_url": "http://backend:8080",
		})
	}))
	defer extSrv.Close()

	cfg := config.Defaults()
	cfg.Redis.Endpoints = []string{mr.Addr()}
	cfg.Redis.Mode = config.RedisModeSingle
	cfg.CacheRedis = &config.RedisConfig{
		Endpoints: []string{cacheMr.Addr()},
		Mode:      config.RedisModeSingle,
	}
	cfg.RateLimit.Static.BackendURL = "http://backend:8080"
	cfg.RateLimit.Static.Average = 0
	cfg.RateLimit.External.Enabled = true
	cfg.RateLimit.External.HTTP.URL = extSrv.URL
	cfg.RateLimit.External.Fallback.BackendURL = "http://backend:8080"

	chain, err := NewChain(context.Background(),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		cfg, testLogger(), testMetrics())
	require.NoError(t, err)
	defer chain.Close()

	chain.mu.RLock()
	require.True(t, chain.cacheRedisOwned, "precondition: cacheRedis must be owned")
	ownedBefore := chain.cacheRedis
	chain.mu.RUnlock()

	freshLimiterClient, err := redis.NewClient(config.RedisConfig{
		Endpoints: []string{mr.Addr()},
		Mode:      config.RedisModeSingle,
	})
	require.NoError(t, err)
	chain.recoveryInstall(freshLimiterClient)

	chain.mu.RLock()
	ownedAfter := chain.cacheRedis
	chain.mu.RUnlock()

	assert.True(t, ownedBefore == ownedAfter,
		"owned cacheRedis must not be replaced by recovery")
	require.NoError(t, ownedAfter.Ping(context.Background()).Err(),
		"owned cache client must remain live")
}

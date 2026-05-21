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

// TestConcurrent_ServeHTTPDuringReload exercises the full hot path
// against Reload churn — the race detector previously flagged this on
// torn reads of primitive scalar fields (c.average, c.burst, c.prefix,
// etc.). After moving those fields into the immutable staticParams /
// fallbackParams snapshots, this test must pass cleanly under -race.
func TestConcurrent_ServeHTTPDuringReload(t *testing.T) {
	mr := miniredis.RunT(t)
	cacheMr := miniredis.RunT(t)

	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer authSrv.Close()

	cfg := config.Defaults()
	cfg.Redis.Endpoints = []string{mr.Addr()}
	cfg.Redis.Mode = config.RedisModeSingle
	cfg.CacheRedis = &config.RedisConfig{
		Endpoints: []string{cacheMr.Addr()},
		Mode:      config.RedisModeSingle,
	}
	cfg.RateLimit.Static.BackendURL = "http://backend:8080"
	cfg.RateLimit.Static.Average = 100
	cfg.RateLimit.Static.Burst = 200
	cfg.RateLimit.FailurePolicy = config.FailurePolicyInMemoryFallback
	cfg.Auth.Enabled = true
	cfg.Auth.HTTP.URL = authSrv.URL

	chain, err := NewChain(context.Background(),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		cfg, testLogger(), testMetrics())
	require.NoError(t, err)
	defer chain.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	const readers = 8
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.RemoteAddr = "10.0.0.1:1234"
				rr := httptest.NewRecorder()
				chain.ServeHTTP(rr, req)
			}
		}()
	}

	const reloads = 30
	for i := range reloads {
		newCfg := *cfg
		newCfg.RateLimit.Static.Average = int64(50 + i)
		newCfg.RateLimit.Static.Burst = int64(100 + i*2)
		if i%2 == 0 {
			newCfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		} else {
			newCfg.RateLimit.FailurePolicy = config.FailurePolicyInMemoryFallback
		}
		require.NoError(t, chain.Reload(&newCfg))
	}

	close(stop)
	wg.Wait()
}

// TestConcurrent_AuthCacheRedisAtomicReadWrite guards H1 directly:
// authCacheRedis must be safe to read concurrently with writes. Several
// readers and writers run for a fixed iteration count and the race
// detector validates the absence of torn interface headers.
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

package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReload_BorrowedCacheRedisSurvivesExternalRLReload guards the
// pre-existing alias-Close bug: when no cache_redis is configured,
// c.cacheRedis is an alias for c.limiter.Client(). reloadExternalRL used
// to Close it unconditionally on every reload, killing the limiter's own
// connection. After the ownership fix, the borrowed alias is left alone
// and the limiter keeps working.
func TestReload_BorrowedCacheRedisSurvivesExternalRLReload(t *testing.T) {
	mr := miniredis.RunT(t)
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
	// cfg.CacheRedis intentionally NOT set — exercises the borrowed-alias path.

	chain, err := NewChain(context.Background(),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		cfg, testLogger(), testMetrics())
	require.NoError(t, err)
	defer chain.Close()

	chain.mu.RLock()
	limiterClientBefore := chain.limiter.Client()
	assert.False(t, chain.cacheRedisOwned,
		"cacheRedis must be marked as borrowed when no dedicated cache_redis is configured")
	assert.Same(t, limiterClientBefore, chain.cacheRedis,
		"cacheRedis must be the same instance as limiter.Client() in the borrowed path")
	chain.mu.RUnlock()

	// Reload triggers reloadExternalRL → releaseCacheRedis. The previous
	// behaviour closed limiterClientBefore here. The current behaviour must
	// keep it alive.
	require.NoError(t, chain.Reload(cfg))

	require.NoError(t, limiterClientBefore.Ping(context.Background()).Err(),
		"limiter's redis client must NOT be closed by reloadExternalRL when cacheRedis was a borrowed alias")
}

// TestReload_OwnedCacheRedisIsClosedOnReload is the positive control: when
// a dedicated cache_redis IS configured, releaseCacheRedis must Close the
// owned client so a fresh one can be created. Without this the connection
// pool would leak on every reload.
func TestReload_OwnedCacheRedisIsClosedOnReload(t *testing.T) {
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
	cacheBefore := chain.cacheRedis
	owned := chain.cacheRedisOwned
	chain.mu.RUnlock()

	assert.True(t, owned, "dedicated cache_redis must be marked owned")
	require.NotNil(t, cacheBefore)
	require.NoError(t, cacheBefore.Ping(context.Background()).Err(),
		"owned cacheRedis must be alive before reload")

	require.NoError(t, chain.Reload(cfg))

	assert.Error(t, cacheBefore.Ping(context.Background()).Err(),
		"owned cacheRedis must be closed after reload (rebuilt fresh)")

	chain.mu.RLock()
	cacheAfter := chain.cacheRedis
	chain.mu.RUnlock()

	assert.NotSame(t, cacheBefore, cacheAfter,
		"reload must produce a fresh owned cacheRedis instance")
	require.NoError(t, cacheAfter.Ping(context.Background()).Err(),
		"fresh cacheRedis must be live")
}

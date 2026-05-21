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

func TestReload_AuthCacheTracksFreshCacheClient(t *testing.T) {
	mr := miniredis.RunT(t)
	cacheMr := miniredis.RunT(t)
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
	// Dedicated cache_redis exercises the path that reloadExternalRL
	// rebuilds (Close + re-resolve). Without it, c.cacheRedis is an alias
	// for the limiter's client and the Close path tramples shared state — a
	// pre-existing bug out of scope for this PR.
	cfg.CacheRedis = &config.RedisConfig{
		Endpoints: []string{cacheMr.Addr()},
		Mode:      config.RedisModeSingle,
	}
	cfg.RateLimit.Static.BackendURL = "http://backend:8080"
	cfg.RateLimit.Static.Average = 0
	cfg.RateLimit.External.Enabled = true
	cfg.RateLimit.External.HTTP.URL = extSrv.URL
	cfg.RateLimit.External.Fallback.BackendURL = "http://backend:8080"
	cfg.Auth.Enabled = true
	cfg.Auth.HTTP.URL = authSrv.URL

	chain, err := NewChain(context.Background(),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		cfg, testLogger(), testMetrics())
	require.NoError(t, err)
	defer chain.Close()

	require.NotNil(t, chain.loadAuthCacheRedis(), "auth cache redis must be initialized")
	require.NotNil(t, chain.cacheRedis, "cache redis must be initialized when external RL is enabled")

	t.Run("auth config unchanged: authCacheRedis still tracks fresh cacheRedis after reload", func(t *testing.T) {
		// Reload with byte-identical auth config; this exercises reloadAuth's
		// early-return path. reloadExternalRL still rebuilds c.cacheRedis,
		// which used to leave authCacheRedis dangling at the closed client.
		newCfg := *cfg
		require.NoError(t, chain.Reload(&newCfg))

		assert.Same(t, chain.cacheRedis, chain.loadAuthCacheRedis(),
			"authCacheRedis must equal cacheRedis (the fresh client) after reload")
		require.NoError(t, chain.cacheRedis.Ping(context.Background()).Err(),
			"cacheRedis must be a live client, not the closed one")
	})

	t.Run("auth config changed: authCacheRedis tracks fresh cacheRedis", func(t *testing.T) {
		newAuthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer newAuthSrv.Close()

		newCfg := *cfg
		newCfg.Auth.HTTP.URL = newAuthSrv.URL
		require.NoError(t, chain.Reload(&newCfg))

		assert.Same(t, chain.cacheRedis, chain.loadAuthCacheRedis())
		require.NoError(t, chain.cacheRedis.Ping(context.Background()).Err())
	})
}

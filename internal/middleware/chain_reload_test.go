package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestChain creates a chain with minimal config and no Redis for reload tests.
func newTestChain(t *testing.T, cfg *config.Config) *Chain {
	t.Helper()

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	logger := slog.Default()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	chain, err := NewChain(context.Background(), next, cfg, logger, metrics)
	require.NoError(t, err)
	t.Cleanup(func() { _ = chain.Close() })
	return chain
}

func baseCfg() *config.Config {
	cfg := config.Defaults()
	cfg.Backend.URL = "http://127.0.0.1:9999"
	cfg.RateLimit.Average = 100
	cfg.RateLimit.Burst = 50
	cfg.RateLimit.Period = "1s"
	return cfg
}

func TestChainReload_UpdatesRateLimitParams(t *testing.T) {
	cfg := baseCfg()
	chain := newTestChain(t, cfg)

	assert.Equal(t, int64(50), chain.burst)
	assert.Equal(t, int64(100), chain.average)

	// Reload with different params.
	newCfg := baseCfg()
	newCfg.RateLimit.Average = 10
	newCfg.RateLimit.Burst = 5
	newCfg.RateLimit.Period = "10s"
	newCfg.RateLimit.FailurePolicy = config.FailurePolicyFailClosed

	err := chain.Reload(newCfg)
	require.NoError(t, err)

	assert.Equal(t, int64(5), chain.burst)
	assert.Equal(t, int64(10), chain.average)
	assert.Equal(t, config.FailurePolicyFailClosed, chain.failurePolicy)
}

func TestChainReload_UpdatesKeyStrategy(t *testing.T) {
	cfg := baseCfg()
	chain := newTestChain(t, cfg)

	// Reload with header-based key strategy.
	newCfg := baseCfg()
	newCfg.RateLimit.KeyStrategy = config.KeyStrategyConfig{
		Type:       config.KeyStrategyHeader,
		HeaderName: "X-Tenant-Id",
	}

	err := chain.Reload(newCfg)
	require.NoError(t, err)

	// Verify the new strategy is used by sending a request with the header.
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Tenant-Id", "acme")
	key, extractErr := chain.keyStrategy.Extract(req)
	require.NoError(t, extractErr)
	assert.Equal(t, "acme", key)
}

func TestChainReload_InvalidKeyStrategyKeepsOld(t *testing.T) {
	cfg := baseCfg()
	chain := newTestChain(t, cfg)

	// Try to reload with an invalid key strategy.
	newCfg := baseCfg()
	newCfg.RateLimit.KeyStrategy = config.KeyStrategyConfig{
		Type: "nonexistent",
	}

	err := chain.Reload(newCfg)
	assert.Error(t, err, "should fail for unknown key strategy")
}

func TestChainReload_MinTTLKnob(t *testing.T) {
	cfg := baseCfg()
	chain := newTestChain(t, cfg)

	// Default TTL for 100 req/s, 1s period should be 2.
	assert.Equal(t, 2, chain.ttl)

	// Reload with min_ttl = 30s.
	newCfg := baseCfg()
	newCfg.RateLimit.MinTTL = "30s"

	err := chain.Reload(newCfg)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, chain.ttl, 30, "TTL should be at least 30 with min_ttl=30s")
}

package middleware

import (
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closeTrackingRedis wraps a redis.Client and counts Close invocations.
type closeTrackingRedis struct {
	redis.Client
	closes atomic.Int32
}

func (t *closeTrackingRedis) Close() error {
	t.closes.Add(1)
	return t.Client.Close()
}

func newTrackingRedis(t *testing.T) *closeTrackingRedis {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redis.NewClient(config.RedisConfig{
		Endpoints: []string{mr.Addr()},
		Mode:      config.RedisModeSingle,
	})
	require.NoError(t, err)
	return &closeTrackingRedis{Client: c}
}

func TestInstallResponseCacheClosesPrior(t *testing.T) {
	cfg := config.Defaults()
	cfg.Cache.Enabled = true

	c := &Chain{metrics: testMetrics()}

	first := newTrackingRedis(t)
	c.installResponseCache(cfg, testLogger(), first)
	assert.Equal(t, int32(0), first.closes.Load(), "first install should not close anyone")

	second := newTrackingRedis(t)
	c.installResponseCache(cfg, testLogger(), second)
	assert.Equal(t, int32(1), first.closes.Load(),
		"prior responseCacheRedis must be closed exactly once on re-install")
	assert.Equal(t, int32(0), second.closes.Load())

	third := newTrackingRedis(t)
	c.installResponseCache(cfg, testLogger(), third)
	assert.Equal(t, int32(1), second.closes.Load(),
		"prior responseCacheRedis must be closed on each subsequent re-install")
}

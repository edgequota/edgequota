package server

import (
	"io"
	"log/slog"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNew(t *testing.T) {
	t.Run("creates server with valid config", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.Redis.Endpoints = []string{mr.Addr()}

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		assert.NotNil(t, srv)
		assert.NotNil(t, srv.mainServer)
		assert.NotNil(t, srv.adminServer)
		assert.NotNil(t, srv.health)
		assert.NotNil(t, srv.metrics)

		// Clean up.
		srv.chain.Close()
	})

	t.Run("returns error for invalid backend URL", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "://invalid"

		_, err := New(cfg, testLogger(), "test")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "create proxy")
	})

	t.Run("creates server with rate limiting disabled", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		assert.NotNil(t, srv)
		srv.chain.Close()
	})

	t.Run("creates server with passthrough on Redis failure", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 100
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		cfg.Redis.Endpoints = []string{"127.0.0.1:1"}
		cfg.Redis.DialTimeout = "100ms"

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		assert.NotNil(t, srv)
		srv.chain.Close()
	})
}

func TestServerConfigAddresses(t *testing.T) {
	t.Run("uses configured server and admin addresses", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.Server.Address = ":7777"
		cfg.Admin.Address = ":7778"
		cfg.Redis.Endpoints = []string{mr.Addr()}

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		assert.Equal(t, ":7777", srv.mainServer.Addr)
		assert.Equal(t, ":7778", srv.adminServer.Addr)
		srv.chain.Close()
	})
}

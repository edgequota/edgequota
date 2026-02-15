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

func TestChainServeHTTPWithExternalRateLimit(t *testing.T) {
	t.Run("uses external rate limit service", func(t *testing.T) {
		externalRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"average": 100,
				"burst":   50,
				"period":  "1s",
			})
		}))
		defer externalRL.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = externalRL.URL
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("falls back to static config when external RL errors", func(t *testing.T) {
		externalRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer externalRL.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = externalRL.URL
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		// Should still work with static rate limiting.
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("creates chain with external RL enabled but no Redis", func(t *testing.T) {
		externalRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"average": 100,
				"burst":   50,
			})
		}))
		defer externalRL.Close()

		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = externalRL.URL
		cfg.Redis.Endpoints = []string{"127.0.0.1:1"}
		cfg.Redis.Mode = config.RedisModeSingle
		cfg.Redis.DialTimeout = "100ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		// With passthrough and no Redis, request should pass.
		assert.Equal(t, http.StatusOK, rr.Code)
	})
}

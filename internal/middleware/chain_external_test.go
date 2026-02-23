package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChainExternalRLReceivesHostHeader(t *testing.T) {
	t.Run("Host header from r.Host is forwarded to external RL service", func(t *testing.T) {
		var receivedHeaders map[string]string
		externalRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Headers map[string]string `json:"headers"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			receivedHeaders = req.Headers

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"average":     100,
				"burst":       50,
				"period":      "1s",
				"backend_url": "http://backend:8080",
			})
		}))
		defer externalRL.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = externalRL.URL
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "http://api.example.com/api/resource", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		require.NotNil(t, receivedHeaders, "external RL service should have been called")
		assert.Equal(t, "api.example.com", receivedHeaders["Host"],
			"Host header must be forwarded from r.Host")
	})

	t.Run("Host header absent when r.Host is empty", func(t *testing.T) {
		var receivedHeaders map[string]string
		externalRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Headers map[string]string `json:"headers"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			receivedHeaders = req.Headers

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"average":     100,
				"burst":       50,
				"period":      "1s",
				"backend_url": "http://backend:8080",
			})
		}))
		defer externalRL.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = externalRL.URL
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.Host = ""
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		require.NotNil(t, receivedHeaders)
		assert.NotContains(t, receivedHeaders, "Host",
			"Host should not be present when r.Host is empty")
	})
}

func TestChainServeHTTPWithExternalRateLimit(t *testing.T) {
	t.Run("uses external rate limit service", func(t *testing.T) {
		externalRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"average":     100,
				"burst":       50,
				"period":      "1s",
				"backend_url": "http://backend:8080",
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
				"average":     100,
				"burst":       50,
				"period":      "1s",
				"backend_url": "http://backend:8080",
			})
		}))
		defer externalRL.Close()

		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0
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

func TestChainBackendURLFromExternalService(t *testing.T) {
	t.Run("injects backend_url from external service into request context", func(t *testing.T) {
		// The real backend where the external service directs traffic.
		tenantBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend", "tenant-backend")
			w.WriteHeader(http.StatusOK)
		}))
		defer tenantBackend.Close()

		externalRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"average":     100,
				"burst":       50,
				"period":      "1s",
				"backend_url": tenantBackend.URL,
			})
		}))
		defer externalRL.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = externalRL.URL
		// Disable SSRF protection so tests can use localhost backends.
		denyPrivate := false
		cfg.Backend.URLPolicy.DenyPrivateNetworks = &denyPrivate

		// The next handler verifies the backend URL is in the context.
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctxURL := r.Context().Value(proxy.BackendURLContextKey)
			assert.NotNil(t, ctxURL, "backend URL should be in context")
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("no context URL when external service returns empty backend_url", func(t *testing.T) {
		externalRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"average":     100,
				"burst":       50,
				"period":      "1s",
				"backend_url": "",
			})
		}))
		defer externalRL.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = externalRL.URL

		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctxURL := r.Context().Value(proxy.BackendURLContextKey)
			assert.Nil(t, ctxURL, "backend URL should NOT be in context when empty")
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("invalid backend_url from external service is ignored", func(t *testing.T) {
		externalRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"average":     100,
				"burst":       50,
				"period":      "1s",
				"backend_url": "://invalid-url",
			})
		}))
		defer externalRL.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = externalRL.URL

		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctxURL := r.Context().Value(proxy.BackendURLContextKey)
			assert.Nil(t, ctxURL, "invalid backend URL should not be in context")
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})
}

func TestChainBackendProtocolFromExternalService(t *testing.T) {
	t.Run("injects backend_protocol from external service into request context", func(t *testing.T) {
		externalRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"average":          100,
				"burst":            50,
				"period":           "1s",
				"backend_url":      "http://backend:8080",
				"backend_protocol": "h2",
			})
		}))
		defer externalRL.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = externalRL.URL

		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctxProto := r.Context().Value(proxy.BackendProtocolContextKey)
			assert.Equal(t, "h2", ctxProto, "backend protocol should be h2 in context")
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("no context protocol when external service omits backend_protocol", func(t *testing.T) {
		externalRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"average":     100,
				"burst":       50,
				"period":      "1s",
				"backend_url": "http://backend:8080",
			})
		}))
		defer externalRL.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = externalRL.URL

		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctxProto := r.Context().Value(proxy.BackendProtocolContextKey)
			assert.Nil(t, ctxProto, "backend protocol should NOT be in context when omitted")
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("invalid backend_protocol from external service is ignored", func(t *testing.T) {
		externalRL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"average":          100,
				"burst":            50,
				"period":           "1s",
				"backend_url":      "http://backend:8080",
				"backend_protocol": "ftp",
			})
		}))
		defer externalRL.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = externalRL.URL

		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctxProto := r.Context().Value(proxy.BackendProtocolContextKey)
			assert.Nil(t, ctxProto, "invalid backend protocol should not be in context")
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})
}

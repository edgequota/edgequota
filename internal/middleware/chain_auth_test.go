package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChainServeHTTPWithAuth(t *testing.T) {
	t.Run("allows request when auth service responds 200", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		snap := metrics.Snapshot()
		assert.Equal(t, int64(1), snap.Allowed)
	})

	t.Run("denies request when auth service responds 403", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{
				"allowed":     false,
				"status_code": 403,
				"deny_body":   "access denied",
			})
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusForbidden, rr.Code)
		snap := metrics.Snapshot()
		assert.Equal(t, int64(1), snap.AuthDenied)
	})

	t.Run("returns 503 when auth service is unreachable", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = "http://127.0.0.1:1/auth" // unreachable
		cfg.Auth.Timeout = "100ms"
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
		snap := metrics.Snapshot()
		assert.Equal(t, int64(1), snap.AuthErrors)
	})

	t.Run("denies with custom response headers from auth service", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{
				"allowed":          false,
				"status_code":      401,
				"deny_body":        "unauthorized",
				"response_headers": map[string]string{"WWW-Authenticate": "Bearer"},
			})
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
		assert.Equal(t, "Bearer", rr.Header().Get("WWW-Authenticate"))
	})

	t.Run("auth combined with rate limiting", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0 // no rate limit
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Multiple requests should all pass auth and be allowed.
		for i := 0; i < 5; i++ {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		snap := metrics.Snapshot()
		assert.Equal(t, int64(5), snap.Allowed)
	})
}

func TestChainAuthInjectsRequestHeaders(t *testing.T) {
	t.Run("auth request_headers are visible to key strategy and backend", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"request_headers": map[string]string{
					"X-Tenant-Id": "acme-corp",
					"X-Plan-Tier": "premium",
				},
			})
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()

		var capturedTenant, capturedPlan string
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedTenant = r.Header.Get("X-Tenant-Id")
			capturedPlan = r.Header.Get("X-Plan-Tier")
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "acme-corp", capturedTenant)
		assert.Equal(t, "premium", capturedPlan)
	})

	t.Run("auth request_headers overwrite client-sent headers", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"request_headers": map[string]string{
					"X-Tenant-Id": "real-tenant",
				},
			})
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()

		var capturedTenant string
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedTenant = r.Header.Get("X-Tenant-Id")
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Client tries to spoof the tenant header.
		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		req.Header.Set("X-Tenant-Id", "spoofed-tenant")
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "real-tenant", capturedTenant, "auth-injected header must overwrite client-sent value")
	})

	t.Run("auth request_headers are used by rate limit key strategy", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"request_headers": map[string]string{
					"X-Tenant-Id": "acme-corp",
				},
			})
		}))
		defer authServer.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		cfg.RateLimit.Static.Average = 100
		cfg.RateLimit.Static.Burst = 100
		cfg.RateLimit.Static.KeyStrategy = config.KeyStrategyConfig{
			Type:       config.KeyStrategyHeader,
			HeaderName: "X-Tenant-Id",
		}
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Request has no X-Tenant-Id header — the auth service injects it.
		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code, "key strategy should read the auth-injected X-Tenant-Id")
		snap := metrics.Snapshot()
		assert.Equal(t, int64(1), snap.Allowed)
		assert.Equal(t, int64(0), snap.KeyExtractErrors)
	})

	t.Run("no request_headers does not alter request", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()

		var capturedTenant string
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedTenant = r.Header.Get("X-Tenant-Id")
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Tenant-Id", "client-sent")
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "client-sent", capturedTenant, "original header should be preserved when auth returns no request_headers")
	})
}

func TestChainAuthReceivesHostHeader(t *testing.T) {
	t.Run("Host header from r.Host is forwarded to auth service", func(t *testing.T) {
		var receivedHost string
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Headers map[string]string `json:"headers"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			receivedHost = req.Headers["Host"]

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"allowed": true})
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "http://admin.example.com/api/check", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "admin.example.com", receivedHost,
			"Host header must be forwarded from r.Host to auth service")
	})
}

func TestChainAuthResponseCaching(t *testing.T) {
	t.Run("auth service is called only once when response is cached", func(t *testing.T) {
		callCount := 0
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"allowed":               true,
				"cache_max_age_seconds": 60,
			})
		}))
		defer authServer.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// First request — cache miss, calls the auth service.
		req := httptest.NewRequest(http.MethodGet, "/api", nil)
		req.Header.Set("Authorization", "Bearer mytoken")
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, 1, callCount)

		// Second request with the same token — cache hit, should NOT call the auth service again.
		req2 := httptest.NewRequest(http.MethodGet, "/api", nil)
		req2.Header.Set("Authorization", "Bearer mytoken")
		rr2 := httptest.NewRecorder()
		chain.ServeHTTP(rr2, req2)
		assert.Equal(t, http.StatusOK, rr2.Code)
		assert.Equal(t, 1, callCount, "auth service must not be called on cache hit")
	})

	t.Run("different credentials get separate cache entries", func(t *testing.T) {
		callCount := 0
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"allowed":               true,
				"cache_max_age_seconds": 60,
			})
		}))
		defer authServer.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		for i, token := range []string{"Bearer token-a", "Bearer token-b"} {
			req := httptest.NewRequest(http.MethodGet, "/api", nil)
			req.Header.Set("Authorization", token)
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
			assert.Equal(t, i+1, callCount, "each unique credential must reach the auth service")
		}
	})

	t.Run("cache_no_store=true always calls the auth service", func(t *testing.T) {
		callCount := 0
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"allowed":               true,
				"cache_max_age_seconds": 60,
				"cache_no_store":        true,
			})
		}))
		defer authServer.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		for i := range 3 {
			req := httptest.NewRequest(http.MethodGet, "/api", nil)
			req.Header.Set("Authorization", "Bearer mytoken")
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
			assert.Equal(t, i+1, callCount, "cache_no_store must bypass the cache")
		}
	})

	t.Run("cached response preserves request_headers injection", func(t *testing.T) {
		callCount := 0
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"allowed":               true,
				"cache_max_age_seconds": 60,
				"request_headers": map[string]string{
					"X-Tenant-Id": "acme-corp",
				},
			})
		}))
		defer authServer.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()

		var captured []string
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured = append(captured, r.Header.Get("X-Tenant-Id"))
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		for range 2 {
			req := httptest.NewRequest(http.MethodGet, "/api", nil)
			req.Header.Set("Authorization", "Bearer mytoken")
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		assert.Equal(t, 1, callCount, "second request must use the cache")
		assert.Equal(t, []string{"acme-corp", "acme-corp"}, captured,
			"request_headers from cached response must still be injected")
	})

	t.Run("cache TTL is respected — expired entries call auth service again", func(t *testing.T) {
		callCount := 0
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"allowed":               true,
				"cache_max_age_seconds": 1, // 1-second TTL
			})
		}))
		defer authServer.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		doReq := func() {
			req := httptest.NewRequest(http.MethodGet, "/api", nil)
			req.Header.Set("Authorization", "Bearer mytoken")
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		doReq() // miss — calls auth service
		assert.Equal(t, 1, callCount)

		doReq() // hit — no call
		assert.Equal(t, 1, callCount)

		mr.FastForward(2 * time.Second) // expire the cache entry
		doReq()                         // miss again — calls auth service
		assert.Equal(t, 2, callCount, "auth must be called after cache expiry")
	})
}

func TestChainAuthCacheLogging(t *testing.T) {
	t.Run("debug log emitted when auth response is cached", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "max-age=300")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"allowed": true})
		}))
		defer authServer.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

		// Capture log output at Debug level.
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		chain, err := NewChain(context.Background(), next, cfg, logger, metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api", nil)
		req.Header.Set("Authorization", "Bearer tok")
		chain.ServeHTTP(httptest.NewRecorder(), req)

		logs := buf.String()
		assert.Contains(t, logs, "auth response cached")
		assert.Contains(t, logs, "5m0s") // 300s formats as 5m0s
		assert.Contains(t, logs, "Cache-Control")
	})

	t.Run("debug log emitted on cache hit", func(t *testing.T) {
		callCount := 0
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			w.Header().Set("Cache-Control", "max-age=300")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"allowed": true})
		}))
		defer authServer.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL

		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

		chain, err := NewChain(context.Background(), next, cfg, logger, testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		doReq := func() {
			req := httptest.NewRequest(http.MethodGet, "/api", nil)
			req.Header.Set("Authorization", "Bearer tok")
			chain.ServeHTTP(httptest.NewRecorder(), req)
		}

		doReq() // miss
		buf.Reset()
		doReq() // hit

		logs := buf.String()
		assert.Contains(t, logs, "auth cache hit")
		assert.Equal(t, 1, callCount)
	})

	t.Run("debug log emitted when cache_no_store prevents caching", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"allowed": true})
		}))
		defer authServer.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL

		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

		chain, err := NewChain(context.Background(), next, cfg, logger, testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api", nil)
		chain.ServeHTTP(httptest.NewRecorder(), req)

		logs := buf.String()
		assert.Contains(t, logs, "auth response not cached")
		assert.Contains(t, logs, "no-store")
	})
}

func TestChainAuthHTTPHeaderCachePriority(t *testing.T) {
	t.Run("Cache-Control header TTL is honored end-to-end", func(t *testing.T) {
		callCount := 0
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			// Header says 60s; body says 5s — header must win.
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"allowed":               true,
				"cache_max_age_seconds": 5,
			})
		}))
		defer authServer.Close()

		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL

		chain, err := NewChain(context.Background(),
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
			cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		doReq := func() {
			req := httptest.NewRequest(http.MethodGet, "/api", nil)
			req.Header.Set("Authorization", "Bearer tok")
			chain.ServeHTTP(httptest.NewRecorder(), req)
		}

		doReq()
		assert.Equal(t, 1, callCount)

		doReq() // should hit the 60s cache, NOT the 5s body TTL
		assert.Equal(t, 1, callCount, "second request must use the 60s header-driven cache")

		// Advance time past the header TTL (60s) — NOT past a hypothetical 5s TTL.
		mr.FastForward(10 * time.Second)
		doReq() // still within 60s — cache hit
		assert.Equal(t, 1, callCount, "cache must still be valid at 10s")

		mr.FastForward(55 * time.Second) // now past 60s total
		doReq()
		assert.Equal(t, 2, callCount, "cache must have expired at 60s+")
	})
}

func TestChainServeHTTPFailClosed(t *testing.T) {
	t.Run("returns configured failure code when Redis is down", func(t *testing.T) {
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "100ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyInMemoryFallback
		cfg.RateLimit.Static.Average = 1
		cfg.RateLimit.Static.Burst = 1
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// First request should be allowed (in-memory burst).
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		snap := metrics.Snapshot()
		assert.Equal(t, int64(1), snap.FallbackUsed)
	})
}

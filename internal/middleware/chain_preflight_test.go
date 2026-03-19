package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsCORSPreflight(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		headers map[string]string
		want    bool
	}{
		{
			name:   "valid preflight",
			method: http.MethodOptions,
			headers: map[string]string{
				"Origin":                        "https://example.com",
				"Access-Control-Request-Method": "POST",
			},
			want: true,
		},
		{
			name:   "OPTIONS without Origin",
			method: http.MethodOptions,
			headers: map[string]string{
				"Access-Control-Request-Method": "POST",
			},
			want: false,
		},
		{
			name:   "OPTIONS without Access-Control-Request-Method",
			method: http.MethodOptions,
			headers: map[string]string{
				"Origin": "https://example.com",
			},
			want: false,
		},
		{
			name:   "GET with CORS headers is not preflight",
			method: http.MethodGet,
			headers: map[string]string{
				"Origin":                        "https://example.com",
				"Access-Control-Request-Method": "POST",
			},
			want: false,
		},
		{
			name:   "plain OPTIONS without any CORS headers",
			method: http.MethodOptions,
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			assert.Equal(t, tt.want, isCORSPreflight(req))
		})
	}
}

func TestPreflightBypass(t *testing.T) {
	t.Run("preflight bypasses auth and reaches backend when enabled", func(t *testing.T) {
		// Auth server that always denies — preflight must never reach it.
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"allowed": false, "status_code": 403})
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		cfg.Server.BypassPreflightAuth = true

		metrics := testMetrics()
		var backendHit bool
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			backendHit = true
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(http.StatusNoContent)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodOptions, "/api/resource", nil)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.True(t, backendHit, "preflight should reach backend")
		assert.Equal(t, http.StatusNoContent, rr.Code)
		snap := metrics.Snapshot()
		assert.Equal(t, int64(1), snap.Allowed)
	})

	t.Run("preflight goes through auth when bypass disabled", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"allowed": false, "status_code": 403})
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		cfg.Server.BypassPreflightAuth = false

		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodOptions, "/api/resource", nil)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusForbidden, rr.Code, "preflight should be denied by auth")
	})

	t.Run("non-preflight OPTIONS goes through auth even when bypass enabled", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"allowed": false, "status_code": 403})
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		cfg.Server.BypassPreflightAuth = true

		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Plain OPTIONS without CORS headers — not a preflight.
		req := httptest.NewRequest(http.MethodOptions, "/api/resource", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusForbidden, rr.Code, "plain OPTIONS should go through auth")
	})
}

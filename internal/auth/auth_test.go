package auth

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

func TestNewClient(t *testing.T) {
	t.Run("creates client with HTTP URL", func(t *testing.T) {
		cfg := config.AuthConfig{
			Timeout: "5s",
			HTTP:    config.AuthHTTPConfig{URL: "http://auth:8080/check"},
		}
		c, err := NewClient(cfg)
		require.NoError(t, err)
		assert.NotNil(t, c)
		assert.Equal(t, "http://auth:8080/check", c.httpURL)
		assert.Nil(t, c.grpcClient)
		require.NoError(t, c.Close())
	})

	t.Run("defaults timeout when invalid", func(t *testing.T) {
		cfg := config.AuthConfig{
			Timeout: "invalid",
			HTTP:    config.AuthHTTPConfig{URL: "http://auth:8080"},
		}
		c, err := NewClient(cfg)
		require.NoError(t, err)
		assert.Equal(t, "5s", c.timeout.String())
		require.NoError(t, c.Close())
	})
}

func TestCheckHTTP(t *testing.T) {
	t.Run("returns allowed for 200 response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := &Client{
			httpURL:    srv.URL,
			httpClient: http.DefaultClient,
			timeout:    5e9,
		}

		resp, err := c.Check(context.Background(), &CheckRequest{
			Method: "GET",
			Path:   "/api/v1",
		})
		require.NoError(t, err)
		assert.True(t, resp.Allowed)
		assert.Equal(t, 200, resp.StatusCode)
	})

	t.Run("returns denied for non-200 response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("access denied"))
		}))
		defer srv.Close()

		c := &Client{
			httpURL:    srv.URL,
			httpClient: http.DefaultClient,
			timeout:    5e9,
		}

		resp, err := c.Check(context.Background(), &CheckRequest{
			Method: "GET",
			Path:   "/admin",
		})
		require.NoError(t, err)
		assert.False(t, resp.Allowed)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Equal(t, "access denied", resp.DenyBody)
	})

	t.Run("returns denied with structured response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(CheckResponse{
				Allowed:    false,
				StatusCode: 401,
				DenyBody:   "invalid token",
				ResponseHeaders: map[string]string{
					"WWW-Authenticate": "Bearer",
				},
			})
		}))
		defer srv.Close()

		c := &Client{
			httpURL:    srv.URL,
			httpClient: http.DefaultClient,
			timeout:    5e9,
		}

		resp, err := c.Check(context.Background(), &CheckRequest{
			Method:  "GET",
			Path:    "/protected",
			Headers: map[string]string{"Authorization": "Bearer expired-token"},
		})
		require.NoError(t, err)
		assert.False(t, resp.Allowed)
		assert.Equal(t, 401, resp.StatusCode)
	})

	t.Run("returns request_headers on allow with structured body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"request_headers": map[string]string{
					"X-Tenant-Id": "acme-corp",
					"X-Plan-Tier": "premium",
				},
			})
		}))
		defer srv.Close()

		c := &Client{
			httpURL:    srv.URL,
			httpClient: http.DefaultClient,
			timeout:    5e9,
		}

		resp, err := c.Check(context.Background(), &CheckRequest{
			Method: "GET",
			Path:   "/api/v1",
		})
		require.NoError(t, err)
		assert.True(t, resp.Allowed)
		assert.Equal(t, 200, resp.StatusCode)
		assert.Equal(t, "acme-corp", resp.RequestHeaders["X-Tenant-Id"])
		assert.Equal(t, "premium", resp.RequestHeaders["X-Plan-Tier"])
	})

	t.Run("returns nil request_headers when 200 body is empty", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := &Client{
			httpURL:    srv.URL,
			httpClient: http.DefaultClient,
			timeout:    5e9,
		}

		resp, err := c.Check(context.Background(), &CheckRequest{
			Method: "GET",
			Path:   "/api/v1",
		})
		require.NoError(t, err)
		assert.True(t, resp.Allowed)
		assert.Nil(t, resp.RequestHeaders)
	})

	t.Run("returns error for unreachable service", func(t *testing.T) {
		c := &Client{
			httpURL:    "http://127.0.0.1:1/check",
			httpClient: http.DefaultClient,
			timeout:    5e9,
		}

		_, err := c.Check(context.Background(), &CheckRequest{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "auth http request")
	})

	t.Run("forwards original headers as X-Original- prefix", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer token123", r.Header.Get("X-Original-Authorization"))
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := &Client{
			httpURL:    srv.URL,
			httpClient: http.DefaultClient,
			timeout:    5e9,
		}

		_, err := c.Check(context.Background(), &CheckRequest{
			Method:  "GET",
			Path:    "/api",
			Headers: map[string]string{"Authorization": "Bearer token123"},
		})
		require.NoError(t, err)
	})

	t.Run("reads request body from check request", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req CheckRequest
			err := json.NewDecoder(r.Body).Decode(&req)
			require.NoError(t, err)
			assert.Equal(t, "POST", req.Method)
			assert.Equal(t, "/api/create", req.Path)
			assert.Equal(t, "10.0.0.1:5555", req.RemoteAddr)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := &Client{
			httpURL:    srv.URL,
			httpClient: http.DefaultClient,
			timeout:    5e9,
		}

		_, err := c.Check(context.Background(), &CheckRequest{
			Method:     "POST",
			Path:       "/api/create",
			RemoteAddr: "10.0.0.1:5555",
		})
		require.NoError(t, err)
	})
}

func TestBuildCheckRequest(t *testing.T) {
	t.Run("builds request from http.Request and filters sensitive headers by default", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodPost, "/api/v1/resource", nil)
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Tenant-Id", "tenant-1")
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "192.168.1.1:5000"

		c := &Client{headerFilter: config.NewHeaderFilter(config.HeaderFilterConfig{})}
		cr := c.BuildCheckRequest(r)

		assert.Equal(t, "POST", cr.Method)
		assert.Equal(t, "/api/v1/resource", cr.Path)
		assert.Equal(t, "192.168.1.1:5000", cr.RemoteAddr)
		// Authorization is in DefaultSensitiveHeaders — should be stripped.
		assert.NotContains(t, cr.Headers, "Authorization")
		// Non-sensitive headers should pass through.
		assert.Equal(t, "tenant-1", cr.Headers["X-Tenant-Id"])
		assert.Equal(t, "application/json", cr.Headers["Content-Type"])
	})

	t.Run("allow list includes Authorization when explicitly allowed", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodPost, "/api/v1/resource", nil)
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Tenant-Id", "tenant-1")
		r.RemoteAddr = "192.168.1.1:5000"

		c := &Client{headerFilter: config.NewHeaderFilter(config.HeaderFilterConfig{
			AllowList: []string{"Authorization", "X-Tenant-Id"},
		})}
		cr := c.BuildCheckRequest(r)

		assert.Equal(t, "Bearer token", cr.Headers["Authorization"])
		assert.Equal(t, "tenant-1", cr.Headers["X-Tenant-Id"])
	})

	t.Run("handles request with no headers", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		// Clear default headers.
		r.Header = http.Header{}
		r.RemoteAddr = "127.0.0.1:1234"

		c := &Client{headerFilter: config.NewHeaderFilter(config.HeaderFilterConfig{})}
		cr := c.BuildCheckRequest(r)

		assert.Equal(t, "GET", cr.Method)
		assert.Equal(t, "/", cr.Path)
		assert.Empty(t, cr.Headers)
	})
}

func TestClientClose(t *testing.T) {
	t.Run("close with no grpc conn returns nil", func(t *testing.T) {
		c := &Client{}
		assert.NoError(t, c.Close())
	})
}

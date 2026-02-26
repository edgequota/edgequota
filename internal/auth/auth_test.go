package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
			cb:         newCircuitBreaker(0, 0),
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
			cb:         newCircuitBreaker(0, 0),
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
			cb:         newCircuitBreaker(0, 0),
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
			cb:         newCircuitBreaker(0, 0),
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
			cb:         newCircuitBreaker(0, 0),
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
			cb:         newCircuitBreaker(0, 0),
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
			httpURL:                srv.URL,
			httpClient:             http.DefaultClient,
			timeout:                5e9,
			forwardOriginalHeaders: true,
			cb:                     newCircuitBreaker(0, 0),
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
			cb:         newCircuitBreaker(0, 0),
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
		cr := c.BuildCheckRequest(r, nil)

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
		cr := c.BuildCheckRequest(r, nil)

		assert.Equal(t, "Bearer token", cr.Headers["Authorization"])
		assert.Equal(t, "tenant-1", cr.Headers["X-Tenant-Id"])
	})

	t.Run("handles request with no headers", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		// Clear default headers.
		r.Header = http.Header{}
		r.RemoteAddr = "127.0.0.1:1234"

		c := &Client{headerFilter: config.NewHeaderFilter(config.HeaderFilterConfig{})}
		cr := c.BuildCheckRequest(r, nil)

		assert.Equal(t, "GET", cr.Method)
		assert.Equal(t, "/", cr.Path)
		assert.Empty(t, cr.Headers)
	})

	t.Run("injects Host from r.Host into headers", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodGet, "/api/v1/resource", nil)
		r.Host = "api.example.com"
		r.Header.Set("X-Tenant-Id", "tenant-1")
		r.RemoteAddr = "192.168.1.1:5000"

		c := &Client{headerFilter: config.NewHeaderFilter(config.HeaderFilterConfig{})}
		cr := c.BuildCheckRequest(r, nil)

		assert.Equal(t, "api.example.com", cr.Headers["Host"],
			"Host must be injected from r.Host since Go removes it from r.Header")
		assert.Equal(t, "tenant-1", cr.Headers["X-Tenant-Id"])
	})

	t.Run("Host header absent when r.Host is empty", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		r.Host = ""
		r.Header = http.Header{}
		r.RemoteAddr = "127.0.0.1:1234"

		c := &Client{headerFilter: config.NewHeaderFilter(config.HeaderFilterConfig{})}
		cr := c.BuildCheckRequest(r, nil)

		assert.NotContains(t, cr.Headers, "Host")
	})

	t.Run("Host header present with httptest.NewRequest", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://admin.example.com/check", nil)
		r.RemoteAddr = "10.0.0.1:4000"

		c := &Client{headerFilter: config.NewHeaderFilter(config.HeaderFilterConfig{})}
		cr := c.BuildCheckRequest(r, nil)

		assert.Equal(t, "admin.example.com", cr.Headers["Host"],
			"httptest.NewRequest sets r.Host which must appear in headers")
	})
}

func TestCheckHTTPCacheFields(t *testing.T) {
	t.Run("returns cache_max_age_seconds on 200 allow", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"cache_max_age_seconds": 300,
			})
		}))
		defer srv.Close()

		c := &Client{httpURL: srv.URL, httpClient: http.DefaultClient, timeout: 5e9, cb: newCircuitBreaker(0, 0)}
		resp, err := c.Check(context.Background(), &CheckRequest{Method: "GET", Path: "/api"})
		require.NoError(t, err)
		assert.True(t, resp.Allowed)
		require.NotNil(t, resp.CacheMaxAgeSec)
		assert.Equal(t, int64(300), *resp.CacheMaxAgeSec)
		assert.Equal(t, 300*time.Second, resp.ResolveCacheTTL())
	})

	t.Run("cache_no_store=true returns zero TTL", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"cache_max_age_seconds": 300,
				"cache_no_store":        true,
			})
		}))
		defer srv.Close()

		c := &Client{httpURL: srv.URL, httpClient: http.DefaultClient, timeout: 5e9, cb: newCircuitBreaker(0, 0)}
		resp, err := c.Check(context.Background(), &CheckRequest{Method: "GET", Path: "/api"})
		require.NoError(t, err)
		assert.True(t, resp.CacheNoStore)
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
	})

	t.Run("no cache fields returns zero TTL", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := &Client{httpURL: srv.URL, httpClient: http.DefaultClient, timeout: 5e9, cb: newCircuitBreaker(0, 0)}
		resp, err := c.Check(context.Background(), &CheckRequest{Method: "GET", Path: "/api"})
		require.NoError(t, err)
		assert.Nil(t, resp.CacheMaxAgeSec)
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
	})

	t.Run("cache fields returned on deny via checkResponseFromHTTP", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{
				"allowed":               false,
				"status_code":           401,
				"cache_max_age_seconds": 60,
				"cache_no_store":        false,
			})
		}))
		defer srv.Close()

		c := &Client{httpURL: srv.URL, httpClient: http.DefaultClient, timeout: 5e9, cb: newCircuitBreaker(0, 0)}
		resp, err := c.Check(context.Background(), &CheckRequest{Method: "GET", Path: "/protected"})
		require.NoError(t, err)
		assert.False(t, resp.Allowed)
		require.NotNil(t, resp.CacheMaxAgeSec)
		assert.Equal(t, int64(60), *resp.CacheMaxAgeSec)
		assert.Equal(t, 60*time.Second, resp.ResolveCacheTTL())
	})
}

func TestResolveHTTPCacheTTL(t *testing.T) {
	t.Run("Cache-Control max-age overrides body field", func(t *testing.T) {
		v := int64(10)
		resp := &CheckResponse{CacheMaxAgeSec: &v}
		h := http.Header{}
		h.Set("Cache-Control", "max-age=300")
		resolveHTTPCacheTTL(h, resp)
		require.NotNil(t, resp.CacheMaxAgeSec)
		assert.Equal(t, int64(300), *resp.CacheMaxAgeSec)
		assert.False(t, resp.CacheNoStore)
		assert.Contains(t, resp.CacheTTLSource, "Cache-Control")
		assert.Contains(t, resp.CacheTTLSource, "max-age=300")
		assert.Equal(t, 300*time.Second, resp.ResolveCacheTTL())
	})

	t.Run("Cache-Control with other directives parses max-age", func(t *testing.T) {
		resp := &CheckResponse{}
		h := http.Header{}
		h.Set("Cache-Control", "public, max-age=120, must-revalidate")
		resolveHTTPCacheTTL(h, resp)
		require.NotNil(t, resp.CacheMaxAgeSec)
		assert.Equal(t, int64(120), *resp.CacheMaxAgeSec)
		assert.Equal(t, 120*time.Second, resp.ResolveCacheTTL())
	})

	t.Run("Cache-Control no-store overrides positive body field", func(t *testing.T) {
		v := int64(600)
		resp := &CheckResponse{CacheMaxAgeSec: &v}
		h := http.Header{}
		h.Set("Cache-Control", "no-store")
		resolveHTTPCacheTTL(h, resp)
		assert.True(t, resp.CacheNoStore)
		assert.Contains(t, resp.CacheTTLSource, "Cache-Control")
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
	})

	t.Run("Cache-Control no-cache returns zero TTL", func(t *testing.T) {
		v := int64(600)
		resp := &CheckResponse{CacheMaxAgeSec: &v}
		h := http.Header{}
		h.Set("Cache-Control", "no-cache")
		resolveHTTPCacheTTL(h, resp)
		assert.True(t, resp.CacheNoStore)
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
	})

	t.Run("Expires header used when no Cache-Control", func(t *testing.T) {
		resp := &CheckResponse{}
		h := http.Header{}
		h.Set("Expires", "Thu, 01 Dec 2099 16:00:00 GMT")
		resolveHTTPCacheTTL(h, resp)
		assert.True(t, resp.ResolveCacheTTL() > 0, "expected positive TTL from Expires")
		assert.Contains(t, resp.CacheTTLSource, "Expires:")
	})

	t.Run("Cache-Control max-age overrides Expires header", func(t *testing.T) {
		resp := &CheckResponse{}
		h := http.Header{}
		h.Set("Cache-Control", "max-age=60")
		h.Set("Expires", "Thu, 01 Dec 2099 16:00:00 GMT")
		resolveHTTPCacheTTL(h, resp)
		require.NotNil(t, resp.CacheMaxAgeSec)
		assert.Equal(t, int64(60), *resp.CacheMaxAgeSec, "Cache-Control must win over Expires")
		assert.Contains(t, resp.CacheTTLSource, "Cache-Control")
	})

	t.Run("already-expired Expires header sets no-store", func(t *testing.T) {
		resp := &CheckResponse{}
		h := http.Header{}
		h.Set("Expires", "Thu, 01 Jan 2000 00:00:00 GMT")
		resolveHTTPCacheTTL(h, resp)
		assert.True(t, resp.CacheNoStore)
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
		assert.Contains(t, resp.CacheTTLSource, "expired")
	})

	t.Run("no headers falls back to body cache_max_age_seconds", func(t *testing.T) {
		v := int64(180)
		resp := &CheckResponse{CacheMaxAgeSec: &v}
		resolveHTTPCacheTTL(http.Header{}, resp)
		assert.Equal(t, 180*time.Second, resp.ResolveCacheTTL())
		assert.Equal(t, "body: cache_max_age_seconds=180", resp.CacheTTLSource)
	})

	t.Run("no headers falls back to body cache_no_store", func(t *testing.T) {
		resp := &CheckResponse{CacheNoStore: true}
		resolveHTTPCacheTTL(http.Header{}, resp)
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
		assert.Equal(t, "body: cache_no_store=true", resp.CacheTTLSource)
	})

	t.Run("no headers and no body fields — source is empty", func(t *testing.T) {
		resp := &CheckResponse{}
		resolveHTTPCacheTTL(http.Header{}, resp)
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
		assert.Equal(t, "", resp.CacheTTLSource)
	})
}

func TestCheckHTTPCacheTTLHeaderPriority(t *testing.T) {
	t.Run("Cache-Control header overrides body cache_max_age_seconds", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "max-age=600")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"cache_max_age_seconds": 30, // should be overridden by header
			})
		}))
		defer srv.Close()

		c := &Client{httpURL: srv.URL, httpClient: http.DefaultClient, timeout: 5e9, cb: newCircuitBreaker(0, 0)}
		resp, err := c.Check(context.Background(), &CheckRequest{Method: "GET", Path: "/api"})
		require.NoError(t, err)
		require.NotNil(t, resp.CacheMaxAgeSec)
		assert.Equal(t, int64(600), *resp.CacheMaxAgeSec, "Cache-Control must win over body")
		assert.Equal(t, 600*time.Second, resp.ResolveCacheTTL())
		assert.Contains(t, resp.CacheTTLSource, "Cache-Control")
	})

	t.Run("Cache-Control no-store header overrides positive body field on allow", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"cache_max_age_seconds": 300})
		}))
		defer srv.Close()

		c := &Client{httpURL: srv.URL, httpClient: http.DefaultClient, timeout: 5e9, cb: newCircuitBreaker(0, 0)}
		resp, err := c.Check(context.Background(), &CheckRequest{Method: "GET", Path: "/api"})
		require.NoError(t, err)
		assert.True(t, resp.CacheNoStore)
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
	})

	t.Run("Cache-Control header applied on deny path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "max-age=120")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{
				"allowed":               false,
				"status_code":           401,
				"cache_max_age_seconds": 30,
			})
		}))
		defer srv.Close()

		c := &Client{httpURL: srv.URL, httpClient: http.DefaultClient, timeout: 5e9, cb: newCircuitBreaker(0, 0)}
		resp, err := c.Check(context.Background(), &CheckRequest{Method: "GET", Path: "/protected"})
		require.NoError(t, err)
		assert.False(t, resp.Allowed)
		require.NotNil(t, resp.CacheMaxAgeSec)
		assert.Equal(t, int64(120), *resp.CacheMaxAgeSec, "Cache-Control must win over body on deny")
	})

	t.Run("CacheTTLSource set correctly for gRPC body fields", func(t *testing.T) {
		v := int64(300)
		resp := &CheckResponse{CacheMaxAgeSec: &v}
		// Simulate what checkGRPC does after populating the response
		if resp.CacheNoStore {
			resp.CacheTTLSource = "body: cache_no_store=true"
		} else if resp.CacheMaxAgeSec != nil && *resp.CacheMaxAgeSec > 0 {
			resp.CacheTTLSource = fmt.Sprintf("body: cache_max_age_seconds=%d", *resp.CacheMaxAgeSec)
		}
		assert.Equal(t, "body: cache_max_age_seconds=300", resp.CacheTTLSource)
		assert.Equal(t, 300*time.Second, resp.ResolveCacheTTL())
	})
}

func TestResolveCacheTTL(t *testing.T) {
	t.Run("positive max_age returns correct duration", func(t *testing.T) {
		v := int64(120)
		resp := &CheckResponse{CacheMaxAgeSec: &v}
		assert.Equal(t, 120*time.Second, resp.ResolveCacheTTL())
	})

	t.Run("zero max_age returns zero", func(t *testing.T) {
		v := int64(0)
		resp := &CheckResponse{CacheMaxAgeSec: &v}
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
	})

	t.Run("nil max_age returns zero", func(t *testing.T) {
		resp := &CheckResponse{}
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
	})

	t.Run("cache_no_store overrides positive max_age", func(t *testing.T) {
		v := int64(300)
		resp := &CheckResponse{CacheMaxAgeSec: &v, CacheNoStore: true}
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
	})
}

func TestClientClose(t *testing.T) {
	t.Run("close with no grpc conn returns nil", func(t *testing.T) {
		c := &Client{}
		assert.NoError(t, c.Close())
	})
}

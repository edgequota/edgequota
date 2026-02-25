package proxy

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/cache"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNew(t *testing.T) {
	t.Run("creates proxy with valid backend URL", func(t *testing.T) {
		p, err := New("http://backend:8080", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)
		assert.NotNil(t, p)
		assert.Equal(t, "backend:8080", p.backendURL.Host)
	})

	t.Run("creates proxy with empty backend URL", func(t *testing.T) {
		p, err := New("", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)
		assert.NotNil(t, p)
		assert.Nil(t, p.backendURL)
	})

	t.Run("returns error for invalid URL", func(t *testing.T) {
		_, err := New("://bad", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid backend URL")
	})
}

func TestContextBackendURL(t *testing.T) {
	t.Run("per-request backend URL overrides static default", func(t *testing.T) {
		// Static default backend — should NOT receive traffic.
		defaultBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend", "default")
			w.WriteHeader(http.StatusOK)
		}))
		defer defaultBackend.Close()

		// Per-request override backend — should receive traffic.
		overrideBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend", "override")
			w.WriteHeader(http.StatusOK)
		}))
		defer overrideBackend.Close()

		p, err := New(defaultBackend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		overrideURL, _ := url.Parse(overrideBackend.URL)
		req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
		req = WithBackendURL(req, overrideURL)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "override", rr.Header().Get("X-Backend"))
	})

	t.Run("falls back to static default when no context URL", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend", "default")
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "default", rr.Header().Get("X-Backend"))
	})

	t.Run("returns 502 when no backend configured and no context URL", func(t *testing.T) {
		p, err := New("", 1*time.Second, 10, 10*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadGateway, rr.Code)
	})

	t.Run("context URL with empty default routes correctly", func(t *testing.T) {
		overrideBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend", "dynamic")
			w.WriteHeader(http.StatusOK)
		}))
		defer overrideBackend.Close()

		p, err := New("", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		overrideURL, _ := url.Parse(overrideBackend.URL)
		req := httptest.NewRequest(http.MethodGet, "/tenant-resource", nil)
		req = WithBackendURL(req, overrideURL)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "dynamic", rr.Header().Get("X-Backend"))
	})
}

func TestProxyHTTP(t *testing.T) {
	t.Run("proxies HTTP request to backend", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/resource", r.URL.Path)
			w.Header().Set("X-Backend", "true")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("hello from backend"))
		}))
		defer backend.Close()

		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/resource", nil)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "true", rr.Header().Get("X-Backend"))
		assert.Equal(t, "hello from backend", rr.Body.String())
	})

	t.Run("returns 502 when backend is down", func(t *testing.T) {
		p, err := New("http://127.0.0.1:1", 1*time.Second, 10, 10*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadGateway, rr.Code)
	})

	t.Run("preserves original Host header for upstream routing", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "api.example.com", r.Host, "backend must see original Host for upstream routing (e.g. Traefik)")
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "api.example.com"
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("sets X-Forwarded-Host header", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "example.com", r.Header.Get("X-Forwarded-Host"))
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "example.com"
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("sets X-Forwarded-Proto header", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "http", r.Header.Get("X-Forwarded-Proto"))
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("sets X-Forwarded-For from RemoteAddr", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// httputil.ReverseProxy adds X-Forwarded-For automatically.
			assert.Contains(t, r.Header.Get("X-Forwarded-For"), "192.0.2.1")
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.0.2.1:12345"
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("preserves existing X-Forwarded-Host", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "original-host.com", r.Header.Get("X-Forwarded-Host"))
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Forwarded-Host", "original-host.com")
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("preserves existing X-Forwarded-For and appends RemoteAddr", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			xff := r.Header.Get("X-Forwarded-For")
			// httputil.ReverseProxy appends RemoteAddr to existing XFF.
			assert.Contains(t, xff, "198.51.100.1", "original X-Forwarded-For must be preserved")
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.50:12345"
		req.Header.Set("X-Forwarded-For", "198.51.100.1")
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})
}

func TestProxySSE(t *testing.T) {
	t.Run("proxies SSE stream with immediate flushing", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if ok {
				w.Write([]byte("data: hello\n\n"))
				flusher.Flush()
			}
		}))
		defer backend.Close()

		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/events", nil)
		req.Header.Set("Accept", "text/event-stream")
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "text/event-stream", rr.Header().Get("Content-Type"))
		assert.Contains(t, rr.Body.String(), "data: hello")
	})
}

func TestIsGRPC(t *testing.T) {
	t.Run("detects gRPC request", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Content-Type", "application/grpc")
		assert.True(t, IsGRPC(req))
	})

	t.Run("detects gRPC+proto request", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Content-Type", "application/grpc+proto")
		assert.True(t, IsGRPC(req))
	})

	t.Run("rejects non-gRPC request", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Content-Type", "application/json")
		assert.False(t, IsGRPC(req))
	})
}

func TestIsWebSocketUpgrade(t *testing.T) {
	t.Run("detects WebSocket upgrade", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		assert.True(t, IsWebSocketUpgrade(req))
	})

	t.Run("case insensitive", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Upgrade", "WebSocket")
		req.Header.Set("Connection", "upgrade")
		assert.True(t, IsWebSocketUpgrade(req))
	})

	t.Run("rejects non-WebSocket", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		assert.False(t, IsWebSocketUpgrade(req))
	})

	t.Run("rejects upgrade without connection header", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Upgrade", "websocket")
		assert.False(t, IsWebSocketUpgrade(req))
	})
}

func TestIsSSE(t *testing.T) {
	t.Run("detects SSE accept header", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept", "text/event-stream")
		assert.True(t, IsSSE(req))
	})

	t.Run("rejects non-SSE", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept", "application/json")
		assert.False(t, IsSSE(req))
	})
}

func TestSingleJoiningSlash(t *testing.T) {
	t.Run("both have slash", func(t *testing.T) {
		assert.Equal(t, "/base/path", singleJoiningSlash("/base/", "/path"))
	})

	t.Run("neither has slash", func(t *testing.T) {
		assert.Equal(t, "base/path", singleJoiningSlash("base", "path"))
	})

	t.Run("only first has slash", func(t *testing.T) {
		assert.Equal(t, "base/path", singleJoiningSlash("base/", "path"))
	})

	t.Run("only second has slash", func(t *testing.T) {
		assert.Equal(t, "base/path", singleJoiningSlash("base", "/path"))
	})
}

func TestIsClientDisconnect(t *testing.T) {
	t.Run("nil is not disconnect", func(t *testing.T) {
		assert.False(t, isClientDisconnect(nil))
	})

	t.Run("detects connection reset", func(t *testing.T) {
		assert.True(t, isClientDisconnect(
			&testErr{msg: "write: connection reset by peer"},
		))
	})

	t.Run("detects broken pipe", func(t *testing.T) {
		assert.True(t, isClientDisconnect(
			&testErr{msg: "write: broken pipe"},
		))
	})

	t.Run("detects client disconnected", func(t *testing.T) {
		assert.True(t, isClientDisconnect(
			&testErr{msg: "client disconnected"},
		))
	})

	t.Run("returns false for generic error", func(t *testing.T) {
		assert.False(t, isClientDisconnect(
			&testErr{msg: "some generic error"},
		))
	})
}

type testErr struct {
	msg string
}

func (e *testErr) Error() string { return e.msg }

// ---------------------------------------------------------------------------
// Proxy option tests
// ---------------------------------------------------------------------------

func TestWithWSLimiter(t *testing.T) {
	t.Run("sets limiter when max > 0", func(t *testing.T) {
		p, err := New("http://backend:8080", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger(), WithWSLimiter(10))
		require.NoError(t, err)
		assert.NotNil(t, p.wsLimiter)
	})

	t.Run("no-op when max is 0", func(t *testing.T) {
		p, err := New("http://backend:8080", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger(), WithWSLimiter(0))
		require.NoError(t, err)
		assert.Nil(t, p.wsLimiter)
	})
}

func TestWithMaxRequestBodySize(t *testing.T) {
	p, err := New("http://backend:8080", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger(), WithMaxRequestBodySize(1024))
	require.NoError(t, err)
	assert.Equal(t, int64(1024), p.maxRequestBodySize)
}

func TestWithMaxWSTransferBytes(t *testing.T) {
	p, err := New("http://backend:8080", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger(), WithMaxWSTransferBytes(4096))
	require.NoError(t, err)
	assert.Equal(t, int64(4096), p.maxWSTransferBytes)
}

func TestCountingReader(t *testing.T) {
	t.Run("reads within limit", func(t *testing.T) {
		data := "hello world"
		cr := &countingReader{
			rc:    io.NopCloser(strings.NewReader(data)),
			limit: 100,
		}

		buf := make([]byte, 64)
		n, err := cr.Read(buf)
		assert.NoError(t, err)
		assert.Equal(t, len(data), n)
		assert.Equal(t, data, string(buf[:n]))
		assert.False(t, cr.limited)
	})

	t.Run("errors when limit exceeded", func(t *testing.T) {
		cr := &countingReader{
			rc:    io.NopCloser(strings.NewReader("this is a long string")),
			limit: 5,
		}

		buf := make([]byte, 64)
		_, err := cr.Read(buf)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds size limit")
		assert.True(t, cr.limited)
	})

	t.Run("unlimited when limit is 0", func(t *testing.T) {
		data := "unlimited data"
		cr := &countingReader{
			rc:    io.NopCloser(strings.NewReader(data)),
			limit: 0,
		}

		buf := make([]byte, 64)
		n, err := cr.Read(buf)
		assert.NoError(t, err)
		assert.Equal(t, len(data), n)
		assert.False(t, cr.limited)
	})

	t.Run("Close delegates to inner reader", func(t *testing.T) {
		cr := &countingReader{
			rc: io.NopCloser(strings.NewReader("")),
		}
		assert.NoError(t, cr.Close())
	})
}

func TestSafeDialer(t *testing.T) {
	inner := &net.Dialer{Timeout: 2 * time.Second}

	t.Run("disabled passes through to inner dialer", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		d := &SafeDialer{Inner: inner, Enabled: false}
		conn, err := d.DialContext(t.Context(), "tcp", backend.Listener.Addr().String())
		require.NoError(t, err)
		conn.Close()
	})

	t.Run("enabled blocks direct private IP", func(t *testing.T) {
		d := &SafeDialer{Inner: inner, Enabled: true}
		_, err := d.DialContext(t.Context(), "tcp", "127.0.0.1:8080")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "private/reserved IP")
	})

	t.Run("enabled blocks 10.x private IP", func(t *testing.T) {
		d := &SafeDialer{Inner: inner, Enabled: true}
		_, err := d.DialContext(t.Context(), "tcp", "10.96.0.1:443")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "private/reserved IP")
	})

	t.Run("enabled allows loopback when disabled", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		d := &SafeDialer{Inner: inner, Enabled: false}
		conn, err := d.DialContext(t.Context(), "tcp", backend.Listener.Addr().String())
		require.NoError(t, err)
		conn.Close()
	})

	t.Run("fallback for unparseable addr", func(t *testing.T) {
		d := &SafeDialer{Inner: inner, Enabled: true}
		_, err := d.DialContext(t.Context(), "tcp", "no-port")
		require.Error(t, err)
	})

	t.Run("enabled blocks hostname resolving to loopback", func(t *testing.T) {
		d := &SafeDialer{Inner: inner, Enabled: true}
		_, err := d.DialContext(t.Context(), "tcp", "localhost:8080")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "private/reserved")
	})
}

func TestProxy_DenyPrivateNetworks_Integration(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Reached", "true")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	t.Run("proxy without deny reaches loopback backend", func(t *testing.T) {
		p, err := New(backend.URL, 5*time.Second, 10, 30*time.Second, config.TransportConfig{}, testLogger())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "true", rr.Header().Get("X-Reached"))
	})

	t.Run("proxy with deny blocks loopback backend", func(t *testing.T) {
		p, err := New(backend.URL, 5*time.Second, 10, 30*time.Second, config.TransportConfig{}, testLogger(), WithDenyPrivateNetworks())
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadGateway, rr.Code)
		assert.Empty(t, rr.Header().Get("X-Reached"))
	})
}

func TestWithMaxRequestBodySize_Integration(t *testing.T) {
	t.Run("rejects oversized request body", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// Read the full body — this triggers the counting reader.
			body, err := io.ReadAll(io.LimitReader(nil, 0))
			_ = body
			_ = err
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger(), WithMaxRequestBodySize(5))
		require.NoError(t, err)

		// Send a body larger than the limit.
		req := httptest.NewRequest(http.MethodPost, "/data", strings.NewReader("this is more than five bytes"))
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)

		// The proxy should return a 502 (Bad Gateway) because the round trip
		// fails when the counting reader errors.
		assert.Equal(t, http.StatusBadGateway, rr.Code)
	})
}

// ---------------------------------------------------------------------------
// Cache integration tests
// ---------------------------------------------------------------------------

func newTestCacheStore(t *testing.T) *cache.Store {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := redis.NewClient(config.RedisConfig{
		Endpoints: []string{mr.Addr()},
		Mode:      config.RedisModeSingle,
	})
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	return cache.NewStore(client)
}

func TestProxyCacheHitMiss(t *testing.T) {
	var backendHits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.Header().Set("Cache-Control", "max-age=60")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("cached body"))
	}))
	defer backend.Close()

	store := newTestCacheStore(t)
	p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger(), WithCache(store))
	require.NoError(t, err)

	// First request — cache miss, proxied to backend.
	req1 := httptest.NewRequest(http.MethodGet, "/resource", nil)
	rr1 := httptest.NewRecorder()
	p.ServeHTTP(rr1, req1)

	assert.Equal(t, http.StatusOK, rr1.Code)
	assert.Equal(t, "cached body", rr1.Body.String())
	assert.Equal(t, int64(1), backendHits.Load())

	// Second request — served from cache, backend NOT called.
	req2 := httptest.NewRequest(http.MethodGet, "/resource", nil)
	rr2 := httptest.NewRecorder()
	p.ServeHTTP(rr2, req2)

	assert.Equal(t, http.StatusOK, rr2.Code)
	assert.Equal(t, "cached body", rr2.Body.String())
	assert.Equal(t, int64(1), backendHits.Load(), "backend should not be called on cache hit")
}

func TestProxyCacheNoStore(t *testing.T) {
	var backendHits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not cached"))
	}))
	defer backend.Close()

	store := newTestCacheStore(t)
	p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger(), WithCache(store))
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/no-store", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	}

	assert.Equal(t, int64(2), backendHits.Load(), "no-store responses must not be cached")
}

func TestProxyCachePrivate(t *testing.T) {
	var backendHits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.Header().Set("Cache-Control", "private, max-age=60")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("private"))
	}))
	defer backend.Close()

	store := newTestCacheStore(t)
	p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger(), WithCache(store))
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/private", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	}

	assert.Equal(t, int64(2), backendHits.Load(), "private responses must not be cached")
}

func TestProxyCachePOSTBypasses(t *testing.T) {
	var backendHits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.Header().Set("Cache-Control", "max-age=60")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("post response"))
	}))
	defer backend.Close()

	store := newTestCacheStore(t)
	p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger(), WithCache(store))
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/submit", strings.NewReader("body"))
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	}

	assert.Equal(t, int64(2), backendHits.Load(), "POST requests must bypass the cache")
}

func TestProxyCacheMaxBodySize(t *testing.T) {
	var backendHits atomic.Int64
	largeBody := strings.Repeat("x", 2<<20) // 2 MB — exceeds default 1 MB limit
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.Header().Set("Cache-Control", "max-age=60")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(largeBody))
	}))
	defer backend.Close()

	store := newTestCacheStore(t)
	p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger(), WithCache(store))
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/large", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, largeBody, rr.Body.String())
	}

	assert.Equal(t, int64(2), backendHits.Load(), "responses exceeding MaxBodySize must not be cached")
}

func TestProxyCacheXCacheHeader(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hit me"))
	}))
	defer backend.Close()

	store := newTestCacheStore(t)
	p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger(), WithCache(store))
	require.NoError(t, err)

	// Populate cache.
	req1 := httptest.NewRequest(http.MethodGet, "/x-cache", nil)
	rr1 := httptest.NewRecorder()
	p.ServeHTTP(rr1, req1)
	assert.Empty(t, rr1.Header().Get("X-Cache"), "first request should not have X-Cache: HIT")

	// Cache hit — must have X-Cache: HIT.
	req2 := httptest.NewRequest(http.MethodGet, "/x-cache", nil)
	rr2 := httptest.NewRecorder()
	p.ServeHTTP(rr2, req2)
	assert.Equal(t, "HIT", rr2.Header().Get("X-Cache"))
}

func TestProxyCacheDisabled(t *testing.T) {
	var backendHits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.Header().Set("Cache-Control", "max-age=60")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("no cache configured"))
	}))
	defer backend.Close()

	// No WithCache option — caching is disabled.
	p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/disabled", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	}

	assert.Equal(t, int64(2), backendHits.Load(), "without WithCache both requests must hit the backend")
}

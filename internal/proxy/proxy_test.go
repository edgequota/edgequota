package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/edgequota/edgequota/internal/config"
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

	t.Run("returns error for invalid URL", func(t *testing.T) {
		_, err := New("://bad", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, testLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid backend URL")
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

	t.Run("sets X-Forwarded-Host header", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.NotEmpty(t, r.Header.Get("X-Forwarded-Host"))
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
		assert.True(t, isGRPC(req))
	})

	t.Run("detects gRPC+proto request", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Content-Type", "application/grpc+proto")
		assert.True(t, isGRPC(req))
	})

	t.Run("rejects non-gRPC request", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Content-Type", "application/json")
		assert.False(t, isGRPC(req))
	})
}

func TestIsWebSocketUpgrade(t *testing.T) {
	t.Run("detects WebSocket upgrade", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		assert.True(t, isWebSocketUpgrade(req))
	})

	t.Run("case insensitive", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Upgrade", "WebSocket")
		req.Header.Set("Connection", "upgrade")
		assert.True(t, isWebSocketUpgrade(req))
	})

	t.Run("rejects non-WebSocket", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		assert.False(t, isWebSocketUpgrade(req))
	})

	t.Run("rejects upgrade without connection header", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Upgrade", "websocket")
		assert.False(t, isWebSocketUpgrade(req))
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

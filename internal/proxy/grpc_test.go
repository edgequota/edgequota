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

func TestProxyGRPCDetection(t *testing.T) {
	t.Run("routes HTTP/2 gRPC request to h2 transport", func(t *testing.T) {
		// gRPC always uses HTTP/2. The transport selector routes based on
		// ProtoMajor, not Content-Type. This test sends an HTTP/2 request
		// which will be routed to the h2 transport — that transport can't
		// connect to an HTTP/1.1 httptest server, so we expect 502.
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/package.Service/Method", nil)
		req.Header.Set("Content-Type", "application/grpc")
		req.ProtoMajor = 2
		req.Proto = "HTTP/2.0"
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusBadGateway, rr.Code)
	})
}

func TestProxyHTTP2Detection(t *testing.T) {
	t.Run("routes plain HTTP/2 request to h2 transport", func(t *testing.T) {
		// Any HTTP/2 request (not just gRPC) must use the h2 transport.
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/data", nil)
		req.ProtoMajor = 2
		req.Proto = "HTTP/2.0"
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		// HTTP/2 transport can't connect to HTTP/1.1 httptest server → 502.
		assert.Equal(t, http.StatusBadGateway, rr.Code)
	})
}

func TestProtocolAwareTransport(t *testing.T) {
	t.Run("routes HTTP/1.1 to http1 transport", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/v1", nil)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("proxies POST requests", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			w.WriteHeader(http.StatusCreated)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/api/resource", nil)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusCreated, rr.Code)
	})

	t.Run("handles backend returning error status", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/not-found", nil)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("preserves response headers from backend", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Custom", "test-value")
			w.Header().Set("X-Request-Id", "abc123")
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, "test-value", rr.Header().Get("X-Custom"))
		assert.Equal(t, "abc123", rr.Header().Get("X-Request-Id"))
	})

	t.Run("passes query parameters to backend", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "bar", r.URL.Query().Get("foo"))
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api?foo=bar", nil)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})
}

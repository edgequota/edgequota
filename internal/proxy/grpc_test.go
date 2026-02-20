package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	t.Run("routes plain HTTP/2 request via h2 transport in auto mode", func(t *testing.T) {
		// In auto mode, non-gRPC HTTP/2 requests are forwarded via the h2
		// transport to preserve protocol end-to-end. The h2 transport can't
		// reach an HTTP/1.1 httptest server, so we expect 502.
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
		assert.Equal(t, http.StatusBadGateway, rr.Code)
	})

	t.Run("gRPC always uses h2 even with backend_protocol=h1", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{
			BackendProtocol: "h1",
		}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/pkg.Svc/Method", nil)
		req.Header.Set("Content-Type", "application/grpc")
		req.ProtoMajor = 2
		req.Proto = "HTTP/2.0"
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		// gRPC h2 transport can't reach HTTP/1.1 backend -> 502.
		assert.Equal(t, http.StatusBadGateway, rr.Code)
	})
}

func TestBackendProtocolConfig(t *testing.T) {
	t.Run("explicit h1 uses http1 transport", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{
			BackendProtocol: "h1",
		}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("explicit h2 uses h2 transport", func(t *testing.T) {
		// h2 transport to an HTTP/1.1 httptest server -> 502, proving h2 was selected.
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{
			BackendProtocol: "h2",
		}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusBadGateway, rr.Code)
	})

	t.Run("auto with cleartext defaults to h1", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{
			BackendProtocol: "auto",
		}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("invalid backend_protocol rejected", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		_, err := New("http://localhost:1234", 30*time.Second, 100, 90*time.Second, config.TransportConfig{
			BackendProtocol: "h4",
		}, logger)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid backend_protocol")
	})

	t.Run("h3 with cleartext backend falls back to h1", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(backend.URL, 30*time.Second, 100, 90*time.Second, config.TransportConfig{
			BackendProtocol: "h3",
		}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})
}

func TestDynamicBackendTransport(t *testing.T) {
	t.Run("auto mode without static backend uses h1 as safe default", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		// Empty backend URL = dynamic-only mode.
		p, err := New("", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		backendURL, err := url.Parse(backend.URL)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/data", nil)
		req = WithBackendURL(req, backendURL)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("explicit h1 applies to dynamic backends", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New("", 30*time.Second, 100, 90*time.Second, config.TransportConfig{
			BackendProtocol: "h1",
		}, logger)
		require.NoError(t, err)

		backendURL, err := url.Parse(backend.URL)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/data", nil)
		req = WithBackendURL(req, backendURL)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("gRPC to dynamic backend still uses h2", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New("", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		backendURL, err := url.Parse(backend.URL)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/pkg.Svc/Method", nil)
		req.Header.Set("Content-Type", "application/grpc")
		req.ProtoMajor = 2
		req.Proto = "HTTP/2.0"
		req = WithBackendURL(req, backendURL)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		// h2 transport can't reach HTTP/1.1 test backend -> 502.
		assert.Equal(t, http.StatusBadGateway, rr.Code)
	})

	t.Run("explicit h2 forces h2 for dynamic backends", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New("", 30*time.Second, 100, 90*time.Second, config.TransportConfig{
			BackendProtocol: "h2",
		}, logger)
		require.NoError(t, err)

		backendURL, err := url.Parse(backend.URL)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		req = WithBackendURL(req, backendURL)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		// h2 transport to HTTP/1.1 backend -> 502, proving h2 was used.
		assert.Equal(t, http.StatusBadGateway, rr.Code)
	})

	t.Run("no context URL and no static backend returns 502", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New("", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusBadGateway, rr.Code)
	})

	t.Run("per-request h2 override with auto default", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New("", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		backendURL, err := url.Parse(backend.URL)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		req = WithBackendURL(req, backendURL)
		req = WithBackendProtocol(req, "h2")
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		// h2 transport to HTTP/1.1 test backend -> 502, proving h2 was used.
		assert.Equal(t, http.StatusBadGateway, rr.Code)
	})

	t.Run("per-request h1 override with h2 default", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New("", 30*time.Second, 100, 90*time.Second, config.TransportConfig{
			BackendProtocol: "h2",
		}, logger)
		require.NoError(t, err)

		backendURL, err := url.Parse(backend.URL)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		req = WithBackendURL(req, backendURL)
		req = WithBackendProtocol(req, "h1")
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		// h1 override should succeed against HTTP/1.1 backend, proving the override worked.
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("per-request unknown protocol falls back to default", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New("", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		backendURL, err := url.Parse(backend.URL)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		req = WithBackendURL(req, backendURL)
		req = WithBackendProtocol(req, "bogus")
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		// Unknown protocol falls back to default (h1 for http:// backends) -> 200.
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("per-request h3 without h3 transport falls back to default", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		// Cleartext backend = no h3 transport available.
		p, err := New("", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		backendURL, err := url.Parse(backend.URL)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		req = WithBackendURL(req, backendURL)
		req = WithBackendProtocol(req, "h3")
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		// h3 requested but no h3 transport -> falls back to default -> 200.
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("gRPC ignores per-request protocol override", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New("", 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		backendURL, err := url.Parse(backend.URL)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/pkg.Svc/Method", nil)
		req.Header.Set("Content-Type", "application/grpc")
		req.ProtoMajor = 2
		req.Proto = "HTTP/2.0"
		req = WithBackendURL(req, backendURL)
		req = WithBackendProtocol(req, "h1")
		rr := httptest.NewRecorder()

		p.ServeHTTP(rr, req)
		// gRPC always uses h2 regardless of the per-request override -> 502.
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

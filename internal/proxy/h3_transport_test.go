package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildTransports_H3UDPBuffers(t *testing.T) {
	t.Run("no h3 transport for cleartext backend", func(t *testing.T) {
		_, _, h3 := buildTransports("http", config.TransportConfig{}, 30*time.Second, 100, 90*time.Second, false, false)
		assert.Nil(t, h3, "h3 transport must be nil for cleartext (http) backends")
	})

	t.Run("h3 transport created for HTTPS backend without buffer config", func(t *testing.T) {
		_, _, h3 := buildTransports("https", config.TransportConfig{}, 30*time.Second, 100, 90*time.Second, true, false)
		require.NotNil(t, h3, "h3 transport must be non-nil for HTTPS backends")

		h3t, ok := h3.(*http3.Transport)
		require.True(t, ok, "h3 transport must be *http3.Transport")
		assert.Nil(t, h3t.Dial, "Dial should be nil when no buffer sizes are configured")
	})

	t.Run("h3 transport gets custom Dial when receive buffer is set", func(t *testing.T) {
		_, _, h3 := buildTransports("https", config.TransportConfig{
			H3UDPReceiveBufferSize: 7500000,
		}, 30*time.Second, 100, 90*time.Second, true, false)
		require.NotNil(t, h3)

		h3t, ok := h3.(*http3.Transport)
		require.True(t, ok)
		assert.NotNil(t, h3t.Dial, "Dial must be set when H3UDPReceiveBufferSize > 0")
	})

	t.Run("h3 transport gets custom Dial when send buffer is set", func(t *testing.T) {
		_, _, h3 := buildTransports("https", config.TransportConfig{
			H3UDPSendBufferSize: 7500000,
		}, 30*time.Second, 100, 90*time.Second, true, false)
		require.NotNil(t, h3)

		h3t, ok := h3.(*http3.Transport)
		require.True(t, ok)
		assert.NotNil(t, h3t.Dial, "Dial must be set when H3UDPSendBufferSize > 0")
	})

	t.Run("h3 transport gets custom Dial when both buffers are set", func(t *testing.T) {
		_, _, h3 := buildTransports("https", config.TransportConfig{
			H3UDPReceiveBufferSize: 7500000,
			H3UDPSendBufferSize:    7500000,
		}, 30*time.Second, 100, 90*time.Second, true, false)
		require.NotNil(t, h3)

		h3t, ok := h3.(*http3.Transport)
		require.True(t, ok)
		assert.NotNil(t, h3t.Dial, "Dial must be set when both buffer sizes > 0")
	})

	t.Run("h3 transport Dial is nil when buffer sizes are zero", func(t *testing.T) {
		_, _, h3 := buildTransports("https", config.TransportConfig{
			H3UDPReceiveBufferSize: 0,
			H3UDPSendBufferSize:    0,
		}, 30*time.Second, 100, 90*time.Second, true, false)
		require.NotNil(t, h3)

		h3t, ok := h3.(*http3.Transport)
		require.True(t, ok)
		assert.Nil(t, h3t.Dial, "Dial should be nil when buffer sizes are 0")
	})

	t.Run("h3 transport preserves TLS config regardless of buffer settings", func(t *testing.T) {
		_, _, h3 := buildTransports("https", config.TransportConfig{
			H3UDPReceiveBufferSize: 7500000,
		}, 30*time.Second, 100, 90*time.Second, true, false)
		require.NotNil(t, h3)

		h3t, ok := h3.(*http3.Transport)
		require.True(t, ok)
		require.NotNil(t, h3t.TLSClientConfig, "TLSClientConfig must always be set")
		assert.True(t, h3t.TLSClientConfig.InsecureSkipVerify,
			"InsecureSkipVerify must match the tlsInsecure parameter")
	})

	t.Run("h3 transport respects TLS insecure flag when buffers configured", func(t *testing.T) {
		_, _, h3 := buildTransports("https", config.TransportConfig{
			H3UDPReceiveBufferSize: 7500000,
		}, 30*time.Second, 100, 90*time.Second, false, false)
		require.NotNil(t, h3)

		h3t, ok := h3.(*http3.Transport)
		require.True(t, ok)
		assert.False(t, h3t.TLSClientConfig.InsecureSkipVerify,
			"InsecureSkipVerify=false when tlsInsecure is false")
	})
}

func TestBuildTransports_H3WithStreamingTransport(t *testing.T) {
	t.Run("buffer-configured h3 is wrapped by streamingTransport when body limit set", func(t *testing.T) {
		tlsCfg := selfSignedTLS(t)
		backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		backend.TLS = tlsCfg
		backend.StartTLS()
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(
			backend.URL,
			30*time.Second, 100, 90*time.Second,
			config.TransportConfig{
				BackendProtocol:        "h3",
				H3UDPReceiveBufferSize: 7500000,
			},
			logger,
			WithBackendTLSInsecure(),
			WithMaxRequestBodySize(1024),
		)
		require.NoError(t, err)
		assert.NotNil(t, p, "proxy must be created successfully")
	})
}

func TestNewProxy_H3BufferConfig(t *testing.T) {
	t.Run("h3 with HTTPS backend and buffer config creates proxy", func(t *testing.T) {
		tlsCfg := selfSignedTLS(t)
		backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		backend.TLS = tlsCfg
		backend.StartTLS()
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(
			backend.URL,
			30*time.Second, 100, 90*time.Second,
			config.TransportConfig{
				BackendProtocol:        "h3",
				H3UDPReceiveBufferSize: 7500000,
				H3UDPSendBufferSize:    7500000,
			},
			logger,
			WithBackendTLSInsecure(),
		)
		require.NoError(t, err)
		require.NotNil(t, p)
	})

	t.Run("h3 with HTTPS backend and zero buffers creates proxy with default h3", func(t *testing.T) {
		tlsCfg := selfSignedTLS(t)
		backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		backend.TLS = tlsCfg
		backend.StartTLS()
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(
			backend.URL,
			30*time.Second, 100, 90*time.Second,
			config.TransportConfig{
				BackendProtocol: "h3",
			},
			logger,
			WithBackendTLSInsecure(),
		)
		require.NoError(t, err)
		require.NotNil(t, p)
	})

	t.Run("h3 with cleartext backend falls back to h1 regardless of buffer config", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(
			backend.URL,
			30*time.Second, 100, 90*time.Second,
			config.TransportConfig{
				BackendProtocol:        "h3",
				H3UDPReceiveBufferSize: 7500000,
				H3UDPSendBufferSize:    7500000,
			},
			logger,
		)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code,
			"h3 with cleartext backend must fall back to h1 and succeed")
	})

	t.Run("per-request h3 override without buffer config still creates proxy", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(
			backend.URL,
			30*time.Second, 100, 90*time.Second,
			config.TransportConfig{},
			logger,
		)
		require.NoError(t, err)
		require.NotNil(t, p)
	})
}

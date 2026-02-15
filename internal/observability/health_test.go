package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHealthChecker(t *testing.T) {
	t.Run("starts in not-ready state", func(t *testing.T) {
		h := NewHealthChecker()
		assert.False(t, h.IsReady())
	})
}

func TestHealthCheckerSetReady(t *testing.T) {
	t.Run("marks service as ready", func(t *testing.T) {
		h := NewHealthChecker()
		h.SetReady()
		assert.True(t, h.IsReady())
	})
}

func TestHealthCheckerSetNotReady(t *testing.T) {
	t.Run("marks service as not ready", func(t *testing.T) {
		h := NewHealthChecker()
		h.SetReady()
		h.SetNotReady()
		assert.False(t, h.IsReady())
	})
}

func TestHealthCheckerSetStarted(t *testing.T) {
	t.Run("marks service as started", func(t *testing.T) {
		h := NewHealthChecker()
		assert.False(t, h.IsStarted())
		h.SetStarted()
		assert.True(t, h.IsStarted())
	})
}

func TestStartzHandler(t *testing.T) {
	t.Run("returns 503 before startup completes", func(t *testing.T) {
		h := NewHealthChecker()
		handler := h.StartzHandler()

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/startz", nil)
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
		assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))

		var body map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, "not_started", body["status"])
	})

	t.Run("returns 200 after startup completes", func(t *testing.T) {
		h := NewHealthChecker()
		h.SetStarted()
		handler := h.StartzHandler()

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/startz", nil)
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)

		var body map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, "started", body["status"])
	})
}

func TestHealthzHandler(t *testing.T) {
	t.Run("returns 200 always", func(t *testing.T) {
		h := NewHealthChecker()
		handler := h.HealthzHandler()

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))

		var body map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, "alive", body["status"])
	})

	t.Run("returns 200 even when not ready", func(t *testing.T) {
		h := NewHealthChecker()
		handler := h.HealthzHandler()

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})
}

func TestReadyzHandler(t *testing.T) {
	t.Run("returns 503 when not ready", func(t *testing.T) {
		h := NewHealthChecker()
		handler := h.ReadyzHandler()

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)

		var body map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, "not_ready", body["status"])
	})

	t.Run("returns 200 when ready", func(t *testing.T) {
		h := NewHealthChecker()
		h.SetReady()
		handler := h.ReadyzHandler()

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)

		var body map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, "ready", body["status"])
	})
}

// ---------------------------------------------------------------------------
// SetRedisPinger + deep health check tests
// ---------------------------------------------------------------------------

type mockPinger struct {
	err error
}

func (m *mockPinger) Ping(_ context.Context) error { return m.err }

func TestSetRedisPinger(t *testing.T) {
	t.Run("sets and clears redis pinger", func(t *testing.T) {
		h := NewHealthChecker()
		h.SetRedisPinger(&mockPinger{})
		h.SetRedisPinger(nil) // clear
	})
}

func TestReadyzHandler_DeepCheck(t *testing.T) {
	t.Run("deep=true returns 200 when Redis is healthy", func(t *testing.T) {
		h := NewHealthChecker()
		h.SetReady()
		h.SetRedisPinger(&mockPinger{err: nil})
		handler := h.ReadyzHandler()

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/readyz?deep=true", nil)
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, "ready", body["status"])
		assert.Equal(t, "ok", body["redis"])
	})

	t.Run("deep=true returns 503 when Redis is unreachable", func(t *testing.T) {
		h := NewHealthChecker()
		h.SetReady()
		h.SetRedisPinger(&mockPinger{err: fmt.Errorf("connection refused")})
		handler := h.ReadyzHandler()

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/readyz?deep=true", nil)
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	})

	t.Run("deep=true returns 200 when no pinger is set", func(t *testing.T) {
		h := NewHealthChecker()
		h.SetReady()
		handler := h.ReadyzHandler()

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/readyz?deep=true", nil)
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})
}

package ratelimit

import (
	"net/http"
	"testing"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientIPStrategy(t *testing.T) {
	t.Run("extracts from X-Forwarded-For", func(t *testing.T) {
		s := &ClientIPStrategy{}
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

		key, err := s.Extract(req)
		require.NoError(t, err)
		assert.Equal(t, "1.2.3.4", key)
	})

	t.Run("extracts from X-Real-IP when XFF missing", func(t *testing.T) {
		s := &ClientIPStrategy{}
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Real-IP", "10.0.0.1")

		key, err := s.Extract(req)
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.1", key)
	})

	t.Run("falls back to RemoteAddr", func(t *testing.T) {
		s := &ClientIPStrategy{}
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.168.1.1:12345"

		key, err := s.Extract(req)
		require.NoError(t, err)
		assert.Equal(t, "192.168.1.1", key)
	})

	t.Run("handles RemoteAddr without port", func(t *testing.T) {
		s := &ClientIPStrategy{}
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.168.1.1"

		key, err := s.Extract(req)
		require.NoError(t, err)
		assert.Equal(t, "192.168.1.1", key)
	})

	t.Run("trims whitespace from XFF", func(t *testing.T) {
		s := &ClientIPStrategy{}
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Forwarded-For", " 1.2.3.4 , 5.6.7.8")

		key, err := s.Extract(req)
		require.NoError(t, err)
		assert.Equal(t, "1.2.3.4", key)
	})
}

func TestHeaderStrategy(t *testing.T) {
	t.Run("extracts header value", func(t *testing.T) {
		s := &HeaderStrategy{HeaderName: "X-Tenant-Id"}
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Tenant-Id", "tenant-123")

		key, err := s.Extract(req)
		require.NoError(t, err)
		assert.Equal(t, "tenant-123", key)
	})

	t.Run("returns error for missing header", func(t *testing.T) {
		s := &HeaderStrategy{HeaderName: "X-Tenant-Id"}
		req, _ := http.NewRequest(http.MethodGet, "/", nil)

		_, err := s.Extract(req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "X-Tenant-Id")
	})

	t.Run("returns error for empty header", func(t *testing.T) {
		s := &HeaderStrategy{HeaderName: "X-Tenant-Id"}
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Tenant-Id", "")

		_, err := s.Extract(req)
		assert.Error(t, err)
	})
}

func TestCompositeStrategy(t *testing.T) {
	t.Run("extracts header without path prefix", func(t *testing.T) {
		s := &CompositeStrategy{HeaderName: "X-Tenant-Id", PathPrefix: false}
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/users", nil)
		req.Header.Set("X-Tenant-Id", "tenant-123")

		key, err := s.Extract(req)
		require.NoError(t, err)
		assert.Equal(t, "tenant-123", key)
	})

	t.Run("extracts header with path prefix", func(t *testing.T) {
		s := &CompositeStrategy{HeaderName: "X-Tenant-Id", PathPrefix: true}
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/users", nil)
		req.Header.Set("X-Tenant-Id", "tenant-123")

		key, err := s.Extract(req)
		require.NoError(t, err)
		assert.Equal(t, "tenant-123:api", key)
	})

	t.Run("returns error for missing header", func(t *testing.T) {
		s := &CompositeStrategy{HeaderName: "X-Tenant-Id", PathPrefix: true}
		req, _ := http.NewRequest(http.MethodGet, "/api/v1", nil)

		_, err := s.Extract(req)
		assert.Error(t, err)
	})
}

func TestFirstPathSegment(t *testing.T) {
	t.Run("extracts first segment", func(t *testing.T) {
		assert.Equal(t, "api", FirstPathSegment("/api/v1/users"))
	})

	t.Run("handles single segment", func(t *testing.T) {
		assert.Equal(t, "api", FirstPathSegment("/api"))
	})

	t.Run("returns root for empty path", func(t *testing.T) {
		assert.Equal(t, "root", FirstPathSegment("/"))
	})

	t.Run("returns root for bare slash", func(t *testing.T) {
		assert.Equal(t, "root", FirstPathSegment(""))
	})

	t.Run("handles no leading slash", func(t *testing.T) {
		assert.Equal(t, "api", FirstPathSegment("api/v1"))
	})
}

func TestNewKeyStrategy(t *testing.T) {
	t.Run("creates clientIP strategy by default", func(t *testing.T) {
		ks, err := NewKeyStrategy(config.KeyStrategyConfig{})
		require.NoError(t, err)
		assert.IsType(t, &ClientIPStrategy{}, ks)
	})

	t.Run("creates clientIP strategy explicitly", func(t *testing.T) {
		ks, err := NewKeyStrategy(config.KeyStrategyConfig{Type: config.KeyStrategyClientIP})
		require.NoError(t, err)
		assert.IsType(t, &ClientIPStrategy{}, ks)
	})

	t.Run("creates header strategy", func(t *testing.T) {
		ks, err := NewKeyStrategy(config.KeyStrategyConfig{
			Type:       config.KeyStrategyHeader,
			HeaderName: "X-Tenant-Id",
		})
		require.NoError(t, err)
		assert.IsType(t, &HeaderStrategy{}, ks)
	})

	t.Run("header strategy requires header name", func(t *testing.T) {
		_, err := NewKeyStrategy(config.KeyStrategyConfig{Type: config.KeyStrategyHeader})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "header_name")
	})

	t.Run("creates composite strategy", func(t *testing.T) {
		ks, err := NewKeyStrategy(config.KeyStrategyConfig{
			Type:       config.KeyStrategyComposite,
			HeaderName: "X-Tenant-Id",
			PathPrefix: true,
		})
		require.NoError(t, err)
		cs, ok := ks.(*CompositeStrategy)
		require.True(t, ok)
		assert.True(t, cs.PathPrefix)
	})

	t.Run("composite strategy requires header name", func(t *testing.T) {
		_, err := NewKeyStrategy(config.KeyStrategyConfig{Type: config.KeyStrategyComposite})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "header_name")
	})

	t.Run("returns error for unknown type", func(t *testing.T) {
		_, err := NewKeyStrategy(config.KeyStrategyConfig{Type: "magic"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unknown key strategy")
	})

	t.Run("header name is canonicalized", func(t *testing.T) {
		ks, err := NewKeyStrategy(config.KeyStrategyConfig{
			Type:       config.KeyStrategyHeader,
			HeaderName: "x-tenant-id",
		})
		require.NoError(t, err)
		hs := ks.(*HeaderStrategy)
		assert.Equal(t, "X-Tenant-Id", hs.HeaderName)
	})
}

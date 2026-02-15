package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewHeaderFilter(t *testing.T) {
	t.Run("empty config uses default sensitive headers deny list", func(t *testing.T) {
		hf := NewHeaderFilter(HeaderFilterConfig{})

		assert.False(t, hf.Allowed("Authorization"))
		assert.False(t, hf.Allowed("authorization"))
		assert.False(t, hf.Allowed("Cookie"))
		assert.False(t, hf.Allowed("Set-Cookie"))
		assert.False(t, hf.Allowed("Proxy-Authorization"))
		assert.False(t, hf.Allowed("X-Api-Key"))
		assert.False(t, hf.Allowed("X-Csrf-Token"))

		// Non-sensitive headers should pass.
		assert.True(t, hf.Allowed("Content-Type"))
		assert.True(t, hf.Allowed("Accept"))
		assert.True(t, hf.Allowed("X-Request-Id"))
		assert.True(t, hf.Allowed("X-Tenant-Id"))
	})

	t.Run("explicit deny list overrides defaults", func(t *testing.T) {
		hf := NewHeaderFilter(HeaderFilterConfig{
			DenyList: []string{"X-Custom-Secret", "X-Internal"},
		})

		// Custom deny list should be enforced.
		assert.False(t, hf.Allowed("X-Custom-Secret"))
		assert.False(t, hf.Allowed("X-Internal"))
		assert.False(t, hf.Allowed("x-custom-secret")) // case insensitive

		// Default sensitive headers NOT in the explicit list should now pass.
		assert.True(t, hf.Allowed("Authorization"))
		assert.True(t, hf.Allowed("Cookie"))
	})

	t.Run("allow list takes precedence over deny list", func(t *testing.T) {
		hf := NewHeaderFilter(HeaderFilterConfig{
			AllowList: []string{"Content-Type", "X-Tenant-Id", "Accept"},
			DenyList:  []string{"Content-Type"}, // Should be ignored.
		})

		assert.True(t, hf.Allowed("Content-Type"))
		assert.True(t, hf.Allowed("X-Tenant-Id"))
		assert.True(t, hf.Allowed("Accept"))

		// Everything else is denied.
		assert.False(t, hf.Allowed("Authorization"))
		assert.False(t, hf.Allowed("Cookie"))
		assert.False(t, hf.Allowed("X-Request-Id"))
	})

	t.Run("case insensitive matching", func(t *testing.T) {
		hf := NewHeaderFilter(HeaderFilterConfig{
			AllowList: []string{"X-Tenant-Id"},
		})

		assert.True(t, hf.Allowed("X-Tenant-Id"))
		assert.True(t, hf.Allowed("x-tenant-id"))
		assert.True(t, hf.Allowed("X-TENANT-ID"))
	})
}

func TestHeaderFilterFilterHeaders(t *testing.T) {
	t.Run("filters map with default deny list", func(t *testing.T) {
		hf := NewHeaderFilter(HeaderFilterConfig{})

		input := map[string]string{
			"Authorization": "Bearer token",
			"Content-Type":  "application/json",
			"Cookie":        "session=abc",
			"X-Request-Id":  "req-123",
			"X-Api-Key":     "secret-key",
		}

		filtered := hf.FilterHeaders(input)

		assert.Equal(t, "application/json", filtered["Content-Type"])
		assert.Equal(t, "req-123", filtered["X-Request-Id"])
		assert.NotContains(t, filtered, "Authorization")
		assert.NotContains(t, filtered, "Cookie")
		assert.NotContains(t, filtered, "X-Api-Key")
	})

	t.Run("does not modify original map", func(t *testing.T) {
		hf := NewHeaderFilter(HeaderFilterConfig{})

		input := map[string]string{
			"Authorization": "Bearer token",
			"Content-Type":  "application/json",
		}

		_ = hf.FilterHeaders(input)

		// Original should be unchanged.
		assert.Equal(t, "Bearer token", input["Authorization"])
		assert.Len(t, input, 2)
	})

	t.Run("allow list only passes specified headers", func(t *testing.T) {
		hf := NewHeaderFilter(HeaderFilterConfig{
			AllowList: []string{"X-Tenant-Id"},
		})

		input := map[string]string{
			"X-Tenant-Id":   "acme",
			"Authorization": "Bearer token",
			"Content-Type":  "application/json",
		}

		filtered := hf.FilterHeaders(input)
		assert.Len(t, filtered, 1)
		assert.Equal(t, "acme", filtered["X-Tenant-Id"])
	})
}

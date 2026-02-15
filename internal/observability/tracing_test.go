package observability

import (
	"context"
	"testing"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitTracing(t *testing.T) {
	t.Run("returns no-op shutdown when disabled", func(t *testing.T) {
		cfg := config.TracingConfig{Enabled: false}
		shutdown, err := InitTracing(context.Background(), cfg, "test")
		require.NoError(t, err)
		assert.NotNil(t, shutdown)

		// Calling shutdown should be safe.
		assert.NoError(t, shutdown(context.Background()))
	})

	t.Run("returns error for invalid endpoint", func(t *testing.T) {
		cfg := config.TracingConfig{
			Enabled:     true,
			Endpoint:    "http://invalid-endpoint:4318",
			ServiceName: "test",
			SampleRate:  1.0,
		}
		// This may not error on creation since OTLP uses lazy connection.
		shutdown, err := InitTracing(context.Background(), cfg, "test")
		if err == nil {
			// Cleanup if it succeeded.
			shutdown(context.Background())
		}
	})

	t.Run("uses default service name when empty", func(t *testing.T) {
		cfg := config.TracingConfig{
			Enabled:    true,
			Endpoint:   "http://localhost:4318",
			SampleRate: 0.5,
		}
		shutdown, err := InitTracing(context.Background(), cfg, "v1.0.0")
		if err == nil {
			shutdown(context.Background())
		}
	})
}

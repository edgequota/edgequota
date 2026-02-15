package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
)

func TestNewMetrics(t *testing.T) {
	t.Run("creates metrics with custom registry", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		m := NewMetrics(reg)
		assert.NotNil(t, m)
		assert.NotNil(t, m.promAllowed)
		assert.NotNil(t, m.promLimited)
		assert.NotNil(t, m.PromRequestDuration)
	})
}

func TestMetricsIncAllowed(t *testing.T) {
	t.Run("increments allowed counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry())
		m.IncAllowed()
		m.IncAllowed()
		m.IncAllowed()

		snap := m.Snapshot()
		assert.Equal(t, int64(3), snap.Allowed)
	})
}

func TestMetricsIncLimited(t *testing.T) {
	t.Run("increments limited counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry())
		m.IncLimited()
		m.IncLimited()

		snap := m.Snapshot()
		assert.Equal(t, int64(2), snap.Limited)
	})
}

func TestMetricsIncRedisErrors(t *testing.T) {
	t.Run("increments redis error counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry())
		m.IncRedisErrors()

		snap := m.Snapshot()
		assert.Equal(t, int64(1), snap.RedisErrors)
	})
}

func TestMetricsIncFallbackUsed(t *testing.T) {
	t.Run("increments fallback counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry())
		m.IncFallbackUsed()
		m.IncFallbackUsed()

		snap := m.Snapshot()
		assert.Equal(t, int64(2), snap.FallbackUsed)
	})
}

func TestMetricsIncReadOnlyRetries(t *testing.T) {
	t.Run("increments readonly retry counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry())
		m.IncReadOnlyRetries()

		snap := m.Snapshot()
		assert.Equal(t, int64(1), snap.ReadOnlyRetries)
	})
}

func TestMetricsIncKeyExtractErrors(t *testing.T) {
	t.Run("increments key extract error counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry())
		m.IncKeyExtractErrors()

		snap := m.Snapshot()
		assert.Equal(t, int64(1), snap.KeyExtractErrors)
	})
}

func TestMetricsIncAuthErrors(t *testing.T) {
	t.Run("increments auth error counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry())
		m.IncAuthErrors()

		snap := m.Snapshot()
		assert.Equal(t, int64(1), snap.AuthErrors)
	})
}

func TestMetricsIncAuthDenied(t *testing.T) {
	t.Run("increments auth denied counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry())
		m.IncAuthDenied()

		snap := m.Snapshot()
		assert.Equal(t, int64(1), snap.AuthDenied)
	})
}

func TestMetricsSnapshot(t *testing.T) {
	t.Run("returns point-in-time snapshot of all counters", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry())

		m.IncAllowed()
		m.IncAllowed()
		m.IncLimited()
		m.IncRedisErrors()
		m.IncFallbackUsed()
		m.IncReadOnlyRetries()
		m.IncKeyExtractErrors()
		m.IncAuthErrors()
		m.IncAuthDenied()

		snap := m.Snapshot()
		assert.Equal(t, int64(2), snap.Allowed)
		assert.Equal(t, int64(1), snap.Limited)
		assert.Equal(t, int64(1), snap.RedisErrors)
		assert.Equal(t, int64(1), snap.FallbackUsed)
		assert.Equal(t, int64(1), snap.ReadOnlyRetries)
		assert.Equal(t, int64(1), snap.KeyExtractErrors)
		assert.Equal(t, int64(1), snap.AuthErrors)
		assert.Equal(t, int64(1), snap.AuthDenied)
	})
}

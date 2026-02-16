package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestNewMetrics(t *testing.T) {
	t.Run("creates metrics with custom registry", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		m := NewMetrics(reg, 0)
		assert.NotNil(t, m)
		assert.NotNil(t, m.promAllowed)
		assert.NotNil(t, m.promLimited)
		assert.NotNil(t, m.PromRequestDuration)
	})
}

func TestMetricsIncAllowed(t *testing.T) {
	t.Run("increments allowed counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry(), 0)
		m.IncAllowed()
		m.IncAllowed()
		m.IncAllowed()

		snap := m.Snapshot()
		assert.Equal(t, int64(3), snap.Allowed)
	})
}

func TestMetricsIncLimited(t *testing.T) {
	t.Run("increments limited counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry(), 0)
		m.IncLimited()
		m.IncLimited()

		snap := m.Snapshot()
		assert.Equal(t, int64(2), snap.Limited)
	})
}

func TestMetricsIncRedisErrors(t *testing.T) {
	t.Run("increments redis error counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry(), 0)
		m.IncRedisErrors()

		snap := m.Snapshot()
		assert.Equal(t, int64(1), snap.RedisErrors)
	})
}

func TestMetricsIncFallbackUsed(t *testing.T) {
	t.Run("increments fallback counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry(), 0)
		m.IncFallbackUsed()
		m.IncFallbackUsed()

		snap := m.Snapshot()
		assert.Equal(t, int64(2), snap.FallbackUsed)
	})
}

func TestMetricsIncReadOnlyRetries(t *testing.T) {
	t.Run("increments readonly retry counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry(), 0)
		m.IncReadOnlyRetries()

		snap := m.Snapshot()
		assert.Equal(t, int64(1), snap.ReadOnlyRetries)
	})
}

func TestMetricsIncKeyExtractErrors(t *testing.T) {
	t.Run("increments key extract error counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry(), 0)
		m.IncKeyExtractErrors()

		snap := m.Snapshot()
		assert.Equal(t, int64(1), snap.KeyExtractErrors)
	})
}

func TestMetricsIncAuthErrors(t *testing.T) {
	t.Run("increments auth error counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry(), 0)
		m.IncAuthErrors()

		snap := m.Snapshot()
		assert.Equal(t, int64(1), snap.AuthErrors)
	})
}

func TestMetricsIncAuthDenied(t *testing.T) {
	t.Run("increments auth denied counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry(), 0)
		m.IncAuthDenied()

		snap := m.Snapshot()
		assert.Equal(t, int64(1), snap.AuthDenied)
	})
}

func TestMetricsIncTenantAllowed(t *testing.T) {
	t.Run("increments per-tenant allowed counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry(), 0)
		m.IncTenantAllowed("acme")
		m.IncTenantAllowed("acme")
		m.IncTenantAllowed("globex")

		// Prometheus counters cannot be read via Snapshot — just verify no panic.
		// The important thing is that the labeled counter is incremented.
	})
}

func TestTenantLabelCardinalityCap(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry(), 5)

	// 1. tenant-1 through tenant-5 should all pass (within cap)
	m.IncTenantAllowed("tenant-1")
	m.IncTenantAllowed("tenant-2")
	m.IncTenantAllowed("tenant-3")
	m.IncTenantAllowed("tenant-4")
	m.IncTenantAllowed("tenant-5")

	// 2. tenant-6 should be bucketed under __overflow__
	m.IncTenantAllowed("tenant-6")
	m.IncTenantAllowed("tenant-6") // call twice to verify overflow counter

	// 3. tenant-1 should still work (existing tenant)
	m.IncTenantAllowed("tenant-1")

	// 4. Verify overflow counter has been incremented
	assert.GreaterOrEqual(t, testutil.ToFloat64(m.promTenantOverflow), float64(1),
		"tenant_label_overflow_total should be incremented when cap is reached")

	// 5. Verify tenant-6 requests were bucketed under __overflow__
	assert.GreaterOrEqual(t, testutil.ToFloat64(m.promTenantAllowed.WithLabelValues(tenantOverflowLabel)), float64(2),
		"tenant-6 requests should be under __overflow__ label")
}

func TestMetricsIncTenantLimited(t *testing.T) {
	t.Run("increments per-tenant limited counter", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry(), 0)
		m.IncTenantLimited("acme")
		m.IncTenantLimited("globex")
	})
}

func TestMetricsObserveRemaining(t *testing.T) {
	t.Run("records remaining tokens histogram observation", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry(), 0)
		m.ObserveRemaining(42)
		m.ObserveRemaining(0)
		m.ObserveRemaining(100)
	})
}

func TestMetricsSnapshot(t *testing.T) {
	t.Run("returns point-in-time snapshot of all counters", func(t *testing.T) {
		m := NewMetrics(prometheus.NewRegistry(), 0)

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

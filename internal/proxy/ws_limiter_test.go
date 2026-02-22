package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewWSLimiter(t *testing.T) {
	l := NewWSLimiter(5)
	assert.NotNil(t, l)
	assert.Equal(t, int64(5), l.maxPerKey)
}

func TestWSLimiter_Acquire(t *testing.T) {
	t.Run("allows up to max connections", func(t *testing.T) {
		l := NewWSLimiter(2)

		assert.True(t, l.Acquire("client-a"))
		assert.True(t, l.Acquire("client-a"))
		assert.False(t, l.Acquire("client-a"), "should reject when at max")

		// Different key is independent.
		assert.True(t, l.Acquire("client-b"))
	})

	t.Run("nil limiter always allows", func(t *testing.T) {
		var l *WSLimiter
		assert.True(t, l.Acquire("anything"))
	})

	t.Run("zero max always allows", func(t *testing.T) {
		l := NewWSLimiter(0)
		assert.True(t, l.Acquire("anything"))
		assert.True(t, l.Acquire("anything"))
	})
}

func TestWSLimiter_Release(t *testing.T) {
	t.Run("release allows new acquire", func(t *testing.T) {
		l := NewWSLimiter(1)

		assert.True(t, l.Acquire("k"))
		assert.False(t, l.Acquire("k"))

		l.Release("k")
		assert.True(t, l.Acquire("k"), "should allow after release")
	})

	t.Run("release cleans up map entry at zero", func(t *testing.T) {
		l := NewWSLimiter(1)
		l.Acquire("k")
		l.Release("k")

		// After release, the slot should be available again.
		assert.True(t, l.Acquire("k"), "key slot should be reusable after full release")
		l.Release("k")
	})

	t.Run("nil limiter release is no-op", func(t *testing.T) {
		var l *WSLimiter
		l.Release("anything") // should not panic
	})

	t.Run("zero max release is no-op", func(t *testing.T) {
		l := NewWSLimiter(0)
		l.Release("anything") // should not panic
	})
}

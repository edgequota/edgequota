package ratelimit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewInMemoryLimiter(t *testing.T) {
	t.Run("creates limiter that allows requests", func(t *testing.T) {
		l := NewInMemoryLimiter(10.0, 5, 10*time.Second)
		defer l.Close()
		assert.NotNil(t, l)
		assert.True(t, l.Allow("test-key"))
	})

	t.Run("creates disabled limiter for zero rate", func(t *testing.T) {
		l := NewInMemoryLimiter(0, 5, 10*time.Second)
		defer l.Close()
		assert.True(t, l.disabled)
	})
}

func TestInMemoryLimiterAllow(t *testing.T) {
	t.Run("allows requests within burst", func(t *testing.T) {
		l := NewInMemoryLimiter(10.0, 5, 10*time.Second)
		defer l.Close()

		for i := 0; i < 5; i++ {
			assert.True(t, l.Allow("key1"), "request %d should be allowed", i)
		}
	})

	t.Run("denies requests after burst exhaustion", func(t *testing.T) {
		l := NewInMemoryLimiter(2.0, 3, 10*time.Second)
		defer l.Close()

		// First call inserts async; wait for ristretto to admit the entry.
		assert.True(t, l.Allow("key2"))
		l.cache.Wait()

		assert.True(t, l.Allow("key2"))
		assert.True(t, l.Allow("key2"))
		assert.False(t, l.Allow("key2"))
	})

	t.Run("disabled limiter always allows", func(t *testing.T) {
		l := NewInMemoryLimiter(0, 5, 10*time.Second)
		defer l.Close()

		for i := 0; i < 100; i++ {
			assert.True(t, l.Allow("key"))
		}
	})

	t.Run("tokens refill over time", func(t *testing.T) {
		l := NewInMemoryLimiter(100.0, 2, 10*time.Second)
		defer l.Close()

		// First call inserts async; wait for ristretto to admit the entry.
		assert.True(t, l.Allow("key3"))
		l.cache.Wait()

		// Exhaust remaining burst.
		assert.True(t, l.Allow("key3"))
		assert.False(t, l.Allow("key3"))

		// Wait for refill (100 tokens/sec → 1 token in 10ms).
		time.Sleep(50 * time.Millisecond)

		assert.True(t, l.Allow("key3"))
	})

	t.Run("different keys are independent", func(t *testing.T) {
		l := NewInMemoryLimiter(2.0, 1, 10*time.Second)
		defer l.Close()

		// First call inserts async; wait for ristretto to admit the entry.
		assert.True(t, l.Allow("key-a"))
		l.cache.Wait()

		assert.False(t, l.Allow("key-a"))

		// key-b is independent.
		assert.True(t, l.Allow("key-b"))
	})
}

func TestInMemoryLimiterClose(t *testing.T) {
	t.Run("close is safe to call multiple times", func(t *testing.T) {
		l := NewInMemoryLimiter(10.0, 5, time.Second)
		l.Close()
		l.Close() // should not panic
	})
}

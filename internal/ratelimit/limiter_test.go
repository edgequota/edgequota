package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRedisClient(t *testing.T) (redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := redis.NewClient(config.RedisConfig{
		Endpoints: []string{mr.Addr()},
		Mode:      config.RedisModeSingle,
	})
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	return client, mr
}

func TestNewLimiter(t *testing.T) {
	t.Run("creates limiter with correct parameters", func(t *testing.T) {
		client, _ := newTestRedisClient(t)
		l := NewLimiter(client, 10.0, 5, 10, "rl:test:")

		assert.NotNil(t, l)
		assert.Equal(t, 10.0/1e6, l.rate)
		assert.Equal(t, int64(5), l.burst)
		assert.Equal(t, 10, l.ttl)
		assert.Equal(t, "rl:test:", l.keyPrefix)
		assert.NotEmpty(t, l.src)
		assert.NotEmpty(t, l.hash)
	})
}

func TestLimiterAllow(t *testing.T) {
	t.Run("allows requests within burst", func(t *testing.T) {
		client, _ := newTestRedisClient(t)
		l := NewLimiter(client, 10.0, 5, 10, "rl:test:")

		for i := 0; i < 5; i++ {
			result, err := l.Allow(context.Background(), "key1")
			require.NoError(t, err)
			assert.True(t, result.Allowed, "request %d should be allowed", i)
		}
	})

	t.Run("denies requests after burst exhaustion", func(t *testing.T) {
		client, _ := newTestRedisClient(t)
		l := NewLimiter(client, 2.0, 3, 10, "rl:test:")

		// Exhaust burst.
		for i := 0; i < 3; i++ {
			result, err := l.Allow(context.Background(), "key2")
			require.NoError(t, err)
			assert.True(t, result.Allowed)
		}

		// Next should be denied.
		result, err := l.Allow(context.Background(), "key2")
		require.NoError(t, err)
		assert.False(t, result.Allowed)
		assert.Greater(t, result.RetryAfter, time.Duration(0))
	})

	t.Run("works after Redis data is flushed", func(t *testing.T) {
		client, mr := newTestRedisClient(t)
		l := NewLimiter(client, 10.0, 5, 10, "rl:test:")

		// First call succeeds.
		result, err := l.Allow(context.Background(), "key3")
		require.NoError(t, err)
		assert.True(t, result.Allowed)

		// Flush all Redis data.
		mr.FlushAll()

		// Should still work — EVAL re-executes the script.
		result, err = l.Allow(context.Background(), "key3")
		require.NoError(t, err)
		assert.True(t, result.Allowed)
	})

	t.Run("different keys have independent buckets", func(t *testing.T) {
		client, _ := newTestRedisClient(t)
		l := NewLimiter(client, 2.0, 2, 10, "rl:test:")

		// Exhaust key-a.
		for i := 0; i < 2; i++ {
			result, err := l.Allow(context.Background(), "key-a")
			require.NoError(t, err)
			assert.True(t, result.Allowed)
		}
		result, err := l.Allow(context.Background(), "key-a")
		require.NoError(t, err)
		assert.False(t, result.Allowed)

		// key-b should still have tokens.
		result, err = l.Allow(context.Background(), "key-b")
		require.NoError(t, err)
		assert.True(t, result.Allowed)
	})
}

func TestLimiterClient(t *testing.T) {
	t.Run("returns the underlying redis client", func(t *testing.T) {
		client, _ := newTestRedisClient(t)
		l := NewLimiter(client, 10.0, 5, 10, "rl:test:")
		assert.Equal(t, client, l.Client())
	})
}

func TestParseScriptResult(t *testing.T) {
	t.Run("parses allowed result", func(t *testing.T) {
		mock := &mockSliceCmd{result: []any{int64(1), int64(0), int64(4), int64(5), int64(1000000)}}
		result, err := parseScriptResult(mock)
		require.NoError(t, err)
		assert.True(t, result.Allowed)
		assert.Equal(t, time.Duration(0), result.RetryAfter)
		assert.Equal(t, int64(4), result.Remaining)
		assert.Equal(t, int64(5), result.Limit)
		assert.Equal(t, time.Second, result.ResetAfter)
	})

	t.Run("parses denied result", func(t *testing.T) {
		mock := &mockSliceCmd{result: []any{int64(0), int64(500000), int64(0), int64(5), int64(5000000)}} // 500ms retry, 5s reset
		result, err := parseScriptResult(mock)
		require.NoError(t, err)
		assert.False(t, result.Allowed)
		assert.Equal(t, 500*time.Millisecond, result.RetryAfter)
		assert.Equal(t, int64(0), result.Remaining)
		assert.Equal(t, int64(5), result.Limit)
		assert.Equal(t, 5*time.Second, result.ResetAfter)
	})

	t.Run("returns error for wrong element count", func(t *testing.T) {
		mock := &mockSliceCmd{result: []any{int64(1)}}
		_, err := parseScriptResult(mock)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "want 5")
	})

	t.Run("returns error when Slice() fails", func(t *testing.T) {
		mock := &mockSliceCmd{err: assert.AnError}
		_, err := parseScriptResult(mock)
		assert.Error(t, err)
	})
}

func TestToInt64(t *testing.T) {
	t.Run("converts int64", func(t *testing.T) {
		v, err := toInt64(int64(42))
		require.NoError(t, err)
		assert.Equal(t, int64(42), v)
	})

	t.Run("converts int", func(t *testing.T) {
		v, err := toInt64(int(42))
		require.NoError(t, err)
		assert.Equal(t, int64(42), v)
	})

	t.Run("converts float64", func(t *testing.T) {
		v, err := toInt64(float64(42.9))
		require.NoError(t, err)
		assert.Equal(t, int64(42), v)
	})

	t.Run("converts string", func(t *testing.T) {
		v, err := toInt64("42")
		require.NoError(t, err)
		assert.Equal(t, int64(42), v)
	})

	t.Run("returns error for invalid string", func(t *testing.T) {
		_, err := toInt64("not-a-number")
		assert.Error(t, err)
	})
}

// mockSliceCmd implements the interface{ Slice() ([]any, error) } for testing.
type mockSliceCmd struct {
	result []any
	err    error
}

func (m *mockSliceCmd) Slice() ([]any, error) {
	return m.result, m.err
}

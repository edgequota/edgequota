// Package ratelimit implements distributed token-bucket rate limiting using
// Redis with a Lua script for atomicity, plus an in-memory fallback for when
// Redis is unavailable.
package ratelimit

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/edgequota/edgequota/internal/redis"
)

// tokenBucketScript is the Lua script for atomic token-bucket rate limiting.
//
// Uses HMGET for deterministic field ordering. Returns {allowed (0|1), retry_after_micros}.
//
// Standard token bucket semantics:
//   - Replenish: tokens = min(burst, tokens + rate * elapsed)
//   - If tokens >= 1: consume 1, allow, retry_after = 0
//   - Else: deny, retry_after = ceil((1 - tokens) / rate) in microseconds
//
// Keys: KEYS[1] = rate-limit key.
// Args: ARGV[1] = rate (tokens/μs), ARGV[2] = burst, ARGV[3] = TTL (s), ARGV[4] = now (μs).
//
//nolint:gosec // G101 false positive — this is a Lua script, not credentials.
const tokenBucketScript = `
local key   = KEYS[1]
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local ttl   = tonumber(ARGV[3])
local now   = tonumber(ARGV[4])

if rate <= 0 then
  return {1, 0}
end

local vals = redis.call('hmget', key, 'last', 'tokens')
local last   = tonumber(vals[1]) or 0
local tokens = tonumber(vals[2]) or burst

if now < last then
  last = now
end

local elapsed = now - last
tokens = math.min(burst, tokens + rate * elapsed)

if tokens >= 1 then
  tokens = tokens - 1
  redis.call('hset', key, 'last', now, 'tokens', tokens)
  redis.call('expire', key, ttl)
  return {1, 0}
end

redis.call('hset', key, 'last', now, 'tokens', tokens)
redis.call('expire', key, ttl)
local retry = math.ceil((1 - tokens) / rate)
return {0, retry}
`

// Result holds the parsed result of a rate-limit check.
type Result struct {
	Allowed    bool
	RetryAfter time.Duration // meaningful only when Allowed == false
}

// Limiter performs token-bucket rate limiting against Redis.
type Limiter struct {
	client    redis.Client
	script    string
	rate      float64 // tokens per microsecond
	burst     int64
	ttl       int // seconds
	keyPrefix string
}

// NewLimiter creates a Redis-backed rate limiter.
func NewLimiter(client redis.Client, ratePerSecond float64, burst int64, ttl int, prefix string) *Limiter {
	return &Limiter{
		client:    client,
		script:    tokenBucketScript,
		rate:      ratePerSecond / 1e6, // convert to per-microsecond
		burst:     burst,
		ttl:       ttl,
		keyPrefix: prefix,
	}
}

// Allow checks whether the request identified by key should be allowed.
// Uses EVAL to execute the Lua script atomically on Redis. Redis caches the
// compiled script internally, so subsequent calls avoid re-parsing overhead.
func (l *Limiter) Allow(ctx context.Context, key string) (*Result, error) {
	fullKey := l.keyPrefix + key
	now := time.Now().UnixMicro()

	cmd := l.client.Eval(ctx, l.script, []string{fullKey}, l.rate, l.burst, l.ttl, now)
	if cmd.Err() != nil {
		return nil, cmd.Err()
	}

	return parseScriptResult(cmd)
}

// Client returns the underlying Redis client (used for lifecycle management).
func (l *Limiter) Client() redis.Client {
	return l.client
}

// parseScriptResult parses the Lua {allowed, retry_after_micros} response.
func parseScriptResult(cmd interface{ Slice() ([]any, error) }) (*Result, error) {
	arr, err := cmd.Slice()
	if err != nil {
		return nil, fmt.Errorf("reading script result: %w", err)
	}

	if len(arr) != 2 {
		return nil, fmt.Errorf("script returned %d elements, want 2", len(arr))
	}

	allowed, err := toInt64(arr[0])
	if err != nil {
		return nil, fmt.Errorf("parsing allowed: %w", err)
	}

	retryMicros, err := toInt64(arr[1])
	if err != nil {
		return nil, fmt.Errorf("parsing retry_after: %w", err)
	}

	if allowed == 1 {
		return &Result{Allowed: true}, nil
	}

	return &Result{
		Allowed:    false,
		RetryAfter: time.Duration(retryMicros) * time.Microsecond,
	}, nil
}

// toInt64 converts a Redis response value to int64.
func toInt64(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case float64:
		return int64(x), nil
	case string:
		return strconv.ParseInt(x, 10, 64)
	default:
		return strconv.ParseInt(fmt.Sprint(v), 10, 64)
	}
}

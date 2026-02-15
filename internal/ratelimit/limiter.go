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
	goredis "github.com/redis/go-redis/v9"
)

// tokenBucketSrc is the Lua source for atomic token-bucket rate limiting.
//
// Uses HMGET for deterministic field ordering.
// Returns {allowed (0|1), retry_after_micros, remaining_tokens, limit, reset_after_micros}.
//
// Standard token bucket semantics:
//   - Replenish: tokens = min(burst, tokens + rate * elapsed)
//   - If tokens >= 1: consume 1, allow, retry_after = 0
//   - Else: deny, retry_after = ceil((1 - tokens) / rate) in microseconds
//
// Keys: KEYS[1] = rate-limit key.
// Args: ARGV[1] = rate (tokens/μs), ARGV[2] = burst, ARGV[3] = TTL (s), ARGV[4] = now (μs).
const tokenBucketSrc = `
local key   = KEYS[1]
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local ttl   = tonumber(ARGV[3])
local now   = tonumber(ARGV[4])

if rate <= 0 then
  return {1, 0, burst, burst, 0}
end

local vals = redis.call('hmget', key, 'last', 'tokens')
local last   = tonumber(vals[1]) or 0
local tokens = tonumber(vals[2]) or burst

if now < last then
  last = now
end

local elapsed = now - last
tokens = math.min(burst, tokens + rate * elapsed)

local reset_after = 0
if tokens < burst then
  reset_after = math.ceil((burst - tokens) / rate)
end

if tokens >= 1 then
  tokens = tokens - 1
  redis.call('hset', key, 'last', now, 'tokens', tokens)
  redis.call('expire', key, ttl)
  local remaining = math.floor(tokens)
  return {1, 0, remaining, burst, reset_after}
end

redis.call('hset', key, 'last', now, 'tokens', tokens)
redis.call('expire', key, ttl)
local retry = math.ceil((1 - tokens) / rate)
return {0, retry, 0, burst, reset_after}
`

// tokenBucketScript uses go-redis to compute the SHA1 hash that Redis expects
// for EVALSHA, avoiding a direct crypto/sha1 import in this package.
var tokenBucketScript = goredis.NewScript(tokenBucketSrc)

// Result holds the parsed result of a rate-limit check.
type Result struct {
	Allowed    bool
	RetryAfter time.Duration // meaningful only when Allowed == false
	Remaining  int64         // remaining tokens in the bucket
	Limit      int64         // bucket capacity (burst)
	ResetAfter time.Duration // time until bucket is fully replenished
}

// Limiter performs token-bucket rate limiting against Redis.
type Limiter struct {
	client    redis.Client
	src       string  // Lua source text (for EVAL fallback)
	hash      string  // SHA1 hex digest (for EVALSHA)
	rate      float64 // tokens per microsecond
	burst     int64
	ttl       int // seconds
	keyPrefix string
}

// NewLimiter creates a Redis-backed rate limiter.
func NewLimiter(client redis.Client, ratePerSecond float64, burst int64, ttl int, prefix string) *Limiter {
	return &Limiter{
		client:    client,
		src:       tokenBucketSrc,
		hash:      tokenBucketScript.Hash(),
		rate:      ratePerSecond / 1e6, // convert to per-microsecond
		burst:     burst,
		ttl:       ttl,
		keyPrefix: prefix,
	}
}

// evalScript executes the Lua script via EVALSHA, falling back to EVAL on
// NOSCRIPT. This avoids sending ~600 bytes of Lua on every request.
func (l *Limiter) evalScript(ctx context.Context, keys []string, args ...any) (interface{ Slice() ([]any, error) }, error) {
	cmd := l.client.EvalSha(ctx, l.hash, keys, args...)
	if cmd.Err() != nil && redis.IsNoScriptErr(cmd.Err()) {
		cmd = l.client.Eval(ctx, l.src, keys, args...)
	}
	if cmd.Err() != nil {
		return nil, cmd.Err()
	}
	return cmd, nil
}

// Allow checks whether the request identified by key should be allowed.
// Uses EVALSHA to execute the Lua script atomically on Redis, falling back
// to EVAL on NOSCRIPT to load the script.
func (l *Limiter) Allow(ctx context.Context, key string) (*Result, error) {
	fullKey := l.keyPrefix + key
	now := time.Now().UnixMicro()

	cmd, err := l.evalScript(ctx, []string{fullKey}, l.rate, l.burst, l.ttl, now)
	if err != nil {
		return nil, err
	}

	return parseScriptResult(cmd)
}

// AllowWithOverrides checks rate limits using dynamic parameters from an
// external rate limit service. This enables per-tenant/per-key limits that
// differ from the static configuration. When ttlOverride > 0 it replaces
// the limiter's default TTL for this call.
func (l *Limiter) AllowWithOverrides(ctx context.Context, key string, ratePerSecond float64, burst int64, ttlOverride int) (*Result, error) {
	fullKey := l.keyPrefix + key
	now := time.Now().UnixMicro()
	rate := ratePerSecond / 1e6 // convert to per-microsecond

	ttl := l.ttl
	if ttlOverride > 0 {
		ttl = ttlOverride
	}

	cmd, err := l.evalScript(ctx, []string{fullKey}, rate, burst, ttl, now)
	if err != nil {
		return nil, err
	}

	return parseScriptResult(cmd)
}

// Client returns the underlying Redis client (used for lifecycle management).
func (l *Limiter) Client() redis.Client {
	return l.client
}

// parseScriptResult parses the Lua {allowed, retry_after_micros, remaining, limit, reset_after_micros} response.
func parseScriptResult(cmd interface{ Slice() ([]any, error) }) (*Result, error) {
	arr, err := cmd.Slice()
	if err != nil {
		return nil, fmt.Errorf("reading script result: %w", err)
	}

	if len(arr) != 5 {
		return nil, fmt.Errorf("script returned %d elements, want 5", len(arr))
	}

	allowed, err := toInt64(arr[0])
	if err != nil {
		return nil, fmt.Errorf("parsing allowed: %w", err)
	}

	retryMicros, err := toInt64(arr[1])
	if err != nil {
		return nil, fmt.Errorf("parsing retry_after: %w", err)
	}

	remaining, err := toInt64(arr[2])
	if err != nil {
		return nil, fmt.Errorf("parsing remaining: %w", err)
	}

	limit, err := toInt64(arr[3])
	if err != nil {
		return nil, fmt.Errorf("parsing limit: %w", err)
	}

	resetMicros, err := toInt64(arr[4])
	if err != nil {
		return nil, fmt.Errorf("parsing reset_after: %w", err)
	}

	return &Result{
		Allowed:    allowed == 1,
		RetryAfter: time.Duration(retryMicros) * time.Microsecond,
		Remaining:  remaining,
		Limit:      limit,
		ResetAfter: time.Duration(resetMicros) * time.Microsecond,
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

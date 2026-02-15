# Rate Limiting

This document describes EdgeQuota's rate limiting algorithm, its distributed correctness guarantees, Redis interaction model, and key expiration strategy.

---

## Algorithm: Token Bucket

EdgeQuota uses a **token bucket** algorithm for rate limiting. The bucket has two parameters:

- **`average`** — tokens added per period (the sustained rate).
- **`burst`** — maximum tokens the bucket can hold (the instantaneous burst capacity).

Each request consumes one token. If the bucket is empty, the request is rejected with `429 Too Many Requests` and a `Retry-After` header indicating when the next token will be available.

### Why Token Bucket

| Algorithm | Pros | Cons |
|-----------|------|------|
| **Fixed Window** | Simple; one counter per window | Allows 2x burst at window boundaries |
| **Sliding Window Log** | Exact; no boundary effects | Requires storing every request timestamp; memory-intensive |
| **Sliding Window Counter** | Good compromise | Approximate near window edges |
| **Token Bucket** | Natural burst handling; constant memory per key; single counter | Slightly more complex Lua logic |
| **Leaky Bucket** | Smooth output rate | No burst tolerance; poor UX for interactive clients |

Token bucket was chosen because:

1. **Natural burst tolerance.** Users can make `burst` requests instantly, then sustain `average` requests per period. This matches real-world API usage patterns.
2. **Constant memory.** Each key requires exactly two fields (`last`, `tokens`), regardless of traffic volume.
3. **Single atomic operation.** The entire check-and-update fits in one Redis Lua script.
4. **Graceful degradation.** When a client exceeds the rate, the `Retry-After` value is exact — not rounded to the next window boundary.

---

## Redis Lua Script

The entire token-bucket logic executes as a single `EVAL` on Redis, guaranteeing atomicity:

```lua
local key   = KEYS[1]
local rate  = tonumber(ARGV[1])   -- tokens per microsecond
local burst = tonumber(ARGV[2])   -- max tokens
local ttl   = tonumber(ARGV[3])   -- key expiry in seconds
local now   = tonumber(ARGV[4])   -- current time in microseconds

-- Rate <= 0 means rate limiting is disabled; allow everything.
if rate <= 0 then
  return {1, 0}
end

-- Read current bucket state.
local vals = redis.call('hmget', key, 'last', 'tokens')
local last   = tonumber(vals[1]) or 0
local tokens = tonumber(vals[2]) or burst  -- new key starts full

-- Guard against clock skew (backwards time).
if now < last then
  last = now
end

-- Replenish tokens based on elapsed time.
local elapsed = now - last
tokens = math.min(burst, tokens + rate * elapsed)

-- Try to consume one token.
if tokens >= 1 then
  tokens = tokens - 1
  redis.call('hset', key, 'last', now, 'tokens', tokens)
  redis.call('expire', key, ttl)
  return {1, 0}            -- {allowed, retry_after_micros}
end

-- Bucket empty: compute retry delay.
redis.call('hset', key, 'last', now, 'tokens', tokens)
redis.call('expire', key, ttl)
local retry = math.ceil((1 - tokens) / rate)
return {0, retry}           -- {denied, retry_after_micros}
```

### Script Arguments

| Argument | Type | Description |
|----------|------|-------------|
| `KEYS[1]` | string | Rate limit key (e.g., `rl:edgequota:10.0.0.1`) |
| `ARGV[1]` | float | Token replenishment rate (tokens per microsecond) |
| `ARGV[2]` | integer | Maximum bucket capacity (burst) |
| `ARGV[3]` | integer | Key TTL in seconds |
| `ARGV[4]` | integer | Current time in microseconds (`time.Now().UnixMicro()`) |

### Return Value

A two-element array: `{allowed, retry_after_micros}`.

- `allowed = 1`: request permitted; `retry_after` is `0`.
- `allowed = 0`: request denied; `retry_after` is the number of microseconds until one token is available.

### Why Microsecond Precision

Microsecond timestamps provide sufficient precision for high-throughput rate limiting (millions of requests per second) while fitting in a 64-bit integer. Sub-microsecond precision is unnecessary for network-bound operations.

---

## Distributed Correctness

### Atomicity

The Lua script executes as a single Redis command (`EVAL`). Redis is single-threaded for command execution, so the script runs atomically — no other command can interleave between the `HMGET`, arithmetic, `HSET`, and `EXPIRE`.

This means:

- **No race conditions.** Two concurrent requests from different EdgeQuota pods cannot both read the same token count and both succeed.
- **No distributed locks needed.** The Lua script is the lock.
- **No optimistic concurrency control.** There is no `WATCH`/`MULTI`/`EXEC` retry loop.

### Consistency Model

Rate limiting is **strongly consistent** within a single Redis master. All EdgeQuota instances talk to the same master, so counter updates are immediately visible.

In Redis Cluster, each key is assigned to a single shard (based on the hash slot of the key). Rate limit operations for the same key always go to the same shard, maintaining strong consistency per key.

### Script Caching

Redis caches the compiled Lua script after the first `EVAL`. Subsequent calls to `EVAL` with the same script body reuse the cached bytecode. EdgeQuota does not use `EVALSHA` explicitly — Redis handles this transparently.

---

## Key Naming Strategy

Rate limit keys follow the pattern:

```
rl:edgequota:<extracted_key>
```

| Strategy | Example Request | Redis Key |
|----------|----------------|-----------|
| `clientIP` | From `10.0.0.1` | `rl:edgequota:10.0.0.1` |
| `header` | `X-Tenant-Id: acme-corp` | `rl:edgequota:acme-corp` |
| `composite` | `X-Tenant-Id: acme-corp`, path `/api/v1/users` | `rl:edgequota:acme-corp:api` |

### Key Extraction

| Strategy | Resolution Order |
|----------|-----------------|
| `clientIP` | `X-Forwarded-For` (first IP) → `X-Real-IP` → `RemoteAddr` |
| `header` | Value of the configured header. Returns error if empty/missing. |
| `composite` | Configured header + optional first path segment (e.g., `tenant:api`). |

### Redis Cluster Considerations

In Redis Cluster, keys are distributed across shards based on a hash slot derived from the key. Rate limit keys for different tenants/IPs will naturally distribute across shards — there is no hash tag (`{...}`) in the key, so each key is independently sharded.

This is intentional: multi-key operations are not needed (each rate-limit check involves exactly one key), so there is no reason to force keys onto the same shard.

---

## TTL and Key Expiration

Every rate limit key has an `EXPIRE` set after each operation. The TTL is calculated as:

```
TTL = max(ceil(burst / average_per_second), period_seconds) + period_seconds
```

This ensures:

1. **Idle keys expire.** If a client stops sending requests, the key is automatically removed after the TTL.
2. **Active keys stay alive.** Every successful `EVAL` resets the `EXPIRE`, so active keys never disappear mid-session.
3. **No manual cleanup needed.** Redis handles all expiration.

### Memory Pressure

For high-cardinality key strategies (e.g., per-IP rate limiting with millions of unique IPs), key memory is bounded by the TTL — idle keys expire automatically. If Redis memory pressure becomes a concern:

1. Use Redis Cluster to distribute keys across shards.
2. Reduce the TTL (lower burst or period values).
3. Consider switching from `clientIP` to `header` or `composite` for lower cardinality.

---

## Clock Skew Handling

The Lua script uses timestamps provided by the calling EdgeQuota instance (not Redis `TIME`). In a multi-instance deployment, clock skew between pods can affect token replenishment accuracy.

The script includes a guard:

```lua
if now < last then
  last = now
end
```

This handles **backwards clock jumps**: if the current timestamp is earlier than the last recorded timestamp, the script resets `last` to `now`, preventing negative elapsed time. This makes the algorithm tolerant of:

- NTP adjustments
- VM live migration
- Pod rescheduling to a node with a slightly behind clock

**Forward clock jumps** can grant extra tokens (because the elapsed time is larger), but this is bounded by `burst` — the bucket can never exceed its maximum capacity.

For strict correctness under clock skew, consider using Redis `TIME` instead of client-provided timestamps. This adds one extra round trip but makes the system independent of client clocks. EdgeQuota intentionally uses client timestamps to avoid this extra round trip; the token bucket's burst cap makes the system tolerant of typical NTP drift (< 100ms).

---

## In-Memory Fallback

When Redis is unreachable and `failure_policy` is `inMemoryFallback`, EdgeQuota activates a local token-bucket limiter.

### Characteristics

- **Per-instance.** Each EdgeQuota pod has its own counters. A client hitting different pods will have separate buckets.
- **Maximum 65,536 keys.** When the cap is reached, approximately 10% of the oldest keys are evicted.
- **Periodic cleanup.** A background goroutine removes keys older than the TTL every cleanup cycle.
- **Not cluster-wide.** The effective rate limit is multiplied by the number of EdgeQuota replicas.

### When to Use

In-memory fallback is appropriate when:

- Brief Redis outages are expected (e.g., during failover).
- Approximate rate limiting is acceptable during the outage.
- Complete rate limit bypass (`passThrough`) is not acceptable.

For strict multi-tenant enforcement, `failClosed` may be more appropriate during Redis outages.

---

## Failure Policies

| Policy | Redis Down | Behavior |
|--------|-----------|----------|
| `passThrough` | Yes | Allow all requests. Rate limiting is disabled. |
| `failClosed` | Yes | Reject all requests with the configured `failure_code` (default: 429). |
| `inMemoryFallback` | Yes | Use local in-memory token bucket (per-instance, not distributed). |

### Recovery

When Redis becomes unavailable, EdgeQuota starts a background recovery loop:

1. Wait with exponential backoff: 1s, 2s, 4s, ... up to 30s.
2. Add random jitter (0 to current delay) to prevent thundering herd.
3. Attempt to ping Redis.
4. On success: recreate the Redis limiter and resume normal operation.
5. On failure: repeat from step 1.

The recovery loop runs for `passThrough` and `inMemoryFallback` policies. For `failClosed`, no recovery is attempted — the assumption is that an operator will intervene.

---

## External Rate Limit Service

When `rate_limit.external.enabled` is `true`, EdgeQuota queries an external service for dynamic per-request rate limits. This enables multi-tenant scenarios where different tenants have different quotas defined in a backend database.

### Request

The external service receives:

```json
{
  "key": "tenant-42",
  "headers": {"X-Tenant-Id": "tenant-42", "X-Plan": "enterprise"},
  "method": "GET",
  "path": "/api/v1/data"
}
```

### Response

The service returns rate limit parameters and optional cache control directives:

```json
{
  "average": 1000,
  "burst": 200,
  "period": "1s",
  "cache_max_age_seconds": 300,
  "cache_no_store": false
}
```

| Field | Type | Description |
|-------|------|-------------|
| `average` | int64 | Maximum requests per period (0 = unlimited). |
| `burst` | int64 | Maximum burst size. |
| `period` | string | Duration string (e.g. `"1s"`, `"1m"`). |
| `cache_max_age_seconds` | int64 (optional) | Cache this response for N seconds. Omit or set to 0 to use the default TTL. |
| `cache_no_store` | bool (optional) | If `true`, do not cache this response. |

These values override the static `rate_limit.average`, `burst`, and `period` from the config file. If the external service is unreachable, the static values are used.

The cache control fields are consistent across HTTP JSON and gRPC (see proto definition in `api/proto/ratelimit/v1/ratelimit.proto`).

### Caching

Responses are cached in **Redis**, making the cache shared across all EdgeQuota instances. This provides:

- **Distributed consistency** — all instances see the same cached limits, eliminating per-instance divergence.
- **No background cleanup** — Redis manages key expiration automatically via TTL.
- **Horizontal scalability** — adding instances does not multiply external service calls.

Cache keys are stored under the `rl:extcache:` prefix with the rate-limit key appended (e.g. `rl:extcache:tenant-1`).

#### TTL Resolution (HTTP)

For HTTP responses, the cache TTL is resolved in this priority order:

1. **`Cache-Control: max-age=N` header** — TTL is `N` seconds.
2. **`Cache-Control: no-cache` or `no-store` header** — response is **not cached** (TTL = 0).
3. **`Expires` header** — TTL is the duration until the specified timestamp.
4. **Body `cache_no_store: true`** — response is **not cached** (TTL = 0).
5. **Body `cache_max_age_seconds: N`** (when present and > 0) — TTL is `N` seconds.
6. **None of the above** — the configured default TTL is used (`rate_limit.external.cache_ttl`, default: 60s).

HTTP cache headers always take precedence over body fields. The body fields serve as a fallback, ensuring consistent behaviour with gRPC.

#### TTL Resolution (gRPC)

gRPC responses do not carry HTTP cache headers. TTL is resolved from the response body:

1. **`cache_no_store: true`** — response is **not cached** (TTL = 0).
2. **`cache_max_age_seconds: N`** (when present and > 0) — TTL is `N` seconds.
3. **Neither field set** — the configured default TTL is used (`rate_limit.external.cache_ttl`, default: 60s).

This gives the external service full control over per-response cache lifetimes regardless of the transport protocol.

#### No Redis Available

If the Redis client is not available when the external rate limit feature is initialized (e.g. Redis is down at startup with a passthrough failure policy), caching is disabled and every request triggers an external service call.

### Protocols

| Protocol | Endpoint |
|----------|----------|
| HTTP | `POST` to `rate_limit.external.http.url` with JSON body |
| gRPC | `edgequota.ratelimit.v1.RateLimitService/GetLimits` with JSON codec |

See `api/proto/ratelimit/v1/ratelimit.proto` for the gRPC service definition.

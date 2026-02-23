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
  return {1, 0, burst, burst, 0}
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

-- Compute time until bucket is fully replenished (for X-RateLimit-Reset).
local reset_after = 0
if tokens < burst then
  reset_after = math.ceil((burst - tokens) / rate)
end

-- Only refresh EXPIRE when remaining TTL is below 50% of target. This
-- halves Redis write amplification for steady-state keys that are accessed
-- frequently.
local needs_expire = true
local cur_ttl = redis.call('ttl', key)
if cur_ttl > 0 and cur_ttl > ttl / 2 then
  needs_expire = false
end

-- Try to consume one token.
if tokens >= 1 then
  tokens = tokens - 1
  redis.call('hset', key, 'last', now, 'tokens', tokens)
  if needs_expire then
    redis.call('expire', key, ttl)
  end
  local remaining = math.floor(tokens)
  return {1, 0, remaining, burst, reset_after}
end

-- Bucket empty: compute retry delay.
redis.call('hset', key, 'last', now, 'tokens', tokens)
if needs_expire then
  redis.call('expire', key, ttl)
end
local retry = math.ceil((1 - tokens) / rate)
return {0, retry, 0, burst, reset_after}
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

A five-element array: `{allowed, retry_after_micros, remaining_tokens, limit, reset_after_micros}`.

| Element | Type | Description |
|---------|------|-------------|
| `allowed` | 0 or 1 | `1` = request permitted, `0` = request denied |
| `retry_after_micros` | integer | Microseconds until one token is available. `0` when allowed. |
| `remaining_tokens` | integer | Tokens remaining in the bucket after this request |
| `limit` | integer | Bucket capacity (burst) |
| `reset_after_micros` | integer | Microseconds until the bucket is fully replenished |

### Conditional EXPIRE Optimization

The script checks the current TTL of the Redis key before calling `EXPIRE`. If the remaining TTL is still above 50% of the configured target, the `EXPIRE` call is skipped entirely. This halves Redis write amplification for steady-state keys that are accessed frequently — a significant optimization for high-throughput deployments where each `EXPIRE` is an additional write to the Redis keyspace.

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

EdgeQuota uses `EVALSHA` to execute the Lua script by its SHA1 hash, avoiding sending ~800 bytes of Lua source on every request. On a `NOSCRIPT` error (e.g., after a Redis restart or failover), it falls back to `EVAL` which loads the script and caches it for subsequent `EVALSHA` calls.

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
| `global` | (any) | `rl:edgequota:global` |

### Key Extraction

| Strategy | Resolution Order |
|----------|-----------------|
| `clientIP` | `X-Forwarded-For` (first IP) → `X-Real-IP` → `RemoteAddr` |
| `header` | Value of the configured header. Returns error if empty/missing. |
| `composite` | Configured header + optional first path segment (e.g., `tenant:api`). |
| `global` | Single shared bucket for all requests. All traffic is rate limited together. Suitable for single-tenant or low-cardinality scenarios (e.g., static assets, frontend bundles). |

### Frontend and Static Assets

For frontend bundles, static assets (JS, CSS, images), or single-page applications where all requests share the same effective quota, use the **`global`** key strategy:

```yaml
rate_limit:
  static:
    average: 500
    burst: 200
    period: "1s"
    key_strategy:
      type: "global"
```

All traffic shares one bucket. This is appropriate when:

- A CDN or cache sits in front and you want to limit origin hits.
- You serve a single tenant and need an overall RPS cap.
- Static asset requests lack tenant identifiers (no `X-Tenant-Id` header).

For path-scoped limits (e.g., different limits for `/api/` vs `/static/`), use the external rate limit service and return `tenant_key` values derived from the path or `Host` header.

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

When external RL is enabled, the `rate_limit.static` block is **not used** -- EdgeQuota skips local key extraction entirely. The `rate_limit.external.fallback` block (required) defines the safety-net limits applied when the external service is unreachable.

### Configuration

Exactly one of `external.http` or `external.grpc` must be configured when the feature is enabled.

**HTTP example:**

```yaml
rate_limit:
  external:
    enabled: true
    timeout: "5s"
    max_concurrent_requests: 50
    http:
      url: "http://limits-service:8080/limits"
    header_filter:
      allow_list:
        - "X-Tenant-Id"
        - "X-Plan"
        - "X-Request-Id"           # forwarded for tracing
    fallback:
      backend_url: "http://my-backend:8080"
      average: 100
      burst: 50
      period: "1s"
      key_strategy:
        type: "header"
        header_name: "X-Tenant-Id"
```

When external RL is enabled, the `static` block is ignored -- EdgeQuota skips local key extraction and the external service owns quota resolution. The `fallback` block is **required** and defines the safety-net limits used when the external service is unavailable or when its response lacks `backend_url`.

Caching of external service responses is driven by `Cache-Control` headers or body fields (`cache_max_age_seconds`, `cache_no_store`) returned by the service. See [Response Caching](caching.md).

**gRPC example:**

```yaml
rate_limit:
  external:
    enabled: true
    timeout: "5s"
    max_concurrent_requests: 50
    grpc:
      address: "limits-service:50052"
      tls:
        enabled: true
        ca_file: "/etc/edgequota/tls/limits-ca.pem"
    fallback:
      backend_url: "http://my-backend:8080"
      average: 100
      burst: 50
      period: "1s"
      key_strategy:
        type: "header"
        header_name: "X-Tenant-Id"
```

| Config Field | Type | Default | Description |
|---|---|---|---|
| `external.enabled` | bool | `false` | Enable external rate limit resolution |
| `external.timeout` | duration | `"5s"` | Max wait for external service response |
| `external.max_concurrent_requests` | int | `50` | Semaphore cap on concurrent external calls |
| `external.http.url` | string | — | HTTP endpoint URL |
| `external.grpc.address` | string | — | gRPC endpoint address |
| `external.grpc.tls.enabled` | bool | `false` | Enable TLS for gRPC |
| `external.grpc.tls.ca_file` | string | — | CA certificate for gRPC TLS verification |
| `external.fallback.backend_url` | string | **(required)** | Backend URL for fallback path. Required when external RL is enabled. |
| `external.fallback.average` | int64 | **(required)** | Fallback requests per period. Must be > 0. |
| `external.fallback.burst` | int64 | `1` | Fallback burst capacity |
| `external.fallback.period` | duration | `"1s"` | Fallback time window |
| `external.fallback.key_strategy` | object | **(required)** | Key extraction strategy during fallback |
| `external.cache_key_headers.include` | list | — | **Deprecated.** Ephemeral headers are now stripped automatically. See [Response Caching](caching.md). |
| `external.cache_key_headers.exclude` | list | — | **Deprecated.** Ephemeral headers are now stripped automatically. See [Response Caching](caching.md). |

### Protocols

| Protocol | Endpoint |
|----------|----------|
| HTTP | `POST` to `rate_limit.external.http.url` with JSON body |
| gRPC | `edgequota.ratelimit.v1.RateLimitService/GetLimits` with JSON codec |

Both protocols use the same request/response schema. The gRPC service uses a JSON codec — the external service can be implemented without protobuf code generation.

See `api/proto/ratelimit/v1/ratelimit.proto` for the formal gRPC service definition.

### Request Format

EdgeQuota sends the request metadata (headers, method, path) to the external service. There is **no `key` field** — the external service derives the tenant/bucket key from the request context (headers, `Host`, path, etc.) and returns it via the `tenant_key` response field.

```json
{
  "headers": {"X-Tenant-Id": "tenant-42", "X-Plan": "enterprise", "Host": "api.example.com"},
  "method": "GET",
  "path": "/api/v1/data"
}
```

This keeps key derivation in the external service, which has access to the full request context and can map multiple input dimensions (headers, path, Host) to quota buckets.

Which headers are forwarded is controlled by `external.header_filter`:

- **`allow_list`** (exclusive) — only these headers are sent; all others are dropped.
- **`deny_list`** — these headers are never sent. Ignored when `allow_list` is set.

Default sensitive headers (`Authorization`, `Cookie`, `X-Api-Key`, etc.) are included in the default deny list.

### Response

The service returns rate limit parameters and optional per-tenant overrides:

```json
{
  "average": 1000,
  "burst": 200,
  "period": "1s",
  "tenant_key": "acme-corp",
  "failure_policy": "passthrough",
  "failure_code": 503,
  "backend_url": "http://tenant-backend:8080",
  "cache_max_age_seconds": 300,
  "cache_no_store": false
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `average` | int64 | yes | Requests per period (0 = unlimited). Overrides static config. |
| `burst` | int64 | yes | Maximum burst capacity. Overrides static config. |
| `period` | string | yes | Duration string (e.g. `"1s"`, `"1m"`). Overrides static config. |
| `tenant_key` | string | no | Custom Redis bucket key. Prefixed with `t:` (e.g. `rl:edgequota:t:acme-corp`). The external service should return this to assign a per-tenant bucket. |
| `failure_policy` | string | no | Per-tenant Redis-down behavior: `passthrough`, `failclosed`, `inmemoryfallback`. Overrides the global `rate_limit.failure_policy`. |
| `failure_code` | int | no | HTTP status for `failclosed` rejections. Overrides the global `rate_limit.failure_code`. |
| `backend_url` | string | **yes** | Per-request backend URL. **Required** when external RL is enabled. EdgeQuota proxies to this URL. Responses without `backend_url` trigger the fallback path. Enables routing different tenants to dedicated clusters. |
| `cache_max_age_seconds` | int64 | no | Cache this response for N seconds. Omit or set to 0 to use `external.cache_ttl`. |
| `cache_no_store` | bool | no | If `true`, do not cache this response. |

### Caching

External service responses and backend responses are cached using EdgeQuota's CDN-style response cache. Cache behavior is driven entirely by the response — via `Cache-Control` headers (HTTP) or body fields like `cache_max_age_seconds` and `cache_no_store` (gRPC). If the external service returns no cache directives, every request hits the service.

Ephemeral headers (tracing IDs, `X-Request-Id`, `X-Forwarded-*`, etc.) are automatically stripped from cache keys. No configuration is needed.

See [Response Caching](caching.md) for full details on cache key construction, `Cache-Control` semantics, conditional requests, invalidation, and Redis configuration.

#### Concurrency Control

Two mechanisms prevent overloading the external service:

| Mechanism | Scope | Behavior |
|-----------|-------|----------|
| **Singleflight** | Per lookup key | Concurrent cache misses for the same lookup key share a single external call. All waiters receive the same result. |
| **Semaphore** | Global | Caps total concurrent external calls at `max_concurrent_requests` (default: 50). Excess requests fall back to stale cache when available. |

### Failure Handling and Circuit Breaker

When the external service is unreachable, times out, or the circuit breaker is open, EdgeQuota falls back through this chain:

1. **Stale cache** — if a previous response for this key exists in `rl:extcache:stale:<key>`, use it.
2. **Fallback config** — use `rate_limit.external.fallback` (`average`, `burst`, `period`, and `key_strategy`). This block is **required** when external RL is enabled.

There is no separate "failure policy" for the external service — it only affects which limits are applied. Rate limiting itself continues using the fallback limits.

#### Per-Key Circuit Breaker

The circuit breaker is **per-request-signature** (one breaker per unique combination of headers, method, and path), preventing a single tenant's slow external lookups from affecting others:

| Parameter | Value |
|-----------|-------|
| Failure threshold | 5 consecutive errors for the same key |
| Open duration | 30 s |
| Half-open probe | 1 request allowed through |
| Idle eviction | Breakers unused for 60 s are evicted |

**Lifecycle:**

1. **Closed** (normal) — requests are forwarded to the external service.
2. **Open** — after 5 consecutive failures for a key, the breaker opens. Requests immediately try stale cache, then fall back to static config.
3. **Half-open** — after 30 s, one probe request is forwarded. Success closes the breaker; failure re-opens it.

---

## See Also

- [Configuration Reference](configuration.md) -- Full `rate_limit` config section.
- [Response Caching](caching.md) -- CDN-style response caching semantics, cache keys, and invalidation.
- [API Reference](api-reference.md) -- Proto and OpenAPI definitions for the external rate limit service.
- [Go SDK](go-sdk.md) -- Server-side helpers for building rate limit services.
- [Architecture](architecture.md) -- Data flow and failure modes.
- [Security](security.md) -- Tenant key validation and backend URL override security.
- [Troubleshooting](troubleshooting.md) -- Common rate limiting issues.

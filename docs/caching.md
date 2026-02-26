# Response Caching (CDN)

EdgeQuota functions as a **CDN-style response cache** in addition to its rate limiting and authentication roles. Together, these form three complementary responsibilities:

1. **Rate-limit enforcer** — Token-bucket quota enforcement via Redis.
2. **Auth gateway** — External authentication forwarding before any backend call.
3. **CDN** — Caches backend responses in Redis, honoring standard `Cache-Control` semantics.

The cache is **purely response-driven**. Origins opt in by returning `Cache-Control` headers (HTTP) or body fields (gRPC). If a backend response carries no cache directives, EdgeQuota does not cache it — there is no implicit caching.

---

## How Cache Keys Work

EdgeQuota derives a cache key from the request's stable attributes:

```
<METHOD>|<PATH>?<QUERY>|<Header1>=<Value1>|<Header2>=<Value2>|...
```

Headers in the key are sorted alphabetically for determinism.

### Ephemeral Header Stripping

Per-request ephemeral headers are **automatically stripped** from the cache key. No user configuration is needed. The built-in ephemeral list covers:

| Category | Headers stripped |
|----------|-----------------|
| **W3C / OpenTelemetry** | `Traceparent`, `Tracestate` |
| **Zipkin / B3** | `X-B3-Traceid`, `X-B3-Spanid`, `X-B3-Parentspanid`, `X-B3-Sampled`, `X-B3-Flags`, `B3` |
| **Jaeger** | `Uber-Trace-Id` |
| **AWS X-Ray** | `X-Amzn-Trace-Id` |
| **Google Cloud Trace** | `X-Cloud-Trace-Context` |
| **Request / correlation IDs** | `X-Request-Id`, `X-Correlation-Id`, `Request-Id`, `X-Req-Id` |
| **Proxy / forwarding** | `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host`, `X-Forwarded-Port`, `X-Real-Ip`, `Forwarded`, `Via`, `True-Client-Ip` |
| **Envoy / service mesh** | `X-Envoy-Attempt-Count`, `X-Envoy-External-Address`, `X-Envoy-Decorator-Operation`, `X-Envoy-Upstream-Service-Time` |
| **CDN / edge** | `X-Amz-Cf-Id`, `Cf-Ray`, `Cdn-Loop`, `X-Request-Start`, `X-Queue-Start` |

### Vary Header Support

When a cached response includes a `Vary` header, the listed headers are included in the cache key. For example, `Vary: Accept-Encoding` causes the `Accept-Encoding` request header value to participate in the key, producing separate cache entries per encoding.

---

## Cache-Control Semantics

EdgeQuota honors standard HTTP `Cache-Control` directives on backend responses:

| Directive | Behavior |
|-----------|----------|
| `max-age=N` | Cache the response for `N` seconds. |
| `no-store` | Do not cache the response. |
| `no-cache` | Do not cache the response (must revalidate every time). |
| `private` | Do not cache the response (intended for a single user). |
| `public` | Explicitly cacheable. Combined with `max-age`, enables caching. |

### What Gets Cached

- Only **200 OK** and **301 Moved Permanently** responses are cached.
- There is **no implicit caching**. If the backend response has no `Cache-Control` header, EdgeQuota passes it through without caching.
- `no-store`, `no-cache`, and `private` directives all prevent caching.

---

## Response Body Size Limits

Responses larger than `max_body_size` pass through to the client uncached. This prevents large downloads from consuming cache storage.

| Field | Default | Description |
|-------|---------|-------------|
| `cache.max_body_size` | `1048576` (1 MB) | Maximum response body size in bytes eligible for caching. |

Responses that exceed this limit are served directly to the client. The `Content-Length` header (when present) is checked before buffering; chunked responses are checked as they stream.

---

## Cache Invalidation

EdgeQuota provides two invalidation mechanisms via the admin API.

### Purge by URL/Key

```
POST /v1/cache/purge
Content-Type: application/json

{
  "keys": [
    "GET|/static/app.css?v=2||",
    "GET|/api/v1/config||"
  ]
}
```

Removes specific cache entries by their exact cache key.

### Purge by Tag

```
POST /v1/cache/purge/tags
Content-Type: application/json

{
  "tags": ["product-123", "homepage"]
}
```

Removes all cache entries associated with the given tags.

#### Tag Mechanism

Tags are assigned to cached responses via the `Surrogate-Key` or `Cache-Tag` response header from the backend. Multiple tags are space-separated:

```
Surrogate-Key: product-123 category-electronics homepage
```

When a tag-based purge is issued, EdgeQuota invalidates every cached response that carries any of the specified tags. This is useful for invalidating groups of related resources (e.g., all pages that display a specific product).

---

## Conditional Requests

EdgeQuota supports conditional request validation using `ETag` and `Last-Modified` headers.

When a cached entry exists and the backend originally returned `ETag` or `Last-Modified`, EdgeQuota sends conditional headers to the backend on revalidation:

| Cached Header | Conditional Request Header |
|---------------|---------------------------|
| `ETag` | `If-None-Match` |
| `Last-Modified` | `If-Modified-Since` |

If the backend responds with **304 Not Modified**, EdgeQuota refreshes the cache entry's TTL and serves the existing cached response. This saves bandwidth and backend processing for resources that haven't changed.

---

## Redis Configuration

The response cache uses Redis for storage, shared across all EdgeQuota instances.

### Fallback Chain

EdgeQuota resolves the Redis connection for the response cache using this priority:

1. **`response_cache_redis`** — Dedicated Redis for the response cache. Use this when you want to separate cache storage from rate-limit counters and external RL caches.
2. **`cache_redis`** — Shared cache Redis (also used by external RL response caches). Used when `response_cache_redis` is not configured.
3. **`redis`** — Main Redis instance. Used when neither `response_cache_redis` nor `cache_redis` is configured.

This allows you to start simple (one Redis for everything) and split workloads as traffic grows.

### Example: Separate Redis Instances

```yaml
redis:
  endpoints:
    - "rl-counter-0:6379"
    - "rl-counter-1:6379"
  mode: "cluster"

cache_redis:
  endpoints:
    - "ext-rl-cache:6379"
  mode: "single"

response_cache_redis:
  endpoints:
    - "cdn-cache-primary:6379"
    - "cdn-cache-replica:6379"
  mode: "replication"
  pool_size: 30
```

The `response_cache_redis` section has the same schema as `redis`. See [Configuration Reference](configuration.md) for all fields.

---

## External RL/Auth Response Caching

External rate-limit and auth services can control their own caching behavior. EdgeQuota applies the same priority order for both:

| Priority | Source | Applies to |
|----------|--------|-----------|
| 1 | HTTP `Cache-Control: max-age=N` | HTTP backends only |
| 2 | HTTP `Expires: <RFC1123>` | HTTP backends only |
| 3 | Body `cache_max_age_seconds` | HTTP and gRPC |
| 4 | Body `cache_no_store: true` | HTTP and gRPC |

**HTTP headers always take precedence over body fields.** If the response carries `Cache-Control: max-age=600` and the body also sets `"cache_max_age_seconds": 30`, EdgeQuota caches for 600 seconds. Body fields are the only option for gRPC services (which have no HTTP headers).

**Body cache fields:**

| Field | Type | Description |
|-------|------|-------------|
| `cache_max_age_seconds` | int64 | Cache the response for N seconds. |
| `cache_no_store` | bool | If `true`, do not cache this response. |

If an external service response contains **no cache directives** (no `Cache-Control` header, no `Expires` header, no body fields), EdgeQuota does not cache it — every request hits the external service. This is the safe default; services must explicitly opt in to caching.

EdgeQuota logs the cache decision at `DEBUG` level on every auth and external-RL call:

```
auth response cached     ttl=5m0s   source="Cache-Control: max-age=300"
auth cache hit, skipping external auth call
auth response not cached source="body: cache_no_store=true"
```

---

## Examples

### Static Asset Caching

A backend serving CSS and JavaScript files returns `Cache-Control: max-age=3600`:

```
GET /static/app.css HTTP/1.1
Host: cdn.example.com

HTTP/1.1 200 OK
Cache-Control: public, max-age=3600
Content-Type: text/css
Surrogate-Key: static-assets v2-release

body...
```

1. First request: EdgeQuota proxies to the backend, caches the response for 3600 seconds.
2. Second request (within 1 hour): EdgeQuota serves directly from cache. No backend call.
3. Purge by tag: `POST /v1/cache/purge/tags` with `{"tags": ["v2-release"]}` invalidates all assets tagged with `v2-release`.

### API Response (No Caching)

A backend serving dynamic API data returns `Cache-Control: no-store`:

```
GET /api/v1/user/profile HTTP/1.1
Host: api.example.com
Authorization: Bearer token123

HTTP/1.1 200 OK
Cache-Control: no-store
Content-Type: application/json

{"name": "Alice", "email": "alice@example.com"}
```

Every request is proxied to the backend. The `no-store` directive prevents EdgeQuota from caching the response.

### API Response (No Cache-Control Header)

```
GET /api/v1/data HTTP/1.1
Host: api.example.com

HTTP/1.1 200 OK
Content-Type: application/json

{"result": "dynamic"}
```

No `Cache-Control` header means no caching. EdgeQuota passes through every request.

---

## Metrics Reference

All response cache metrics are exposed on the admin server at `GET :9090/metrics`.

### Counters

| Metric | Description |
|--------|-------------|
| `edgequota_response_cache_hits_total` | Requests served from cache |
| `edgequota_response_cache_misses_total` | Requests that missed the cache and were proxied to the backend |
| `edgequota_response_cache_stores_total` | Responses stored in cache |
| `edgequota_response_cache_store_errors_total` | Errors storing responses in cache (Redis write failures) |
| `edgequota_response_cache_evictions_total` | Cache entries evicted (TTL expiry or explicit purge) |
| `edgequota_response_cache_purge_total` | Purge operations executed (by key or by tag) |
| `edgequota_response_cache_revalidations_total` | Conditional requests that received 304 Not Modified |
| `edgequota_response_cache_skip_total` | Responses skipped from caching (no-store, too large, non-cacheable status) |

### Histograms

| Metric | Description |
|--------|-------------|
| `edgequota_response_cache_body_size_bytes` | Distribution of cached response body sizes |

---

## Configuration Reference

### `cache` — Response Cache Settings

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_CACHE_ENABLED` | Enable CDN-style response caching |
| `max_body_size` | int64 | `1048576` | `EDGEQUOTA_CACHE_MAX_BODY_SIZE` | Maximum response body size in bytes eligible for caching (default: 1 MB) |

```yaml
cache:
  enabled: true
  max_body_size: 5242880  # 5 MB
```

### `response_cache_redis` — Dedicated Redis for Response Cache (Optional)

When configured, the response cache uses this Redis connection instead of `cache_redis` or `redis`. The field schema is identical to the `redis` section.

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `endpoints` | []string | — | `EDGEQUOTA_RESPONSE_CACHE_REDIS_ENDPOINTS` | Response cache Redis endpoints |
| `mode` | string | `"single"` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_MODE` | Topology: `single`, `replication`, `sentinel`, `cluster` |
| `master_name` | string | `""` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_MASTER_NAME` | Master name (sentinel mode) |
| `username` | string | `""` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_USERNAME` | Redis username |
| `password` | string | `""` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_PASSWORD` | Redis password |
| `db` | int | `0` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_DB` | Redis database |
| `pool_size` | int | `10` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_POOL_SIZE` | Connection pool size |
| `dial_timeout` | duration | `"5s"` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_DIAL_TIMEOUT` | Connection timeout |
| `read_timeout` | duration | `"3s"` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_READ_TIMEOUT` | Read timeout |
| `write_timeout` | duration | `"3s"` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_WRITE_TIMEOUT` | Write timeout |

TLS sub-fields: `EDGEQUOTA_RESPONSE_CACHE_REDIS_TLS_ENABLED`, `EDGEQUOTA_RESPONSE_CACHE_REDIS_TLS_INSECURE_SKIP_VERIFY`.

```yaml
response_cache_redis:
  endpoints:
    - "cdn-cache-primary:6379"
    - "cdn-cache-replica:6379"
  mode: "replication"
  pool_size: 30
```

---

## See Also

- [Configuration Reference](configuration.md) — Full `cache` and `response_cache_redis` config sections.
- [Rate Limiting](rate-limiting.md) — Rate-limit algorithm and external service caching.
- [Observability](observability.md) — All metrics including response cache counters.
- [Troubleshooting](troubleshooting.md) — Cache debugging and common issues.
- [Architecture](architecture.md) — System design and data flow.
- [Deployment Scenarios](deployment-scenarios.md) — Static asset caching scenario.

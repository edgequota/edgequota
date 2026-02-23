# Deployment Scenarios

This page walks through four common deployment patterns. Each includes the relevant `rate_limit` and `auth` YAML config and an explanation of how the pieces interact.

For the full field reference, see [Configuration](configuration.md).

---

## Scenario 1: Static Rate Limiting Only

**Use case:** Single backend, no auth, per-IP or per-tenant rate limiting with fixed quotas.

```yaml
rate_limit:
  static:
    backend_url: "http://my-backend:8080"
    average: 100
    burst: 50
    period: "1s"
    key_strategy:
      type: "clientIP"
```

EdgeQuota extracts the client IP from every request, hashes it into a Redis bucket, and enforces 100 req/s with a burst of 50. No external services are involved.

**Variant -- per-tenant via header:**

If an upstream load balancer or auth proxy injects a tenant header:

```yaml
rate_limit:
  static:
    backend_url: "http://my-backend:8080"
    average: 100
    burst: 50
    period: "1s"
    key_strategy:
      type: "header"
      header_name: "X-Tenant-Id"
```

Requests without the header receive HTTP 500 (`key_extraction_failed`). Make sure the header is always present.

---

## Scenario 2: External Auth + Static Rate Limiting

**Use case:** External auth service validates credentials and injects `X-Tenant-Id`. Static rate limiting keys off that header.

```yaml
auth:
  enabled: true
  timeout: "5s"
  failure_policy: "failclosed"
  http:
    url: "http://auth-service:8080/check"

rate_limit:
  static:
    backend_url: "http://my-backend:8080"
    average: 100
    burst: 50
    period: "1s"
    key_strategy:
      type: "header"
      header_name: "X-Tenant-Id"
```

Request flow:

1. EdgeQuota forwards the request to the auth service.
2. Auth service validates credentials and returns `X-Tenant-Id: tenant-123` in the response headers.
3. EdgeQuota injects the header into the request and extracts it as the rate-limit key.
4. Rate limiting is applied per tenant.

The `header` strategy is safe here because auth guarantees the header is always present on allowed requests.

---

## Scenario 3: No Auth + External Rate Limiting (FE/Static Assets)

**Use case:** Frontend assets served through EdgeQuota. Browsers do not send custom headers. An external rate-limit service derives quotas from `Host`, `path`, and other standard headers.

```yaml
rate_limit:
  external:
    enabled: true
    timeout: "5s"
    http:
      url: "http://limits-service:8080/limits"
    fallback:
      backend_url: "http://my-backend:8080"
      average: 5000
      burst: 2000
      period: "1s"
      key_strategy:
        type: "global"
        global_key: "fe-assets"
```

How it works:

- EdgeQuota **skips local key extraction** entirely. The `static` block is ignored.
- EdgeQuota sends the request headers (including `Host`), method, and path to the external rate-limit service. There is no `key` field -- the external service derives the tenant from the request context.
- Ephemeral headers (`X-Request-Id`, `Traceparent`, `Cf-Ray`, etc.) are automatically excluded from cache keys, giving high cache hit rates. No configuration needed.
- The external service derives the appropriate tenant/quota from `Host` and path (e.g., `cdn.example.com` gets 10,000 req/s, `api.example.com` gets 500 req/s).
- If the external service is unreachable, EdgeQuota falls back to the `fallback` block: 5,000 req/s with a single shared `"fe-assets"` bucket for all requests.

The `global` fallback strategy is ideal here because FE traffic lacks tenant headers. A single shared bucket provides platform-wide protection during outages without per-IP cardinality explosion in Redis.

---

## Scenario 4: External Auth + External Rate Limiting

**Use case:** Full multi-tenant setup. Auth injects `X-Tenant-Id`, external rate-limit service reads it from request headers and returns per-tenant quotas from a database.

```yaml
auth:
  enabled: true
  timeout: "5s"
  failure_policy: "failclosed"
  http:
    url: "http://auth-service:8080/check"

rate_limit:
  external:
    enabled: true
    timeout: "5s"
    http:
      url: "http://limits-service:8080/limits"
    header_filter:
      allow_list:
        - "X-Tenant-Id"
        - "X-Plan"
    fallback:
      backend_url: "http://my-backend:8080"
      average: 100
      burst: 50
      period: "1s"
      key_strategy:
        type: "header"
        header_name: "X-Tenant-Id"
```

Request flow:

1. Auth service validates the request and injects `X-Tenant-Id: tenant-123` and `X-Plan: enterprise`.
2. EdgeQuota forwards `X-Tenant-Id` and `X-Plan` to the external rate-limit service (filtered by `header_filter.allow_list`).
3. The external service looks up `tenant-123` in its database and returns `average: 10000, burst: 5000, tenant_key: "tenant-123"`.
4. EdgeQuota applies the dynamic limits using a per-tenant Redis bucket (`t:tenant-123`).

If the external service is unreachable, EdgeQuota falls back to the `fallback` block: 100 req/s per tenant, keyed by the `X-Tenant-Id` header (which auth already injected). This is safe because auth runs before rate limiting -- if auth fails, the request is rejected before it reaches the fallback path.

---

## Scenario 5: Static Asset Caching (CDN)

**Use case:** Backend serves static assets (CSS, JS, images) with `Cache-Control` headers. EdgeQuota caches these responses in Redis and serves subsequent requests without hitting the backend.

```yaml
cache:
  enabled: true
  max_body_size: 10485760  # 10 MB — large enough for JS bundles

rate_limit:
  static:
    backend_url: "http://my-backend:8080"
    average: 5000
    burst: 2000
    period: "1s"
    key_strategy:
      type: "global"

response_cache_redis:
  endpoints:
    - "cdn-cache-primary:6379"
    - "cdn-cache-replica:6379"
  mode: "replication"
  pool_size: 30
```

How it works:

- The backend returns `Cache-Control: public, max-age=86400` for static assets.
- EdgeQuota caches the response in Redis for 86,400 seconds (24 hours).
- Subsequent requests for the same asset are served directly from cache — no backend call.
- Dynamic API endpoints return `Cache-Control: no-store`, so they are never cached.
- The backend can tag responses with `Surrogate-Key: v2-release static-assets` for group invalidation.
- To invalidate after a deployment: `POST /v1/cache/purge/tags` with `{"tags": ["v2-release"]}`.

A dedicated `response_cache_redis` in replication mode is recommended for read-heavy cache workloads. The rate-limit counters stay on the main `redis` instance (cluster mode for write-heavy workloads).

See [Response Caching](caching.md) for full cache semantics, `Cache-Control` directives, conditional requests, and invalidation.

---

## Choosing the Right Pattern

| Scenario | Auth? | External RL? | CDN Cache? | Key Source | Fallback Strategy |
|----------|-------|-------------|------------|------------|-------------------|
| 1. Static only | No | No | No | `clientIP` or `header` | N/A (no external) |
| 2. Auth + static RL | Yes | No | No | `header` (injected by auth) | N/A (no external) |
| 3. External RL (FE) | No | Yes | No | External service (from Host/path) | `global` |
| 4. Auth + external RL | Yes | Yes | No | External service (from headers) | `header` (from auth) |
| 5. Static asset CDN | No | No | Yes | `global` | N/A (no external) |

---

## See Also

- [Configuration Reference](configuration.md) -- Full field reference with env vars.
- [Rate Limiting](rate-limiting.md) -- Algorithm details and Redis interaction.
- [Response Caching](caching.md) -- CDN-style response caching semantics.
- [Helm Chart](helm-chart.md) -- Kubernetes deployment with Helm values.

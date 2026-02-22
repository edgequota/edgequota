# API Reference

EdgeQuota defines three gRPC services and their HTTP/JSON equivalents for integration with external services. All services use a **JSON codec** — implementations do not require protobuf code generation.

---

## External Rate Limit Service

Used when `rate_limit.external.enabled: true`. EdgeQuota queries this service for dynamic per-request rate limits.

### Proto Definition

```
edgequota.ratelimit.v1.RateLimitService/GetLimits
```

Source: `api/proto/edgequota/ratelimit/v1/ratelimit.proto`

### Request

Both HTTP (`POST` to the configured URL) and gRPC use the same JSON schema:

```json
{
  "key": "tenant-42",
  "headers": {
    "X-Tenant-Id": "tenant-42",
    "X-Plan": "enterprise",
    "Host": "api.example.com"
  },
  "method": "GET",
  "path": "/api/v1/data"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `key` | string | Extracted rate-limit key |
| `headers` | map[string]string | Filtered request headers (controlled by `external.header_filter`). Includes `Host` (re-injected from `r.Host`). |
| `method` | string | HTTP method |
| `path` | string | Request path |

### Response

```json
{
  "average": 1000,
  "burst": 200,
  "period": "1s",
  "tenant_key": "acme-corp",
  "failure_policy": "passthrough",
  "failure_code": 503,
  "backend_url": "http://tenant-backend:8080",
  "backend_protocol": "h2",
  "cache_max_age_seconds": 300,
  "cache_no_store": false
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `average` | int64 | yes | Requests per period. `0` = unlimited. |
| `burst` | int64 | yes | Maximum burst capacity |
| `period` | string | yes | Duration string (e.g., `"1s"`, `"1m"`) |
| `tenant_key` | string | no | Custom Redis bucket key. Replaces the extracted key with `t:` prefix (e.g., `rl:edgequota:t:acme-corp`). |
| `failure_policy` | string | no | Per-tenant Redis-down behavior: `passthrough`, `failclosed`, `inmemoryfallback` |
| `failure_code` | int | no | HTTP status for `failclosed` rejections |
| `backend_url` | string | no | Per-request backend URL override |
| `backend_protocol` | BackendProtocol | no | Per-request backend protocol override: `BACKEND_PROTOCOL_H1`, `BACKEND_PROTOCOL_H2`, `BACKEND_PROTOCOL_H3` |
| `cache_max_age_seconds` | int64 | no | Cache this response for N seconds. `0` = use `external.cache_ttl`. |
| `cache_no_store` | bool | no | Do not cache this response |

### BackendProtocol Enum

The `backend_protocol` field is a proper enum in both proto and OpenAPI:

| Proto Value | JSON Value | Description |
|-------------|------------|-------------|
| `BACKEND_PROTOCOL_UNSPECIFIED` | `""` | Use static config |
| `BACKEND_PROTOCOL_H1` | `"h1"` | Force HTTP/1.1 |
| `BACKEND_PROTOCOL_H2` | `"h2"` | Force HTTP/2 |
| `BACKEND_PROTOCOL_H3` | `"h3"` | Force HTTP/3 (QUIC) |

### HTTP Cache Control

For HTTP responses, the cache TTL is resolved in priority order:

1. `Cache-Control: max-age=N` header
2. `Cache-Control: no-cache` or `no-store` header (not cached)
3. `Expires` header
4. Body `cache_no_store: true` (not cached)
5. Body `cache_max_age_seconds: N`
6. Default: `rate_limit.external.cache_ttl` (60s)

For gRPC responses, only body fields 4–6 apply.

---

## External Auth Service

Used when `auth.enabled: true`. EdgeQuota sends every incoming request to this service before rate limiting.

### Proto Definition

```
edgequota.auth.v1.AuthService/Check
```

Source: `api/proto/edgequota/auth/v1/auth.proto`

### Request

```json
{
  "method": "POST",
  "path": "/api/v1/resources",
  "headers": {
    "Authorization": "Bearer eyJhbGciOi...",
    "Content-Type": "application/json",
    "Host": "api.example.com",
    "X-Forwarded-For": "203.0.113.42"
  },
  "remote_addr": "10.244.0.15:34892",
  "body": "eyJuYW1lIjoiZm9vIn0="
}
```

| Field | Type | Description |
|-------|------|-------------|
| `method` | string | HTTP method |
| `path` | string | Request path |
| `headers` | map[string]string | Request headers (filtered by `auth.header_filter`). Includes `Host`. |
| `remote_addr` | string | Direct connection IP and port |
| `body` | string | Base64-encoded request body (only when `auth.propagate_request_body: true`) |

### Response — Allow (HTTP 200)

```json
{
  "request_headers": {
    "X-User-Id": "user-123",
    "X-Tenant-Id": "acme-corp"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `request_headers` | map[string]string | Headers to inject into the upstream request |

A `200` response means the request is allowed. `request_headers` are merged into the request before proxying to the backend. Certain headers are restricted from injection (see [Authentication](authentication.md)).

### Response — Deny (non-200)

```json
{
  "status_code": 403,
  "deny_body": "{\"error\":\"forbidden\"}",
  "response_headers": {
    "X-Auth-Reason": "invalid_token"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `status_code` | int | HTTP status code to return to the client. If present, overrides the HTTP response status. |
| `deny_body` | string | Response body to return to the client |
| `response_headers` | map[string]string | Headers to include in the denial response |

---

## Events Service

Used when `events.enabled: true`. EdgeQuota sends batched usage events to this service asynchronously.

### Proto Definition

```
edgequota.events.v1.EventService/PublishEvents
```

Source: `api/proto/edgequota/events/v1/events.proto`

### Request

```json
{
  "events": [
    {
      "key": "10.0.0.1",
      "tenant_key": "acme-corp",
      "method": "GET",
      "path": "/api/v1/data",
      "allowed": true,
      "remaining": 42,
      "limit": 100,
      "timestamp": "2026-02-20T18:30:00.000Z",
      "status_code": 200,
      "request_id": "abc-123",
      "reason": ""
    }
  ]
}
```

For HTTP delivery, events are sent as a JSON array (not wrapped in an `events` field):

```json
[
  {"key":"10.0.0.1","method":"GET","path":"/api","allowed":true,...}
]
```

### Response

Any `2xx` status acknowledges the batch. Non-2xx triggers a retry.

---

## Go SDK

The [`edgequota-go`](go-sdk.md) SDK provides server-side helpers for building these services without protobuf code generation. See the [Go SDK](go-sdk.md) documentation.

---

## OpenAPI Specifications

HTTP equivalents of all three services are defined as OpenAPI 3.0 specifications:

| Service | File | Endpoint |
|---------|------|----------|
| Rate Limit | `api/openapi/ratelimit/v1/ratelimit.yaml` | `POST /limits` |
| Auth | `api/openapi/auth/v1/auth.yaml` | `POST /check` |
| Events | `api/openapi/events/v1/events.yaml` | `POST /events` |

These can be used to generate client or server code in any language using `openapi-generator` or similar tools.

---

## Admin API

EdgeQuota exposes operational endpoints on the admin server (default: `:9090`):

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/startz` | GET | Startup probe |
| `/healthz` | GET | Liveness probe |
| `/readyz` | GET | Readiness probe. Pass `?deep=true` to actively ping Redis. |
| `/metrics` | GET | Prometheus metrics |
| `/v1/config` | GET | Current sanitized configuration (secrets redacted) |
| `/v1/stats` | GET | Runtime statistics (atomic counter snapshot) |

---

## See Also

- [Rate Limiting](rate-limiting.md) -- Full protocol details for the external rate limit service.
- [Authentication](authentication.md) -- Full auth flow and security model.
- [Events](events.md) -- Event buffering, batching, and delivery.
- [Go SDK](go-sdk.md) -- Server-side helpers for implementing these services.

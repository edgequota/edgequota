# Authentication

This document describes EdgeQuota's external authentication integration, request flow, security model, and failure handling.

---

## Overview

EdgeQuota supports optional external authentication by forwarding every incoming request to an auth service **before** rate limiting and proxying. The auth service makes the allow/deny decision and can customize the denial response.

Authentication is disabled by default. Enable it with `auth.enabled: true` in the configuration.

```
Client ──► EdgeQuota ──► Auth Service
                │
                ├── 200 OK → proceed to rate limiting → proxy to backend
                │
                └── 401/403/etc → deny; forward auth response to client
```

---

## Auth Flow

### Step-by-step

1. Client sends a request to EdgeQuota.
2. EdgeQuota builds a `CheckRequest` containing the request's method, path, headers, and remote address.
3. EdgeQuota sends the `CheckRequest` to the configured auth service (HTTP or gRPC).
4. The auth service evaluates the request and returns a response.
5. If the auth service returns **HTTP 200**: the request proceeds to rate limiting and proxying.
6. If the auth service returns **any other status**: EdgeQuota writes the auth service's response back to the client and stops processing.

### Request Passthrough

The auth service receives the request context including method, path, remote address, and a filtered set of headers. By default only `Authorization` and `X-Api-Key` are forwarded (see [Header Filtering](#header-filtering) below):

```json
{
  "method": "POST",
  "path": "/api/v1/resources",
  "headers": {
    "Authorization": "Bearer eyJhbGciOi..."
  },
  "remote_addr": "10.244.0.15:34892"
}
```

This enables the auth service to implement any credential-based strategy: JWT validation, API key lookup, session verification, HMAC signature validation, or custom policies. The `Host` header is also always injected (from `r.Host`) so the auth service can make per-host decisions.

---

## HTTP Auth Backend

Configure with:

```yaml
auth:
  enabled: true
  timeout: "5s"
  failure_policy: "failclosed"      # failclosed (default) | failopen
  http:
    url: "http://auth-service:8080/check"
    forward_original_headers: false  # also send headers as X-Original-*
```

### Request

EdgeQuota sends a `POST` to the configured URL with a JSON body:

```http
POST /check HTTP/1.1
Host: auth-service:8080
Content-Type: application/json

{
  "method": "GET",
  "path": "/api/v1/users",
  "headers": {
    "Authorization": "Bearer token123",
    "X-Tenant-Id": "acme"
  },
  "remote_addr": "10.0.0.1:5555"
}
```

### Response — Allow

```http
HTTP/1.1 200 OK
```

Any 200 response means the request is allowed. The response body is ignored.

### Response — Deny

```http
HTTP/1.1 403 Forbidden
Content-Type: application/json
X-Auth-Reason: invalid_token

{"error": "token expired", "code": "AUTH_EXPIRED"}
```

The auth service controls the denial:

| Auth Response Field | EdgeQuota Behavior |
|---|---|
| HTTP status code | Forwarded to client as-is (e.g., 401, 403) |
| Response body (`deny_body`) | Written to client response body |
| Response headers (`response_headers`) | Merged into client response headers |
| `status_code` in JSON body | If present, overrides the HTTP status code |

This gives the auth service full control over the error experience the client sees.

---

## gRPC Auth Backend

Configure with:

```yaml
auth:
  enabled: true
  timeout: "5s"
  failure_policy: "failclosed"
  grpc:
    address: "auth-service:50051"
    tls:
      enabled: false
      ca_file: ""
```

### Method

```
edgequota.auth.v1.AuthService/Check
```

The gRPC client uses a **JSON codec** (not protobuf) for simplicity. The request and response structures match the HTTP JSON format exactly. This means the auth service can be implemented without protobuf code generation — any HTTP/gRPC server that accepts and returns the documented JSON is compatible.

### TLS

When `auth.grpc.tls.enabled` is `true`, the gRPC client uses TLS with the specified CA file for certificate verification. This is appropriate for cross-cluster or cross-network auth service calls.

For in-cluster pod-to-pod calls, TLS is typically unnecessary (use `enabled: false`).

See `api/proto/auth/v1/auth.proto` for the formal service definition.

---

## Security Model

### Trust Boundary

EdgeQuota sits at the edge and is the first service to inspect incoming traffic. The auth service sits behind EdgeQuota in the trusted network.

```
Untrusted ──► EdgeQuota ──► Auth Service (trusted)
              │
              └── Rate Limit ──► Backend (trusted)
```

| Component | Trust Level | Notes |
|-----------|-------------|-------|
| Client | Untrusted | All headers can be spoofed |
| EdgeQuota | Edge boundary | Enforces auth + rate limits |
| Auth service | Trusted | Makes allow/deny decisions |
| Backend | Trusted | Only receives allowed requests |

### Header Trust

EdgeQuota forwards all client headers to the auth service, including those that can be spoofed (e.g., `X-Forwarded-For`). The auth service should:

1. **Not trust `X-Forwarded-For` from clients.** Use `remote_addr` for the direct connection IP.
2. **Validate all tokens cryptographically.** Do not rely on header presence alone.
3. **Set appropriate `X-Forwarded-*` headers** if the auth service needs to distinguish between direct clients and proxied clients.

### Timeout, Failure Policy, and Circuit Breaker

| Config | Default | Description |
|--------|---------|-------------|
| `auth.timeout` | `"5s"` | Maximum time to wait for the auth service response |
| `auth.failure_policy` | `"failclosed"` | Behavior when the auth service is unreachable, times out, or the circuit breaker is open |

#### Failure Policy

| Policy | Auth unavailable | Behavior |
|--------|-----------------|----------|
| `failclosed` (default) | Yes | Deny with `503 Service Unavailable`. The safe default for security-critical deployments. |
| `failopen` | Yes | Allow the request; skip auth entirely. Use only when availability is more important than strict auth enforcement. |

When the auth service does not respond within the timeout, EdgeQuota:

1. Increments `edgequota_auth_errors_total`.
2. Applies the configured `failure_policy`.

#### Circuit Breaker

EdgeQuota includes a built-in circuit breaker for the auth service to prevent cascading timeouts when the auth backend is down:

| Parameter | Value |
|-----------|-------|
| Failure threshold | 5 consecutive errors |
| Open duration | 30 s |
| Half-open probe | 1 request allowed through |

**Lifecycle:**

1. **Closed** (normal) — all requests are forwarded to the auth service.
2. **Open** — after 5 consecutive failures, the breaker opens. Requests immediately receive the `failure_policy` result without waiting for the timeout. `edgequota_auth_errors_total` is still incremented.
3. **Half-open** — after 30 s, one probe request is forwarded. If it succeeds, the breaker closes. If it fails, the breaker re-opens for another 30 s.

The circuit breaker is transparent to clients — the `failure_policy` determines what the client sees while the breaker is open.

### Header Filtering

The auth service exists specifically to validate credentials, so EdgeQuota's default is an **allow-list** that forwards only credential headers:

| Default forwarded headers | Rationale |
|---|---|
| `Authorization` | Bearer tokens, Basic auth |
| `X-Api-Key` | API key authentication |

All other headers are **dropped by default**. This is a safe, minimal-footprint default — the auth service receives exactly what it needs and nothing else.

**Widening the default.** To forward additional headers, set an explicit `allow_list`:

```yaml
auth:
  header_filter:
    allow_list:
      - "Authorization"
      - "X-Tenant-Id"   # forwarded in addition
      - "X-Api-Key"
```

**Switching to a deny-list.** When `deny_list` is set (and `allow_list` is empty), EdgeQuota forwards all headers except those listed. This is useful when the auth service needs broad access to request context:

```yaml
auth:
  header_filter:
    deny_list:               # forward everything except these
      - "Cookie"
      - "Set-Cookie"
```

> **Note:** When `allow_list` is set, `deny_list` is ignored. When both are empty, the `DefaultAuthAllowHeaders` allow-list (`Authorization`, `X-Api-Key`) applies automatically.

### Auth Response Caching

EdgeQuota can cache allow/deny decisions in Redis so repeated requests with the same credential skip the external auth call entirely. The auth service opts in by returning cache directives — there is no implicit caching.

#### How the TTL is resolved

The same priority order as the external rate-limit service applies:

| Priority | Source | Example |
|----------|--------|---------|
| 1 | HTTP `Cache-Control: max-age=N` | Auth service sets `Cache-Control: max-age=3600` |
| 2 | HTTP `Expires: <RFC1123 date>` | Auth service sets `Expires: Thu, 01 Jan 2026 12:00:00 GMT` |
| 3 | Body field `cache_max_age_seconds` | gRPC or JSON body `"cache_max_age_seconds": 3600` |
| 4 | Body field `cache_no_store: true` | Prevent caching regardless of other fields |

**HTTP headers always win.** If `Cache-Control: max-age=600` is returned alongside `"cache_max_age_seconds": 30` in the body, EdgeQuota caches for 600 seconds.

**The auth service must explicitly opt in.** If no cache directive is present, every request hits the auth service.

#### Cache key

The cache key is a SHA-256 hash of the forwarded request headers (after filtering). Requests with different credentials produce different keys. Ephemeral headers (`X-Request-Id`, trace IDs, etc.) are excluded from the key automatically.

#### Typical use: token expiry

JWT auth services commonly set `cache_max_age_seconds` to the token's remaining TTL, so EdgeQuota caches the auth decision until the token expires:

**HTTP response:**
```http
HTTP/1.1 200 OK
Cache-Control: max-age=3543
```

**gRPC response (protobuf):**
```json
{
  "allowed": true,
  "cache_max_age_seconds": 3543
}
```

EdgeQuota logs the cache decision at `DEBUG` level on every request, including the TTL and which source drove it:

```
auth response cached  ttl=59m3s  source="Cache-Control: max-age=3543"
auth cache hit, skipping external auth call
auth response not cached  source="Cache-Control: no-store"
```

### Forward Original Headers

When `auth.http.forward_original_headers` is `true`, EdgeQuota also sends all headers with an `X-Original-` prefix alongside the normal `headers` map. This is useful for auth services that need to inspect the raw header values while EdgeQuota normalizes the primary ones.

```yaml
auth:
  http:
    url: "http://auth-service:8080/check"
    forward_original_headers: true
```

---

## Implementation Notes

### Request Building

`BuildCheckRequest(r *http.Request)` constructs the check request by extracting:

- `r.Method` → `method`
- `r.URL.Path` → `path`
- `r.Header` → `headers` (all headers, canonicalized)
- `r.RemoteAddr` → `remote_addr`

The request body is **not forwarded**. Only metadata is sent to the auth service. This avoids buffering potentially large request bodies for auth checks.

### Connection Pooling

The HTTP auth client uses Go's default `http.Client` with connection pooling. The gRPC auth client uses a persistent connection with automatic reconnection.

### Concurrency

Auth checks are fully concurrent — there is no per-request serialization. The auth service should be designed to handle the same request concurrency as EdgeQuota itself.

---

## Example Auth Service

A minimal auth service that validates Bearer tokens:

```go
package main

import (
    "encoding/json"
    "net/http"
    "strings"
)

type CheckRequest struct {
    Method     string            `json:"method"`
    Path       string            `json:"path"`
    Headers    map[string]string `json:"headers"`
    RemoteAddr string            `json:"remote_addr"`
}

func main() {
    http.HandleFunc("/check", func(w http.ResponseWriter, r *http.Request) {
        var req CheckRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }

        auth := req.Headers["Authorization"]
        if !strings.HasPrefix(auth, "Bearer ") {
            w.WriteHeader(http.StatusUnauthorized)
            json.NewEncoder(w).Encode(map[string]string{
                "error": "missing bearer token",
            })
            return
        }

        token := strings.TrimPrefix(auth, "Bearer ")
        if !validateToken(token) {
            w.WriteHeader(http.StatusForbidden)
            json.NewEncoder(w).Encode(map[string]string{
                "error": "invalid token",
            })
            return
        }

        w.WriteHeader(http.StatusOK)
    })

    http.ListenAndServe(":8080", nil)
}

func validateToken(token string) bool {
    // Implement your token validation logic here.
    return token != ""
}
```

---

## See Also

- [Configuration Reference](configuration.md) -- Full `auth` config section.
- [API Reference](api-reference.md) -- Proto and OpenAPI definitions for the auth service.
- [Go SDK](go-sdk.md) -- Server-side helpers for building auth services.
- [Security](security.md) -- Trust boundaries and header security.
- [Troubleshooting](troubleshooting.md) -- Common auth issues.

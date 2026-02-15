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

The auth service receives the full request context:

```json
{
  "method": "POST",
  "path": "/api/v1/resources",
  "headers": {
    "Authorization": "Bearer eyJhbGciOi...",
    "Content-Type": "application/json",
    "X-Forwarded-For": "203.0.113.42",
    "X-Request-Id": "abc-123"
  },
  "remote_addr": "10.244.0.15:34892"
}
```

All headers are forwarded, including:

- `Authorization` (Bearer tokens, API keys, Basic auth)
- `Cookie` (session tokens)
- `X-Forwarded-For`, `X-Real-IP` (client identity)
- Custom headers added by upstream load balancers

This enables the auth service to implement any authentication strategy: JWT validation, API key lookup, session verification, HMAC signature validation, or custom policies.

---

## HTTP Auth Backend

Configure with:

```yaml
auth:
  enabled: true
  timeout: "5s"
  http:
    url: "http://auth-service:8080/check"
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

### Timeout and Availability

| Config | Default | Description |
|--------|---------|-------------|
| `auth.timeout` | `5s` | Maximum time to wait for the auth service response |

If the auth service does not respond within the timeout, EdgeQuota treats it as an error:

- The `edgequota_auth_errors_total` metric is incremented.
- The request is denied (the auth call failure is treated as a non-200 response).

This is **fail-closed by default**: if the auth service is down, all requests are rejected. This is the safe default for a security-critical component.

### Fail-Open Considerations

EdgeQuota does not support an explicit fail-open mode for auth. If you need fail-open behavior (allow requests when auth is unavailable), implement it in the auth service itself by returning 200 on internal errors, or use a circuit breaker in front of the auth service.

The rationale: fail-open for authentication is a security risk. If the auth service is down, allowing all traffic means unauthenticated requests reach the backend. This should be an explicit, deliberate choice made by the auth service — not a default behavior.

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

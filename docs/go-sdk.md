# Go SDK (`edgequota-go`)

The `edgequota-go` module provides server-side helpers for building external rate limit, authentication, and events services that integrate with EdgeQuota. It wraps the generated HTTP and gRPC types with ergonomic Go functions.

---

## Installation

```bash
go get github.com/edgequota/edgequota-go
```

---

## Packages

| Package | Purpose |
|---------|---------|
| `ratelimit` | Helpers for building `GetLimitsResponse` in external rate limit services |
| `auth` | Helpers for building `CheckResponse` in external auth services, plus JWT utilities |
| `events` | Helpers for building `PublishEventsResponse` in events receivers |
| `gen/http/ratelimit/v1` | Generated HTTP types from the rate limit OpenAPI spec |
| `gen/http/auth/v1` | Generated HTTP types from the auth OpenAPI spec |
| `gen/http/events/v1` | Generated HTTP types from the events OpenAPI spec |
| `gen/grpc/ratelimit/v1` | Generated gRPC stubs from the rate limit proto |
| `gen/grpc/auth/v1` | Generated gRPC stubs from the auth proto |
| `gen/grpc/events/v1` | Generated gRPC stubs from the events proto |

---

## Rate Limit Helpers

The `ratelimit` package provides helpers for building responses in external rate limit services.

### `TenantLimits`

A convenience struct for defining rate limit parameters:

```go
type TenantLimits struct {
    Average int64  `json:"average"`
    Burst   int64  `json:"burst"`
    Period  string `json:"period"`
}
```

### `NewResponse`

Creates a `GetLimitsResponse` from tenant limits with a tenant key for Redis bucket isolation:

```go
import "github.com/edgequota/edgequota-go/ratelimit"

resp := ratelimit.NewResponse("acme-corp", ratelimit.TenantLimits{
    Average: 1000,
    Burst:   200,
    Period:  "1s",
})
```

### `WithCache`

Sets cache control on the response:

```go
resp = ratelimit.WithCache(resp, 300) // cache for 5 minutes
```

### `WithNoStore`

Disables caching for the response:

```go
resp = ratelimit.WithNoStore(resp)
```

### `WithBackendProtocol`

Sets a per-request backend protocol override:

```go
import rlv1http "github.com/edgequota/edgequota-go/gen/http/ratelimit/v1"

resp = ratelimit.WithBackendProtocol(resp, rlv1http.BackendProtocolH2)
```

### Example: HTTP Rate Limit Service

```go
package main

import (
    "encoding/json"
    "net/http"

    "github.com/edgequota/edgequota-go/ratelimit"
    rlv1http "github.com/edgequota/edgequota-go/gen/http/ratelimit/v1"
)

func main() {
    http.HandleFunc("/limits", func(w http.ResponseWriter, r *http.Request) {
        var req rlv1http.GetLimitsRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }

        tenantID := req.Headers["X-Tenant-Id"]

        limits := lookupTenantLimits(tenantID)
        resp := ratelimit.NewResponse(tenantID, limits)
        resp = ratelimit.WithCache(resp, 300)

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(resp)
    })

    http.ListenAndServe(":8080", nil)
}

func lookupTenantLimits(tenantID string) ratelimit.TenantLimits {
    // Look up limits from your database.
    return ratelimit.TenantLimits{
        Average: 1000,
        Burst:   200,
        Period:  "1s",
    }
}
```

---

## Auth Helpers

The `auth` package provides helpers for building responses in external auth services, plus JWT validation utilities.

### `Allow`

Returns a response that allows the request, optionally injecting headers into the upstream request:

```go
import "github.com/edgequota/edgequota-go/auth"

// Allow with injected headers
resp := auth.Allow(map[string]string{
    "X-User-Id":   "user-123",
    "X-Tenant-Id": "acme-corp",
})

// Allow without injected headers
resp := auth.Allow(nil)
```

### `Deny`

Returns a response that denies the request with a status code, body, and optional headers:

```go
resp := auth.Deny(403, `{"error":"forbidden"}`, map[string]string{
    "X-Auth-Reason": "invalid_token",
})
```

### `ExtractBearerToken`

Extracts a Bearer token from the `Authorization` header in a `CheckRequest`:

```go
token := auth.ExtractBearerToken(&req)
if token == "" {
    // No bearer token found
}
```

### `JWTValidator`

Validates HMAC-signed JWTs and extracts claims:

```go
validator := auth.NewJWTValidator("my-secret-key")

// Validate
claims, err := validator.ValidateToken(token)
if err != nil {
    // Invalid token
}

userID := claims["sub"].(string)

// Create (for testing)
token, err := validator.CreateToken(map[string]interface{}{
    "sub":    "user-123",
    "tenant": "acme-corp",
}, 24*time.Hour)
```

### Example: HTTP Auth Service

```go
package main

import (
    "encoding/json"
    "net/http"

    "github.com/edgequota/edgequota-go/auth"
    authv1http "github.com/edgequota/edgequota-go/gen/http/auth/v1"
)

func main() {
    validator := auth.NewJWTValidator("my-secret")

    http.HandleFunc("/check", func(w http.ResponseWriter, r *http.Request) {
        var req authv1http.CheckRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }

        token := auth.ExtractBearerToken(&req)
        if token == "" {
            resp := auth.Deny(401, `{"error":"missing token"}`, nil)
            w.WriteHeader(int(resp.StatusCode))
            json.NewEncoder(w).Encode(resp)
            return
        }

        claims, err := validator.ValidateToken(token)
        if err != nil {
            resp := auth.Deny(403, `{"error":"invalid token"}`, nil)
            w.WriteHeader(int(resp.StatusCode))
            json.NewEncoder(w).Encode(resp)
            return
        }

        resp := auth.Allow(map[string]string{
            "X-User-Id":   claims["sub"].(string),
            "X-Tenant-Id": claims["tenant"].(string),
        })
        json.NewEncoder(w).Encode(resp)
    })

    http.ListenAndServe(":8080", nil)
}
```

---

## Events Helpers

The `events` package provides helpers for building responses in events receivers.

### `Accepted`

Returns a response indicating all events were accepted:

```go
import "github.com/edgequota/edgequota-go/events"

resp := events.Accepted(len(receivedEvents))
```

---

## Generated Types

The `gen/` packages contain types generated from the OpenAPI and proto definitions. These are the wire types used by EdgeQuota.

### HTTP Types (oapi-codegen)

| Package | Key Types |
|---------|-----------|
| `gen/http/ratelimit/v1` | `GetLimitsRequest`, `GetLimitsResponse`, `BackendProtocol` |
| `gen/http/auth/v1` | `CheckRequest`, `CheckResponse` |
| `gen/http/events/v1` | `PublishEventsRequest`, `PublishEventsResponse` |

### gRPC Types (protoc)

| Package | Key Types |
|---------|-----------|
| `gen/grpc/ratelimit/v1` | `RateLimitServiceClient`, `GetLimitsRequest`, `GetLimitsResponse` |
| `gen/grpc/auth/v1` | `AuthServiceClient`, `CheckRequest`, `CheckResponse` |
| `gen/grpc/events/v1` | `EventServiceClient`, `PublishEventsRequest`, `PublishEventsResponse` |

---

## Template Project

The [`external-auth-template`](https://github.com/edgequota/external-auth-template) repository provides a complete starter project for building an external auth service with both HTTP and gRPC support. It includes:

- HTTP and gRPC server setup
- JWT validation example
- Docker build
- Tests
- EdgeQuota configuration examples

---

## See Also

- [API Reference](api-reference.md) -- Full request/response schemas.
- [Rate Limiting](rate-limiting.md) -- External rate limit service protocol details.
- [Authentication](authentication.md) -- Auth service flow and security model.
- [Events](events.md) -- Events service protocol and delivery semantics.

# Events

EdgeQuota can emit rate-limit decisions as structured usage events to an external HTTP or gRPC service. Events are batched, buffered, and sent asynchronously — they never block the request hot path.

Events enable downstream analytics, billing, auditing, and anomaly detection without coupling these concerns to the proxy itself.

---

## Overview

```
Client ──► EdgeQuota ──► Backend
               │
               └──► Events Buffer ──► (batch) ──► Events Receiver
                                                   (HTTP or gRPC)
```

Every request that passes through the rate-limit stage produces a `UsageEvent`. Events are written to an in-memory ring buffer and flushed in batches to the configured receiver. If the receiver is down, events are retried with exponential backoff and eventually dropped after exhausting retries.

---

## Configuration

### Enabling Events

```yaml
events:
  enabled: true
  http:
    url: "https://events-receiver.internal/ingest"
```

### Full Configuration

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_EVENTS_ENABLED` | Enable usage event emission |
| `batch_size` | int | `100` | `EDGEQUOTA_EVENTS_BATCH_SIZE` | Number of events per batch |
| `flush_interval` | duration | `"5s"` | `EDGEQUOTA_EVENTS_FLUSH_INTERVAL` | Maximum time between flushes |
| `buffer_size` | int | `10000` | `EDGEQUOTA_EVENTS_BUFFER_SIZE` | Ring buffer capacity. Oldest events are dropped on overflow. |
| `max_retries` | int | `3` | `EDGEQUOTA_EVENTS_MAX_RETRIES` | Number of send attempts per batch before giving up |
| `retry_backoff` | duration | `"100ms"` | `EDGEQUOTA_EVENTS_RETRY_BACKOFF` | Initial retry backoff (doubles on each attempt) |

### HTTP Receiver

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `http.url` | string | `""` | `EDGEQUOTA_EVENTS_HTTP_URL` | URL of the HTTP events receiver (receives POST with JSON array) |
| `http.headers` | map[string]string | `{}` | — | Custom headers attached to every request. Values are redacted in logs. |

### gRPC Receiver

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `grpc.address` | string | `""` | `EDGEQUOTA_EVENTS_GRPC_ADDRESS` | Address of the gRPC events service |
| `grpc.tls.enabled` | bool | `false` | `EDGEQUOTA_EVENTS_GRPC_TLS_ENABLED` | Enable TLS for gRPC events |
| `grpc.tls.ca_file` | string | `""` | `EDGEQUOTA_EVENTS_GRPC_TLS_CA_FILE` | CA certificate file for gRPC TLS verification |

### Header Filtering

Events include request headers for context. Control which headers are forwarded:

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `header_filter.allow_list` | []string | `[]` | `EDGEQUOTA_EVENTS_HEADER_FILTER_ALLOW_LIST` | Exclusive: only include these headers |
| `header_filter.deny_list` | []string | `[]` | `EDGEQUOTA_EVENTS_HEADER_FILTER_DENY_LIST` | Never include these headers |

When neither is set, the default sensitive headers (`Authorization`, `Cookie`, `X-Api-Key`, etc.) are automatically stripped from events.

---

## Custom HTTP Headers

The `headers` map allows you to attach arbitrary headers to every HTTP events request. Common use cases:

- **Authentication**: `Authorization: Bearer <token>`, `X-Api-Key: <key>`
- **Routing**: `X-Destination: analytics`, `X-Environment: production`
- **Correlation**: `X-Source: edgequota`

```yaml
events:
  enabled: true
  http:
    url: "https://events-receiver.internal/ingest"
    headers:
      Authorization: "Bearer eyJhbGciOi..."
      X-Source: "edgequota"
      X-Environment: "production"
```

### Security Restrictions

The following headers are rejected at startup to prevent breaking HTTP transport semantics:

| Restricted Headers |
|-------------------|
| `Host` |
| `Content-Type` |
| `Content-Length` |
| `Transfer-Encoding` |
| `Connection` |
| `Te` |
| `Upgrade` |
| `Proxy-Authorization` |
| `Proxy-Connection` |
| `Keep-Alive` |
| `Trailer` |

`Content-Type` is always set to `application/json` by EdgeQuota and cannot be overridden.

### Redaction

Header values are stored using `RedactedString`, so they are masked in:

- Structured log output
- The `/v1/config` admin endpoint
- Debug output and JSON serialization

This makes it safe to store tokens and API keys in the `headers` map.

### Deprecated Fields

The following fields are deprecated and will be removed in a future release:

| Field | Type | Env Var | Description |
|-------|------|---------|-------------|
| `http.auth_token` | string | `EDGEQUOTA_EVENTS_HTTP_AUTH_TOKEN` | Migrated to `headers[auth_header]` with `Bearer ` prefix |
| `http.auth_header` | string | `EDGEQUOTA_EVENTS_HTTP_AUTH_HEADER` | Header name for `auth_token`. Defaults to `Authorization`. |

When `auth_token` is set and the equivalent header is not already present in `headers`, EdgeQuota automatically migrates it at startup:

```yaml
# Deprecated:
events:
  http:
    auth_token: "my-secret"
    auth_header: "X-Api-Key"

# Equivalent (preferred):
events:
  http:
    headers:
      X-Api-Key: "Bearer my-secret"
```

If both `auth_token` and an explicit `headers` entry for the same header name exist, the explicit `headers` entry takes precedence.

---

## Event Schema

Each event is a JSON object representing a single rate-limit decision:

```json
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
  "request_id": "abc-123-def",
  "reason": ""
}
```

| Field | Type | Description |
|-------|------|-------------|
| `key` | string | Extracted rate-limit key (IP, header value, or composite) |
| `tenant_key` | string | Custom tenant key from the external rate limit service (empty if not set) |
| `method` | string | HTTP method |
| `path` | string | Request path |
| `allowed` | bool | Whether the request passed rate limiting |
| `remaining` | int64 | Tokens remaining in the bucket after this request |
| `limit` | int64 | Bucket capacity (burst) |
| `timestamp` | string | RFC 3339 timestamp |
| `status_code` | int | HTTP status code returned to the client |
| `request_id` | string | `X-Request-Id` for correlation and deduplication |
| `reason` | string | Non-empty for anomaly events (e.g., `"tenant_key_rejected"`) |

### HTTP Delivery

Events are sent as a `POST` request with a JSON array body:

```http
POST /ingest HTTP/1.1
Host: events-receiver.internal
Content-Type: application/json
Authorization: Bearer eyJhbGciOi...

[
  {"key":"10.0.0.1","method":"GET","path":"/api","allowed":true,...},
  {"key":"10.0.0.2","method":"POST","path":"/api","allowed":false,...}
]
```

The receiver should return `2xx` to acknowledge the batch. Any other status triggers a retry.

### gRPC Delivery

Events are sent via `edgequota.events.v1.EventService/PublishEvents` using a JSON codec. See [API Reference](api-reference.md) for the formal service definition.

---

## Buffering and Batching

Events are stored in a fixed-size ring buffer and flushed to the receiver in batches:

```
Request → UsageEvent → Ring Buffer → [batch_size events or flush_interval] → HTTP/gRPC POST
```

| Parameter | Default | Behavior |
|-----------|---------|----------|
| `buffer_size` | 10,000 | Maximum events in the ring buffer. When full, the oldest events are overwritten. |
| `batch_size` | 100 | Events per batch. A flush sends up to `batch_size` events at a time. |
| `flush_interval` | 5s | Maximum time between flushes, even if `batch_size` is not reached. |

### Buffer Overflow

When the ring buffer is full, new events overwrite the oldest entries. This is a deliberate design choice — events are fire-and-forget and must never block the request hot path.

When events are dropped, EdgeQuota increments:

- The `edgequota_events_dropped_total` Prometheus counter
- A rate-limited warning log (once per flush interval)

### Retry Logic

When a send fails, the batch is retried with exponential backoff:

| Attempt | Backoff |
|---------|---------|
| 1 | `retry_backoff` (default: 100ms) |
| 2 | `retry_backoff * 2` (200ms) |
| 3 | `retry_backoff * 4` (400ms) |

After `max_retries` attempts, the batch is discarded and `edgequota_events_send_failures_total` is incremented.

---

## Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `edgequota_events_dropped_total` | Counter | Events dropped due to ring buffer overflow |
| `edgequota_events_send_failures_total` | Counter | Event batches that failed after all retries |

---

## Anomaly Events

EdgeQuota emits special events with a non-empty `reason` field for operational anomalies:

| Reason | When |
|--------|------|
| `tenant_key_rejected` | The external rate limit service returned a `tenant_key` that failed validation (length > 256 or invalid characters) |

These events appear alongside normal usage events in the same stream, enabling downstream systems to alert on external service misbehavior.

---

## Example Receiver

A minimal HTTP events receiver in Go:

```go
package main

import (
    "encoding/json"
    "fmt"
    "net/http"
)

type UsageEvent struct {
    Key       string `json:"key"`
    TenantKey string `json:"tenant_key,omitempty"`
    Method    string `json:"method"`
    Path      string `json:"path"`
    Allowed   bool   `json:"allowed"`
    Remaining int64  `json:"remaining"`
    Limit     int64  `json:"limit"`
    Timestamp string `json:"timestamp"`
    StatusCode int   `json:"status_code"`
    RequestID string `json:"request_id,omitempty"`
    Reason    string `json:"reason,omitempty"`
}

func main() {
    http.HandleFunc("/ingest", func(w http.ResponseWriter, r *http.Request) {
        var events []UsageEvent
        if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }

        for _, e := range events {
            fmt.Printf("key=%s allowed=%v remaining=%d path=%s\n",
                e.Key, e.Allowed, e.Remaining, e.Path)
        }

        w.WriteHeader(http.StatusOK)
    })

    http.ListenAndServe(":8081", nil)
}
```

---

## See Also

- [Configuration Reference](configuration.md) -- Full `events` config section.
- [API Reference](api-reference.md) -- Proto and OpenAPI definitions for the events service.
- [Observability](observability.md) -- Events-related Prometheus metrics.

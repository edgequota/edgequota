# Observability

This document describes EdgeQuota's Prometheus metrics, health endpoints, structured logging, OpenTelemetry tracing, and suggested Grafana dashboards.

---

## Prometheus Metrics

Metrics are exposed on the admin server at `GET :9090/metrics` in Prometheus exposition format.

### Counters

| Metric | Description | When Incremented |
|--------|-------------|-----------------|
| `edgequota_requests_allowed_total` | Requests that passed rate limiting | Every request allowed by the token bucket |
| `edgequota_requests_limited_total` | Requests rejected by rate limiting | Every 429 response from rate limiting |
| `edgequota_redis_errors_total` | Redis operation errors | Redis connection failures, timeouts, command errors |
| `edgequota_fallback_used_total` | In-memory fallback activations | Every request handled by the in-memory limiter when Redis is down |
| `edgequota_auth_errors_total` | Auth service call errors | Auth service timeout, connection error, or non-HTTP error |
| `edgequota_auth_denied_total` | Requests denied by auth service | Auth service returns non-200 status |
| `edgequota_key_extract_errors_total` | Key extraction failures | Missing required header, malformed remote address |
| `edgequota_tenant_requests_allowed_total` | Per-tenant allowed requests | Label: `tenant`. Capped at `max_tenant_labels`; excess tenants aggregated under `__overflow__`. |
| `edgequota_tenant_requests_limited_total` | Per-tenant rate-limited requests | Label: `tenant`. Same cardinality cap as above. |
| `edgequota_tenant_label_overflow_total` | Tenant label cap exceeded | A request's tenant was bucketed under `__overflow__` |
| `edgequota_tenant_key_rejected_total` | Tenant key validation failures | External RL service returned an invalid `tenant_key` (length > 256 or invalid characters) |
| `edgequota_events_dropped_total` | Events dropped from ring buffer | Ring buffer overflow; oldest events overwritten |
| `edgequota_events_send_failures_total` | Event batch send failures | Batch failed to send after all retries |
| `edgequota_concurrent_requests_rejected_total` | Concurrency limit rejections | `max_concurrent_requests` exceeded; request received 503 |
| `edgequota_external_rl_semaphore_rejected_total` | External RL semaphore rejections | `max_concurrent_requests` for external RL calls exceeded |
| `edgequota_external_rl_singleflight_shared_total` | Singleflight shared results | A concurrent cache miss shared the result of another in-flight external call |

### Gauges

| Metric | Description |
|--------|-------------|
| `edgequota_redis_healthy` | Whether Redis is reachable: `1` = healthy, `0` = unhealthy. Updated by the recovery loop. |

### Histograms

| Metric | Labels | Buckets | Description |
|--------|--------|---------|-------------|
| `edgequota_request_duration_seconds` | `method`, `status_code` | 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5, 10 | End-to-end request latency including auth, rate limit check, and proxy time |
| `edgequota_auth_duration_seconds` | — | 0.0005–5s | Latency of external auth checks |
| `edgequota_external_rl_duration_seconds` | — | 0.0005–5s | Latency of external rate limit service calls |
| `edgequota_backend_duration_seconds` | — | 0.001–30s | Latency of backend proxy calls |
| `edgequota_ratelimit_remaining_tokens` | — | 0–1000 | Distribution of remaining tokens across rate-limit checks |

### Go Runtime Metrics

The admin server also exposes standard Go and process metrics via `collectors.NewGoCollector()` and `collectors.NewProcessCollector()`:

| Metric | Description |
|--------|-------------|
| `go_goroutines` | Number of active goroutines |
| `go_memstats_alloc_bytes` | Current heap allocation |
| `go_gc_duration_seconds` | GC pause duration |
| `process_cpu_seconds_total` | Total CPU time |
| `process_resident_memory_bytes` | Resident memory size |
| `process_open_fds` | Open file descriptors |

### Scrape Configuration

For Kubernetes-native scraping, add annotations to the pod:

```yaml
metadata:
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "9090"
    prometheus.io/path: "/metrics"
```

For Prometheus Operator, use a ServiceMonitor:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: edgequota
  labels:
    release: prometheus
spec:
  selector:
    matchLabels:
      app: edgequota
  endpoints:
    - port: admin
      path: /metrics
      interval: 15s
```

---

## Health Probes

All health endpoints are served on the admin port (`:9090`) and return JSON responses.

### `GET /startz` — Startup Probe

| State | Status | Body |
|-------|--------|------|
| Initializing | 503 | `{"status":"not_started"}` |
| Started | 200 | `{"status":"started"}` |

The startup probe gates liveness and readiness checks in Kubernetes. It transitions to 200 once the server has completed initialization (listeners bound, Redis connected).

### `GET /healthz` — Liveness Probe

| State | Status | Body |
|-------|--------|------|
| Alive | 200 | `{"status":"alive"}` |

Always returns 200 while the process is running. If this probe fails, Kubernetes restarts the pod.

### `GET /readyz` — Readiness Probe

| State | Status | Body |
|-------|--------|------|
| Ready | 200 | `{"status":"ready"}` |
| Draining | 503 | `{"status":"not_ready"}` |

Returns 200 when the instance is accepting traffic. During graceful shutdown, it transitions to 503, which causes Kubernetes to remove the pod from Service endpoints.

#### Deep Health Check: `GET /readyz?deep=true`

When `?deep=true` is passed, the readiness probe actively pings Redis before responding. This verifies end-to-end connectivity to the rate-limit backing store, not just process liveness.

| State | Status | Body |
|-------|--------|------|
| Redis reachable | 200 | `{"status":"ready"}` |
| Redis unreachable | 503 | `{"status":"not_ready","redis":"unreachable"}` |

Use `?deep=true` for external health checks (load balancer, monitoring) but **not** for the Kubernetes readiness probe — frequent deep checks under load can cause unnecessary probe failures during Redis latency spikes.

---

## Access Logs

When `logging.access_log_enabled` is `true` (the default), EdgeQuota emits a structured log line for every proxied request at `INFO` level.

### Access Log Fields

| Field | Type | Description |
|-------|------|-------------|
| `msg` | string | Always `"access"` |
| `method` | string | HTTP method |
| `path` | string | Request path |
| `status` | int | HTTP status code returned to the client |
| `duration_ms` | float64 | Request duration in milliseconds |
| `bytes` | int64 | Response body bytes written |
| `remote_addr` | string | Client remote address (IP:port) |
| `request_id` | string | `X-Request-Id` (generated or client-supplied) |
| `user_agent` | string | Client User-Agent header |
| `proto` | string | HTTP protocol version (e.g., `HTTP/1.1`, `HTTP/2.0`, `HTTP/3.0`) |
| `originalHost` | string | Original `Host` header from the client request |
| `backend_url` | string | Backend URL the request was forwarded to |
| `backend_protocol` | string | Protocol used for the backend connection (`h1`, `h2`, `h3`) |
| `rl_key` | string | Rate-limit key used for this request |
| `tenant_id` | string | Tenant key (when set by the external rate limit service) |

### Example Access Log (JSON)

```json
{
  "time": "2026-02-20T18:28:06.115Z",
  "level": "INFO",
  "msg": "access",
  "method": "GET",
  "path": "/v1/menus/test-menu-sushi/published",
  "status": 200,
  "duration_ms": 3109,
  "bytes": 23055,
  "remote_addr": "2.36.228.80:61362",
  "request_id": "301fd0b3012663ed731769bdb07e4535",
  "user_agent": "curl/8.18.0",
  "proto": "HTTP/3.0",
  "originalHost": "api.example.com",
  "backend_url": "http://backend:8080",
  "backend_protocol": "h1",
  "rl_key": "acme-corp",
  "tenant_id": "acme-corp"
}
```

---

## Structured Logging

EdgeQuota uses Go's `log/slog` for structured logging.

### Configuration

```yaml
logging:
  level: "info"     # debug | info | warn | error
  format: "json"    # json | text
```

### Output Format (JSON)

```json
{
  "time": "2025-01-15T10:30:45.123Z",
  "level": "INFO",
  "msg": "server started",
  "address": ":8080",
  "version": "v1.2.0"
}
```

### Output Format (Text)

```
time=2025-01-15T10:30:45.123Z level=INFO msg="server started" address=:8080 version=v1.2.0
```

### Log Levels

| Level | When to Use |
|-------|-------------|
| `debug` | Detailed per-request tracing, Redis commands, transport selection |
| `info` | Server lifecycle events, configuration summary, connection state changes |
| `warn` | Redis reconnection attempts, deprecated configuration, non-fatal errors |
| `error` | Auth service failures, proxy errors, unrecoverable configuration errors |

### Production Recommendation

- **Level:** `info` (default). Use `debug` only for troubleshooting.
- **Format:** `json` (default). Machine-parseable for log aggregation systems.

---

## OpenTelemetry Tracing

EdgeQuota supports distributed tracing via OpenTelemetry with OTLP HTTP export.

### Configuration

```yaml
tracing:
  enabled: true
  endpoint: "http://otel-collector:4318"
  service_name: "edgequota"
  sample_rate: 0.1    # Sample 10% of requests
```

### Trace Attributes

| Attribute | Value |
|-----------|-------|
| `service.name` | Configured `service_name` |
| `service.version` | Binary version (set via `-ldflags`) |

### Sampling

EdgeQuota uses a **parent-based trace ID ratio sampler**:

- If the incoming request has a trace context (e.g., from an upstream proxy), the sampling decision is inherited.
- If there is no parent trace, the configured `sample_rate` is used.

### Production Recommendation

- **Sample rate:** 0.01–0.1 (1–10%) for high-traffic services.
- **Endpoint:** Point to an OpenTelemetry Collector, not directly to a tracing backend.
- **Cost:** Higher sample rates increase storage and network costs.

---

## Suggested Grafana Dashboards

### Dashboard 1: Request Overview

**Purpose:** High-level traffic view.

| Panel | Query | Visualization |
|-------|-------|---------------|
| Requests/sec | `rate(edgequota_requests_allowed_total[5m]) + rate(edgequota_requests_limited_total[5m])` | Time series |
| Allow/Deny ratio | `rate(edgequota_requests_allowed_total[5m])` vs `rate(edgequota_requests_limited_total[5m])` | Stacked time series |
| Rate limit hit ratio | `rate(edgequota_requests_limited_total[5m]) / (rate(edgequota_requests_allowed_total[5m]) + rate(edgequota_requests_limited_total[5m]))` | Gauge (0–100%) |
| P50/P95/P99 latency | `histogram_quantile(0.5, rate(edgequota_request_duration_seconds_bucket[5m]))` | Time series |
| Error rate | `rate(edgequota_redis_errors_total[5m]) + rate(edgequota_auth_errors_total[5m])` | Time series |

### Dashboard 2: Redis Health

**Purpose:** Redis connectivity and performance.

| Panel | Query | Visualization |
|-------|-------|---------------|
| Redis errors/sec | `rate(edgequota_redis_errors_total[5m])` | Time series |
| Fallback activations/sec | `rate(edgequota_fallback_used_total[5m])` | Time series |
| Fallback active (boolean) | `edgequota_fallback_used_total > bool 0` | Stat |
| Redis connection pool | `go_goroutines` (as a proxy for connection activity) | Time series |

### Dashboard 3: Auth Service

**Purpose:** External auth service health.

| Panel | Query | Visualization |
|-------|-------|---------------|
| Auth errors/sec | `rate(edgequota_auth_errors_total[5m])` | Time series |
| Auth denials/sec | `rate(edgequota_auth_denied_total[5m])` | Time series |
| Auth error ratio | `rate(edgequota_auth_errors_total[5m]) / (rate(edgequota_requests_allowed_total[5m]) + rate(edgequota_requests_limited_total[5m]))` | Gauge |

### Dashboard 4: Infrastructure

**Purpose:** Resource consumption and runtime health.

| Panel | Query | Visualization |
|-------|-------|---------------|
| CPU usage | `rate(process_cpu_seconds_total[5m])` | Time series |
| Memory usage | `process_resident_memory_bytes` | Time series |
| Goroutines | `go_goroutines` | Time series |
| GC pause | `rate(go_gc_duration_seconds_sum[5m])` | Time series |
| Open file descriptors | `process_open_fds` | Time series |
| Key extraction errors | `rate(edgequota_key_extract_errors_total[5m])` | Time series |

---

## Alerting Rules

Suggested Prometheus alerting rules for production:

```yaml
groups:
  - name: edgequota
    rules:
      - alert: EdgeQuotaHighRateLimitRatio
        expr: |
          rate(edgequota_requests_limited_total[5m])
          / (rate(edgequota_requests_allowed_total[5m]) + rate(edgequota_requests_limited_total[5m]))
          > 0.5
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "More than 50% of requests are being rate-limited"

      - alert: EdgeQuotaRedisErrors
        expr: rate(edgequota_redis_errors_total[5m]) > 0
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "EdgeQuota is experiencing Redis errors"

      - alert: EdgeQuotaFallbackActive
        expr: rate(edgequota_fallback_used_total[1m]) > 0
        for: 1m
        labels:
          severity: warning
        annotations:
          summary: "EdgeQuota is using in-memory fallback (Redis may be down)"

      - alert: EdgeQuotaAuthErrors
        expr: rate(edgequota_auth_errors_total[5m]) > 0
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "EdgeQuota auth service is returning errors"

      - alert: EdgeQuotaHighLatency
        expr: |
          histogram_quantile(0.99, rate(edgequota_request_duration_seconds_bucket[5m])) > 1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "EdgeQuota P99 latency exceeds 1 second"

      - alert: EdgeQuotaEventsDropped
        expr: rate(edgequota_events_dropped_total[5m]) > 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "EdgeQuota is dropping usage events (buffer overflow)"

      - alert: EdgeQuotaConcurrencyRejections
        expr: rate(edgequota_concurrent_requests_rejected_total[1m]) > 0
        for: 1m
        labels:
          severity: warning
        annotations:
          summary: "EdgeQuota is rejecting requests due to concurrency limit"
```

---

## Admin API

The admin server exposes operational endpoints alongside health probes and metrics:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/config` | GET | Current sanitized configuration. Secrets (`RedactedString` fields) are masked. |
| `/v1/stats` | GET | Runtime statistics: atomic counter snapshot (allowed, limited, redis errors, etc.). |

### Example: Query Stats

```bash
curl http://localhost:9090/v1/stats | jq .
```

```json
{
  "allowed": 15234,
  "limited": 42,
  "redis_errors": 0,
  "fallback_used": 0,
  "readonly_retries": 0,
  "key_extract_errors": 0,
  "auth_errors": 0,
  "auth_denied": 3
}
```

---

## See Also

- [Configuration Reference](configuration.md) -- Logging and tracing config fields.
- [Deployment](deployment.md) -- Probe configuration and Kubernetes setup.
- [Events](events.md) -- Events-related metrics.
- [Troubleshooting](troubleshooting.md) -- Debugging with metrics and probes.

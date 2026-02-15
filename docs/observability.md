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

### Histograms

| Metric | Labels | Buckets | Description |
|--------|--------|---------|-------------|
| `edgequota_request_duration_seconds` | `method`, `status_code` | Default Prometheus buckets (0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10) | End-to-end request latency including auth, rate limit check, and proxy time |

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
```

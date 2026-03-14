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
| `edgequota_external_rl_cache_hits_total` | External RL cache hits | External RL lookup served from fresh Redis cache |
| `edgequota_external_rl_cache_misses_total` | External RL cache misses | External RL lookup required a service call (cache miss) |
| `edgequota_external_rl_cache_stale_hits_total` | External RL stale cache hits | External RL lookup served from stale cache (circuit open, semaphore full, or fetch error) |
| `edgequota_response_cache_hits_total` | Response cache hits | Request served from the CDN-style response cache |
| `edgequota_response_cache_misses_total` | Response cache misses | Request missed the cache and was proxied to the backend |
| `edgequota_response_cache_stale_hits_total` | Response cache stale hits | Stale cache entry revalidated via conditional request (304 Not Modified) |
| `edgequota_response_cache_stores_total` | Response cache stores | Backend response stored in cache |
| `edgequota_response_cache_skips_total` | Response cache skips | Response skipped from caching (no-store, too large, streaming, non-cacheable status) |
| `edgequota_response_cache_purges_total` | Response cache purge operations | Purge executed (by key or by tag) |

### Gauges

| Metric | Description |
|--------|-------------|
| `edgequota_redis_healthy` | Whether Redis is reachable: `1` = healthy, `0` = unhealthy. Updated by the recovery loop. |
| `edgequota_build_info` | Build information (value always 1). Labels: `version`, `goversion`. |
| `edgequota_config_last_reload_timestamp_seconds` | Unix timestamp of the last successful config reload. |
| `edgequota_tls_last_reload_timestamp_seconds` | Unix timestamp of the last successful TLS certificate reload. |
| `edgequota_mtls_ca_last_reload_timestamp_seconds` | Unix timestamp of the last successful mTLS CA certificate reload. |

### Histograms

| Metric | Labels | Buckets | Description |
|--------|--------|---------|-------------|
| `edgequota_request_duration_seconds` | `method`, `status_code` | 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5, 10 | End-to-end request latency including auth, rate limit check, and proxy time |
| `edgequota_auth_duration_seconds` | — | 0.0005–5s | Latency of external auth checks |
| `edgequota_external_rl_duration_seconds` | — | 0.0005–5s | Latency of external rate limit service calls |
| `edgequota_backend_duration_seconds` | — | 0.001–30s | Latency of backend proxy calls |
| `edgequota_ratelimit_remaining_tokens` | — | 0–1000 | Distribution of remaining tokens across rate-limit checks |
| `edgequota_response_cache_body_size_bytes` | — | 100–10M | Distribution of cached response body sizes |

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

EdgeQuota has full OpenTelemetry support: it extracts W3C `traceparent`/`tracestate` headers from every incoming request, creates a root span, and propagates the trace context to all downstream calls (auth service, external RL service, Redis). Spans are exported via OTLP using either gRPC (default) or HTTP transport.

### Trace Context Propagation

The W3C TraceContext propagator is **always active**, regardless of whether tracing export is enabled. This means:

- Incoming `traceparent`/`tracestate` headers are extracted and attached to the request context.
- All outgoing HTTP/gRPC calls (auth, external RL, backend) carry the propagated trace context.
- `traceparent`/`tracestate` are excluded from auth and external RL cache keys so they do not defeat caching.

If you want EdgeQuota to participate in an existing trace from an upstream proxy or API gateway, simply ensure the upstream sets `traceparent` — EdgeQuota will inherit the sampling decision and attach all its spans as children.

### Configuration

```yaml
tracing:
  enabled: true
  protocol: "grpc"                  # "grpc" (default) or "http"
  endpoint: "otel-collector:4317"   # grpc: host:port | http: http://host:4318
  insecure: true                    # plaintext (no TLS)
  service_name: "edgequota"
  sample_rate: 0.1    # Sample 10% of requests when no upstream trace is present
  level: "external"   # Instrumentation depth: basic | external | full
```

### Instrumentation Levels

The `level` field controls how many spans EdgeQuota generates per request:

| Level | Spans created |
|-------|--------------|
| `basic` | One root span per request: `edgequota.request` |
| `external` *(default)* | Root span + child spans for auth (`edgequota.auth.http`) and external RL (`edgequota.external_rl.http`) HTTP calls, plus gRPC spans (always on via `otelgrpc`) |
| `full` | Everything in `external` + child spans for every Redis operation: `edgequota.redis.ratelimit`, `edgequota.redis.auth_cache_get`, `edgequota.redis.auth_cache_set` |

The default (`external`) is a good balance between visibility and overhead. Use `full` only when debugging Redis latency or cache behaviour.

### Span Inventory

| Span name | Level | Description |
|-----------|-------|-------------|
| `edgequota.request` | basic+ | Root span — wraps the entire request pipeline |
| `edgequota.auth` | basic+ | Auth check decision (allowed/denied) |
| `edgequota.external_rl` | basic+ | External rate limit fetch (includes cache lookup) |
| `edgequota.proxy` | basic+ | Backend proxy call |
| `edgequota.auth.http` | external+ | Outgoing HTTP call to auth service |
| `edgequota.external_rl.http` | external+ | Outgoing HTTP call to external RL service |
| `edgequota.redis.ratelimit` | full | Redis Lua script execution for token bucket |
| `edgequota.redis.auth_cache_get` | full | Redis GET for auth response cache lookup |
| `edgequota.redis.auth_cache_set` | full | Redis SET when caching a fresh auth response |

> **gRPC note:** Auth and external RL gRPC transports are always instrumented via `otelgrpc` regardless of level, because the gRPC stats handler is part of the dial options and cannot be selectively disabled.

### Trace Attributes

| Attribute | Value |
|-----------|-------|
| `service.name` | Configured `service_name` |
| `service.version` | Binary version (set via `-ldflags`) |
| `tenant_key` | Tenant identifier (on `edgequota.proxy` span) |
| `rate_limit.allowed` | Whether the request was allowed (on `edgequota.proxy` span) |
| `rate_limit.remaining` | Remaining tokens in bucket (on `edgequota.proxy` span) |
| `db.system` | `"redis"` (on Redis spans) |

### Sampling

EdgeQuota uses a **parent-based trace ID ratio sampler**:

- If the incoming request has a trace context (e.g., from an upstream proxy), the sampling decision is inherited.
- If there is no parent trace, the configured `sample_rate` is used.

### Production Recommendation

- **Level:** `external` (default). Use `full` only for debugging Redis performance.
- **Sample rate:** 0.01–0.1 (1–10%) for high-traffic services.
- **Endpoint:** Point to an OpenTelemetry Collector, not directly to a tracing backend.
- **Cost:** Higher sample rates and `full` level increase storage and network costs.

---

## Grafana Dashboard

A comprehensive, importable Grafana dashboard is available at [`deploy/grafana/edgequota-dashboard.json`](../deploy/grafana/edgequota-dashboard.json).

**Sections:**

| Row                  | Panels                                                                                   |
| -------------------- | ---------------------------------------------------------------------------------------- |
| Overview | Request rate, 5xx error rate, P95 latency, rate-limited %, Redis health, cache hit rate |
| Traffic | Request rate by status code, allowed vs rate-limited, by method, by pod |
| Latency | E2E P50/P95/P99, breakdown (auth/ext-RL/backend), backend P50/P95/P99, heatmap |
| Rate Limiting | Remaining tokens distribution, top tenants, top rate-limited tenants, key errors |
| Redis & Fallback | Redis errors/sec, fallback/sec, health by pod |
| Auth & External RL | Auth errors/denials, auth latency, ext-RL cache performance, ext-RL concurrency, ext-RL latency |
| Response Cache | Cache operations/sec, hit ratio, body size, purges |
| Events | Events dropped/sec, send failures/sec |
| System Resources | CPU, memory (RSS + heap), goroutines, GC, file descriptors, network I/O |
| Config & TLS Reloads | Time since last config/TLS/mTLS-CA reload, build info table |

**Template variables:** `$datasource`, `$namespace`, `$pod`, `$rate_interval`

**Annotations:** Config reloads and version rollouts are automatically annotated on all panels.

> **Traffic by path:** Per-path metrics are intentionally omitted to avoid unbounded label cardinality. Use access logs (structured JSON with `path` field) with a log aggregation system (Loki, Elasticsearch) for per-path traffic analysis.
>
> **Redis cluster latency:** EdgeQuota exposes Redis health and error counts but not per-cluster latency. For detailed Redis observability, deploy a dedicated redis-exporter alongside each Redis cluster.

---

## Alerting Rules

Production-ready Prometheus alerting rules are available at [`deploy/prometheus/alerts.yaml`](../deploy/prometheus/alerts.yaml).

| Alert | Severity | Condition |
|-------|----------|-----------|
| EdgeQuotaHighErrorRate | critical | 5xx error rate > 5% for 5m |
| EdgeQuotaTrafficSpike | warning | Current rate > 2x the rate 1 hour ago |
| EdgeQuotaNoTraffic | critical | Zero requests for 10m while pods are running |
| EdgeQuotaHighP99Latency | warning/critical | P99 > 2s (warning), > 5s (critical) |
| EdgeQuotaBackendLatencyHigh | warning | Backend P95 > 5s |
| EdgeQuotaHighRateLimitRatio | warning | > 50% of requests rate-limited |
| EdgeQuotaConcurrencyRejections | warning | Requests rejected by concurrency limit |
| EdgeQuotaRedisUnhealthy | critical | `edgequota_redis_healthy == 0` |
| EdgeQuotaRedisErrors | warning | Redis errors > 1/s |
| EdgeQuotaFallbackActive | warning | In-memory fallback in use |
| EdgeQuotaAuthErrors | critical | Auth service returning errors |
| EdgeQuotaAuthLatencyHigh | warning | Auth P95 > 1s |
| EdgeQuotaEventsDropped | warning | Events dropped from ring buffer |
| EdgeQuotaEventSendFailures | warning | Event batches failing to send |
| EdgeQuotaLowCacheHitRate | warning | Cache hit rate < 30% |
| EdgeQuotaHighMemory | warning | RSS > 85% of memory limit |
| EdgeQuotaHighGoroutines | warning | Goroutine count > 10,000 |
| EdgeQuotaConfigReloadStale | info | No config reload in 24 hours |

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

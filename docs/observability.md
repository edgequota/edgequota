# Observability

This document describes EdgeQuota's OpenTelemetry metrics, health endpoints, structured logging, OpenTelemetry tracing, and suggested dashboards.

---

## Metrics

EdgeQuota emits **portable OpenTelemetry metrics** and **pushes them over OTLP** to the configured collector (the same endpoint as tracing — see [Metrics configuration](#configuration-1)). There is **no Prometheus `/metrics` scrape endpoint**: names are dotted OTel semconv identifiers (no `_total`/`_seconds` suffixes; the unit is a separate field), attributes replace Prometheus labels, and the five `*.duration` histograms carry **exemplars** that link a data point back to the trace that recorded it.

> **PromQL note (Dash0 / OTLP→Prometheus backends):** the metric name is surfaced **dotted, verbatim** as the value of `otel_metric_name` (e.g. `otel_metric_name="edgequota.ratelimit.decisions"` — no `_total`/`_bucket`), while attribute **keys** are surfaced with dots replaced by underscores (e.g. `edgequota_ratelimit_decision="allowed"`). Scope queries by `service_name="edgequota"`.

### Counters

| Metric | Attributes | Description |
|--------|-----------|-------------|
| `edgequota.requests` | `http.request.method`, `edgequota.status_class` (`2xx`..`5xx`/`unknown`), `network.protocol.version` | Traffic source of truth — recorded on every terminal path, including streaming and preflight |
| `edgequota.ratelimit.decisions` | `edgequota.ratelimit.decision` (`allowed`\|`limited`) | Rate-limit decisions (replaces `requests_allowed_total`/`requests_limited_total`) |
| `edgequota.ratelimit.fallback_used` | `edgequota.redis.pool` | In-memory fallback activations when Redis is down |
| `edgequota.concurrency.rejected` | — | `max_concurrent_requests` 503s (their only home — they return before the traffic counter) |
| `edgequota.key_extract.errors` | — | Rate-limit key extraction failures |
| `edgequota.tenant_key.rejected` | — | External RL service returned an invalid `tenant_key` |
| `edgequota.auth.outcomes` | `edgequota.auth.outcome` (`error`\|`denied`\|`canceled`) | External auth outcomes (`canceled` = client disconnect, not an auth failure) |
| `edgequota.external_ratelimit.cache.lookups` | `edgequota.cache.result` (`hit`\|`miss`\|`stale`) | External RL cache lookups |
| `edgequota.external_ratelimit.semaphore_rejected` | — | External RL requests rejected by the concurrency semaphore |
| `edgequota.external_ratelimit.singleflight_shared` | — | External RL requests that shared a singleflight result |
| `edgequota.response_cache.lookups` | `edgequota.cache.result` (`hit`\|`miss`\|`stale`) | Response-cache lookups |
| `edgequota.response_cache.operations` | `edgequota.cache.operation` (`store`\|`skip`\|`purge`) | Response-cache operations |
| `edgequota.redis.errors` | `edgequota.redis.pool` | Redis operation errors |
| `edgequota.redis.readonly_retries` | — | Redis `READONLY` retries (replica failover) |
| `edgequota.connections` | `network.protocol.name`, `network.protocol.version`, `edgequota.tls.mode` (`mtls`\|`tls`\|`plain`) | Requests by protocol and TLS mode |
| `edgequota.streaming.connections` | `edgequota.stream.protocol` | Streaming connections opened (SSE/WS/gRPC) |
| `edgequota.events.dropped` | — | Usage events dropped from the ring buffer |
| `edgequota.events.send_failures` | — | Event batches that failed after all retries |
| `edgequota.cors.preflight_bypassed` | — | CORS preflights that bypassed auth + rate limiting |

### Up/down counters (gauges)

| Metric | Attributes | Description |
|--------|-----------|-------------|
| `http.server.active_requests` | — | In-flight HTTP requests |
| `edgequota.streaming.active_connections` | `edgequota.stream.protocol` | Active streaming connections |

### Observable gauges

| Metric | Attributes | Description |
|--------|-----------|-------------|
| `edgequota.redis.healthy` | `edgequota.redis.pool` (`ratelimit`\|`external_rl_cache`\|`response_cache`) | `1` = reachable, `0` = unhealthy |
| `edgequota.reload.last_success.timestamp` | `edgequota.reload.target` (`config`\|`tls`\|`mtls_ca`) | Unix seconds of the last successful reload (`0` until the first) |

### Histograms

| Metric | Unit | Attributes | Exemplars | Description |
|--------|------|-----------|-----------|-------------|
| `http.server.request.duration` | s | `http.request.method`, `http.response.status_code`, `network.protocol.*`, `http.route` | ✅ | End-to-end request latency — **emitted automatically by otelhttp** (not hand-rolled). Its `_count` excludes streaming/preflight; use `edgequota.requests` for traffic. |
| `edgequota.auth.duration` | s | — | ✅ | External auth check latency |
| `edgequota.external_ratelimit.duration` | s | — | ✅ | External rate-limit call latency |
| `edgequota.backend.duration` | s | — | ✅ | Backend proxy latency |
| `edgequota.streaming.duration` | s | `edgequota.stream.protocol` | ✅ | Streaming connection lifetime |
| `edgequota.ratelimit.remaining_tokens` | `{token}` | — | — | Distribution of remaining tokens per rate-limit check |
| `edgequota.response_cache.body.size` | `By` | — | — | Cached response body sizes |

Explicit histogram bucket boundaries (seconds/bytes/tokens) are preserved from the previous Prometheus histograms via SDK Views, so `by(le)` quantile queries remain accurate across the migration.

### Go runtime metrics

Go runtime and GC metrics are emitted via the portable `go.opentelemetry.io/contrib/instrumentation/runtime` instrumentation (the OTel replacement for the removed `go_*`/`process_*` Prometheus collectors): `go.goroutine.count`, `go.memory.used`, `go.memory.allocated`, `go.memory.gc.goal`, `go.processor.limit`, `go.config.gogc`. Container CPU/memory come from the platform's kubelet/cAdvisor pipeline.

### Configuration

Metrics are enabled independently of tracing but **share the OTLP transport** (endpoint/protocol/insecure) from the [`tracing`](#configuration) block:

```yaml
metrics:
  enabled: true    # env: METRICS_ENABLED — default true in dev/stage (helm/TF)
```

When `metrics.enabled` is false the MeterProvider is left no-op (no exporter, no push). Metrics can run with tracing off and vice-versa. Dropped in the migration: per-tenant metrics (`max_tenant_labels` removed — use traces/ClickHouse for per-tenant analysis) and `build_info` (now the `service.name`/`service.version` resource attributes).

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
| `service.instance.id` | Per-pod identity (`POD_NAME` env, else hostname) — keeps each replica's metric streams distinct |
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

## Dashboard

The canonical dashboard definition lives in the repo at [`deploy/observability/dashboard.yaml`](../deploy/observability/dashboard.yaml) (vendor-neutral; the infra observability stack mirrors it byte-for-byte — the repo is the source of truth, infra never ahead). Its queries use the dotted OTel metric names via the backend's `otel_metric_name` surface (see [`deploy/observability/README.md`](../deploy/observability/README.md) for the surfaced-name convention).

**Sections:**

| Row                  | Panels                                                                                   |
| -------------------- | ---------------------------------------------------------------------------------------- |
| Overview | Request rate, 5xx error ratio, P95 latency, rate-limited %, Redis health, cache hit rate |
| Traffic | Rate by status class, allowed vs rate-limited, by method, by protocol, by TLS mode |
| Response codes (by family) | 2xx / 3xx / 4xx / 5xx rate |
| Latency | E2E P50/P95/P99, breakdown (auth/ext-RL/backend P95), backend P50/P95/P99 |
| Streaming connections | Active by protocol, new/sec by protocol, duration P50/P95 |
| Rate limiting / keys | Key-extract & tenant-key-rejected errors |
| Redis pools | Health per pool, errors/sec, fallback/sec, remaining tokens, ext-RL cache, response-cache ops & ratio |
| Auth & external rate limit | Auth outcomes (error/denied/canceled), auth latency, ext-RL latency |
| Events | Events dropped/sec, send failures/sec |
| System resources (OTel-native) | In-flight requests, CPU, memory, goroutines (`go.goroutine.count`), network I/O |
| Config & TLS reloads | Time since last config / TLS / mTLS-CA reload |

Per-tenant panels were removed in the OTel migration (per-tenant cardinality belongs in traces/ClickHouse, not metrics), as was the `build_info` panel.

> **Traffic by path:** Per-path metrics are intentionally omitted to avoid unbounded label cardinality. Use access logs (structured JSON with `path` field) with a log aggregation system (Loki, Elasticsearch) for per-path traffic analysis.
>
> **Redis cluster latency:** EdgeQuota exposes Redis health and error counts but not per-cluster latency. For detailed Redis observability, deploy a dedicated redis-exporter alongside each Redis cluster.

---

## Alerting Rules

The canonical alerting rules live in the repo at [`deploy/observability/alerts.yaml`](../deploy/observability/alerts.yaml) (Prometheus rule format; mirrored into the infra observability stack, adapted to the backend's threshold model). They query the dotted OTel metric names via the backend's `otel_metric_name` surface. Traffic/error/no-traffic rules key off `edgequota.requests` (spans streaming); the goroutine alerts and the `EdgeQuotaNoTraffic` liveness guard use `go.goroutine.count` (OTel runtime metrics).

| Alert | Severity | Condition |
|-------|----------|-----------|
| EdgeQuotaHighErrorRate | critical | 5xx error rate > 5% for 5m |
| EdgeQuotaTrafficSpike | warning | Current rate > 2x the rate 1 hour ago |
| EdgeQuotaNoTraffic | critical | Zero requests for 10m while pods are running |
| EdgeQuotaHighP99Latency | warning/critical | P99 > 2s (warning), > 5s (critical) |
| EdgeQuotaBackendLatencyHigh | warning | Backend P95 > 5s |
| EdgeQuotaHighRateLimitRatio | warning | > 50% of requests rate-limited |
| EdgeQuotaConcurrencyRejections | warning | Requests rejected by concurrency limit |
| EdgeQuotaRedisUnhealthy | critical | `edgequota.redis.healthy == 0` |
| EdgeQuotaRedisErrors | warning | Redis errors > 1/s |
| EdgeQuotaFallbackActive | warning | In-memory fallback in use |
| EdgeQuotaAuthErrors | critical | Auth service returning errors |
| EdgeQuotaAuthLatencyHigh | warning | Auth P95 > 1s |
| EdgeQuotaEventsDropped | warning | Events dropped from ring buffer |
| EdgeQuotaEventSendFailures | warning | Event batches failing to send |
| EdgeQuotaLowCacheHitRate | warning | Cache hit rate < 30% |
| EdgeQuotaHighMemory | warning | RSS > 85% of memory limit |
| EdgeQuotaHighGoroutines | warning | `go.goroutine.count` > 10,000 |
| EdgeQuotaGoroutineGrowth | warning | Goroutines growing > 1/s, sustained 30m |

---

## Admin API

The admin server exposes operational endpoints alongside health probes (metrics are pushed via OTLP, not scraped — there is no `/metrics` endpoint):

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
  "auth_denied": 3,
  "auth_canceled": 1
}
```

---

## See Also

- [Configuration Reference](configuration.md) -- Logging and tracing config fields.
- [Deployment](deployment.md) -- Probe configuration and Kubernetes setup.
- [Events](events.md) -- Events-related metrics.
- [Troubleshooting](troubleshooting.md) -- Debugging with metrics and probes.

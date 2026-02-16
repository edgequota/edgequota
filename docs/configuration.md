# Configuration Reference

This document provides the complete configuration reference for EdgeQuota, including every field, its type, default value, and corresponding environment variable.

---

## Configuration Loading

Configuration is loaded in three layers (later layers override earlier ones):

1. **Defaults** — Hardcoded sensible values for every field.
2. **YAML file** — Loaded from the path specified by `EDGEQUOTA_CONFIG_FILE` (default: `/etc/edgequota/config.yaml`). If the file does not exist, defaults are used.
3. **Environment variables** — Override any field. Always take precedence.

### Environment Variable Naming

Variables are derived from the YAML path, uppercased, with dots replaced by underscores, and prefixed with `EDGEQUOTA_`:

```
server.address         → EDGEQUOTA_SERVER_ADDRESS
rate_limit.average     → EDGEQUOTA_RATE_LIMIT_AVERAGE
redis.endpoints        → EDGEQUOTA_REDIS_ENDPOINTS (comma-separated)
backend.transport.dial_timeout → EDGEQUOTA_BACKEND_TRANSPORT_DIAL_TIMEOUT
```

### Type Handling

| YAML Type | Env Var Format | Example |
|-----------|---------------|---------|
| `string` | Raw string | `EDGEQUOTA_SERVER_ADDRESS=:8080` |
| `int` / `int64` | Decimal integer | `EDGEQUOTA_RATE_LIMIT_AVERAGE=100` |
| `bool` | `true` / `false` | `EDGEQUOTA_AUTH_ENABLED=true` |
| `float64` | Decimal float | `EDGEQUOTA_TRACING_SAMPLE_RATE=0.1` |
| `[]string` | Comma-separated | `EDGEQUOTA_REDIS_ENDPOINTS=redis-0:6379,redis-1:6379` |
| Duration | Go duration string | `EDGEQUOTA_SERVER_READ_TIMEOUT=30s` |

---

## Full Reference

### `server` — Main Proxy Server

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `address` | string | `":8080"` | `EDGEQUOTA_SERVER_ADDRESS` | Listen address for proxy traffic (TCP, and UDP if HTTP/3 enabled) |
| `read_timeout` | duration | `"30s"` | `EDGEQUOTA_SERVER_READ_TIMEOUT` | Maximum time to read the entire request including body |
| `write_timeout` | duration | `"30s"` | `EDGEQUOTA_SERVER_WRITE_TIMEOUT` | Maximum time to write the response |
| `idle_timeout` | duration | `"120s"` | `EDGEQUOTA_SERVER_IDLE_TIMEOUT` | Maximum time to wait for the next request on keep-alive connections |
| `drain_timeout` | duration | `"30s"` | `EDGEQUOTA_SERVER_DRAIN_TIMEOUT` | Grace period during shutdown for in-flight requests to complete |
| `max_websocket_conns_per_key` | int64 | `0` | `EDGEQUOTA_SERVER_MAX_WEBSOCKET_CONNS_PER_KEY` | Max concurrent WebSocket connections per rate-limit key. 0 = unlimited |

### `server.tls` — TLS Termination

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_SERVER_TLS_ENABLED` | Enable TLS termination |
| `cert_file` | string | `""` | `EDGEQUOTA_SERVER_TLS_CERT_FILE` | Path to TLS certificate file (PEM) |
| `key_file` | string | `""` | `EDGEQUOTA_SERVER_TLS_KEY_FILE` | Path to TLS private key file (PEM) |
| `http3_enabled` | bool | `false` | `EDGEQUOTA_SERVER_TLS_HTTP3_ENABLED` | Enable HTTP/3 (QUIC) on the same address over UDP. Requires TLS. |
| `min_version` | string | `""` | `EDGEQUOTA_SERVER_TLS_MIN_VERSION` | Minimum TLS version: `"1.2"` or `"1.3"`. Defaults to Go's default (TLS 1.2). |

### `admin` — Admin/Observability Server

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `address` | string | `":9090"` | `EDGEQUOTA_ADMIN_ADDRESS` | Listen address for health probes and Prometheus metrics |
| `read_timeout` | duration | `"5s"` | `EDGEQUOTA_ADMIN_READ_TIMEOUT` | Read timeout for admin connections |
| `write_timeout` | duration | `"10s"` | `EDGEQUOTA_ADMIN_WRITE_TIMEOUT` | Write timeout for admin connections |
| `idle_timeout` | duration | `"30s"` | `EDGEQUOTA_ADMIN_IDLE_TIMEOUT` | Idle timeout for admin connections |

### `backend` — Upstream Backend

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `url` | string | **(required)** | `EDGEQUOTA_BACKEND_URL` | Backend URL to proxy to (e.g., `http://my-service:8080`). Normalized at load time to always include a port. |
| `timeout` | duration | `"30s"` | `EDGEQUOTA_BACKEND_TIMEOUT` | Response timeout for backend requests |
| `max_idle_conns` | int | `100` | `EDGEQUOTA_BACKEND_MAX_IDLE_CONNS` | Maximum idle connections to the backend |
| `idle_conn_timeout` | duration | `"90s"` | `EDGEQUOTA_BACKEND_IDLE_CONN_TIMEOUT` | Idle connection timeout |
| `tls_insecure_skip_verify` | bool | `false` | `EDGEQUOTA_BACKEND_TLS_INSECURE_SKIP_VERIFY` | Skip TLS certificate verification for backend connections |

### `backend.transport` — HTTP Transport Tuning

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `dial_timeout` | duration | `"30s"` | `EDGEQUOTA_BACKEND_TRANSPORT_DIAL_TIMEOUT` | TCP dial timeout for backend connections |
| `dial_keep_alive` | duration | `"30s"` | `EDGEQUOTA_BACKEND_TRANSPORT_DIAL_KEEP_ALIVE` | TCP keep-alive interval |
| `tls_handshake_timeout` | duration | `"10s"` | `EDGEQUOTA_BACKEND_TRANSPORT_TLS_HANDSHAKE_TIMEOUT` | TLS handshake timeout |
| `expect_continue_timeout` | duration | `"1s"` | `EDGEQUOTA_BACKEND_TRANSPORT_EXPECT_CONTINUE_TIMEOUT` | Timeout for `Expect: 100-continue` |
| `h2_read_idle_timeout` | duration | `"30s"` | `EDGEQUOTA_BACKEND_TRANSPORT_H2_READ_IDLE_TIMEOUT` | HTTP/2 read idle timeout (health check for gRPC streams) |
| `h2_ping_timeout` | duration | `"15s"` | `EDGEQUOTA_BACKEND_TRANSPORT_H2_PING_TIMEOUT` | HTTP/2 ping timeout |
| `websocket_dial_timeout` | duration | `"10s"` | `EDGEQUOTA_BACKEND_TRANSPORT_WEBSOCKET_DIAL_TIMEOUT` | WebSocket backend dial timeout |

### `auth` — External Authentication

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_AUTH_ENABLED` | Enable external authentication |
| `timeout` | duration | `"5s"` | `EDGEQUOTA_AUTH_TIMEOUT` | Timeout for auth service calls |
| `failure_policy` | string | `"failclosed"` | `EDGEQUOTA_AUTH_FAILURE_POLICY` | Behavior when auth is unreachable: `failclosed`, `failopen` |

### `auth.http` — HTTP Auth Backend

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `url` | string | `""` | `EDGEQUOTA_AUTH_HTTP_URL` | URL of the HTTP auth endpoint (receives POST with JSON) |
| `forward_original_headers` | bool | `false` | `EDGEQUOTA_AUTH_HTTP_FORWARD_ORIGINAL_HEADERS` | Also send headers with `X-Original-` prefix |

### `auth.grpc` — gRPC Auth Backend

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `address` | string | `""` | `EDGEQUOTA_AUTH_GRPC_ADDRESS` | Address of the gRPC auth service |

### `auth.grpc.tls` — gRPC Auth TLS

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_AUTH_GRPC_TLS_ENABLED` | Enable TLS for gRPC auth |
| `ca_file` | string | `""` | `EDGEQUOTA_AUTH_GRPC_TLS_CA_FILE` | CA certificate file for verification |

### `auth.header_filter` — Auth Header Filtering

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `allow_list` | []string | `[]` | `EDGEQUOTA_AUTH_HEADER_FILTER_ALLOW_LIST` | Exclusive: only forward these headers (deny_list ignored when set) |
| `deny_list` | []string | `[]` | `EDGEQUOTA_AUTH_HEADER_FILTER_DENY_LIST` | Never forward these headers |

### `rate_limit` — Rate Limiting

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `average` | int64 | `0` | `EDGEQUOTA_RATE_LIMIT_AVERAGE` | Requests per period. `0` disables rate limiting. |
| `burst` | int64 | `1` | `EDGEQUOTA_RATE_LIMIT_BURST` | Maximum burst capacity. Minimum: 1. |
| `period` | duration | `"1s"` | `EDGEQUOTA_RATE_LIMIT_PERIOD` | Rate limit time window |
| `failure_policy` | string | `"passThrough"` | `EDGEQUOTA_RATE_LIMIT_FAILURE_POLICY` | Behavior when Redis is down: `passThrough`, `failClosed`, `inMemoryFallback` |
| `failure_code` | int | `429` | `EDGEQUOTA_RATE_LIMIT_FAILURE_CODE` | HTTP status code for `failClosed` rejections. Must be 4xx or 5xx. |
| `min_ttl` | duration | `""` (auto) | `EDGEQUOTA_RATE_LIMIT_MIN_TTL` | Floor for Redis key TTL. Reduces EXPIRE churn for high-cardinality keys. |

### `rate_limit.key_strategy` — Key Extraction

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `type` | string | `"clientIP"` | `EDGEQUOTA_RATE_LIMIT_KEY_STRATEGY_TYPE` | Key strategy: `clientIP`, `header`, `composite` |
| `header_name` | string | `""` | `EDGEQUOTA_RATE_LIMIT_KEY_STRATEGY_HEADER_NAME` | Header to extract key from (required for `header` and `composite`) |
| `path_prefix` | bool | `false` | `EDGEQUOTA_RATE_LIMIT_KEY_STRATEGY_PATH_PREFIX` | Include first path segment in composite key |
| `trusted_proxies` | []string | `[]` | `EDGEQUOTA_RATE_LIMIT_KEY_STRATEGY_TRUSTED_PROXIES` | CIDRs whose X-Forwarded-For / X-Real-IP are trusted. Empty = only RemoteAddr. |
| `trusted_ip_depth` | int | `0` | `EDGEQUOTA_RATE_LIMIT_KEY_STRATEGY_TRUSTED_IP_DEPTH` | Which XFF entry to use: 0=leftmost, N=Nth from right |

### `rate_limit.external` — External Rate Limit Service

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_ENABLED` | Enable external rate limit resolution |
| `timeout` | duration | `"5s"` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_TIMEOUT` | Timeout for external rate limit calls |
| `cache_ttl` | duration | `"60s"` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_CACHE_TTL` | Default cache TTL when response has no cache hints |
| `max_concurrent_requests` | int | `50` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_MAX_CONCURRENT_REQUESTS` | Semaphore cap on concurrent external calls |

### `rate_limit.external.http` — External HTTP Backend

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `url` | string | `""` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_HTTP_URL` | URL for HTTP rate limit service |

### `rate_limit.external.grpc` — External gRPC Backend

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `address` | string | `""` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_GRPC_ADDRESS` | Address for gRPC rate limit service |

### `rate_limit.external.grpc.tls` — External gRPC TLS

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_GRPC_TLS_ENABLED` | Enable TLS for external gRPC |
| `ca_file` | string | `""` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_GRPC_TLS_CA_FILE` | CA file for external gRPC TLS |

### `rate_limit.external.header_filter` — External Rate Limit Header Filtering

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `allow_list` | []string | `[]` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_HEADER_FILTER_ALLOW_LIST` | Exclusive: only forward these headers (deny_list ignored when set) |
| `deny_list` | []string | `[]` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_HEADER_FILTER_DENY_LIST` | Never forward these headers |

### `redis` — Redis Connection

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `endpoints` | []string | `["localhost:6379"]` | `EDGEQUOTA_REDIS_ENDPOINTS` | Redis endpoints (comma-separated for env var) |
| `mode` | string | `"single"` | `EDGEQUOTA_REDIS_MODE` | Topology: `single`, `replication`, `sentinel`, `cluster` |
| `master_name` | string | `""` | `EDGEQUOTA_REDIS_MASTER_NAME` | Master name (required for sentinel mode) |
| `username` | string | `""` | `EDGEQUOTA_REDIS_USERNAME` | Redis username (ACL) |
| `password` | string | `""` | `EDGEQUOTA_REDIS_PASSWORD` | Redis password |
| `db` | int | `0` | `EDGEQUOTA_REDIS_DB` | Redis database number |
| `pool_size` | int | `10` | `EDGEQUOTA_REDIS_POOL_SIZE` | Connection pool size per EdgeQuota instance |
| `dial_timeout` | duration | `"5s"` | `EDGEQUOTA_REDIS_DIAL_TIMEOUT` | Redis connection timeout |
| `read_timeout` | duration | `"3s"` | `EDGEQUOTA_REDIS_READ_TIMEOUT` | Redis read timeout |
| `write_timeout` | duration | `"3s"` | `EDGEQUOTA_REDIS_WRITE_TIMEOUT` | Redis write timeout |
| `sentinel_username` | string | `""` | `EDGEQUOTA_REDIS_SENTINEL_USERNAME` | Sentinel username |
| `sentinel_password` | string | `""` | `EDGEQUOTA_REDIS_SENTINEL_PASSWORD` | Sentinel password |

### `redis.tls` — Redis TLS

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_REDIS_TLS_ENABLED` | Enable TLS for Redis connections |
| `insecure_skip_verify` | bool | `false` | `EDGEQUOTA_REDIS_TLS_INSECURE_SKIP_VERIFY` | Skip Redis TLS certificate verification |

### `cache_redis` — Dedicated Redis for External Response Caches (Optional)

By default, cached responses from the external rate limit service are stored in the same Redis instance used for rate limit counters. If you need to separate these workloads — for example, using a **replication** topology for the read-heavy cache and a **cluster** topology for the write-heavy rate limit counters — configure `cache_redis` with its own connection parameters.

When `cache_redis` is omitted or has no endpoints, the main `redis` connection is reused automatically. To point both at the same Redis, simply set the same endpoints in both sections.

The field schema is identical to the `redis` section above:

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `endpoints` | []string | — | `EDGEQUOTA_CACHE_REDIS_ENDPOINTS` | Cache Redis endpoints |
| `mode` | string | `"single"` | `EDGEQUOTA_CACHE_REDIS_MODE` | Topology: `single`, `replication`, `sentinel`, `cluster` |
| `master_name` | string | `""` | `EDGEQUOTA_CACHE_REDIS_MASTER_NAME` | Master name (sentinel mode) |
| `username` | string | `""` | `EDGEQUOTA_CACHE_REDIS_USERNAME` | Cache Redis username |
| `password` | string | `""` | `EDGEQUOTA_CACHE_REDIS_PASSWORD` | Cache Redis password |
| `db` | int | `0` | `EDGEQUOTA_CACHE_REDIS_DB` | Cache Redis database |
| `pool_size` | int | `10` | `EDGEQUOTA_CACHE_REDIS_POOL_SIZE` | Connection pool size |
| `dial_timeout` | duration | `"5s"` | `EDGEQUOTA_CACHE_REDIS_DIAL_TIMEOUT` | Connection timeout |
| `read_timeout` | duration | `"3s"` | `EDGEQUOTA_CACHE_REDIS_READ_TIMEOUT` | Read timeout |
| `write_timeout` | duration | `"3s"` | `EDGEQUOTA_CACHE_REDIS_WRITE_TIMEOUT` | Write timeout |

TLS sub-fields: `EDGEQUOTA_CACHE_REDIS_TLS_ENABLED`, `EDGEQUOTA_CACHE_REDIS_TLS_INSECURE_SKIP_VERIFY`.

**Example** — rate limit counters on a Redis Cluster, caches on a Redis Replication set:

```yaml
redis:
  endpoints:
    - "rl-node-0:6379"
    - "rl-node-1:6379"
    - "rl-node-2:6379"
  mode: "cluster"
cache_redis:
  endpoints:
    - "cache-primary:6379"
    - "cache-replica-1:6379"
    - "cache-replica-2:6379"
  mode: "replication"
  pool_size: 20
```

### `logging` — Structured Logging

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `level` | string | `"info"` | `EDGEQUOTA_LOGGING_LEVEL` | Log level: `debug`, `info`, `warn`, `error` |
| `format` | string | `"json"` | `EDGEQUOTA_LOGGING_FORMAT` | Output format: `json`, `text` |

### `tracing` — OpenTelemetry Tracing

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_TRACING_ENABLED` | Enable distributed tracing |
| `endpoint` | string | `""` | `EDGEQUOTA_TRACING_ENDPOINT` | OTLP HTTP endpoint (e.g., `http://otel-collector:4318`) |
| `service_name` | string | `"edgequota"` | `EDGEQUOTA_TRACING_SERVICE_NAME` | Service name in traces |
| `sample_rate` | float64 | `0.1` | `EDGEQUOTA_TRACING_SAMPLE_RATE` | Sampling ratio (0.0–1.0) |

---

## Validation Rules

The following constraints are enforced at load time:

| Rule | Error |
|------|-------|
| `backend.url` is empty | `backend.url is required` |
| `backend.url` has no scheme or host | `invalid backend.url: scheme and host are required` |
| Any duration field is not a valid Go duration | `invalid <field> "<value>"` |
| TLS enabled but `cert_file` or `key_file` missing | `server.tls.cert_file and server.tls.key_file are required` |
| HTTP/3 enabled without TLS | `server.tls.http3_enabled requires server.tls.enabled` |
| `min_version` not `1.2` or `1.3` | `invalid server.tls.min_version` |
| Auth enabled but no backend URL | `auth.http.url or auth.grpc.address is required` |
| `average < 0` | `rate_limit.average must be >= 0` |
| `failure_policy` not recognized | `invalid rate_limit.failure_policy` |
| `failure_code` not 4xx or 5xx | `invalid rate_limit.failure_code` |
| `header`/`composite` strategy without `header_name` | `header_name is required` |
| External RL enabled without backend | `rate_limit.external requires http.url or grpc.address` |
| Redis mode not recognized | `invalid redis.mode` |
| Single mode with > 1 endpoint | `single mode requires exactly one endpoint` |
| Sentinel mode without `master_name` | `redis.master_name is required for sentinel mode` |
| Replication mode with < 2 endpoints | `replication mode requires at least 2 endpoints` |
| Cache Redis mode not recognized | `invalid cache_redis.mode` |
| Cache Redis sentinel without `master_name` | `cache_redis.master_name is required for sentinel mode` |
| Logging level not recognized | `invalid logging.level` |
| Logging format not recognized | `invalid logging.format` |
| Tracing enabled without endpoint | `tracing.endpoint is required` |

---

## Example: Minimal Production Config

```yaml
backend:
  url: "http://my-backend:8080"
rate_limit:
  average: 100
  burst: 50
  period: "1s"
  key_strategy:
    type: "header"
    header_name: "X-Tenant-Id"
redis:
  endpoints:
    - "redis-master:6379"
  mode: "single"
```

Everything else uses defaults. Override individual fields with environment variables as needed.

---

## Hot Reload

EdgeQuota watches the config file for changes using `fsnotify`. When the file is modified (e.g. via `kubectl patch configmap`), the following happens automatically:

1. The file is re-read and validated.
2. If validation fails, the old config is kept and an error is logged.
3. If validation passes, the following are hot-swapped **without restart**:
   - Rate-limit parameters (`average`, `burst`, `period`, `min_ttl`, `failure_policy`, `failure_code`)
   - Key strategy (type, header name, trusted proxies)
   - Auth client (reconnects if URL/TLS config changed)
   - External rate-limit client (reconnects if URL/TLS config changed)
   - TLS certificates (via `GetCertificate` — new connections pick up the new cert immediately)

Rapid file writes are debounced (300ms) to handle editors that use atomic rename-on-save.

**Note:** Redis connection parameters and server listen addresses are NOT hot-reloaded; they require a restart.

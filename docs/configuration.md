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
rate_limit.static.average → EDGEQUOTA_RATE_LIMIT_STATIC_AVERAGE
redis.endpoints        → EDGEQUOTA_REDIS_ENDPOINTS (comma-separated)
backend.transport.dial_timeout → EDGEQUOTA_BACKEND_TRANSPORT_DIAL_TIMEOUT
```

### Type Handling

| YAML Type | Env Var Format | Example |
|-----------|---------------|---------|
| `string` | Raw string | `EDGEQUOTA_SERVER_ADDRESS=:8080` |
| `int` / `int64` | Decimal integer | `EDGEQUOTA_RATE_LIMIT_STATIC_AVERAGE=100` |
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
| `request_timeout` | duration | `""` | `EDGEQUOTA_SERVER_REQUEST_TIMEOUT` | Per-request timeout applied to the entire middleware chain. Empty = no timeout (backend timeout still applies). |
| `max_concurrent_requests` | int64 | `0` | `EDGEQUOTA_SERVER_MAX_CONCURRENT_REQUESTS` | Cap on in-flight requests. Excess requests receive 503. 0 = unlimited. |
| `allowed_websocket_origins` | []string | `[]` | `EDGEQUOTA_SERVER_ALLOWED_WEBSOCKET_ORIGINS` | WebSocket Origin header allowlist. Empty = allow all origins. |
| `max_websocket_conns_per_key` | int64 | `0` | `EDGEQUOTA_SERVER_MAX_WEBSOCKET_CONNS_PER_KEY` | Max concurrent WebSocket connections per rate-limit key. 0 = unlimited. |
| `max_websocket_transfer_bytes` | int64 | `0` | `EDGEQUOTA_SERVER_MAX_WEBSOCKET_TRANSFER_BYTES` | Max bytes per direction on a single WebSocket connection. 0 = unlimited. |
| `websocket_idle_timeout` | duration | `"5m"` | `EDGEQUOTA_SERVER_WEBSOCKET_IDLE_TIMEOUT` | Close WebSocket connections with no data transfer for this duration. 0 = unlimited. |

### `server.tls` — TLS Termination

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_SERVER_TLS_ENABLED` | Enable TLS termination |
| `cert_file` | string | `""` | `EDGEQUOTA_SERVER_TLS_CERT_FILE` | Path to TLS certificate file (PEM) |
| `key_file` | string | `""` | `EDGEQUOTA_SERVER_TLS_KEY_FILE` | Path to TLS private key file (PEM) |
| `http3_enabled` | bool | `false` | `EDGEQUOTA_SERVER_TLS_HTTP3_ENABLED` | Enable HTTP/3 (QUIC) on the same address over UDP. Requires TLS. |
| `http3_advertise_port` | int | `0` | `EDGEQUOTA_SERVER_TLS_HTTP3_ADVERTISE_PORT` | External port for the `Alt-Svc` header. 0 = use the listen port. |
| `min_version` | string | `""` | `EDGEQUOTA_SERVER_TLS_MIN_VERSION` | Minimum TLS version: `"1.2"` or `"1.3"`. Defaults to Go's default (TLS 1.2). |

### `admin` — Admin/Observability Server

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `address` | string | `":9090"` | `EDGEQUOTA_ADMIN_ADDRESS` | Listen address for health probes and Prometheus metrics |
| `read_timeout` | duration | `"5s"` | `EDGEQUOTA_ADMIN_READ_TIMEOUT` | Read timeout for admin connections |
| `write_timeout` | duration | `"10s"` | `EDGEQUOTA_ADMIN_WRITE_TIMEOUT` | Write timeout for admin connections |
| `idle_timeout` | duration | `"30s"` | `EDGEQUOTA_ADMIN_IDLE_TIMEOUT` | Idle timeout for admin connections |

### `backend` — Upstream Backend (Transport Settings)

The `backend` block contains only transport settings (timeout, connections, TLS). The backend URL is configured in `rate_limit.static.backend_url` when external RL is disabled, or in `rate_limit.external.fallback.backend_url` when external RL is enabled.

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `timeout` | duration | `"30s"` | `EDGEQUOTA_BACKEND_TIMEOUT` | Response timeout for backend requests |
| `max_idle_conns` | int | `100` | `EDGEQUOTA_BACKEND_MAX_IDLE_CONNS` | Maximum idle connections to the backend |
| `idle_conn_timeout` | duration | `"90s"` | `EDGEQUOTA_BACKEND_IDLE_CONN_TIMEOUT` | Idle connection timeout |
| `tls_insecure_skip_verify` | bool | `false` | `EDGEQUOTA_BACKEND_TLS_INSECURE_SKIP_VERIFY` | Skip TLS certificate verification for backend connections |
| `max_request_body_size` | int64 | `0` | `EDGEQUOTA_BACKEND_MAX_REQUEST_BODY_SIZE` | Maximum request body size in bytes. 0 = unlimited. |

### `backend.url_policy` — Dynamic Backend URL Policy (SSRF Protection)

Controls which dynamic `backend_url` values (from the external rate limit service) are allowed. Prevents SSRF via backend URL injection.

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `allowed_schemes` | []string | `["http", "https"]` | `EDGEQUOTA_BACKEND_URL_POLICY_ALLOWED_SCHEMES` | Reject URLs with other schemes |
| `deny_private_networks` | bool | `true` | `EDGEQUOTA_BACKEND_URL_POLICY_DENY_PRIVATE_NETWORKS` | Block RFC 1918, loopback, link-local, and cloud metadata IPs |
| `allowed_hosts` | []string | `[]` | `EDGEQUOTA_BACKEND_URL_POLICY_ALLOWED_HOSTS` | When non-empty, only these hostnames are permitted (exact match, case-insensitive) |

When the external rate limit service returns a `backend_url`, EdgeQuota validates it against this policy before connecting. Additionally, DNS resolution results are re-validated at TCP dial time (`SafeDialer`) to prevent DNS rebinding attacks. See [Security](security.md) for the full threat model.

```yaml
backend:
  url_policy:
    deny_private_networks: true
    allowed_hosts:
      - "backend-a.internal"
      - "backend-b.internal"
```

### `backend.transport` — HTTP Transport Tuning

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `backend_protocol` | string | `"auto"` | `EDGEQUOTA_BACKEND_TRANSPORT_BACKEND_PROTOCOL` | Protocol: `auto`, `h1`, `h2`, `h3`. See [Multi-Protocol Proxy](proxy.md). |
| `dial_timeout` | duration | `"30s"` | `EDGEQUOTA_BACKEND_TRANSPORT_DIAL_TIMEOUT` | TCP dial timeout for backend connections |
| `dial_keep_alive` | duration | `"30s"` | `EDGEQUOTA_BACKEND_TRANSPORT_DIAL_KEEP_ALIVE` | TCP keep-alive interval |
| `tls_handshake_timeout` | duration | `"10s"` | `EDGEQUOTA_BACKEND_TRANSPORT_TLS_HANDSHAKE_TIMEOUT` | TLS handshake timeout |
| `expect_continue_timeout` | duration | `"1s"` | `EDGEQUOTA_BACKEND_TRANSPORT_EXPECT_CONTINUE_TIMEOUT` | Timeout for `Expect: 100-continue` |
| `h2_read_idle_timeout` | duration | `"30s"` | `EDGEQUOTA_BACKEND_TRANSPORT_H2_READ_IDLE_TIMEOUT` | HTTP/2 read idle timeout (health check for gRPC streams) |
| `h2_ping_timeout` | duration | `"15s"` | `EDGEQUOTA_BACKEND_TRANSPORT_H2_PING_TIMEOUT` | HTTP/2 ping timeout |
| `websocket_dial_timeout` | duration | `"10s"` | `EDGEQUOTA_BACKEND_TRANSPORT_WEBSOCKET_DIAL_TIMEOUT` | WebSocket backend dial timeout |
| `h3_udp_receive_buffer_size` | int | `0` | `EDGEQUOTA_BACKEND_TRANSPORT_H3_UDP_RECEIVE_BUFFER_SIZE` | QUIC UDP receive buffer (bytes). 0 = let quic-go manage. Recommended: 7500000. |
| `h3_udp_send_buffer_size` | int | `0` | `EDGEQUOTA_BACKEND_TRANSPORT_H3_UDP_SEND_BUFFER_SIZE` | QUIC UDP send buffer (bytes). 0 = let quic-go manage. Recommended: 7500000. |

### `auth` — External Authentication

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_AUTH_ENABLED` | Enable external authentication |
| `timeout` | duration | `"5s"` | `EDGEQUOTA_AUTH_TIMEOUT` | Timeout for auth service calls |
| `failure_policy` | string | `"failclosed"` | `EDGEQUOTA_AUTH_FAILURE_POLICY` | Behavior when auth is unreachable: `failclosed`, `failopen` |
| `propagate_request_body` | bool | `false` | `EDGEQUOTA_AUTH_PROPAGATE_REQUEST_BODY` | Include the request body in the auth check (increases latency) |
| `max_auth_body_size` | int64 | `65536` | `EDGEQUOTA_AUTH_MAX_AUTH_BODY_SIZE` | Maximum body bytes sent to the auth service (default: 64 KiB) |
| `allowed_injection_headers` | []string | `[]` | `EDGEQUOTA_AUTH_ALLOWED_INJECTION_HEADERS` | Headers the auth service is permitted to inject even if in the deny list |

### `auth.circuit_breaker` — Auth Circuit Breaker

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `threshold` | int | `5` | `EDGEQUOTA_AUTH_CIRCUIT_BREAKER_THRESHOLD` | Consecutive failures before opening |
| `reset_timeout` | duration | `"30s"` | `EDGEQUOTA_AUTH_CIRCUIT_BREAKER_RESET_TIMEOUT` | Time the breaker stays open before half-open probe |

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
| `server_name` | string | `""` | `EDGEQUOTA_AUTH_GRPC_TLS_SERVER_NAME` | Override TLS server name verification. Empty = use hostname from address. |

### `auth.header_filter` — Auth Header Filtering

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `allow_list` | []string | `[]` | `EDGEQUOTA_AUTH_HEADER_FILTER_ALLOW_LIST` | Exclusive: only forward these headers (deny_list ignored when set) |
| `deny_list` | []string | `[]` | `EDGEQUOTA_AUTH_HEADER_FILTER_DENY_LIST` | Never forward these headers |

### `rate_limit` — Rate Limiting (Top-Level)

These fields apply regardless of whether static or external rate limiting is used.

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `failure_policy` | string | `"passThrough"` | `EDGEQUOTA_RATE_LIMIT_FAILURE_POLICY` | Behavior when Redis is down: `passThrough`, `failClosed`, `inMemoryFallback` |
| `failure_code` | int | `429` | `EDGEQUOTA_RATE_LIMIT_FAILURE_CODE` | HTTP status code for `failClosed` rejections. Must be 4xx or 5xx. |
| `min_ttl` | duration | `""` (auto) | `EDGEQUOTA_RATE_LIMIT_MIN_TTL` | Floor for Redis key TTL. Reduces EXPIRE churn for high-cardinality keys. |
| `key_prefix` | string | `""` | `EDGEQUOTA_RATE_LIMIT_KEY_PREFIX` | Optional prefix prepended to all rate-limit keys. |
| `max_tenant_labels` | int | `1000` | `EDGEQUOTA_RATE_LIMIT_MAX_TENANT_LABELS` | Max distinct tenant labels in per-tenant Prometheus metrics. Prevents cardinality explosion. |
| `global_passthrough_rps` | float64 | `0` | `EDGEQUOTA_RATE_LIMIT_GLOBAL_PASSTHROUGH_RPS` | Safety valve: max global RPS when in passthrough mode (Redis down). 0 = unlimited. |

### `rate_limit.static` — Static Rate-Limit Bucket

Defines the token-bucket dimensions and key extraction strategy used when **external RL is disabled**. When external RL is enabled, this block is ignored (key extraction is skipped and the external service owns quota resolution).

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `backend_url` | string | **(required when external RL disabled)** | `EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL` | Backend URL to proxy to (e.g., `http://my-service:8080`). Normalized at load time to always include a port. |
| `average` | int64 | `0` | `EDGEQUOTA_RATE_LIMIT_STATIC_AVERAGE` | Requests per period. `0` disables rate limiting. |
| `burst` | int64 | `1` | `EDGEQUOTA_RATE_LIMIT_STATIC_BURST` | Maximum burst capacity. Minimum: 1. |
| `period` | duration | `"1s"` | `EDGEQUOTA_RATE_LIMIT_STATIC_PERIOD` | Rate limit time window |

### `rate_limit.static.key_strategy` — Key Extraction (Static)

Controls how the per-client rate-limit key is derived from incoming requests.

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `type` | string | `"clientIP"` | `EDGEQUOTA_RATE_LIMIT_STATIC_KEY_STRATEGY_TYPE` | Key strategy: `clientIP`, `header`, `composite`, `global` |
| `header_name` | string | `""` | `EDGEQUOTA_RATE_LIMIT_STATIC_KEY_STRATEGY_HEADER_NAME` | Header to extract key from (required for `header` and `composite`) |
| `path_prefix` | bool | `false` | `EDGEQUOTA_RATE_LIMIT_STATIC_KEY_STRATEGY_PATH_PREFIX` | Include first path segment in composite key |
| `global_key` | string | `""` | `EDGEQUOTA_RATE_LIMIT_STATIC_KEY_STRATEGY_GLOBAL_KEY` | Fixed key for `global` strategy. Defaults to `"global"` when empty. |
| `trusted_proxies` | []string | `[]` | `EDGEQUOTA_RATE_LIMIT_STATIC_KEY_STRATEGY_TRUSTED_PROXIES` | CIDRs whose X-Forwarded-For / X-Real-IP are trusted. Empty = only RemoteAddr. |
| `trusted_ip_depth` | int | `0` | `EDGEQUOTA_RATE_LIMIT_STATIC_KEY_STRATEGY_TRUSTED_IP_DEPTH` | Which XFF entry to use: 0=leftmost, N=Nth from right |

**Key strategy types:**

| Type | Description | Redis cardinality |
|------|-------------|-------------------|
| `clientIP` | Rate limit by client IP address. Default. | One key per unique IP. |
| `header` | Rate limit by a request header (e.g. `X-Tenant-Id`). Requires `header_name`. | One key per unique header value. |
| `composite` | Combine header + optional first path segment. Requires `header_name`. | One key per header+path combination. |
| `global` | Fixed key -- all requests share one bucket. Ideal for FE/static assets. | Exactly one key. |

### `rate_limit.external` — External Rate Limit Service

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_ENABLED` | Enable external rate limit resolution |
| `timeout` | duration | `"5s"` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_TIMEOUT` | Timeout for external rate limit calls |
| `cache_ttl` | duration | `"60s"` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_CACHE_TTL` | **Deprecated.** Default cache TTL when response has no cache directives. Ignored when CDN-style caching is enabled — external services should return `Cache-Control` headers or body fields instead. See [Response Caching](caching.md). |
| `max_concurrent_requests` | int | `50` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_MAX_CONCURRENT_REQUESTS` | Semaphore cap on concurrent external calls |
| `failure_policy` | string | `"static_fallback"` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_FAILURE_POLICY` | Behavior when external service is down: `static_fallback`, `failclosed` |

### `rate_limit.external.circuit_breaker` — External RL Circuit Breaker

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `threshold` | int | `5` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_CIRCUIT_BREAKER_THRESHOLD` | Consecutive failures per key before opening |
| `reset_timeout` | duration | `"30s"` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_CIRCUIT_BREAKER_RESET_TIMEOUT` | Time the breaker stays open before half-open probe |

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
| `server_name` | string | `""` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_GRPC_TLS_SERVER_NAME` | Override TLS server name verification. Empty = use hostname from address. |

### `rate_limit.external.header_filter` — External Rate Limit Header Filtering

Controls which headers are **sent to the external service** over the wire. This is a security/privacy concern — sensitive headers should not reach the external service.

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `allow_list` | []string | `[]` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_HEADER_FILTER_ALLOW_LIST` | Exclusive: only forward these headers (deny_list ignored when set) |
| `deny_list` | []string | `[]` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_HEADER_FILTER_DENY_LIST` | Never forward these headers |

### `rate_limit.external.fallback` — Fallback When External Service Is Unreachable

**Required when `external.enabled` is `true`.** When the external rate limit service is unreachable (timeout, circuit breaker open, no cached response, or response lacks `backend_url`) and the failure policy is not `failclosed`, EdgeQuota applies rate limits from this block instead.

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `backend_url` | string | **(required when external RL enabled)** | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_FALLBACK_BACKEND_URL` | Backend URL used when the external service response does not include `backend_url` or when the external service is unreachable. |
| `average` | int64 | **(required)** | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_FALLBACK_AVERAGE` | Requests per period. Must be > 0 when external RL is enabled. |
| `burst` | int64 | `1` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_FALLBACK_BURST` | Maximum burst capacity. Minimum: 1. |
| `period` | duration | `"1s"` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_FALLBACK_PERIOD` | Rate limit time window. |

### `rate_limit.external.fallback.key_strategy` — Fallback Key Extraction

**Required when `external.enabled` is `true`.** The key strategy used to extract the rate-limit key during fallback. Same structure as `rate_limit.static.key_strategy`.

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `type` | string | **(required)** | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_FALLBACK_KEY_STRATEGY_TYPE` | `clientIP`, `header`, `composite`, or `global` |
| `header_name` | string | `""` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_FALLBACK_KEY_STRATEGY_HEADER_NAME` | Required for `header` / `composite` |
| `global_key` | string | `""` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_FALLBACK_KEY_STRATEGY_GLOBAL_KEY` | Fixed key for `global` strategy |

The `global` type is recommended for FE/static asset fallbacks since browsers do not send custom headers. For backend APIs where auth injects tenant headers, `header` or `clientIP` may be more appropriate.

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

### `cache` — Response Cache (CDN)

EdgeQuota can cache backend responses in Redis, acting as a CDN. The cache is response-driven — backends opt in via `Cache-Control` headers. See [Response Caching](caching.md) for full semantics.

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_CACHE_ENABLED` | Enable CDN-style response caching |
| `max_body_size` | int64 | `1048576` | `EDGEQUOTA_CACHE_MAX_BODY_SIZE` | Maximum response body size in bytes eligible for caching (default: 1 MB) |

```yaml
cache:
  enabled: true
  max_body_size: 5242880  # 5 MB
```

### `response_cache_redis` — Dedicated Redis for Response Cache (Optional)

When configured, the response cache uses this Redis connection instead of `cache_redis` or `redis`. The field schema is identical to the `redis` section. If omitted, the fallback chain is: `cache_redis` → `redis`.

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `endpoints` | []string | — | `EDGEQUOTA_RESPONSE_CACHE_REDIS_ENDPOINTS` | Response cache Redis endpoints |
| `mode` | string | `"single"` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_MODE` | Topology: `single`, `replication`, `sentinel`, `cluster` |
| `master_name` | string | `""` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_MASTER_NAME` | Master name (sentinel mode) |
| `username` | string | `""` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_USERNAME` | Redis username |
| `password` | string | `""` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_PASSWORD` | Redis password |
| `db` | int | `0` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_DB` | Redis database |
| `pool_size` | int | `10` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_POOL_SIZE` | Connection pool size |
| `dial_timeout` | duration | `"5s"` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_DIAL_TIMEOUT` | Connection timeout |
| `read_timeout` | duration | `"3s"` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_READ_TIMEOUT` | Read timeout |
| `write_timeout` | duration | `"3s"` | `EDGEQUOTA_RESPONSE_CACHE_REDIS_WRITE_TIMEOUT` | Write timeout |

TLS sub-fields: `EDGEQUOTA_RESPONSE_CACHE_REDIS_TLS_ENABLED`, `EDGEQUOTA_RESPONSE_CACHE_REDIS_TLS_INSECURE_SKIP_VERIFY`.

```yaml
response_cache_redis:
  endpoints:
    - "cdn-cache-primary:6379"
    - "cdn-cache-replica:6379"
  mode: "replication"
  pool_size: 30
```

### `events` — Usage Event Emission

EdgeQuota can emit rate-limit decisions as usage events to an external HTTP or gRPC service (webhook pattern). Events are batched, buffered, and sent asynchronously — they never block the request hot path.

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `enabled` | bool | `false` | `EDGEQUOTA_EVENTS_ENABLED` | Enable usage event emission |
| `batch_size` | int | `100` | `EDGEQUOTA_EVENTS_BATCH_SIZE` | Number of events per batch |
| `flush_interval` | duration | `"5s"` | `EDGEQUOTA_EVENTS_FLUSH_INTERVAL` | Maximum time between flushes |
| `buffer_size` | int | `10000` | `EDGEQUOTA_EVENTS_BUFFER_SIZE` | Ring buffer capacity. Oldest events are dropped on overflow. |
| `max_retries` | int | `3` | `EDGEQUOTA_EVENTS_MAX_RETRIES` | Number of send attempts per batch before giving up |
| `retry_backoff` | duration | `"100ms"` | `EDGEQUOTA_EVENTS_RETRY_BACKOFF` | Initial retry backoff (doubles on each attempt) |

### `events.http` — HTTP Event Receiver

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `url` | string | `""` | `EDGEQUOTA_EVENTS_HTTP_URL` | URL of the HTTP events receiver (receives POST with JSON) |
| `headers` | map[string]string | `{}` | — | Custom headers attached to every events HTTP request. Values are treated as secrets (redacted in logs and `/config`). See below for details. |

#### Custom Headers

The `headers` map allows you to attach arbitrary headers to every events HTTP request. Common use cases:

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

**Security restrictions**: The following headers are rejected at startup to prevent breaking HTTP transport semantics:

`Host`, `Content-Type`, `Content-Length`, `Transfer-Encoding`, `Connection`, `Te`, `Upgrade`, `Proxy-Authorization`, `Proxy-Connection`, `Keep-Alive`, `Trailer`

`Content-Type` is always set to `application/json` by EdgeQuota and cannot be overridden.

Header values are stored using `RedactedString`, so they are masked in logs, debug output, and the `/v1/config` admin endpoint — safe for tokens and API keys.

#### Deprecated Fields

The following fields are deprecated and will be removed in a future release. Use `headers` instead.

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `auth_token` | string | `""` | `EDGEQUOTA_EVENTS_HTTP_AUTH_TOKEN` | **Deprecated.** Migrated to `headers[auth_header]` with `Bearer ` prefix. |
| `auth_header` | string | `"Authorization"` | `EDGEQUOTA_EVENTS_HTTP_AUTH_HEADER` | **Deprecated.** Header name for `auth_token`. Defaults to `Authorization`. |

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

### `events.grpc` — gRPC Event Receiver

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `address` | string | `""` | `EDGEQUOTA_EVENTS_GRPC_ADDRESS` | Address of the gRPC events service |

### `logging` — Structured Logging

| Field | Type | Default | Env Var | Description |
|-------|------|---------|---------|-------------|
| `level` | string | `"info"` | `EDGEQUOTA_LOGGING_LEVEL` | Log level: `debug`, `info`, `warn`, `error` |
| `format` | string | `"json"` | `EDGEQUOTA_LOGGING_FORMAT` | Output format: `json`, `text` |
| `access_log_enabled` | bool | `true` | `EDGEQUOTA_LOGGING_ACCESS_LOG_ENABLED` | Emit a structured access log line for every proxied request. See [Observability](observability.md). |

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
| External RL disabled and `rate_limit.static.backend_url` empty | `rate_limit.static.backend_url is required when external RL is disabled` |
| External RL enabled and `rate_limit.external.fallback.backend_url` empty | `rate_limit.external.fallback.backend_url is required when external RL is enabled` |
| `backend_url` has no scheme or host | `invalid backend_url: scheme and host are required` |
| Any duration field is not a valid Go duration | `invalid <field> "<value>"` |
| TLS enabled but `cert_file` or `key_file` missing | `server.tls.cert_file and server.tls.key_file are required` |
| HTTP/3 enabled without TLS | `server.tls.http3_enabled requires server.tls.enabled` |
| `min_version` not `1.2` or `1.3` | `invalid server.tls.min_version` |
| Auth enabled but no backend URL | `auth.http.url or auth.grpc.address is required` |
| `static.average < 0` | `rate_limit.static.average must be >= 0` |
| `failure_policy` not recognized | `invalid rate_limit.failure_policy` |
| `failure_code` not 4xx or 5xx | `invalid rate_limit.failure_code` |
| `header`/`composite` strategy without `header_name` | `header_name is required` |
| External RL enabled without backend | `rate_limit.external requires http.url or grpc.address` |
| External RL enabled without `fallback.key_strategy` | `rate_limit.external.fallback.key_strategy is required` |
| External RL enabled with `fallback.average <= 0` | `rate_limit.external.fallback.average must be > 0` |
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
  timeout: "30s"
  max_idle_conns: 100
rate_limit:
  static:
    backend_url: "http://my-backend:8080"
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
   - Rate-limit parameters (`rate_limit.static.average`, `burst`, `period`, `min_ttl`, `failure_policy`, `failure_code`)
   - Key strategy (`rate_limit.static.key_strategy`: type, header name, trusted proxies)
   - Auth client (reconnects if URL/TLS config changed)
   - External rate-limit client (reconnects if URL/TLS config changed)
   - TLS certificates (via `GetCertificate` — new connections pick up the new cert immediately)

Rapid file writes are debounced (300ms) to handle editors that use atomic rename-on-save.

**Note:** Redis connection parameters and server listen addresses are NOT hot-reloaded; they require a restart.

---

## See Also

- [Getting Started](getting-started.md) -- Quick start guide with minimal configs.
- [Multi-Protocol Proxy](proxy.md) -- Backend protocol selection and transport tuning.
- [Rate Limiting](rate-limiting.md) -- Algorithm details and external rate limit service.
- [Response Caching](caching.md) -- CDN-style response caching semantics and configuration.
- [Authentication](authentication.md) -- Auth flow and security model.
- [Events](events.md) -- Usage event emission.
- [Security](security.md) -- Backend URL policy and SSRF protection.
- [Deployment](deployment.md) -- Production deployment and hot-reload behavior.

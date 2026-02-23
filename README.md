# EdgeQuota

> **Developer Preview** — This project is under active development and is **not production-ready**. APIs, configuration formats, and behavior may change without notice. If you choose to deploy EdgeQuota in a production environment, you do so at your own risk and accept full responsibility for any consequences. We strongly recommend thorough testing in a staging environment before any production use.

**Edge-native distributed quota enforcement and authentication orchestration for Kubernetes.**

EdgeQuota is a high-performance reverse proxy that sits at the edge of your infrastructure — before your ingress controller, API gateway, or core services — enforcing rate limits and validating authentication at line speed.

It is purpose-built for multi-tenant systems that require:

- **Distributed rate limiting** with Redis-backed atomic token buckets.
- **Dynamic quota resolution** via an optional external service.
- **CDN-style response caching** honoring `Cache-Control` from any upstream origin — with tag-based invalidation, conditional requests, and configurable body size limits.
- **Pluggable authentication** forwarding to external auth services.
- **Multi-protocol proxying** over HTTP/1.1, HTTP/2, HTTP/3, gRPC, SSE, and WebSocket — all on a single port.
- **Horizontal scalability** with fully stateless instances coordinated through Redis.

EdgeQuota is written in Go with zero CGO dependencies, ships as a distroless container image, and is designed to be the first layer traffic touches in a Kubernetes cluster.

---

## Table of Contents

- [Why EdgeQuota](#why-edgequota)
- [Core Capabilities](#core-capabilities)
- [Architecture](#architecture)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
- [Rate Limiting](#rate-limiting)
- [Response Caching](#response-caching-cdn)
- [Authentication](#authentication)
- [Protocol Support](#protocol-support)
- [Observability](#observability)
- [Deployment](#deployment)
- [Documentation](#documentation)
- [Development](#development)
- [License](#license)

---

## Why EdgeQuota

Traditional rate limiting middleware runs inside your application process. This approach fails at scale:

- **No coordination.** Each instance maintains its own counters, making limits inaccurate across replicas.
- **No tenant isolation.** Per-client or per-tenant limits require custom plumbing in every service.
- **No edge enforcement.** Abusive traffic reaches your application before being rejected.
- **No protocol awareness.** gRPC, WebSocket, and SSE traffic bypasses HTTP-only middleware.

EdgeQuota solves these problems as a dedicated, distributed policy enforcement layer:

| Problem | EdgeQuota Solution |
|---------|--------------------|
| Counters diverge across replicas | Redis-backed atomic token bucket with Lua scripts |
| No tenant-aware rate limits | Configurable key strategies (IP, header, composite) |
| Traffic reaches backend before rejection | Edge-first deployment — sits before ingress |
| gRPC/WebSocket/SSE not covered | Full multi-protocol proxying on a single port |
| Hardcoded limits | Optional external service for dynamic limit resolution |
| No auth at the edge | Optional external auth service with full request forwarding |

---

## Core Capabilities

### Distributed Rate Limiting

- **Token bucket algorithm** implemented as an atomic Redis Lua script — single `EVAL` per request.
- **Key extraction strategies:** client IP, request header, or composite (header + path prefix).
- **Redis topologies:** single instance, replication with auto-discovery, Sentinel, and Cluster.
- **Failure policies:** pass through, fail closed, or in-memory fallback when Redis is unavailable.
- **Automatic recovery:** exponential backoff reconnection with jitter.
- **Config hot-reload:** rate limits, key strategies, auth clients, and TLS certificates are reloaded on-the-fly when the config file changes — zero downtime.

### External Authentication

- Forward the full incoming request to an external auth service via HTTP or gRPC.
- The auth service decides allow/deny and can inject custom response headers and status codes.
- Fail-open or fail-closed behavior is configurable.

### Dynamic Quota Resolution

- Query an external service for per-request rate limits based on the extracted key.
- Supports multi-tenant use cases where different tenants have different quotas.
- **Tenant-aware backend routing:** the external service must return `backend_url` in every response (required when external RL is enabled). Responses without it trigger the fallback path. Configurable `fallback.backend_url` for when the external service is unreachable.
- Responses are cached locally with a configurable TTL to minimize external calls.

### Response Caching (CDN)

- Cache backend responses in Redis, honoring standard `Cache-Control` semantics (`max-age`, `no-store`, `no-cache`, `private`, `public`).
- Purely response-driven — backends opt in via `Cache-Control` headers. No implicit caching.
- Tag-based invalidation via `Surrogate-Key` / `Cache-Tag` headers and `POST /v1/cache/purge/tags`.
- Conditional request support with `ETag` / `Last-Modified` (304 revalidation).
- Configurable body size limits (`max_body_size`, default 1 MB).
- Optional dedicated `response_cache_redis` for separating cache workloads from rate-limit counters.

### Multi-Protocol Reverse Proxy

- HTTP/1.1, HTTP/2 (h2c), HTTP/3 (QUIC), gRPC, Server-Sent Events, and WebSocket.
- Protocol-aware transport selection: HTTP/2 requests use a dedicated HTTP/2 transport end-to-end.
- All protocols served on a single port with automatic detection.

### Observability

- Prometheus metrics on a dedicated admin port.
- Structured JSON logging via `log/slog`.
- OpenTelemetry tracing with OTLP export.
- Kubernetes probe endpoints: `/startz`, `/healthz`, `/readyz`.

---

## Architecture

### Request Flow

Every incoming request passes through a three-stage middleware chain. Each stage can short-circuit the request and return a response directly to the client.

```
  Client
    │
    ▼
┌─────────┐      ┌──────────────┐      ┌───────────────┐      ┌─────────┐
│  Auth   │─────►│  Rate Limit  │─────►│ Reverse Proxy │─────►│ Backend │
│(optional)│      │              │      │               │      │         │
└────┬────┘      └──────┬───────┘      └───────────────┘      └─────────┘
     │                  │
     │ deny → 401/403   │ deny → 429
     │ to client        │ to client
```

1. **Auth** (optional) — Forwards the request to an external auth service (HTTP or gRPC). On deny, returns the auth service's status code and headers directly.
2. **Rate Limit** — Evaluates the request against the token bucket for the extracted key. On deny, returns `429 Too Many Requests` with standard rate-limit headers.
3. **Reverse Proxy** — Forwards the request to the backend over the appropriate protocol (HTTP/1.1, HTTP/2, HTTP/3, gRPC, SSE, or WebSocket), detected automatically.

### External Dependencies

The middleware stages above rely on up to three backing services. Only Redis is required (and only when rate limiting is enabled); the other two are opt-in.

- **Redis** -- Stores rate-limit counters via an atomic Lua token-bucket script. Supports single-instance, replication, Sentinel, and Cluster topologies. If Redis becomes unavailable, the configured failure policy takes over: pass-through, fail-closed, or in-memory fallback with automatic reconnection.
- **External Auth Service** (optional) -- Validates requests before rate limiting. Supports HTTP and gRPC. See [Authentication](#authentication).
- **External Limits Service** (optional) -- Resolves per-key quotas dynamically for multi-tenant use cases. Responses are cached with a configurable TTL. See [Dynamic Quota Resolution](#dynamic-quota-resolution).

### Admin Server (`:9090`)

A separate listener, isolated from proxy traffic, exposes operational endpoints:

- `GET /startz` -- Startup probe. Returns 200 once initialization completes, 503 before.
- `GET /healthz` -- Liveness probe. Always returns 200 while the process is running.
- `GET /readyz` -- Readiness probe. Returns 503 during startup and graceful drain. Pass `?deep=true` to actively ping Redis.
- `GET /metrics` -- Prometheus metrics endpoint.

See [docs/architecture.md](docs/architecture.md) for the full design, failure modes, and scaling model.

---

## Quick Start

### Binary

```bash
# Build
make build

# Run with a config file
EDGEQUOTA_CONFIG_FILE=config.example.yaml ./bin/edgequota

# Run with environment variables only
EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL=http://my-backend:8080 \
EDGEQUOTA_RATE_LIMIT_STATIC_AVERAGE=100 \
EDGEQUOTA_RATE_LIMIT_STATIC_BURST=50 \
./bin/edgequota
```

### Docker

```bash
# Build the image
make docker

# Run
docker run --rm -p 8080:8080 -p 9090:9090 \
  -e EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL=http://backend:8080 \
  -e EDGEQUOTA_RATE_LIMIT_STATIC_AVERAGE=100 \
  -e EDGEQUOTA_RATE_LIMIT_STATIC_BURST=50 \
  edgequota:dev
```

### Kubernetes

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: edgequota-config
data:
  config.yaml: |
    server:
      address: ":8080"
    admin:
      address: ":9090"
    backend:
      timeout: "30s"
      max_idle_conns: 100
    rate_limit:
      static:
        backend_url: "http://my-backend-service:8080"
        average: 100
        burst: 50
        period: "1s"
        key_strategy:
          type: "header"
          header_name: "X-Tenant-Id"
    redis:
      endpoints:
        - "redis:6379"
      mode: "single"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: edgequota
spec:
  replicas: 3
  selector:
    matchLabels:
      app: edgequota
  template:
    metadata:
      labels:
        app: edgequota
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9090"
        prometheus.io/path: "/metrics"
    spec:
      containers:
        - name: edgequota
          image: edgequota:latest
          ports:
            - containerPort: 8080
              name: proxy
            - containerPort: 9090
              name: admin
          volumeMounts:
            - name: config
              mountPath: /etc/edgequota
          startupProbe:
            httpGet:
              path: /startz
              port: admin
            failureThreshold: 30
            periodSeconds: 1
          livenessProbe:
            httpGet:
              path: /healthz
              port: admin
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /readyz
              port: admin
            periodSeconds: 5
          resources:
            requests:
              cpu: 100m
              memory: 64Mi
            limits:
              cpu: "1"
              memory: 256Mi
      volumes:
        - name: config
          configMap:
            name: edgequota-config
```

See [docs/deployment.md](docs/deployment.md) for production sizing, autoscaling, and high-availability guidance.

---

## Configuration

EdgeQuota is configured via a YAML file (default: `/etc/edgequota/config.yaml`) and/or environment variables. **Environment variables always override file values.**

| YAML Path | Environment Variable | Default |
|---|---|---|
| `server.address` | `EDGEQUOTA_SERVER_ADDRESS` | `:8080` |
| `rate_limit.static.backend_url` | `EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL` | *(required when external RL disabled)* |
| `rate_limit.external.fallback.backend_url` | `EDGEQUOTA_RATE_LIMIT_EXTERNAL_FALLBACK_BACKEND_URL` | *(required when external RL enabled)* |
| `rate_limit.static.average` | `EDGEQUOTA_RATE_LIMIT_STATIC_AVERAGE` | `0` (disabled) |
| `rate_limit.static.burst` | `EDGEQUOTA_RATE_LIMIT_STATIC_BURST` | `1` |
| `rate_limit.failure_policy` | `EDGEQUOTA_RATE_LIMIT_FAILURE_POLICY` | `passThrough` |
| `redis.endpoints` | `EDGEQUOTA_REDIS_ENDPOINTS` | `localhost:6379` |
| `redis.mode` | `EDGEQUOTA_REDIS_MODE` | `single` |

Set the config file path with `EDGEQUOTA_CONFIG_FILE` (default: `/etc/edgequota/config.yaml`).

See [config.example.yaml](config.example.yaml) for all options, or [docs/configuration.md](docs/configuration.md) for the full reference.

---

## Rate Limiting

EdgeQuota uses a **distributed token bucket** algorithm executed as a single atomic Redis Lua script. Each request costs one token; tokens refill at a configurable rate.

### Key Strategies

| Strategy | Key Source | Example Key | Config |
|---|---|---|---|
| `clientIP` | X-Forwarded-For, X-Real-IP, RemoteAddr | `10.0.0.1` | `type: "clientIP"` |
| `header` | Named request header | `tenant-123` | `type: "header"`, `header_name: "X-Tenant-Id"` |
| `composite` | Header + first path segment | `tenant-123:api` | `type: "composite"`, `header_name: "X-Tenant-Id"`, `path_prefix: true` |
| `global` | Fixed key (all requests share one bucket) | `global` | `type: "global"` (optionally `global_key: "custom"`) |

### Failure Policies

| Policy | Redis Down Behavior |
|---|---|
| `passThrough` | Allow all requests (default) |
| `failClosed` | Reject with configured status code |
| `inMemoryFallback` | Use local per-instance token bucket |

A background recovery loop automatically reconnects to Redis with exponential backoff (1s–30s) and jitter.

### Redis Topologies

| Mode | Description |
|---|---|
| `single` | Single Redis instance |
| `replication` | Auto-discovers master via `ROLE` command; retries on `READONLY` |
| `sentinel` | Redis Sentinel with automatic failover |
| `cluster` | Redis Cluster with slot-based sharding |

### External Rate Limit Service

When `rate_limit.external.enabled` is `true`, EdgeQuota queries an external service for **per-request** rate limits instead of using the static `rate_limit.static` values. This enables multi-tenant scenarios where each tenant has different quotas managed in a backend database.

**No key in request:** When external RL is enabled, EdgeQuota skips local key extraction and does not send a `key` field. The request contains only `headers`, `method`, and `path`. The external service derives tenant keys from these — for example, from `X-Tenant-Id`, path segments, or host-based routing.

**Fallback block:** When the external service is unreachable (timeout, circuit open, etc.), EdgeQuota uses `rate_limit.external.fallback` — a required block with `average`, `burst`, `period`, and `key_strategy`. This ensures predictable behavior during outages.

**HTTP example:**

```yaml
rate_limit:
  external:
    enabled: true
    timeout: "5s"
    cache_ttl: "60s"
    max_concurrent_requests: 50
    http:
      url: "http://limits-service:8080/limits"
    header_filter:
      allow_list:
        - "X-Tenant-Id"
        - "X-Plan"
    fallback:
      backend_url: "http://my-backend:8080"
      average: 100
      burst: 50
      period: "1s"
      key_strategy:
        type: "header"
        header_name: "X-Tenant-Id"
```

**gRPC example:**

```yaml
rate_limit:
  external:
    enabled: true
    timeout: "5s"
    cache_ttl: "60s"
    grpc:
      address: "limits-service:50052"
      tls:
        enabled: true
        ca_file: "/etc/edgequota/tls/limits-ca.pem"
    fallback:
      backend_url: "http://my-backend:8080"
      average: 5000
      burst: 2000
      period: "1s"
      key_strategy:
        type: "global"
        global_key: "fe-assets"
```

When external RL is enabled, the `static` block is not used. Configure only `external` with its required `fallback`.

The external service receives the extracted key, request headers, method, and path, and returns dynamic limits:

| Response Field | Description |
|---|---|
| `average`, `burst`, `period` | Override `rate_limit.static` parameters |
| `tenant_key` | Custom Redis bucket key (replaces the extracted key) |
| `backend_url` | Per-request backend URL (required; responses without it use fallback) |
| `failure_policy`, `failure_code` | Per-tenant Redis-down behavior |
| `cache_max_age_seconds`, `cache_no_store` | Response-level cache control |

Responses are cached in Redis (shared across all instances) with singleflight deduplication. A per-key **circuit breaker** opens after 5 consecutive failures and falls back to stale cache, then to the `rate_limit.external.fallback` values (or `rate_limit.static` when fallback is not used).

See [docs/rate-limiting.md](docs/rate-limiting.md) for algorithm details, the full external service protocol, caching semantics, and distributed correctness analysis. See [docs/deployment.md](docs/deployment.md) for production topology and deployment scenarios.

---

## Authentication

When enabled, EdgeQuota forwards every incoming request to an external auth service **before** rate limiting and proxying. Both HTTP and gRPC backends are supported; configure exactly one.

### HTTP Auth

```yaml
auth:
  enabled: true
  timeout: "5s"
  failure_policy: "failclosed"   # failclosed | failopen
  http:
    url: "http://auth-service:8080/check"
```

EdgeQuota sends a `POST` with JSON:

```json
{
  "method": "GET",
  "path": "/api/v1/resource",
  "headers": {"Authorization": "Bearer token123"},
  "remote_addr": "10.0.0.1:5555"
}
```

- **200** → Request allowed; proceed to rate limiting. The response body can optionally return `request_headers` to inject headers into the upstream request.
- **Any other status** → Request denied; the auth service's status code, headers, and body are forwarded to the client.

### gRPC Auth

```yaml
auth:
  enabled: true
  timeout: "5s"
  failure_policy: "failclosed"
  grpc:
    address: "auth-service:50051"
    tls:
      enabled: true
      ca_file: "/etc/edgequota/tls/auth-ca.pem"
```

Uses `edgequota.auth.v1.AuthService/Check` with a JSON codec — the auth service can be implemented without protobuf code generation. See `api/proto/edgequota/auth/v1/auth.proto` for the formal definition.

### Failure Policy

| Policy | Auth unreachable / timeout / circuit open |
|--------|-------------------------------------------|
| `failclosed` (default) | Deny with `503 Service Unavailable` |
| `failopen` | Allow request; skip auth |

A built-in **circuit breaker** opens after 5 consecutive auth failures and stays open for 30 s, preventing cascading timeouts. While open, the failure policy applies immediately without waiting for the timeout.

### Header Filtering

By default a set of sensitive headers (`Authorization`, `Cookie`, `X-Api-Key`, etc.) is forwarded to the auth service. You can override this behavior:

```yaml
auth:
  header_filter:
    deny_list: ["Cookie", "Set-Cookie"]   # never forward these
    # allow_list: ["Authorization"]        # exclusive: only these (deny_list ignored)
```

See [docs/authentication.md](docs/authentication.md) for the full flow, security model, and example auth service implementation.

---

## Protocol Support

All protocols are served on a single port (`:8080` by default).

| Protocol | Detection | Notes |
|---|---|---|
| HTTP/1.1 | Default | Full request/response proxying |
| HTTP/2 | `ProtoMajor >= 2` | Cleartext (h2c) via protocol upgrade; dedicated HTTP/2 transport |
| HTTP/3 | QUIC (UDP) | Requires TLS; `Alt-Svc` header advertised automatically |
| gRPC | HTTP/2 + `Content-Type: application/grpc` | TE: trailers preserved through the proxy |
| SSE | `Accept: text/event-stream` | Immediate flush; streaming support |
| WebSocket | `Upgrade: websocket` | Connection hijack + bidirectional TCP relay |

---

## Observability

### Prometheus Metrics (`:9090/metrics`)

| Metric | Type | Description |
|---|---|---|
| `edgequota_requests_allowed_total` | Counter | Requests that passed rate limiting |
| `edgequota_requests_limited_total` | Counter | Requests rejected by rate limiting (429) |
| `edgequota_redis_errors_total` | Counter | Redis operation errors |
| `edgequota_fallback_used_total` | Counter | In-memory fallback activations |
| `edgequota_auth_errors_total` | Counter | Auth service call errors |
| `edgequota_auth_denied_total` | Counter | Requests denied by auth service |
| `edgequota_key_extract_errors_total` | Counter | Key extraction failures |
| `edgequota_request_duration_seconds` | Histogram | Request latency by method and status code |

### Health Probes (`:9090`)

| Endpoint | Probe | Description |
|---|---|---|
| `GET /startz` | Startup | 200 once initialization is complete; 503 otherwise |
| `GET /healthz` | Liveness | Always returns 200 while process is alive |
| `GET /readyz` | Readiness | 200 when ready; 503 during graceful drain |

See [docs/observability.md](docs/observability.md) for metric definitions and suggested Grafana dashboards.

---

## Deployment

EdgeQuota is designed for **Kubernetes edge deployment**:

- **Stateless.** All coordination happens through Redis. Scale horizontally without reservation.
- **Distroless container.** No shell, no package manager, non-root user (UID 65534).
- **Startup/liveness/readiness probes.** Kubernetes-native lifecycle management.
- **Graceful shutdown.** Configurable drain timeout; connections are drained before exit.
- **Resource-efficient.** Typical production footprint: 64–256 Mi memory, 0.1–1 CPU.

See [docs/deployment.md](docs/deployment.md) for production topology, autoscaling guidance, and high-availability patterns.

---

## Documentation

Browse the full documentation at [docs/index.md](docs/index.md) or use the table below:

| Document | Description |
|---|---|
| [docs/index.md](docs/index.md) | Documentation home and navigation |
| [docs/getting-started.md](docs/getting-started.md) | Quick start guide for binary, Docker, and Kubernetes |
| [docs/configuration.md](docs/configuration.md) | Full configuration reference with every field documented |
| [docs/rate-limiting.md](docs/rate-limiting.md) | Algorithm, distributed correctness, Redis atomicity, key naming |
| [docs/caching.md](docs/caching.md) | CDN-style response caching, Cache-Control, invalidation, conditional requests |
| [docs/authentication.md](docs/authentication.md) | Auth flow, security model, timeout and retry strategy |
| [docs/proxy.md](docs/proxy.md) | Multi-protocol proxy: HTTP/1.1, HTTP/2, HTTP/3, gRPC, SSE, WebSocket |
| [docs/events.md](docs/events.md) | Async usage event emission via HTTP/gRPC webhooks |
| [docs/deployment.md](docs/deployment.md) | Kubernetes deployment, HA, sizing, autoscaling |
| [docs/observability.md](docs/observability.md) | Metrics, access logs, health probes, tracing, Grafana dashboards |
| [docs/security.md](docs/security.md) | Threat model, SSRF protection, header trust, Redis security, mTLS |
| [docs/go-sdk.md](docs/go-sdk.md) | Go SDK for building external rate limit, auth, and events services |
| [docs/helm-chart.md](docs/helm-chart.md) | Helm chart deployment guide |
| [docs/api-reference.md](docs/api-reference.md) | Protocol Buffer and OpenAPI API definitions |
| [docs/architecture.md](docs/architecture.md) | System design, data flow, failure modes, scaling model |
| [docs/troubleshooting.md](docs/troubleshooting.md) | Common issues, debugging techniques, FAQ |

---

## Development

### Prerequisites

- Go 1.26+
- Docker (for container builds)
- golangci-lint v2+ (for linting)
- [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen) v2.5+ (for OpenAPI type generation)
- [buf](https://buf.build) (for protobuf generation)
- Minikube + Terraform (for E2E tests)

### Build and Test

```bash
make build           # Compile binary to bin/edgequota
make lint            # Run golangci-lint
make test            # Run unit tests with coverage
make coverage        # Run tests with race detector; enforce >=80% coverage
make docker          # Build Docker image
make e2e             # Full E2E cycle: minikube + terraform + tests + teardown
```

### Code Generation

EdgeQuota defines its external service APIs (auth, rate limit, events) as both Protocol Buffer and OpenAPI 3.0 specs. Go types are generated from both:

```bash
make generate        # Run all code generation (proto + OpenAPI)
make proto           # Generate gRPC stubs from .proto files (requires buf)
make generate-http   # Generate HTTP model types from OpenAPI specs (requires oapi-codegen)
```

Generated code lives under `api/gen/` and must not be edited manually:

- `api/gen/grpc/` — gRPC client/server stubs from Protocol Buffers
- `api/gen/http/` — HTTP request/response types from OpenAPI specs

### Project Structure

```
cmd/edgequota/           Main entry point
internal/
  config/                Configuration loading (YAML + env overrides)
  redis/                 Redis client factory (single, replication, sentinel, cluster)
  ratelimit/             Token-bucket limiter, in-memory fallback, key strategies, external service
  auth/                  External authentication client (HTTP + gRPC)
  events/                Async usage event emitter
  proxy/                 Multi-protocol reverse proxy (HTTP, gRPC, SSE, WebSocket)
  middleware/            Request processing pipeline (auth → rate limit → proxy)
  observability/         Metrics, health probes, structured logging, tracing
  server/                Server orchestration and lifecycle management
api/
  proto/                 gRPC service definitions (.proto files)
  openapi/               OpenAPI 3.0 specs (HTTP wire format)
  gen/                   Generated code (DO NOT edit)
    grpc/                Generated from proto (buf generate)
    http/                Generated from OpenAPI (oapi-codegen)
e2e/                     End-to-end tests (minikube + terraform)
docs/                    Project documentation
```

---

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.

# EdgeQuota

**Edge-native distributed quota enforcement and authentication orchestration for Kubernetes.**

EdgeQuota is a high-performance reverse proxy that sits at the edge of your infrastructure — before your ingress controller, API gateway, or core services — enforcing rate limits and validating authentication at line speed.

It is purpose-built for multi-tenant systems that require:

- **Distributed rate limiting** with Redis-backed atomic token buckets.
- **Dynamic quota resolution** via an optional external service.
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
- Responses are cached locally with a configurable TTL to minimize external calls.

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

```
                 ┌──────────────────────────────────────────────────────────────┐
                 │                        EdgeQuota                             │
                 │                                                              │
  Incoming ─────►│  Auth ─────► Rate Limit ─────► Reverse Proxy ──────────────►│──► Backend
  Requests       │   │              │                   │                       │    Service
                 │   ▼              ▼                   ▼                       │
                 │  External   Redis (Lua)         HTTP/1.1, HTTP/2            │
                 │  Auth Svc   + In-Memory         HTTP/3, gRPC                │
                 │             Fallback            SSE, WebSocket              │
                 └──────────────────────────────────────────────────────────────┘
                      │                                        │
                      ▼                                        ▼
                 Optional:                               Admin Server (:9090)
                 Dynamic Limits Svc                      /startz  /healthz  /readyz  /metrics
```

The middleware chain executes in order: **Auth** (optional) → **Rate Limit** → **Proxy**. Each step can short-circuit the request. See [docs/architecture.md](docs/architecture.md) for the full design.

---

## Quick Start

### Binary

```bash
# Build
make build

# Run with a config file
EDGEQUOTA_CONFIG_FILE=config.example.yaml ./bin/edgequota

# Run with environment variables only
EDGEQUOTA_BACKEND_URL=http://my-backend:8080 \
EDGEQUOTA_RATE_LIMIT_AVERAGE=100 \
EDGEQUOTA_RATE_LIMIT_BURST=50 \
./bin/edgequota
```

### Docker

```bash
# Build the image
make docker

# Run
docker run --rm -p 8080:8080 -p 9090:9090 \
  -e EDGEQUOTA_BACKEND_URL=http://backend:8080 \
  -e EDGEQUOTA_RATE_LIMIT_AVERAGE=100 \
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
      url: "http://my-backend-service:8080"
    rate_limit:
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
| `backend.url` | `EDGEQUOTA_BACKEND_URL` | *(required)* |
| `rate_limit.average` | `EDGEQUOTA_RATE_LIMIT_AVERAGE` | `0` (disabled) |
| `rate_limit.burst` | `EDGEQUOTA_RATE_LIMIT_BURST` | `1` |
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

See [docs/rate-limiting.md](docs/rate-limiting.md) for algorithm details, distributed correctness analysis, and key naming strategy.

---

## Authentication

When enabled, EdgeQuota forwards every incoming request to an external auth service **before** rate limiting and proxying.

### HTTP Auth

The auth service receives a `POST` with JSON:

```json
{
  "method": "GET",
  "path": "/api/v1/resource",
  "headers": {"Authorization": "Bearer token123"},
  "remote_addr": "10.0.0.1:5555"
}
```

- **200** → Request allowed; proceed to rate limiting.
- **Any other status** → Request denied; response headers and body are forwarded to the client.

### gRPC Auth

Uses `edgequota.auth.v1.AuthService/Check` with a JSON codec. See `api/proto/auth/v1/auth.proto`.

See [docs/authentication.md](docs/authentication.md) for the full flow, timeout handling, and fail-open/fail-closed implications.

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

| Document | Description |
|---|---|
| [docs/architecture.md](docs/architecture.md) | System design, data flow, failure modes, scaling model |
| [docs/rate-limiting.md](docs/rate-limiting.md) | Algorithm, distributed correctness, Redis atomicity, key naming |
| [docs/authentication.md](docs/authentication.md) | Auth flow, security model, timeout and retry strategy |
| [docs/configuration.md](docs/configuration.md) | Full configuration reference with every field documented |
| [docs/deployment.md](docs/deployment.md) | Kubernetes deployment, HA, sizing, autoscaling |
| [docs/observability.md](docs/observability.md) | Metrics, logging, tracing, Grafana dashboards |
| [docs/security.md](docs/security.md) | Threat model, header trust, Redis security, mTLS |

---

## Development

### Prerequisites

- Go 1.25+
- Docker (for container builds)
- golangci-lint v2+ (for linting)
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

### Project Structure

```
cmd/edgequota/           Main entry point
internal/
  config/                Configuration loading (YAML + env overrides)
  redis/                 Redis client factory (single, replication, sentinel, cluster)
  ratelimit/             Token-bucket limiter, in-memory fallback, key strategies, external service
  auth/                  External authentication client (HTTP + gRPC)
  proxy/                 Multi-protocol reverse proxy (HTTP, gRPC, SSE, WebSocket)
  middleware/            Request processing pipeline (auth → rate limit → proxy)
  observability/         Metrics, health probes, structured logging, tracing
  server/                Server orchestration and lifecycle management
api/proto/               gRPC service definitions (.proto files)
e2e/                     End-to-end tests (minikube + terraform)
docs/                    Project documentation
```

---

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.

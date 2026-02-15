# Architecture

This document describes the internal architecture of EdgeQuota, its data flow, failure modes, scaling model, and design decisions.

---

## System Overview

EdgeQuota is a standalone reverse proxy that enforces rate limits and authentication at the edge of a Kubernetes cluster. It receives all inbound traffic, applies policy decisions, and forwards allowed requests to the configured backend.

```
                         Internet / Internal Clients
                                    │
                                    ▼
                    ┌───────────────────────────────┐
                    │         EdgeQuota Pod          │
                    │                               │
                    │  ┌─────────────────────────┐  │
                    │  │   h2c Handler (HTTP/2)   │  │
                    │  │   + HTTP/1.1 fallback    │  │
                    │  │   + HTTP/3 (optional)    │  │
                    │  └────────────┬────────────┘  │
                    │               │               │
                    │  ┌────────────▼────────────┐  │
                    │  │    Middleware Chain      │  │
                    │  │                         │  │
                    │  │  1. Auth Check          │  │
                    │  │     (optional)          │  │
                    │  │         │               │  │
                    │  │  2. Rate Limit          │  │
                    │  │     (Redis + fallback)  │  │
                    │  │         │               │  │
                    │  │  3. Reverse Proxy       │  │
                    │  │     (protocol-aware)    │  │
                    │  └────────────┬────────────┘  │
                    │               │               │
                    │               ▼               │
                    │         Backend Service       │
                    └───────────────────────────────┘
                         │                   │
                         ▼                   ▼
                    Redis Cluster       Admin Server
                    (counters)          :9090/metrics
                                        :9090/healthz
                                        :9090/readyz
                                        :9090/startz
```

---

## Component Architecture

### Entry Point (`cmd/edgequota`)

The binary loads configuration, initializes the logger and tracing, creates the server, and blocks on `Run()`. Shutdown is triggered by `SIGINT` or `SIGTERM` via Go's `signal.NotifyContext`.

### Configuration (`internal/config`)

Configuration is loaded in three layers:

1. **Defaults** — hardcoded sensible values for every field.
2. **YAML file** — loaded from `/etc/edgequota/config.yaml` (or `EDGEQUOTA_CONFIG_FILE`).
3. **Environment variables** — override any field. Naming follows `EDGEQUOTA_<SECTION>_<FIELD>`.

This layering is Kubernetes-friendly: mount a ConfigMap for the base config and use env vars for secrets or per-pod overrides.

The backend URL is normalized at load time to always include an explicit port, so downstream code never guesses default ports.

### Server (`internal/server`)

The server manages two HTTP listeners:

| Listener | Default Port | Purpose |
|----------|-------------|---------|
| Main | `:8080` (TCP + optional UDP for HTTP/3) | Proxy traffic |
| Admin | `:9090` (TCP) | Health probes, Prometheus metrics |

The main listener uses `h2c.NewHandler` to support cleartext HTTP/2 (required for gRPC without TLS) while also accepting HTTP/1.1 connections. When TLS is enabled with `http3_enabled: true`, an additional QUIC/UDP listener is started on the same address.

Startup sequence:

1. Start admin server.
2. Start main TCP server (with optional TLS).
3. Start HTTP/3 UDP server (if enabled).
4. Mark `started` and `ready` on the health checker.
5. Block until context cancellation.

Shutdown sequence:

1. Mark `not ready` (readiness probe starts failing).
2. Wait for `drain_timeout` to allow in-flight requests to complete.
3. Shut down main server, admin server, HTTP/3 server.
4. Close the middleware chain (Redis connections, fallback limiter).
5. Shut down OpenTelemetry tracing.

### Middleware Chain (`internal/middleware`)

The chain executes three stages in sequence. Each stage can short-circuit:

```
Request ──► Auth Check ──► Rate Limit ──► Proxy ──► Backend
               │                │
               ▼                ▼
           Deny (4xx)       429 Too Many Requests
```

**Stage 1: Auth Check** (if `auth.enabled`)

Calls the external auth service with the full request metadata. If the auth service returns anything other than 200, the response is forwarded to the client and the chain stops.

**Stage 2: Rate Limit**

1. If `average == 0` and no external rate limit service: skip (no limits).
2. Extract rate limit key using the configured strategy.
3. If external rate limit service is enabled: fetch dynamic limits (cached with TTL).
4. Execute the Redis Lua script for atomic token-bucket check.
5. If Redis is unreachable: apply the configured failure policy.

**Stage 3: Proxy**

Forward the request to the backend via the protocol-aware reverse proxy.

### Proxy (`internal/proxy`)

The proxy handles four protocols on a single port:

| Protocol | Detection | Handler |
|----------|-----------|---------|
| WebSocket | `Upgrade: websocket` header | Connection hijack + bidirectional TCP relay |
| gRPC | `ProtoMajor >= 2` + `Content-Type: application/grpc` | HTTP/2 transport with TE: trailers |
| HTTP/2 | `ProtoMajor >= 2` | Dedicated `http2.Transport` (h2c) |
| HTTP/1.1 + SSE | Default | `httputil.ReverseProxy` with `FlushInterval: -1` |

Transport selection is handled by `protocolAwareTransport`, which inspects `req.ProtoMajor` to route requests to the appropriate transport. This ensures HTTP/2 requests are forwarded as HTTP/2 end-to-end, not downgraded to HTTP/1.1.

### Rate Limiter (`internal/ratelimit`)

Three components:

1. **Redis Limiter** — Atomic token bucket via Lua `EVAL`. Single round-trip per request. See [rate-limiting.md](rate-limiting.md) for the algorithm.
2. **In-Memory Fallback** — Per-key token bucket with a 65,536-key cap and LRU-style eviction. Activated only when Redis is unavailable and `failure_policy: inMemoryFallback`.
3. **External Service Client** — Optional HTTP/gRPC client that fetches dynamic rate limits per key. Results cached locally with configurable TTL.

### Redis Client (`internal/redis`)

Supports four topologies with a unified `Client` interface:

| Mode | Implementation |
|------|---------------|
| `single` | Standard `go-redis` client |
| `replication` | Custom client with master auto-discovery via `ROLE` command; automatic retry on `READONLY` |
| `sentinel` | `go-redis` failover client |
| `cluster` | `go-redis` cluster client |

The replication client caches the discovered master for 30 seconds and invalidates the cache on `READONLY` errors, triggering re-discovery.

### Auth Client (`internal/auth`)

Supports HTTP (JSON POST) and gRPC (`edgequota.auth.v1.AuthService/Check`) backends. The auth client forwards the full request context — method, path, headers, and remote address — to the external service.

### Observability (`internal/observability`)

Four subsystems:

- **Metrics** — Prometheus counters and histograms with atomic fast-path counters for the hot path.
- **Health** — Startup, liveness, and readiness endpoints backed by atomic flags.
- **Logging** — Structured logging via `log/slog` (JSON or text format).
- **Tracing** — OpenTelemetry with OTLP HTTP export and configurable sample rate.

---

## Data Flow

### Happy Path (Rate Limit Allowed)

```
Client ──► EdgeQuota
              │
              ├── (1) Auth Check → External Auth Service → 200 OK
              │
              ├── (2) Extract Key (e.g., X-Tenant-Id: "tenant-42")
              │
              ├── (3) Redis EVAL tokenBucketScript
              │       Key: "rl:edgequota:tenant-42"
              │       Args: rate, burst, TTL, now
              │       Response: {1, 0} (allowed)
              │
              ├── (4) Proxy → Backend Service
              │
              └── Response → Client
```

### Rate Limited Path

```
Client ──► EdgeQuota
              │
              ├── (1-2) Auth + Key extraction (same as above)
              │
              ├── (3) Redis EVAL → {0, 42000} (denied, retry in 42ms)
              │
              └── 429 Too Many Requests
                  Retry-After: 0.042
```

### Redis Failure Path (passThrough)

```
Client ──► EdgeQuota
              │
              ├── (1-2) Auth + Key extraction
              │
              ├── (3) Redis EVAL → connection error
              │       Policy: passThrough → allow
              │       Background: start recovery loop
              │
              ├── (4) Proxy → Backend Service
              │
              └── Response → Client
```

---

## Failure Modes

| Failure | Impact | Mitigation |
|---------|--------|------------|
| Redis unreachable | Rate limiting unavailable | Configurable policy: passThrough, failClosed, or inMemoryFallback |
| Redis READONLY (replica promoted) | Single request error | Replication client retries once after invalidating master cache |
| Auth service timeout | Request delayed | Configurable timeout; auth can be disabled |
| Auth service down | Requests blocked or passed | Fail-open/fail-closed is determined by the auth service HTTP status |
| Backend unreachable | 502 Bad Gateway | Proxy error handler returns 502; client disconnects are detected |
| Key extraction fails | Request rejected | 500 Internal Server Error; `key_extract_errors_total` metric incremented |
| In-memory fallback full | Eviction of oldest keys | ~10% of keys evicted when 65,536 cap is reached |

### Recovery Loop

When Redis becomes unreachable, EdgeQuota starts a background goroutine that attempts reconnection with exponential backoff:

- Initial delay: 1 second
- Maximum delay: 30 seconds
- Jitter: random up to current delay
- On success: recreate the Redis limiter and resume normal operation

---

## Scaling Model

### Horizontal Scaling

EdgeQuota instances are **fully stateless**. All shared state lives in Redis. You can scale from 1 to N replicas without coordination:

```
                    ┌───────────────┐
Client ──► LB ──►  │ EdgeQuota #1  │──┐
                    └───────────────┘  │
                    ┌───────────────┐  │    ┌─────────┐
Client ──► LB ──►  │ EdgeQuota #2  │──┼───►│  Redis   │
                    └───────────────┘  │    └─────────┘
                    ┌───────────────┐  │
Client ──► LB ──►  │ EdgeQuota #N  │──┘
                    └───────────────┘
```

Rate limit counters are globally consistent because every instance executes the same atomic Lua script on the same Redis key.

### Redis Scaling

| Topology | Best For |
|----------|---------|
| Single | Development, low-traffic |
| Replication | Read-heavy workloads (but rate limiting is write-heavy) |
| Sentinel | High availability without sharding |
| Cluster | High throughput with automatic sharding across slots |

For production multi-tenant deployments with thousands of keys, Redis Cluster distributes keys across shards. The rate limit key prefix (`rl:edgequota:`) is deliberately simple to avoid hash-tag collisions.

### Key Naming Strategy

Rate limit keys follow the pattern:

```
rl:edgequota:<extracted_key>
```

Examples:

| Strategy | Request | Redis Key |
|----------|---------|-----------|
| clientIP | From 10.0.0.1 | `rl:edgequota:10.0.0.1` |
| header | X-Tenant-Id: acme | `rl:edgequota:acme` |
| composite | X-Tenant-Id: acme, path /api/v1 | `rl:edgequota:acme:api` |

Keys are stored as Redis hashes with two fields (`last`, `tokens`) and an automatic TTL.

---

## Latency Considerations

EdgeQuota is designed for microsecond-level overhead on the critical path:

| Operation | Typical Latency |
|-----------|----------------|
| Key extraction | < 1 μs |
| Redis Lua EVAL (single instance, local) | 50–200 μs |
| Redis Lua EVAL (cluster, cross-AZ) | 0.5–2 ms |
| Auth service call (pod-to-pod) | 1–10 ms |
| External rate limit service call | 1–10 ms (cached: 0 μs) |
| Proxy overhead (header copy + routing) | < 50 μs |

The hot path avoids allocations by using atomic counters for metrics. Prometheus counters are incremented alongside atomic counters but are only scraped periodically.

---

## Design Decisions

### Why Token Bucket

Token bucket provides natural burst handling: a client can send up to `burst` requests instantly, then is throttled to `average` requests per period. This is more user-friendly than fixed-window (which can allow 2x the rate at window boundaries) and more memory-efficient than sliding-window log (which requires storing every request timestamp).

### Why Redis Lua

Executing the entire token-bucket logic in a single `EVAL` ensures atomicity without distributed locks. Redis caches the compiled Lua script after the first `EVAL`, so subsequent calls avoid parsing overhead. This is both faster and simpler than multi-step `GET`/`SET` with `WATCH`.

### Why h2c for gRPC

gRPC requires HTTP/2. In Kubernetes, TLS termination typically happens at the ingress controller, so pod-to-pod traffic is cleartext. EdgeQuota uses `h2c.NewHandler` to accept both HTTP/1.1 and HTTP/2 cleartext on the same port, with no TLS configuration required for the common case.

### Why Protocol-Aware Transport

A single `httputil.ReverseProxy` with a `protocolAwareTransport` that inspects `req.ProtoMajor` to select HTTP/1.1 or HTTP/2 transport. This preserves the client's protocol end-to-end without requiring separate ports or listeners for different protocols.

### Why Distroless

The container image is based on `gcr.io/distroless/static-debian12:nonroot`. It contains no shell, no package manager, and runs as a non-root user. This minimizes the attack surface for a network-facing component at the edge.

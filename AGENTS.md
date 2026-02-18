# EdgeQuota — Agent Guide

## Project Overview

EdgeQuota is a high-performance reverse proxy (Go 1.25) that enforces rate limits and external authentication at the edge of a Kubernetes cluster. It sits in front of backend services and applies a middleware chain: **Auth Check → Rate Limit → Reverse Proxy**.

All shared state lives in Redis. EdgeQuota instances are fully stateless and horizontally scalable.

## Repository Layout

```
cmd/edgequota/          Entry point (main.go)
internal/
  auth/                 External auth client (HTTP + gRPC)
  config/               YAML + env config loading, hot-reload via fsnotify
  events/               Usage event emission
  middleware/            Auth → Rate Limit → Proxy pipeline
  observability/        Metrics, health, logging, tracing
  proxy/                Multi-protocol reverse proxy (HTTP/1.1, HTTP/2, HTTP/3, gRPC, WebSocket, SSE)
  ratelimit/            Token bucket (Redis Lua), in-memory fallback, external service client
  redis/                Redis client (single, replication, sentinel, cluster)
  server/               Server orchestration, graceful shutdown
api/
  proto/                Protobuf definitions (auth, events, ratelimit services)
  gen/                  Generated Go code from protos (DO NOT edit manually)
docs/                   Architecture, authentication, configuration, deployment, observability, rate-limiting, security
e2e/                    End-to-end tests (minikube + Terraform + Go test orchestrator)
  terraform/            K8s infrastructure modules (Redis topologies, test backends, TLS certs)
  mockextrl/            Mock external rate limit service
  testbackend/          Multi-protocol test backend (HTTP, gRPC, SSE, WebSocket)
  reports/              Test report output (JSON + Markdown)
```

## Protobuf Workflow

Proto definitions live in `api/proto/edgequota/`. Generated Go code goes to `api/gen/`.

**After any change to `.proto` files, always run:**

```sh
make proto
```

This runs `buf lint`, `buf breaking` (against main branch), and `buf generate`. Never edit files under `api/gen/` manually — they are overwritten by code generation.

Proto services:
- `auth/v1/auth.proto` — `AuthService.Check`
- `events/v1/events.proto` — `EventService.PublishEvents`
- `ratelimit/v1/ratelimit.proto` — `RateLimitService.GetLimits`

Buf configuration: `buf.yaml` (module + lint rules) and `buf.gen.yaml` (plugins + output paths).

## Build & Development

| Command | Purpose |
|---------|---------|
| `make build` | Compile binary to `bin/edgequota` |
| `make lint` | Run golangci-lint (see `.golangci.yml` for enabled linters) |
| `make fmt` | Run gofumpt formatter |
| `make test` | Unit tests with verbose output and coverage |
| `make coverage` | Tests with race detector, enforces >= 80% total coverage |
| `make coverage-per-package` | Enforces >= 70% on core packages (middleware, ratelimit, proxy, redis, auth) |
| `make vulncheck` | govulncheck for known vulnerabilities |
| `make proto` | Regenerate Go code from proto definitions |
| `make docker` | Build Docker image |
| `make e2e` | Full E2E cycle (minikube + Terraform + tests + teardown) |
| `make e2e-setup` | Provision minikube and deploy infrastructure |
| `make e2e-test` | Run E2E tests (cluster must be up) |
| `make e2e-teardown` | Destroy cluster and resources |

## Code Style & Linting

- Formatter: **gofumpt** (stricter than gofmt). Run `make fmt` before committing.
- Linter config: `.golangci.yml` — uses golangci-lint v2 with govet, errcheck, staticcheck, revive, gosec, funlen (60 statements max), gocyclo (15 complexity max), and more.
- Test files have relaxed lint rules (errcheck, funlen, gosec, etc. are disabled).
- Use `log/slog` for structured logging — no `fmt.Println` or `log.Printf`.
- Context should be threaded through the call chain; functions that do I/O accept `context.Context`.

## Testing Conventions

- Unit tests use Go's standard `testing` package.
- Tests run with `-race` in CI. Avoid data races.
- Coverage thresholds: **80% total**, **70% per core package**.
- Benchmarks exist for `internal/middleware` and `internal/ratelimit` — run with `make bench`.
- E2E tests orchestrate a minikube cluster with Terraform, deploying multiple Redis topologies and EdgeQuota configurations. They test rate limiting, auth, protocol support, config hot-reload, and failure modes.

## Configuration

Configuration loads in three layers: **defaults → YAML file → environment variables** (`EDGEQUOTA_*` prefix).

See `config.example.yaml` for the full reference and `docs/configuration.md` for details. Hot-reload is supported for rate limit settings, key strategy, auth/external clients, and TLS certificates (via fsnotify).

## Architecture Reference

Read these docs for deeper context:
- `docs/architecture.md` — Component design, data flow, failure modes, scaling model, design decisions
- `docs/rate-limiting.md` — Token bucket algorithm, Redis Lua script, distributed behavior, key strategies, external service
- `docs/authentication.md` — Auth flow, HTTP/gRPC backends, header filtering, failure policies
- `docs/security.md` — Threat model, trust boundaries, header security, Redis security, TLS/mTLS
- `docs/deployment.md` — K8s deployment, HA, sizing, autoscaling, network policies
- `docs/observability.md` — Prometheus metrics, health probes, structured logging, OpenTelemetry tracing, Grafana dashboards

## CI/CD

GitHub Actions workflow (`.github/workflows/ci.yml`):
- **lint** — golangci-lint, go mod tidy check, gofumpt check, buf breaking (PRs only)
- **test** — Unit tests with race detector, 80% total / 70% per-package coverage enforcement
- **build** — `make build` + `make docker`
- **security** — govulncheck
- **buf-publish** — Push protos to Buf Schema Registry (on main/tags)
- **publish** — Push Docker image to `ghcr.io` (on version tags)

## Key Design Patterns

- **Atomic Redis Lua scripts** for token bucket rate limiting — single round-trip per request.
- **Protocol-aware transport** — single port handles HTTP/1.1, HTTP/2 (h2c), HTTP/3 (QUIC), gRPC, WebSocket, and SSE.
- **Circuit breaker** pattern for external auth and rate limit services.
- **Singleflight** for cache misses on external rate limit responses.
- **Distroless container** — `gcr.io/distroless/static-debian12:nonroot`, no shell, non-root user (UID 65534).

## Common Pitfalls

- Forgetting `make proto` after editing `.proto` files — the build will use stale generated code.
- Editing files in `api/gen/` directly — they will be overwritten on next `make proto`.
- Redis key prefix is `rl:edgequota:` — keep it simple to avoid Redis Cluster hash-tag collisions.
- The in-memory fallback limiter has a 65,536-key cap with LRU eviction — it is not a replacement for Redis.
- `failure_policy` values are camelCase: `passThrough`, `failClosed`, `inMemoryFallback`.

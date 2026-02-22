# EdgeQuota Documentation

EdgeQuota is an edge-native reverse proxy that enforces distributed rate limiting and pluggable authentication for Kubernetes workloads. It sits before your ingress controller or API gateway, applying policy decisions at line speed across HTTP/1.1, HTTP/2, HTTP/3, gRPC, SSE, and WebSocket traffic on a single port.

---

## Getting Started

- [Getting Started](getting-started.md) -- Quick start guide for binary, Docker, and Kubernetes deployments.
- [Configuration Reference](configuration.md) -- Every configuration field, type, default, and environment variable.

## Core Features

- [Rate Limiting](rate-limiting.md) -- Token bucket algorithm, Redis Lua script, distributed correctness, key strategies, failure policies, and the external rate limit service.
- [Authentication](authentication.md) -- External auth flow, HTTP/gRPC backends, failure policies, circuit breaker, and header filtering.
- [Multi-Protocol Proxy](proxy.md) -- HTTP/1.1, HTTP/2, HTTP/3 (QUIC), gRPC, SSE, and WebSocket proxying.
- [Events](events.md) -- Asynchronous usage event emission via HTTP/gRPC webhooks.

## Operations

- [Deployment](deployment.md) -- Kubernetes deployment, horizontal scaling, Redis topology, HA, autoscaling, and production sizing.
- [Observability](observability.md) -- Prometheus metrics, health probes, structured logging, OpenTelemetry tracing, and Grafana dashboards.
- [Security](security.md) -- Threat model, SSRF protection, header trust, Redis security, TLS, and container hardening.

## Ecosystem

- [Go SDK (`edgequota-go`)](go-sdk.md) -- Server-side helpers for building external rate limit, auth, and events services.
- [Helm Chart](helm-chart.md) -- Deploying EdgeQuota with the official Helm chart.
- [API Reference](api-reference.md) -- Protocol Buffer and OpenAPI schema definitions.

## Reference

- [Architecture](architecture.md) -- Internal design, data flow, failure modes, scaling model, and design decisions.
- [Troubleshooting](troubleshooting.md) -- Common issues, debugging techniques, and FAQ.

---

## Documentation Site

This documentation is designed to be consumed as a static site. Each Markdown file is self-contained and cross-linked. To generate a documentation website, use any Markdown-based static site generator:

**MkDocs (recommended):**

```bash
pip install mkdocs-material
mkdocs serve
```

**Docusaurus:**

Copy the `docs/` folder into a Docusaurus project's `docs/` directory. The frontmatter-free Markdown is compatible out of the box.

**Hugo:**

Place files under `content/docs/` and configure the navigation in `config.toml`.

### Navigation Structure

The recommended sidebar order for a documentation site:

```
1. Getting Started
2. Configuration Reference
3. Rate Limiting
4. Authentication
5. Multi-Protocol Proxy
6. Events
7. Deployment
8. Observability
9. Security
10. Go SDK
11. Helm Chart
12. API Reference
13. Architecture
14. Troubleshooting
```

### MkDocs Configuration

A minimal `mkdocs.yml` for the documentation site:

```yaml
site_name: EdgeQuota
site_description: Edge-native distributed quota enforcement for Kubernetes
repo_url: https://github.com/edgequota/edgequota

theme:
  name: material
  features:
    - navigation.sections
    - navigation.expand
    - search.suggest
    - content.code.copy

nav:
  - Home: index.md
  - Getting Started: getting-started.md
  - Configuration Reference: configuration.md
  - Rate Limiting: rate-limiting.md
  - Authentication: authentication.md
  - Multi-Protocol Proxy: proxy.md
  - Events: events.md
  - Deployment: deployment.md
  - Observability: observability.md
  - Security: security.md
  - Go SDK: go-sdk.md
  - Helm Chart: helm-chart.md
  - API Reference: api-reference.md
  - Architecture: architecture.md
  - Troubleshooting: troubleshooting.md

markdown_extensions:
  - admonition
  - pymdownx.details
  - pymdownx.superfences
  - pymdownx.tabbed:
      alternate_style: true
  - pymdownx.highlight:
      anchor_linenums: true
  - tables
  - toc:
      permalink: true
```

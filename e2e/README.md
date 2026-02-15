# EdgeQuota E2E Tests

End-to-end tests that deploy EdgeQuota and all Redis topologies into a local
**minikube** Kubernetes cluster using **Terraform**.

## Prerequisites

| Tool       | Version  | Purpose                          |
|------------|----------|----------------------------------|
| minikube   | ≥ 1.33   | Local Kubernetes cluster         |
| docker     | ≥ 24     | Build EdgeQuota image            |
| terraform  | ≥ 1.5    | Deploy infrastructure            |
| kubectl    | ≥ 1.30   | Interact with the cluster        |
| go         | ≥ 1.22   | Run the test orchestrator        |

## Quick Start

From the project root:

```bash
# Full cycle: provision → test → teardown
go run ./e2e all
```

Or run each phase independently:

```bash
# 1. Provision minikube + deploy infrastructure
go run ./e2e setup

# 2. Run E2E tests (cluster must be up)
go run ./e2e test

# 3. Tear everything down
go run ./e2e teardown
```

## Architecture

```
minikube cluster
├── Redis Single           (1 standalone pod)
├── Redis Replication      (1 primary + 2 replicas)
├── Redis Sentinel         (1 primary + 2 replicas + 3 sentinels)
├── Redis Cluster          (6-node cluster: 3 masters + 3 replicas)
├── Whoami                 (2-replica HTTP echo backend)
└── EdgeQuota instances    (one per test scenario, each with its own config)
    ├── single-pt          NodePort 30101 — passThrough
    ├── single-fc          NodePort 30102 — failClosed
    ├── single-fb          NodePort 30103 — inMemoryFallback
    ├── repl-basic         NodePort 30104 — replication mode
    ├── sentinel-basic     NodePort 30105 — sentinel mode
    ├── cluster-basic      NodePort 30106 — cluster mode
    ├── key-header         NodePort 30107 — header key strategy
    ├── key-composite      NodePort 30108 — composite key strategy
    ├── burst-test         NodePort 30109 — low burst (average=2, burst=3)
    └── no-limit           NodePort 30110 — rate limiting disabled
```

Each EdgeQuota instance runs as a standalone reverse proxy with its own
ConfigMap containing the specific scenario configuration. The test runner
sends requests directly to each instance's NodePort.

## Test Scenarios

### Topology Tests
- **Single mode** — passThrough, failClosed, inMemoryFallback
- **Replication mode** — basic rate limiting
- **Sentinel mode** — master discovery
- **Cluster mode** — MOVED redirect handling

### Key Strategy Tests
- **Header-based** — rate limit key from `X-Tenant-Id` header
- **Composite** — key from header + path prefix

### Rate Limiting Behavior
- **Burst exhaustion** — 429 after burst is consumed
- **No limit** — `average=0` passes all requests
- **Retry-After** — header present on 429 responses

### Failure Injection
- Kill Redis pod → verify passThrough allows requests
- Kill Redis pod → verify inMemoryFallback kicks in
- Kill and restart Redis → verify rate limiting resumes

### Concurrency
- 50 concurrent requests — no 500 errors under load

## Terraform Structure

```
e2e/terraform/
├── main.tf                         # Root — wires all modules
├── providers.tf                    # Kubernetes provider (minikube)
├── variables.tf                    # namespace, edgequota_image
├── outputs.tf                      # NodePorts and endpoints
└── modules/
    ├── redis-single/main.tf
    ├── redis-replication/main.tf
    ├── redis-sentinel/main.tf
    ├── redis-cluster/main.tf
    ├── whoami/main.tf
    └── edgequota/main.tf           # Reusable: 1 Deployment + ConfigMap + NodePort per scenario
```

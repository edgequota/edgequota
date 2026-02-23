# Helm Chart

The official Helm chart deploys EdgeQuota to Kubernetes with production-ready defaults including health probes, resource limits, security contexts, and optional autoscaling.

---

## Prerequisites

- Kubernetes 1.24+
- Helm 3.x
- A running Redis instance (any topology)

---

## Quick Start

```bash
helm repo add edgequota https://edgequota.github.io/edgequota-helm
helm repo update

helm install edgequota edgequota/edgequota \
  --set edgequota.rateLimit.static.backendUrl=http://my-backend:8080 \
  --set edgequota.rateLimit.static.average=100 \
  --set edgequota.rateLimit.static.burst=50 \
  --set edgequota.redis.endpoints[0]=redis:6379
```

---

## What Gets Deployed

| Resource | Description |
|----------|-------------|
| Deployment | EdgeQuota pods with configurable replicas |
| Service | ClusterIP service for proxy (8080) and admin (9090) ports |
| ConfigMap | Generated from `edgequota.*` values |
| Secret | Redis password (when set) |
| ServiceAccount | Dedicated service account |
| HPA | Horizontal Pod Autoscaler (when `autoscaling.enabled`) |
| VPA | Vertical Pod Autoscaler (when `vpa.enabled`) |
| PDB | PodDisruptionBudget (when `pdb.enabled`) |
| NetworkPolicy | Network restrictions (when `networkPolicy.enabled`) |
| ServiceMonitor | Prometheus Operator scraping (when `serviceMonitor.enabled`) |
| Ingress | Ingress resource (when `ingress.enabled`) |
| Certificate | cert-manager Certificate (when `certificate.enabled`) |
| Role/RoleBinding | RBAC for the service account |

---

## Configuration

All EdgeQuota configuration is exposed under the `edgequota` key in `values.yaml`. The chart generates the YAML config file from these values.

### Core Settings

```yaml
edgequota:
  server:
    address: ":8080"
    readTimeout: "30s"
    writeTimeout: "30s"
    idleTimeout: "120s"
    drainTimeout: "30s"

  admin:
    address: ":9090"

  backend:
    timeout: "30s"
    maxIdleConns: 100

  rateLimit:
    static:
      backendUrl: "http://my-backend:8080"
      average: 100
      burst: 50
      period: "1s"
      keyStrategy:
        type: "header"
        headerName: "X-Tenant-Id"
    failurePolicy: "passThrough"

  redis:
    endpoints:
      - "redis:6379"
    mode: "single"
    poolSize: 10

  logging:
    level: "info"
    format: "json"
```

### Redis Credentials

```yaml
edgequota:
  redis:
    password: "my-redis-password"
```

The chart creates a Kubernetes Secret and injects the password via `EDGEQUOTA_REDIS_PASSWORD`.

### Authentication

```yaml
edgequota:
  auth:
    enabled: true
    timeout: "5s"
    failurePolicy: "failclosed"
    http:
      url: "http://auth-service:8080/check"
```

### External Rate Limiting

When external RL is enabled, EdgeQuota delegates key derivation and quota resolution entirely to the external service. The `static` block is only used when external RL is **disabled**. The `external.fallback` block defines the safety-net limits applied when the external service is unavailable.

```yaml
edgequota:
  rateLimit:
    external:
      enabled: true
      timeout: "5s"
      http:
        url: "http://limits-service:8080/limits"
      headerFilter:
        allowList:
          - "Host"
          - "X-Tenant-Id"
          - "X-Request-Id"       # forwarded for tracing
      fallback:
        backendUrl: "http://my-backend:8080"
        average: 5000
        burst: 2000
        period: "1s"
        keyStrategy:
          type: "global"
          globalKey: "fe-fallback"
```

The `fallback` block is **required** when external RL is enabled. It defines what happens if the external service is down:

- `fallback.average` / `burst` / `period` -- the rate-limit bucket dimensions.
- `fallback.keyStrategy` -- how the rate-limit key is extracted during fallback. The `global` type is recommended for FE/static assets (all requests share one bucket); use `header` or `clientIP` for backend APIs.

Caching of external service responses is driven by `Cache-Control` headers or body fields returned by the service. Ephemeral headers (tracing IDs, `X-Request-Id`, etc.) are automatically excluded from cache keys. See [Response Caching](caching.md).

You do **not** need to configure the `static` block when using external RL. It is completely ignored while `external.enabled` is `true`.

### Response Cache (CDN)

Enable CDN-style response caching to cache backend responses in Redis. Backends opt in by returning `Cache-Control` headers. See [Response Caching](caching.md).

```yaml
edgequota:
  cache:
    enabled: true
    maxBodySize: 5242880  # 5 MB
```

### Response Cache Redis (Optional)

Dedicated Redis for the response cache. Falls back to `cacheRedis`, then `redis` when omitted.

```yaml
edgequota:
  responseCacheRedis:
    endpoints:
      - "cdn-cache-primary:6379"
      - "cdn-cache-replica:6379"
    mode: "replication"
    poolSize: 30
```

### Events

```yaml
edgequota:
  events:
    enabled: true
    http:
      url: "https://events-receiver.internal/ingest"
      headers:
        Authorization: "Bearer my-token"
```

### TLS

```yaml
edgequota:
  server:
    tls:
      enabled: true
      certFile: "/etc/edgequota/tls/tls.crt"
      keyFile: "/etc/edgequota/tls/tls.key"
      http3Enabled: true
```

### Tracing

```yaml
edgequota:
  tracing:
    enabled: true
    endpoint: "http://otel-collector:4318"
    serviceName: "edgequota"
    sampleRate: 0.1
```

---

## Infrastructure Settings

### Image

```yaml
image:
  repository: edgequota
  tag: "latest"
  pullPolicy: IfNotPresent
```

### Replicas and Resources

```yaml
replicaCount: 3

resources:
  requests:
    cpu: 100m
    memory: 64Mi
  limits:
    cpu: "2"
    memory: 512Mi
```

### Security Context

The chart applies hardened security defaults:

```yaml
securityContext:
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  runAsUser: 65534
  allowPrivilegeEscalation: false
  capabilities:
    drop:
      - ALL
```

### Autoscaling

```yaml
autoscaling:
  enabled: true
  minReplicas: 3
  maxReplicas: 50
  targetCPUUtilizationPercentage: 70
  targetMemoryUtilizationPercentage: 80
```

### PodDisruptionBudget

```yaml
pdb:
  enabled: true
  minAvailable: 2
```

### Network Policies

```yaml
networkPolicy:
  enabled: true
```

When enabled, restricts ingress to proxy and admin ports, and egress to Redis, backend, and DNS.

### ServiceMonitor

```yaml
serviceMonitor:
  enabled: true
  interval: 15s
  labels:
    release: prometheus
```

### Ingress

```yaml
ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt
  hosts:
    - host: api.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: edgequota-tls
      hosts:
        - api.example.com
```

---

## HTTP/3 Init Container

For HTTP/3 backend support, the kernel UDP buffer must be raised. The chart supports an init container for this:

```yaml
initContainers:
  - name: sysctl
    image: busybox
    command:
      - sh
      - -c
      - sysctl -w net.core.rmem_max=7500000 && sysctl -w net.core.wmem_max=7500000
    securityContext:
      privileged: true
```

---

## Escape Hatches

The chart provides escape hatches for injecting arbitrary configuration:

### Extra Environment Variables

```yaml
extraEnv:
  - name: EDGEQUOTA_BACKEND_TLS_INSECURE_SKIP_VERIFY
    value: "true"
```

### Extra Volumes and Volume Mounts

```yaml
extraVolumes:
  - name: tls-certs
    secret:
      secretName: edgequota-tls

extraVolumeMounts:
  - name: tls-certs
    mountPath: /etc/edgequota/tls
    readOnly: true
```

### Pod Annotations and Labels

```yaml
podAnnotations:
  sidecar.istio.io/inject: "false"

podLabels:
  team: platform
```

---

## Upgrading

### From 0.x to 0.y

Check the [CHANGELOG](https://github.com/edgequota/edgequota-helm/releases) for breaking changes. The chart follows semantic versioning — minor bumps may add new values but do not change defaults.

```bash
helm repo update
helm upgrade edgequota edgequota/edgequota -f my-values.yaml
```

---

## See Also

- [Deployment](deployment.md) -- General Kubernetes deployment guidance, HA, and sizing.
- [Configuration Reference](configuration.md) -- Full EdgeQuota config reference.
- [Security](security.md) -- Container hardening and network policies.
- [Observability](observability.md) -- ServiceMonitor setup and Grafana dashboards.

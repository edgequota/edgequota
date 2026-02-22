# Deployment

This document covers Kubernetes deployment, horizontal scaling, Redis topology selection, high-availability patterns, production sizing, and autoscaling.

---

## Kubernetes Deployment

EdgeQuota is designed to run at the edge of a Kubernetes cluster — before the ingress controller or API gateway. It is a stateless workload that coordinates through Redis.

### Minimal Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: edgequota
  labels:
    app: edgequota
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
      terminationGracePeriodSeconds: 60
      containers:
        - name: edgequota
          image: edgequota:latest
          ports:
            - containerPort: 8080
              name: proxy
              protocol: TCP
            - containerPort: 8080
              name: proxy-udp
              protocol: UDP   # HTTP/3 (QUIC), if TLS + http3_enabled
            - containerPort: 9090
              name: admin
              protocol: TCP
          volumeMounts:
            - name: config
              mountPath: /etc/edgequota
              readOnly: true
          env:
            - name: EDGEQUOTA_REDIS_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: redis-credentials
                  key: password
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
            failureThreshold: 3
          readinessProbe:
            httpGet:
              path: /readyz
              port: admin
            periodSeconds: 5
            failureThreshold: 2
          resources:
            requests:
              cpu: 100m
              memory: 64Mi
            limits:
              cpu: "2"
              memory: 512Mi
      volumes:
        - name: config
          configMap:
            name: edgequota-config
---
apiVersion: v1
kind: Service
metadata:
  name: edgequota
spec:
  type: ClusterIP
  selector:
    app: edgequota
  ports:
    - name: proxy
      port: 8080
      targetPort: proxy
      protocol: TCP
    - name: admin
      port: 9090
      targetPort: admin
      protocol: TCP
```

### ConfigMap

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: edgequota-config
data:
  config.yaml: |
    server:
      address: ":8080"
      drain_timeout: "30s"
    admin:
      address: ":9090"
    backend:
      url: "http://my-backend-service:8080"
      timeout: "30s"
      max_idle_conns: 200
    rate_limit:
      average: 100
      burst: 50
      period: "1s"
      failure_policy: "passThrough"
      key_strategy:
        type: "header"
        header_name: "X-Tenant-Id"
    redis:
      endpoints:
        - "redis-master:6379"
      mode: "single"
      pool_size: 20
    logging:
      level: "info"
      format: "json"
```

### Secrets

Never put Redis passwords or TLS keys in ConfigMaps. Use Kubernetes Secrets:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: redis-credentials
type: Opaque
stringData:
  password: "your-redis-password"
```

Reference in the Deployment as environment variables:

```yaml
env:
  - name: EDGEQUOTA_REDIS_PASSWORD
    valueFrom:
      secretKeyRef:
        name: redis-credentials
        key: password
```

---

## Health Probes

EdgeQuota exposes three probe endpoints on the admin server:

| Endpoint | Kubernetes Probe | Behavior |
|----------|-----------------|----------|
| `/startz` | `startupProbe` | Returns 200 once initialization is complete (Redis connected, proxy ready). Returns 503 during startup. |
| `/healthz` | `livenessProbe` | Always returns 200 while the process is running. |
| `/readyz` | `readinessProbe` | Returns 200 when the instance is ready to accept traffic. Returns 503 during graceful shutdown (drain). |

### Recommended Probe Configuration

```yaml
startupProbe:
  httpGet:
    path: /startz
    port: 9090
  failureThreshold: 30  # Allow up to 30s for startup
  periodSeconds: 1

livenessProbe:
  httpGet:
    path: /healthz
    port: 9090
  periodSeconds: 10
  failureThreshold: 3

readinessProbe:
  httpGet:
    path: /readyz
    port: 9090
  periodSeconds: 5
  failureThreshold: 2
```

The startup probe gates liveness and readiness checks. Kubernetes will not send liveness or readiness probes until the startup probe succeeds. This prevents premature restarts during slow Redis connections or large initial configurations.

---

## Graceful Shutdown

When EdgeQuota receives `SIGTERM` (Kubernetes pod termination):

1. `/readyz` starts returning 503 → Kubernetes removes the pod from Service endpoints.
2. EdgeQuota waits `drain_timeout` (default: 30s) for in-flight requests to complete.
3. Servers shut down; Redis connections are closed.
4. Process exits.

Set `terminationGracePeriodSeconds` in the pod spec to at least `drain_timeout + 10s` to ensure Kubernetes does not force-kill the pod before drainage is complete:

```yaml
spec:
  terminationGracePeriodSeconds: 60  # drain_timeout (30s) + buffer
```

---

## Horizontal Scaling

EdgeQuota is **fully stateless**. All shared state (rate limit counters) is stored in Redis. You can scale the Deployment to any number of replicas without coordination:

```bash
kubectl scale deployment edgequota --replicas=10
```

### What Changes with More Replicas

| Aspect | Effect |
|--------|--------|
| Rate limit accuracy | **No change.** All instances share the same Redis counters. |
| Throughput | **Increases linearly.** Each instance handles its share of traffic. |
| Redis connections | **Increases.** Each instance opens `pool_size` connections to Redis. |
| In-memory fallback | **Less accurate.** Each instance has its own local bucket. |
| Auth service load | **Increases linearly.** Each instance makes its own auth calls. |

### Connection Budget

With `N` EdgeQuota replicas and a Redis `pool_size` of `P`:

- Single Redis: `N * P` connections.
- Redis Cluster with `S` shards: up to `N * P * S` connections (go-redis maintains a pool per shard).

Plan the Redis `maxclients` setting accordingly. A reasonable default is `pool_size: 20` with up to 50 EdgeQuota replicas = 1,000 connections.

---

## Redis Topology Selection

### Single Instance

```yaml
redis:
  endpoints: ["redis:6379"]
  mode: "single"
```

**Best for:** Development, testing, low-traffic production (< 10,000 req/s).

**Risks:** Single point of failure. If Redis is down, rate limiting is governed by `failure_policy`.

### Replication

```yaml
redis:
  endpoints: ["redis-0:6379", "redis-1:6379", "redis-2:6379"]
  mode: "replication"
```

**Best for:** Medium traffic where you want automatic master discovery.

EdgeQuota discovers the master by issuing `ROLE` to each endpoint. The result is cached for 30 seconds. On `READONLY` errors (master changed), the cache is invalidated and the master is re-discovered.

**Note:** Rate limiting is write-heavy (every request writes to Redis), so read replicas provide limited benefit. Replication mode's primary value is automatic master discovery after manual failover.

### Sentinel

```yaml
redis:
  endpoints: ["sentinel-0:26379", "sentinel-1:26379", "sentinel-2:26379"]
  mode: "sentinel"
  master_name: "mymaster"
```

**Best for:** Production deployments requiring automatic failover without manual intervention.

Redis Sentinel monitors the master and promotes a replica if the master fails. EdgeQuota's `go-redis` failover client automatically follows the promotion.

### Cluster

```yaml
redis:
  endpoints: ["redis-0:6379", "redis-1:6379", "redis-2:6379", "redis-3:6379", "redis-4:6379", "redis-5:6379"]
  mode: "cluster"
```

**Best for:** High throughput (> 100,000 req/s) or large key spaces (millions of unique rate limit keys).

Keys are automatically distributed across shards. Each shard handles its own key range, so total throughput scales with the number of shards.

---

## High Availability

### EdgeQuota HA

Run at least 3 replicas across different nodes:

```yaml
spec:
  replicas: 3
  template:
    spec:
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: kubernetes.io/hostname
          whenUnsatisfiable: DoNotSchedule
          labelSelector:
            matchLabels:
              app: edgequota
```

This ensures EdgeQuota pods are spread across nodes. Combined with a Kubernetes Service, traffic is load-balanced across all healthy pods.

### Redis HA

| Topology | Availability | Recovery Time |
|----------|-------------|---------------|
| Single | No HA | Manual restart |
| Replication + manual failover | HA with operator intervention | Minutes |
| Sentinel | Automatic failover | 10–30 seconds |
| Cluster | Automatic failover per shard | 10–30 seconds |

For production, use Sentinel or Cluster. The EdgeQuota `failure_policy` provides a safety net during the failover window.

### End-to-End HA

```
                    ┌──────────────┐
                    │  Kubernetes  │
                    │   Service    │
                    └──────┬───────┘
                           │
              ┌────────────┼────────────┐
              │            │            │
        ┌─────▼────┐ ┌────▼─────┐ ┌────▼─────┐
        │ EQ Pod 1 │ │ EQ Pod 2 │ │ EQ Pod 3 │
        └─────┬────┘ └────┬─────┘ └────┬─────┘
              │            │            │
              └────────────┼────────────┘
                           │
                    ┌──────▼───────┐
                    │ Redis        │
                    │ (Sentinel    │
                    │  or Cluster) │
                    └──────────────┘
```

---

## Production Sizing

### EdgeQuota Resources

| Workload | CPU Request | CPU Limit | Memory Request | Memory Limit | Replicas |
|----------|------------|-----------|---------------|-------------|----------|
| Low (< 1K req/s) | 50m | 500m | 32Mi | 128Mi | 2 |
| Medium (1K–10K req/s) | 100m | 1 | 64Mi | 256Mi | 3 |
| High (10K–100K req/s) | 250m | 2 | 128Mi | 512Mi | 5–10 |
| Very High (> 100K req/s) | 500m | 4 | 256Mi | 1Gi | 10–50 |

### Redis Resources

| Workload | Redis Type | Memory | CPU | Notes |
|----------|-----------|--------|-----|-------|
| Low | Single | 128Mi | 250m | Sufficient for development and low-traffic |
| Medium | Sentinel (3 nodes) | 256Mi each | 500m each | Automatic failover |
| High | Cluster (6 nodes) | 512Mi each | 1 each | 3 masters + 3 replicas |
| Very High | Cluster (12+ nodes) | 1Gi each | 2 each | More shards for higher throughput |

### Connection Pooling

Set `redis.pool_size` based on expected concurrency per EdgeQuota pod:

| Concurrency | Pool Size |
|-------------|-----------|
| Low (< 100 concurrent) | 10 |
| Medium (100–1000) | 20 |
| High (1000+) | 50 |

Formula: `pool_size = expected_concurrent_requests * 1.2` (20% headroom).

---

## Autoscaling

### Horizontal Pod Autoscaler

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: edgequota
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: edgequota
  minReplicas: 3
  maxReplicas: 50
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
    - type: Resource
      resource:
        name: memory
        target:
          type: Utilization
          averageUtilization: 80
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 30
      policies:
        - type: Percent
          value: 50
          periodSeconds: 60
    scaleDown:
      stabilizationWindowSeconds: 300
      policies:
        - type: Percent
          value: 10
          periodSeconds: 60
```

### Custom Metrics (Advanced)

For more precise autoscaling, use Prometheus metrics via the Prometheus adapter:

```yaml
metrics:
  - type: Pods
    pods:
      metric:
        name: edgequota_requests_allowed_total
      target:
        type: AverageValue
        averageValue: "1000"  # Scale up at 1000 req/s per pod
```

### Scaling Considerations

| Factor | Recommendation |
|--------|---------------|
| Minimum replicas | 3 (for availability across node failures) |
| Scale-up speed | Aggressive (traffic spikes are bursty) |
| Scale-down speed | Conservative (avoid flapping) |
| Redis connection budget | Ensure `maxclients` can handle `max_replicas * pool_size` |
| In-memory fallback accuracy | Decreases with more replicas (each has its own counters) |

---

## Network Policies

Restrict EdgeQuota's network access to only what it needs:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: edgequota
spec:
  podSelector:
    matchLabels:
      app: edgequota
  policyTypes:
    - Ingress
    - Egress
  ingress:
    - ports:
        - port: 8080    # Proxy traffic
          protocol: TCP
        - port: 9090    # Admin (metrics scraping)
          protocol: TCP
  egress:
    - to:
        - podSelector:
            matchLabels:
              app: redis
      ports:
        - port: 6379
          protocol: TCP
    - to:
        - podSelector:
            matchLabels:
              app: my-backend
      ports:
        - port: 8080
          protocol: TCP
    - to:                # DNS resolution
        - namespaceSelector: {}
      ports:
        - port: 53
          protocol: UDP
        - port: 53
          protocol: TCP
```

---

## PodDisruptionBudget

Ensure at least 2 pods remain during voluntary disruptions (node drains, upgrades):

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: edgequota
spec:
  minAvailable: 2
  selector:
    matchLabels:
      app: edgequota
```

---

## See Also

- [Configuration Reference](configuration.md) -- Full config reference.
- [Helm Chart](helm-chart.md) -- Deploying with the official Helm chart.
- [Observability](observability.md) -- Prometheus, probes, and Grafana dashboards.
- [Security](security.md) -- Container hardening and network policies.
- [Architecture](architecture.md) -- Scaling model and failure modes.
- [Troubleshooting](troubleshooting.md) -- Common deployment issues.

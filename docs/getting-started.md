# Getting Started

This guide walks you through building, running, and deploying EdgeQuota for the first time.

---

## Prerequisites

- **Go 1.26+** (for building from source)
- **Docker** (for container builds)
- **Redis** (any topology: single, replication, sentinel, or cluster)
- **A backend service** to proxy to (any HTTP, gRPC, or WebSocket service)

---

## Build from Source

```bash
git clone https://github.com/edgequota/edgequota.git
cd edgequota
make build
```

This produces `bin/edgequota`.

---

## Run Locally

### With a Config File

```bash
EDGEQUOTA_CONFIG_FILE=config.example.yaml ./bin/edgequota
```

### With Environment Variables Only

```bash
EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL=http://localhost:3000 \
EDGEQUOTA_RATE_LIMIT_STATIC_AVERAGE=100 \
EDGEQUOTA_RATE_LIMIT_STATIC_BURST=50 \
EDGEQUOTA_REDIS_ENDPOINTS=localhost:6379 \
./bin/edgequota
```

### Verify

```bash
# Proxy port
curl -i http://localhost:8080/

# Health probes (admin port)
curl http://localhost:9090/healthz
curl http://localhost:9090/readyz
curl http://localhost:9090/startz

# Prometheus metrics
curl http://localhost:9090/metrics
```

---

## Docker

```bash
# Build the image
make docker

# Run
docker run --rm -p 8080:8080 -p 9090:9090 \
  -e EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL=http://host.docker.internal:3000 \
  -e EDGEQUOTA_RATE_LIMIT_STATIC_AVERAGE=100 \
  -e EDGEQUOTA_RATE_LIMIT_STATIC_BURST=50 \
  -e EDGEQUOTA_REDIS_ENDPOINTS=host.docker.internal:6379 \
  edgequota:dev
```

The container image is based on `gcr.io/distroless/static-debian12:nonroot` — no shell, no package manager, non-root UID 65534.

---

## Kubernetes Quick Start

### 1. Create a ConfigMap

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
    logging:
      level: "info"
      format: "json"
```

### 2. Create a Secret for Redis Credentials

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: redis-credentials
type: Opaque
stringData:
  password: "your-redis-password"
```

### 3. Deploy

```yaml
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
      terminationGracePeriodSeconds: 60
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
    - name: admin
      port: 9090
      targetPort: admin
```

### 4. Verify

```bash
kubectl port-forward svc/edgequota 8080:8080 9090:9090

curl -i http://localhost:8080/
curl http://localhost:9090/healthz
```

---

## CLI Reference

EdgeQuota has a minimal CLI interface. All configuration is done via YAML and environment variables.

| Command | Description |
|---------|-------------|
| `edgequota` | Start the proxy (default) |
| `edgequota version` | Print the binary version and exit |

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `EDGEQUOTA_CONFIG_FILE` | `/etc/edgequota/config.yaml` | Path to the YAML configuration file |

All other configuration is done through the config file or `EDGEQUOTA_*` environment variables. See [Configuration Reference](configuration.md) for the complete list.

---

## Minimal Configuration

The only required field is `rate_limit.static.backend_url`:

```yaml
rate_limit:
  static:
    backend_url: "http://my-backend:8080"
```

Everything else has sensible defaults. Rate limiting is disabled when `rate_limit.static.average` is `0` (the default).

### Enable Rate Limiting

```yaml
rate_limit:
  static:
    backend_url: "http://my-backend:8080"
    average: 100
    burst: 50
    period: "1s"
redis:
  endpoints:
    - "redis:6379"
```

### Enable Authentication

```yaml
rate_limit:
  static:
    backend_url: "http://my-backend:8080"
auth:
  enabled: true
  timeout: "5s"
  failure_policy: "failclosed"
  http:
    url: "http://auth-service:8080/check"
```

### Enable Usage Events

```yaml
events:
  enabled: true
  http:
    url: "https://events-receiver.internal/ingest"
    headers:
      Authorization: "Bearer my-token"
```

---

## Next Steps

- [Configuration Reference](configuration.md) -- Full reference for every configuration field.
- [Rate Limiting](rate-limiting.md) -- Algorithm details and external rate limit service.
- [Deployment](deployment.md) -- Production deployment, scaling, and HA.
- [Security](security.md) -- Hardening guide and threat model.

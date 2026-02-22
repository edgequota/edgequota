# Troubleshooting

Common issues, debugging techniques, and frequently asked questions.

---

## Startup Issues

### EdgeQuota fails to start with "backend.url is required"

The `backend.url` field is mandatory. Set it via the config file or environment variable:

```bash
EDGEQUOTA_BACKEND_URL=http://my-backend:8080 ./bin/edgequota
```

### EdgeQuota fails to start with "invalid redis.mode"

Redis mode must be one of: `single`, `replication`, `sentinel`, `cluster`. The value is case-sensitive (lowercase).

### EdgeQuota starts but /readyz returns 503

The readiness probe returns 503 during startup and during graceful shutdown (drain). If it remains 503 after startup, check:

1. **Redis connectivity**: use `/readyz?deep=true` to actively ping Redis.
2. **Config validation**: check logs for validation errors.
3. **Startup probe**: ensure `startupProbe` is configured so Kubernetes waits for initialization.

### TLS certificate errors

Verify the certificate and key files exist and are valid:

```bash
openssl x509 -in /etc/edgequota/tls/cert.pem -noout -text
```

For Kubernetes, ensure the Secret is mounted correctly and the paths match `server.tls.cert_file` and `server.tls.key_file`.

---

## Redis Issues

### "redis: connection pool: failed to dial after 5 attempts"

EdgeQuota retries Redis connections indefinitely with capped exponential backoff (up to 60 seconds). This log message means Redis is unreachable. Check:

1. **Redis is running**: `redis-cli ping`
2. **Network connectivity**: verify the pod can reach Redis (DNS resolution, NetworkPolicy)
3. **Authentication**: ensure `redis.password` is correct
4. **TLS**: if `redis.tls.enabled`, verify the Redis server has TLS configured

### Rate limiting is not working (all requests pass through)

1. **`rate_limit.average` is 0**: this disables rate limiting entirely.
2. **`failure_policy: passThrough`**: if Redis is down, all requests are allowed.
3. **External rate limit service returns `average: 0`**: this means "unlimited" for that key.
4. **Check metrics**: `edgequota_redis_errors_total` indicates Redis connectivity problems.

### In-memory fallback is active

When `edgequota_fallback_used_total` is incrementing, Redis is unreachable and the in-memory fallback limiter is active. This provides approximate per-instance rate limiting until Redis recovers.

The recovery loop runs in the background and reconnects automatically. Monitor `edgequota_redis_healthy` (0 = unhealthy, 1 = healthy).

### Redis log format is inconsistent

Redis client logs use Go's `log/slog` structured format, matching EdgeQuota's own log output. If you see unstructured Redis logs, ensure EdgeQuota has initialized the Redis logger (this happens automatically at startup via `redis.InitLogger`).

---

## Proxy Issues

### 502 Bad Gateway

The backend is unreachable or returned an error. Check:

1. **Backend URL**: verify `backend.url` resolves and the service is healthy.
2. **Timeouts**: `backend.timeout` may be too short. Increase it for slow backends.
3. **TLS mismatch**: if the backend requires TLS, use `https://` in the URL.
4. **HTTP/3 issues**: see "HTTP/3 connections timing out" below.

### HTTP/3 connections timing out (QUIC failures)

When using `backend_protocol: h3`, QUIC requires large UDP buffers:

```
failed to sufficiently increase receive buffer size
(was: 208 kiB, wanted: 7168 kiB, got: 416 kiB)
```

Fix: raise the kernel limit:

```bash
sysctl -w net.core.rmem_max=7500000
sysctl -w net.core.wmem_max=7500000
```

In Kubernetes, use a privileged init container. See [Multi-Protocol Proxy](proxy.md) for details.

### WebSocket connections rejected with 403

When `server.allowed_websocket_origins` is configured, the `Origin` header must match one of the allowed values. Check:

1. The client is sending the correct `Origin` header.
2. The allowed list includes the client's origin (exact match).
3. An empty list allows all origins.

### gRPC requests return unexpected errors

Ensure:

1. The backend supports HTTP/2 (gRPC requires it).
2. EdgeQuota is accepting h2c connections (automatic with `h2c.NewHandler`).
3. `TE: trailers` is not being stripped by an intermediate proxy.

### Host header not forwarded to external services

EdgeQuota re-injects `r.Host` into the header maps sent to external rate limit and auth services. Go's `net/http` promotes the `Host` header (and HTTP/2's `:authority`) to `r.Host` and removes it from `r.Header`. EdgeQuota handles this automatically.

If you are not seeing the `Host` header in your external service, ensure you are reading it from the `headers` map in the request payload, not from HTTP transport headers.

---

## Auth Issues

### All requests denied with 503

When `auth.failure_policy: failclosed` (default) and the auth service is unreachable, all requests are denied with 503. Check:

1. The auth service is running and reachable.
2. `auth.timeout` is sufficient.
3. The circuit breaker may be open (5 consecutive failures opens it for 30s).

### Auth service receives no Host header

EdgeQuota re-injects the `Host` header from `r.Host` into the `headers` map of the `CheckRequest`. The auth service should read `headers["Host"]` from the JSON body, not from the HTTP transport headers of EdgeQuota's request to the auth service.

### Headers not injected by auth service

The auth service can inject headers into the upstream request via `request_headers` in the allow response. However, certain headers are restricted from injection:

- Hop-by-hop headers (`Connection`, `Transfer-Encoding`, etc.)
- `X-Forwarded-*` headers (unless explicitly allowed via `auth.allowed_injection_headers`)

---

## Events Issues

### Events are being dropped

When `edgequota_events_dropped_total` is incrementing, the ring buffer is full. Increase `events.buffer_size` or decrease `events.flush_interval` to flush more frequently.

### Events fail to send

When `edgequota_events_send_failures_total` is incrementing, the events receiver is not responding with 2xx. Check:

1. The receiver URL is correct and reachable.
2. Custom headers (authentication) are configured correctly.
3. The receiver can handle the JSON array payload.

### Restricted header error at startup

```
events.http.headers: "Host" is a restricted header and cannot be set
```

Certain headers cannot be set as custom event headers because they would break HTTP transport. See [Events](events.md) for the restricted header list.

---

## Configuration Issues

### Hot reload not working

Config hot-reload uses `fsnotify` to watch for file changes. Ensure:

1. The config file is a regular file (not a symlink to a ConfigMap that Kubernetes updates atomically — `fsnotify` may not detect atomic rename; EdgeQuota debounces for 300ms to handle this).
2. Rapid writes are debounced; wait 300ms after the last write.
3. Some fields are NOT hot-reloaded: Redis connection parameters and server listen addresses require a restart.

### Environment variables not overriding config

Environment variables must follow the `EDGEQUOTA_*` naming convention with dots replaced by underscores:

```
server.tls.enabled → EDGEQUOTA_SERVER_TLS_ENABLED
rate_limit.key_strategy.type → EDGEQUOTA_RATE_LIMIT_KEY_STRATEGY_TYPE
```

### Deprecated auth_token/auth_header warnings

The `events.http.auth_token` and `events.http.auth_header` fields are deprecated. Migrate to `events.http.headers`:

```yaml
# Before (deprecated):
events:
  http:
    auth_token: "my-secret"
    auth_header: "Authorization"

# After:
events:
  http:
    headers:
      Authorization: "Bearer my-secret"
```

---

## Performance Issues

### High latency (P99 > 1s)

Check which stage is the bottleneck:

| Metric | Stage |
|--------|-------|
| `edgequota_auth_duration_seconds` | Auth service |
| `edgequota_external_rl_duration_seconds` | External rate limit service |
| `edgequota_backend_duration_seconds` | Backend proxy |
| `edgequota_request_duration_seconds` | End-to-end |

If auth or external RL latency is high, check the external service performance and consider increasing timeout values.

### 503 from concurrency limiter

When `edgequota_concurrent_requests_rejected_total` is incrementing, `server.max_concurrent_requests` is being exceeded. Either increase the limit or scale out to more replicas.

### Redis latency spikes

For high-throughput deployments:

1. Use Redis Cluster for sharding.
2. Increase `redis.pool_size` to match concurrency.
3. Ensure Redis is on the same network (avoid cross-AZ latency).
4. Monitor `redis SLOWLOG` for slow commands.

---

## Diagnostic Commands

### Check EdgeQuota health

```bash
curl http://localhost:9090/healthz
curl http://localhost:9090/readyz
curl "http://localhost:9090/readyz?deep=true"
curl http://localhost:9090/startz
```

### View current configuration (secrets redacted)

```bash
curl http://localhost:9090/v1/config | jq .
```

### View runtime statistics

```bash
curl http://localhost:9090/v1/stats | jq .
```

### Check Prometheus metrics

```bash
curl http://localhost:9090/metrics | grep edgequota_
```

### Test rate limiting

```bash
for i in $(seq 1 200); do
  curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/
done | sort | uniq -c
```

---

## See Also

- [Observability](observability.md) -- Metrics, logging, and tracing.
- [Configuration Reference](configuration.md) -- Full config reference.
- [Security](security.md) -- Security hardening.
- [Deployment](deployment.md) -- Production deployment guidance.

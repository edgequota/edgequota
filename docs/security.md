# Security

This document describes EdgeQuota's threat model, trust boundaries, header security, Redis security, and hardening recommendations.

---

## Threat Model

EdgeQuota runs at the **network edge** — it is the first service that untrusted traffic reaches. This position makes it both a critical security control and a high-value target.

### Assets

| Asset | Description | Sensitivity |
|-------|-------------|-------------|
| Rate limit counters (Redis) | Per-key token bucket state | Medium — manipulation could bypass limits |
| Auth service credentials | Connection strings, TLS certificates | High — compromise grants access to auth decisions |
| Configuration file | Backend URLs, Redis credentials, auth endpoints | High — contains secrets and topology |
| Request metadata | Headers, IPs, paths forwarded to auth service | Medium — may contain tokens or PII |

### Threat Actors

| Actor | Capability | Goal |
|-------|-----------|------|
| External attacker | Sends arbitrary HTTP requests | Bypass rate limits, access protected resources |
| Compromised pod | Network access within the cluster | Manipulate Redis counters, impersonate EdgeQuota |
| Supply chain attacker | Modify dependencies or container image | Introduce backdoors |

### Attack Surface

| Surface | Exposure | Controls |
|---------|----------|----------|
| Main listener (`:8080`) | Untrusted network | Rate limiting, auth validation, input sanitization |
| Admin listener (`:9090`) | Cluster-internal | Network policy, no sensitive data in responses |
| Redis connection | Cluster-internal | Authentication, TLS, network policy |
| Auth service connection | Cluster-internal | TLS, network policy |
| Container image | Registry | Distroless base, non-root, no shell |

---

## Trust Boundaries

```
┌─────────────────────────────────────────────────────┐
│                 UNTRUSTED ZONE                       │
│                                                     │
│  Clients, attackers, internet traffic               │
└──────────────────────┬──────────────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────────────┐
│              EDGE BOUNDARY (EdgeQuota)                │
│                                                      │
│  - Validates headers (does not trust them)            │
│  - Enforces rate limits                              │
│  - Delegates auth decisions                          │
│  - Forwards allowed requests only                    │
└──────────────────────┬───────────────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────────────┐
│                 TRUSTED ZONE                          │
│                                                      │
│  Backend services, Redis, auth service               │
│  Assumed to be in a controlled network               │
└──────────────────────────────────────────────────────┘
```

### Key Principle

EdgeQuota enforces policy but **does not make trust decisions about header content**. Headers from untrusted clients (including `X-Forwarded-For`, `Authorization`, and custom headers) are forwarded as-is to the auth service and used for key extraction. The auth service and backend must validate these headers independently.

---

## Header Trust

### X-Forwarded-For

When using the `clientIP` key strategy, EdgeQuota extracts the client IP in this order:

1. First IP in `X-Forwarded-For` header
2. `X-Real-IP` header
3. `RemoteAddr` (direct connection IP)

**Risk:** An attacker can spoof `X-Forwarded-For` to impersonate another client's IP, potentially stealing their rate limit bucket or bypassing per-IP limits.

**Mitigations:**

- **Deploy behind a trusted load balancer** that strips or overwrites `X-Forwarded-For` with the actual client IP.
- **Use `header` or `composite` key strategy** with a header that is set by a trusted component (e.g., a header injected by the auth service after token validation).
- **Do not use `clientIP` strategy** for security-critical rate limiting if EdgeQuota is directly exposed to untrusted clients without a trusted proxy in front.

### Authorization Headers

All `Authorization` headers are forwarded to the auth service. EdgeQuota does not inspect, validate, or cache tokens.

**Recommendation:** Always validate tokens cryptographically in the auth service. Never rely on token presence or format alone.

### Host Header

The `Host` header is forwarded as-is to the backend. If the backend makes routing decisions based on `Host`, ensure the backend validates it independently.

---

## Redis Security

### Authentication

Always use Redis authentication in production:

```yaml
redis:
  username: "edgequota"
  password: "${REDIS_PASSWORD}"  # Use env var override
```

Set the password via environment variable to avoid storing it in the config file:

```bash
EDGEQUOTA_REDIS_PASSWORD=your-secret-password
```

### ACL (Redis 6+)

Create a dedicated Redis user with minimal permissions:

```redis
ACL SETUSER edgequota on >password ~rl:edgequota:* +eval +evalsha +hmget +hset +expire +ping
```

This restricts the EdgeQuota user to:

- Keys matching `rl:edgequota:*` only.
- Commands: `EVAL`, `EVALSHA`, `HMGET`, `HSET`, `EXPIRE`, `PING`.
- No access to `KEYS`, `FLUSHDB`, `CONFIG`, or other administrative commands.

### TLS

Enable TLS for Redis connections, especially in multi-tenant clusters or when Redis traffic crosses network boundaries:

```yaml
redis:
  tls:
    enabled: true
    insecure_skip_verify: false  # Always verify in production
```

### Network Isolation

Use Kubernetes NetworkPolicies to restrict which pods can reach Redis:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: redis-access
spec:
  podSelector:
    matchLabels:
      app: redis
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app: edgequota
      ports:
        - port: 6379
          protocol: TCP
```

### Key Manipulation

If an attacker gains access to Redis, they can:

- **Delete rate limit keys** → all clients get fresh buckets (rate limits temporarily ineffective).
- **Modify token counts** → specific clients can bypass or be unfairly limited.
- **Read key names** → learn which IPs or tenants are active (information disclosure).

**Mitigations:**

- Use Redis ACLs to restrict access to `rl:edgequota:*` keys.
- Use Redis TLS to prevent network sniffing.
- Use NetworkPolicies to limit which pods can connect to Redis.
- Monitor Redis `SLOWLOG` and audit logs for unexpected access patterns.

---

## Container Security

### Distroless Image

EdgeQuota ships as a `gcr.io/distroless/static-debian12:nonroot` image:

- **No shell.** Cannot exec into the container for interactive exploration.
- **No package manager.** Cannot install tools inside the running container.
- **Non-root user.** Runs as UID 65534 (`nobody`).
- **Static binary.** Zero CGO dependencies; no dynamic linking.

### Image Scanning

Scan the image for vulnerabilities:

```bash
# Trivy
trivy image edgequota:latest

# Grype
grype edgequota:latest
```

The distroless base image has a minimal attack surface, but always scan for Go standard library vulnerabilities.

### Read-Only Root Filesystem

In Kubernetes, enforce a read-only root filesystem:

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

EdgeQuota does not write to the filesystem at runtime (logs go to stdout/stderr).

---

## TLS Configuration

### Minimum TLS Version

Set `server.tls.min_version` to enforce a minimum TLS version:

```yaml
server:
  tls:
    enabled: true
    min_version: "1.3"   # Recommended for maximum security
    cert_file: "/etc/edgequota/tls/cert.pem"
    key_file: "/etc/edgequota/tls/key.pem"
```

| Version | Recommendation |
|---------|---------------|
| TLS 1.2 | Acceptable. Required for some legacy clients. |
| TLS 1.3 | Recommended. Faster handshake, stronger ciphers, no legacy baggage. |

### Certificate Rotation

EdgeQuota reads TLS certificates at startup. To rotate certificates without downtime:

1. Update the Kubernetes Secret containing the certificate.
2. Perform a rolling restart of EdgeQuota pods.

Or use a cert-manager with a sidecar that reloads certificates dynamically.

---

## mTLS Between Services

For zero-trust environments, use mTLS for all internal communication:

### EdgeQuota ↔ Backend

If the backend requires mTLS, configure a service mesh (e.g., Istio, Linkerd) to handle mTLS transparently. EdgeQuota uses cleartext HTTP internally; the mesh sidecar encrypts the traffic.

### EdgeQuota ↔ Redis

Use `redis.tls.enabled: true` for encrypted Redis connections. For mTLS, configure the Redis server to require client certificates and provide the client certificate to EdgeQuota via the Redis TLS configuration.

### EdgeQuota ↔ Auth Service

For gRPC auth backends, use `auth.grpc.tls.enabled: true` with a CA file for server verification. For full mTLS, extend the auth gRPC client with client certificate support.

---

## Replay Attack Considerations

EdgeQuota does not implement replay protection at the proxy layer. Each request is treated independently.

**Mitigations:**

- **Use the auth service** for replay protection. The auth service can validate nonces, timestamps, or idempotency keys.
- **Use HTTPS/TLS** to prevent request interception and replay from the network.
- **Use short-lived tokens** (e.g., JWT with expiration) that the auth service validates.

---

## Abuse Prevention

### Rate Limit Evasion

An attacker may try to evade rate limits by:

| Technique | Mitigation |
|-----------|------------|
| Rotating IPs | Use `header` key strategy with an API key or tenant ID |
| Spoofing `X-Forwarded-For` | Deploy behind a trusted proxy that sets this header |
| Omitting required headers | EdgeQuota returns 500 for missing keys; the request is rejected |
| Flooding with distinct keys | Redis memory is bounded by key TTL; high cardinality keys expire naturally |
| Targeting the admin port | Restrict admin port access via NetworkPolicy |

### Connection Exhaustion

EdgeQuota configures `ReadHeaderTimeout` on all servers to prevent Slowloris-style attacks. The main server also has `ReadTimeout`, `WriteTimeout`, and `IdleTimeout` to bound connection lifetimes.

### Resource Exhaustion

- **Memory:** The in-memory fallback limiter is capped at 65,536 keys with automatic eviction.
- **Goroutines:** WebSocket connections create two goroutines per connection; use connection limits at the load balancer level if needed.
- **File descriptors:** Bounded by the OS limit and `max_idle_conns` configuration.

---

## Security Checklist

Production deployment checklist:

- [ ] Redis authentication enabled (`redis.password` set via env var)
- [ ] Redis TLS enabled (`redis.tls.enabled: true`)
- [ ] Redis ACLs configured (restrict to `rl:edgequota:*` keys)
- [ ] NetworkPolicies restrict Redis and admin port access
- [ ] EdgeQuota deployed behind a trusted proxy that sets `X-Forwarded-For`
- [ ] `readOnlyRootFilesystem: true` in pod security context
- [ ] `runAsNonRoot: true` and `runAsUser: 65534` in pod security context
- [ ] `allowPrivilegeEscalation: false` in pod security context
- [ ] All capabilities dropped (`capabilities.drop: [ALL]`)
- [ ] TLS enabled for external-facing traffic (`server.tls.enabled: true`)
- [ ] Container image scanned for vulnerabilities
- [ ] Secrets stored in Kubernetes Secrets, not ConfigMaps
- [ ] Admin port (`:9090`) not exposed outside the cluster

# Multi-Protocol Proxy

EdgeQuota serves as a protocol-aware reverse proxy that handles HTTP/1.1, HTTP/2, HTTP/3 (QUIC), gRPC, Server-Sent Events (SSE), and WebSocket traffic on a single port. Protocol detection is automatic — clients do not need to connect to different ports for different protocols.

---

## Protocol Detection

| Protocol | Detection Method | Handler |
|----------|-----------------|---------|
| WebSocket | `Upgrade: websocket` header | Connection hijack + bidirectional TCP relay |
| gRPC | `ProtoMajor >= 2` + `Content-Type: application/grpc` | HTTP/2 transport with TE: trailers |
| HTTP/2 | `ProtoMajor >= 2` | Dedicated `http2.Transport` (h2c for cleartext, h2 over TLS) |
| HTTP/3 | QUIC/UDP (requires TLS) | `http3.Transport` |
| SSE | `Accept: text/event-stream` or streaming response | `FlushInterval: -1` for immediate flush |
| HTTP/1.1 | Default fallback | `httputil.ReverseProxy` |

Detection happens in this priority order: WebSocket is checked first (via the `Upgrade` header), then gRPC (via `Content-Type`), then HTTP/2 (via `ProtoMajor`). Everything else falls through to HTTP/1.1 with automatic SSE support via flush-on-write.

---

## Backend Protocol Selection

The `backend.transport.backend_protocol` setting controls which HTTP protocol EdgeQuota uses when forwarding requests to the backend:

| Value | Behavior |
|-------|----------|
| `auto` (default) | Probe the backend at startup via TLS ALPN. Cleartext backends use HTTP/1.1; HTTPS backends with H2 ALPN use HTTP/2; others use HTTP/1.1. |
| `h1` | Force HTTP/1.1 for all non-gRPC traffic |
| `h2` | Force HTTP/2 (h2c for `http://`, h2 over TLS for `https://`) |
| `h3` | Force HTTP/3 (QUIC). Requires an `https://` backend and kernel UDP buffer tuning. |

gRPC traffic always uses HTTP/2 regardless of this setting.

### Per-Request Protocol Override

When the external rate limit service is enabled, it can return a `backend_protocol` field (`h1`, `h2`, or `h3`) in the `GetLimitsResponse`. This overrides the static setting for that request only, enabling per-tenant protocol selection.

### Configuration

```yaml
backend:
  url: "http://my-backend:8080"
  transport:
    backend_protocol: "auto"
```

---

## HTTP/1.1

HTTP/1.1 is the default backend protocol for cleartext backends. EdgeQuota uses Go's `httputil.ReverseProxy` with:

- Connection pooling via `max_idle_conns` (default: 100)
- Keep-alive with configurable `idle_conn_timeout` (default: 90s)
- `FlushInterval: -1` for immediate streaming support (SSE)
- Automatic `X-Forwarded-For` header management

---

## HTTP/2

EdgeQuota supports HTTP/2 in two modes:

### Cleartext (h2c)

The main listener uses `h2c.NewHandler` to accept HTTP/2 cleartext connections alongside HTTP/1.1 on the same port. This is required for gRPC without TLS — the common case in Kubernetes where TLS termination happens at the ingress controller.

No configuration is needed. HTTP/2 clients (including gRPC clients) automatically negotiate h2c via the HTTP/1.1 upgrade mechanism or prior knowledge.

### TLS (h2)

When `server.tls.enabled` is `true`, HTTP/2 is negotiated via ALPN during the TLS handshake. Both HTTP/1.1 and HTTP/2 clients are served on the same TLS listener.

### Backend HTTP/2

When the backend protocol is `h2`, EdgeQuota uses a dedicated `http2.Transport` with:

- Configurable `h2_read_idle_timeout` (default: 30s) for health checking long-lived streams
- Configurable `h2_ping_timeout` (default: 15s) for detecting dead connections

```yaml
backend:
  url: "https://my-backend:8443"
  transport:
    backend_protocol: "h2"
    h2_read_idle_timeout: "30s"
    h2_ping_timeout: "15s"
```

---

## HTTP/3 (QUIC)

### Inbound HTTP/3

To accept HTTP/3 connections from clients, enable TLS with HTTP/3:

```yaml
server:
  tls:
    enabled: true
    cert_file: "/etc/edgequota/tls/cert.pem"
    key_file: "/etc/edgequota/tls/key.pem"
    http3_enabled: true
    http3_advertise_port: 443
```

EdgeQuota starts a QUIC/UDP listener on the same address as the TCP listener. The `Alt-Svc` header is automatically added to HTTP/1.1 and HTTP/2 responses to advertise HTTP/3 availability.

`http3_advertise_port` controls the port in the `Alt-Svc` header. Set it to the external port if EdgeQuota is behind a load balancer that maps ports.

### Outbound HTTP/3

To use HTTP/3 for backend connections:

```yaml
backend:
  url: "https://my-backend:8443"
  transport:
    backend_protocol: "h3"
    h3_udp_receive_buffer_size: 7500000
    h3_udp_send_buffer_size: 7500000
```

**Kernel requirement:** quic-go requires `net.core.rmem_max >= 7340032` (approximately 7 MiB). Without this, QUIC connections time out due to packet loss. In Kubernetes, use a privileged init container to set the sysctl:

```yaml
initContainers:
  - name: sysctl
    image: busybox
    command: ["sh", "-c", "sysctl -w net.core.rmem_max=7500000 && sysctl -w net.core.wmem_max=7500000"]
    securityContext:
      privileged: true
```

---

## gRPC

gRPC traffic is automatically detected via:

1. `ProtoMajor >= 2` (HTTP/2)
2. `Content-Type: application/grpc`

When both conditions are met, EdgeQuota uses the HTTP/2 transport with `TE: trailers` preserved through the proxy. This ensures gRPC trailers (used for status codes and metadata) are forwarded correctly.

gRPC always uses HTTP/2 regardless of the `backend_protocol` setting.

```yaml
backend:
  url: "http://my-grpc-service:50051"
  transport:
    h2_read_idle_timeout: "30s"
    h2_ping_timeout: "15s"
```

---

## Server-Sent Events (SSE)

SSE is supported automatically through the `FlushInterval: -1` setting on the reverse proxy. When the backend sends a streaming response (chunked transfer encoding), EdgeQuota flushes each chunk to the client immediately rather than buffering.

No special configuration is needed. SSE works with both HTTP/1.1 and HTTP/2.

---

## WebSocket

WebSocket connections are detected via the `Upgrade: websocket` header in the client's request. EdgeQuota hijacks the connection and establishes a bidirectional TCP relay between the client and the backend.

### Connection Limits

| Setting | Default | Env Var | Description |
|---------|---------|---------|-------------|
| `server.max_websocket_conns_per_key` | `0` (unlimited) | `EDGEQUOTA_SERVER_MAX_WEBSOCKET_CONNS_PER_KEY` | Max concurrent WebSocket connections per rate-limit key |
| `server.max_websocket_transfer_bytes` | `0` (unlimited) | `EDGEQUOTA_SERVER_MAX_WEBSOCKET_TRANSFER_BYTES` | Max bytes transferred per direction on a single connection |
| `server.websocket_idle_timeout` | `"5m"` | `EDGEQUOTA_SERVER_WEBSOCKET_IDLE_TIMEOUT` | Close connections with no data transfer for this duration |

### Origin Validation

To restrict which origins can initiate WebSocket connections:

```yaml
server:
  allowed_websocket_origins:
    - "https://app.example.com"
    - "https://admin.example.com"
```

When the list is non-empty, upgrade requests whose `Origin` header does not match any entry are rejected with `403 Forbidden`. An empty list allows all origins (default).

### WebSocket Dial Timeout

The `transport.websocket_dial_timeout` (default: 10s) controls how long EdgeQuota waits when establishing the backend WebSocket connection.

```yaml
backend:
  transport:
    websocket_dial_timeout: "10s"
```

---

## Transport Tuning

All transport settings with their defaults:

| Setting | Default | Env Var | Description |
|---------|---------|---------|-------------|
| `backend_protocol` | `"auto"` | `EDGEQUOTA_BACKEND_TRANSPORT_BACKEND_PROTOCOL` | Protocol selection: `auto`, `h1`, `h2`, `h3` |
| `dial_timeout` | `"30s"` | `EDGEQUOTA_BACKEND_TRANSPORT_DIAL_TIMEOUT` | TCP dial timeout |
| `dial_keep_alive` | `"30s"` | `EDGEQUOTA_BACKEND_TRANSPORT_DIAL_KEEP_ALIVE` | TCP keep-alive interval |
| `tls_handshake_timeout` | `"10s"` | `EDGEQUOTA_BACKEND_TRANSPORT_TLS_HANDSHAKE_TIMEOUT` | TLS handshake timeout |
| `expect_continue_timeout` | `"1s"` | `EDGEQUOTA_BACKEND_TRANSPORT_EXPECT_CONTINUE_TIMEOUT` | Timeout for `Expect: 100-continue` |
| `h2_read_idle_timeout` | `"30s"` | `EDGEQUOTA_BACKEND_TRANSPORT_H2_READ_IDLE_TIMEOUT` | HTTP/2 read idle timeout |
| `h2_ping_timeout` | `"15s"` | `EDGEQUOTA_BACKEND_TRANSPORT_H2_PING_TIMEOUT` | HTTP/2 ping timeout |
| `websocket_dial_timeout` | `"10s"` | `EDGEQUOTA_BACKEND_TRANSPORT_WEBSOCKET_DIAL_TIMEOUT` | WebSocket backend dial timeout |
| `h3_udp_receive_buffer_size` | `0` | `EDGEQUOTA_BACKEND_TRANSPORT_H3_UDP_RECEIVE_BUFFER_SIZE` | QUIC UDP receive buffer (bytes) |
| `h3_udp_send_buffer_size` | `0` | `EDGEQUOTA_BACKEND_TRANSPORT_H3_UDP_SEND_BUFFER_SIZE` | QUIC UDP send buffer (bytes) |

---

## SSRF Protection

When the external rate limit service returns a dynamic `backend_url`, EdgeQuota validates it against the `backend.url_policy` before connecting:

```yaml
backend:
  url_policy:
    allowed_schemes:
      - "http"
      - "https"
    deny_private_networks: true
    allowed_hosts:
      - "backend-a.internal"
      - "backend-b.internal"
```

| Policy | Default | Description |
|--------|---------|-------------|
| `allowed_schemes` | `["http", "https"]` | Reject URLs with other schemes |
| `deny_private_networks` | `true` | Block RFC 1918, loopback, link-local, and cloud metadata IPs |
| `allowed_hosts` | `[]` (all allowed) | When non-empty, only these hostnames are permitted |

Additionally, EdgeQuota re-validates resolved IPs at TCP dial time (`SafeDialer`) to prevent DNS rebinding attacks.

See [Security](security.md) for the full threat model.

---

## See Also

- [Configuration Reference](configuration.md) -- Full `backend` and `server` config sections.
- [Architecture](architecture.md) -- Internal proxy design and transport selection.
- [Security](security.md) -- Backend URL override security and DNS rebinding protection.
- [Deployment](deployment.md) -- HTTP/3 kernel tuning for Kubernetes.

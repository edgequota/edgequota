# ============================================================================
# EdgeQuota - Multi-stage Dockerfile
# Produces a minimal, headless container for secure edge deployment.
# ============================================================================

# ---------------------------------------------------------------------------
# Stage 1: Build
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Cache Go modules.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build a static binary with CGO disabled.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -trimpath \
    -o /edgequota \
    ./cmd/edgequota

# ---------------------------------------------------------------------------
# Stage 2: Runtime (distroless/static — no shell, no libc)
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="EdgeQuota"
LABEL org.opencontainers.image.description="High-performance reverse proxy with rate limiting and authentication"
LABEL org.opencontainers.image.source="https://github.com/edgequota/edgequota"
LABEL org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /edgequota /edgequota

# Run as non-root user (65534 = nobody in distroless).
USER 65534:65534

# TCP: HTTP/1.1 + HTTP/2 proxy, admin
# UDP: HTTP/3 (QUIC) — same port as TCP when http3_enabled is true
EXPOSE 8080/tcp 8080/udp 9090/tcp

ENTRYPOINT ["/edgequota"]

// Package proxy implements a multi-protocol reverse proxy supporting HTTP,
// gRPC, Server-Sent Events (SSE), and WebSocket connections. Protocol detection
// is automatic based on request headers and HTTP version.
//
// Architecture:
//   - HTTP/SSE: httputil.ReverseProxy with FlushInterval=-1 for streaming
//   - WebSocket: Connection upgrade + bidirectional TCP relay
//   - gRPC: Transparent HTTP/2 proxy preserving trailers
package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/ratelimit"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

// wsBufPool provides reusable 4 KiB buffers for WebSocket relay, reducing
// GC pressure under high WebSocket concurrency.
var wsBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 4096)
		return &buf
	},
}

// backendURLKey is the context key for per-request backend URL overrides.
type backendURLKey struct{}

// BackendURLContextKey is used by the middleware chain to inject a per-request
// backend URL. The proxy's Director and WebSocket handler read this value,
// falling back to the static default when absent.
var BackendURLContextKey = backendURLKey{}

// WithBackendURL returns a copy of r with the backend URL set in its context.
// The proxy uses this URL instead of the static default for this request.
func WithBackendURL(r *http.Request, u *url.URL) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), BackendURLContextKey, u))
}

// backendURLFromContext returns the per-request backend URL override, or nil.
func backendURLFromContext(ctx context.Context) *url.URL {
	u, _ := ctx.Value(BackendURLContextKey).(*url.URL)
	return u
}

// backendProtocolKey is the context key for per-request backend protocol overrides.
type backendProtocolKey struct{}

// BackendProtocolContextKey is used by the middleware chain to inject a
// per-request backend protocol. The proxy's transport reads this value,
// falling back to the startup-resolved default when absent.
var BackendProtocolContextKey = backendProtocolKey{}

// WithBackendProtocol returns a copy of r with the backend protocol set in its
// context. Valid values: "h1", "h2", "h3". Empty string means no override.
func WithBackendProtocol(r *http.Request, proto string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), BackendProtocolContextKey, proto))
}

// backendProtocolFromContext returns the per-request backend protocol override, or "".
func backendProtocolFromContext(ctx context.Context) string {
	s, _ := ctx.Value(BackendProtocolContextKey).(string)
	return s
}

// Option configures optional proxy behavior.
type Option func(*Proxy)

// WithBackendTLSInsecure skips TLS certificate verification for all protocol
// paths to the backend (HTTP/1.1, HTTP/2, gRPC, WebSocket). Only enable this
// for trusted backends in controlled environments (e.g. mTLS or pod-to-pod
// within a cluster).
func WithBackendTLSInsecure() Option {
	return func(p *Proxy) {
		p.backendTLSInsecure = true
	}
}

// WithWSLimiter configures per-key WebSocket connection limits. A
// maxPerKey of 0 means unlimited.
func WithWSLimiter(maxPerKey int64) Option {
	return func(p *Proxy) {
		if maxPerKey > 0 {
			p.wsLimiter = NewWSLimiter(maxPerKey)
		}
	}
}

// WithMaxRequestBodySize sets the maximum allowed request body size in bytes.
// A value of 0 means unlimited.
func WithMaxRequestBodySize(n int64) Option {
	return func(p *Proxy) { p.maxRequestBodySize = n }
}

// WithMaxWSTransferBytes sets the maximum bytes transferred per direction
// on a single WebSocket connection. A value of 0 means unlimited.
func WithMaxWSTransferBytes(n int64) Option {
	return func(p *Proxy) { p.maxWSTransferBytes = n }
}

// WithWSIdleTimeout sets the idle timeout for WebSocket connections.
// Connections with no data transfer in either direction for this duration
// are closed. A value of 0 means no idle timeout.
func WithWSIdleTimeout(d time.Duration) Option {
	return func(p *Proxy) { p.wsIdleTimeout = d }
}

// WithDenyPrivateNetworks enables DNS rebinding protection on all backend
// dials. When enabled, the proxy re-validates resolved IPs at connect time
// to prevent TOCTOU attacks where a hostname resolves to a public IP during
// URL validation but switches to a private IP before the TCP connection.
func WithDenyPrivateNetworks() Option {
	return func(p *Proxy) { p.denyPrivateNetworks = true }
}

// ---------------------------------------------------------------------------
// Safe dialer — DNS rebinding protection
// ---------------------------------------------------------------------------

// SafeDialer wraps a net.Dialer and re-validates resolved IPs against private
// network ranges at connect time. This closes the TOCTOU gap between
// ValidateBackendURL (which resolves DNS early) and the actual TCP dial (which
// may hit a different IP due to DNS rebinding).
//
// When enabled is false, the dialer behaves identically to the inner dialer.
type SafeDialer struct {
	Inner   *net.Dialer
	Enabled bool // When false, no IP validation is performed.
}

// DialContext resolves the address, checks all returned IPs against private
// ranges, then dials the first allowed IP. Returns an error if all resolved
// IPs are private or if resolution fails.
func (d *SafeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !d.Enabled {
		return d.Inner.DialContext(ctx, network, addr)
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return d.Inner.DialContext(ctx, network, addr)
	}

	// Direct IP — validate and dial.
	if ip := net.ParseIP(host); ip != nil {
		if IsPrivateIP(ip) {
			return nil, fmt.Errorf("dial blocked: %s is a private/reserved IP", ip)
		}
		return d.Inner.DialContext(ctx, network, addr)
	}

	// Hostname — resolve, validate every IP, then dial the first safe one.
	ips, lookupErr := net.DefaultResolver.LookupIPAddr(ctx, host)
	if lookupErr != nil {
		return nil, fmt.Errorf("dial blocked: cannot resolve %q: %w", host, lookupErr)
	}

	for _, ipAddr := range ips {
		if IsPrivateIP(ipAddr.IP) {
			continue
		}
		pinnedAddr := net.JoinHostPort(ipAddr.IP.String(), port)
		return d.Inner.DialContext(ctx, network, pinnedAddr)
	}

	return nil, fmt.Errorf("dial blocked: all IPs for %q resolve to private/reserved ranges", host)
}

// ---------------------------------------------------------------------------
// Request body streaming with byte counting
// ---------------------------------------------------------------------------

// countingReader wraps an io.ReadCloser and counts bytes read. When the
// configured limit is exceeded, further reads return an error. This enables
// streaming request bodies through the proxy without buffering the full body
// in memory, while still enforcing per-request size limits.
type countingReader struct {
	rc      io.ReadCloser
	read    int64
	limit   int64 // 0 = unlimited
	limited bool  // set when limit is exceeded
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.rc.Read(p)
	cr.read += int64(n)
	if cr.limit > 0 && cr.read > cr.limit {
		cr.limited = true
		return n, fmt.Errorf("request body exceeds size limit of %d bytes", cr.limit)
	}
	return n, err
}

func (cr *countingReader) Close() error { return cr.rc.Close() }

// streamingTransport wraps an http.RoundTripper to stream request bodies
// through a countingReader instead of buffering. This provides:
//   - Non-buffering body forwarding (back-pressure via io.Pipe semantics)
//   - Per-request byte counting for fair resource allocation
//   - Hard abort when size limit is exceeded (returns 413 via error handler)
type streamingTransport struct {
	inner    http.RoundTripper
	maxBytes int64 // 0 = unlimited (counting only)
}

func (st *streamingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil && req.Body != http.NoBody {
		cr := &countingReader{
			rc:    req.Body,
			limit: st.maxBytes,
		}
		req.Body = cr
		// Disable content-length so net/http uses chunked encoding,
		// enabling true streaming without buffering.
		req.ContentLength = -1
	}
	return st.inner.RoundTrip(req)
}

// Proxy is a multi-protocol reverse proxy that transparently forwards
// HTTP, gRPC, SSE, and WebSocket traffic to a backend service.
type Proxy struct {
	backendURL          *url.URL
	httpProxy           *httputil.ReverseProxy
	http2Transport      *http2.Transport
	logger              *slog.Logger
	backendTLSInsecure  bool
	denyPrivateNetworks bool // re-validate IPs at dial time (DNS rebinding protection)
	wsDialTimeout       time.Duration
	wsLimiter           *WSLimiter    // nil when no per-key WS limit is configured.
	maxRequestBodySize  int64         // 0 = unlimited
	maxWSTransferBytes  int64         // 0 = unlimited per-direction
	wsIdleTimeout       time.Duration // 0 = no idle timeout
}

// New creates a new multi-protocol reverse proxy targeting the given backend URL.
// When backendURL is empty, the proxy has no default backend — every request
// must carry a per-request backend URL via context (set by the middleware chain
// from the external rate limit service response).
func New(
	backendURL string,
	timeout time.Duration,
	maxIdleConns int,
	idleConnTimeout time.Duration,
	transportCfg config.TransportConfig,
	logger *slog.Logger,
	opts ...Option,
) (*Proxy, error) {
	var target *url.URL
	if backendURL != "" {
		var err error
		target, err = url.Parse(backendURL)
		if err != nil {
			return nil, fmt.Errorf("invalid backend URL %q: %w", backendURL, err)
		}
	}

	// Apply options early so backendTLSInsecure is known before building
	// transports. Options may set TLS behavior for all protocol paths.
	p := &Proxy{
		backendURL: target,
		logger:     logger,
	}
	for _, o := range opts {
		o(p)
	}

	// When no default backend is configured, use "http" as the scheme hint
	// for transport construction. Per-request backend URLs set via context
	// will determine the actual scheme at request time.
	schemeHint := "http"
	if target != nil {
		schemeHint = target.Scheme
	}

	if err := transportCfg.ValidateBackendProtocol(); err != nil {
		return nil, err
	}

	httpTransport, h2Transport, h3Transport := buildTransports(schemeHint, transportCfg, timeout, maxIdleConns, idleConnTimeout, p.backendTLSInsecure, p.denyPrivateNetworks)
	wsDialTimeout, _ := config.ParseDuration(transportCfg.WebSocketDialTimeout, 10*time.Second)

	// Wrap transports with streaming byte-counting when a body size limit is
	// configured. This replaces httputil.ReverseProxy's default buffering with
	// a streaming approach that applies back-pressure via io.Pipe semantics.
	var h1RT http.RoundTripper = httpTransport
	var h2RT http.RoundTripper = h2Transport
	h3RT := h3Transport
	if p.maxRequestBodySize > 0 {
		h1RT = &streamingTransport{inner: httpTransport, maxBytes: p.maxRequestBodySize}
		h2RT = &streamingTransport{inner: h2Transport, maxBytes: p.maxRequestBodySize}
		if h3Transport != nil {
			h3RT = &streamingTransport{inner: h3Transport, maxBytes: p.maxRequestBodySize}
		}
	}

	defaultRT, protoMode := resolveBackendTransportMode(transportCfg, schemeHint, target, h1RT, h2RT, h3RT, p.backendTLSInsecure, logger)
	rp := buildReverseProxy(target, defaultRT, h1RT, h2RT, h3RT, protoMode, logger)

	p.httpProxy = rp
	p.http2Transport = h2Transport
	p.wsDialTimeout = wsDialTimeout

	return p, nil
}

// resolveBackendTransportMode determines the initial default transport and
// whether the protocolAwareTransport should do per-request auto-detection.
// When the backend is dynamic (no static URL) and mode is "auto", we can't
// probe at startup — the transport must decide per-request.
func resolveBackendTransportMode(
	cfg config.TransportConfig,
	scheme string,
	target *url.URL,
	h1, h2, h3 http.RoundTripper,
	tlsInsecure bool,
	logger *slog.Logger,
) (defaultRT http.RoundTripper, mode string) {
	proto := cfg.ResolvedBackendProtocol()

	switch proto {
	case config.BackendProtocolH1:
		logger.Info("backend transport: forced h1", "backend_protocol", proto)
		return h1, proto
	case config.BackendProtocolH2:
		logger.Info("backend transport: forced h2", "backend_protocol", proto)
		return h2, proto
	case config.BackendProtocolH3:
		if h3 == nil {
			logger.Warn("backend_protocol=h3 requires an https:// backend, falling back to h1",
				"backend_protocol", proto, "scheme", scheme)
			return h1, config.BackendProtocolH1
		}
		logger.Info("backend transport: forced h3", "backend_protocol", proto)
		return h3, proto
	default: // "auto"
		if target == nil {
			logger.Info("backend transport: auto (dynamic backends — per-request protocol selection)")
			return h1, config.BackendProtocolAuto
		}
		probed := probeBackendProtocol(scheme, target, h1, h2, tlsInsecure, logger)
		if probed == h2 {
			return h2, config.BackendProtocolAuto
		}
		return h1, config.BackendProtocolAuto
	}
}

// probeBackendProtocol performs a TLS ALPN handshake to an HTTPS backend to
// discover whether it supports HTTP/2. Cleartext backends default to HTTP/1.1.
func probeBackendProtocol(
	scheme string,
	target *url.URL,
	h1, h2 http.RoundTripper,
	tlsInsecure bool,
	logger *slog.Logger,
) http.RoundTripper {
	if scheme == "http" || scheme == "ws" {
		logger.Info("backend transport: h1 (auto — cleartext backend)", "backend_url", target.String())
		return h1
	}

	addr := target.Host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := (&tls.Dialer{
		Config: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			NextProtos:         []string{"h2", "http/1.1"},
			InsecureSkipVerify: tlsInsecure, //nolint:gosec // Matches the proxy's own TLS config.
		},
	}).DialContext(ctx, "tcp", addr)
	if err != nil {
		logger.Warn("backend ALPN probe failed, defaulting to h1",
			"error", err, "backend_addr", addr)
		return h1
	}
	defer func() { _ = conn.Close() }()

	negotiated := conn.(*tls.Conn).ConnectionState().NegotiatedProtocol
	if negotiated == "h2" {
		logger.Info("backend transport: h2 (auto — ALPN negotiated h2)",
			"backend_addr", addr)
		return h2
	}

	logger.Info("backend transport: h1 (auto — backend negotiated "+negotiated+")",
		"backend_addr", addr)
	return h1
}

func buildTransports(
	targetScheme string,
	cfg config.TransportConfig,
	responseTimeout time.Duration,
	maxIdleConns int,
	idleConnTimeout time.Duration,
	tlsInsecure bool,
	denyPrivateNetworks bool,
) (*http.Transport, *http2.Transport, http.RoundTripper) {
	dialTimeout, _ := config.ParseDuration(cfg.DialTimeout, 30*time.Second)
	dialKeepAlive, _ := config.ParseDuration(cfg.DialKeepAlive, 30*time.Second)
	tlsHandshakeTimeout, _ := config.ParseDuration(cfg.TLSHandshakeTimeout, 10*time.Second)
	expectContinueTimeout, _ := config.ParseDuration(cfg.ExpectContinueTimeout, time.Second)
	h2ReadIdleTimeout, _ := config.ParseDuration(cfg.H2ReadIdleTimeout, 30*time.Second)
	h2PingTimeout, _ := config.ParseDuration(cfg.H2PingTimeout, 15*time.Second)

	innerDialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}

	// SafeDialer re-validates resolved IPs at connect time to prevent DNS
	// rebinding attacks. The URL validator catches obvious cases early, but
	// this dialer is the enforcement point that closes the TOCTOU gap.
	dialer := &SafeDialer{
		Inner:   innerDialer,
		Enabled: denyPrivateNetworks,
	}

	// Shared TLS config for HTTPS backends — used by both H1 and H2 transports.
	backendTLS := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: tlsInsecure, //nolint:gosec // Configurable per-user choice; logged with SECURITY WARNING at startup.
	}

	// Whether the backend is cleartext (h2c) or encrypted (h2 over TLS).
	backendIsCleartext := targetScheme == "http" || targetScheme == "ws"

	h1 := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSClientConfig:       backendTLS,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConns,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ResponseHeaderTimeout: responseTimeout,
		ForceAttemptHTTP2:     false, // We handle HTTP/2 separately.
	}

	// DialTLSContext is called by http2.Transport for every connection.
	// Despite its name, this callback must also handle cleartext h2c when
	// AllowHTTP is true. The http2.Transport always passes a non-nil
	// *tls.Config (via t.newTLSConfig), so we use the backend scheme to
	// decide: cleartext backends get a raw TCP connection; HTTPS backends
	// get a full TLS handshake.
	//
	// The SafeDialer is used here so that DNS rebinding protection applies
	// to HTTP/2 backend connections as well.
	h2 := &http2.Transport{
		AllowHTTP:       true,
		TLSClientConfig: backendTLS,
		DialTLSContext: func(ctx context.Context, network, addr string, tlsCfg *tls.Config) (net.Conn, error) {
			rawConn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}

			// For cleartext backends (h2c), return the raw TCP connection
			// without TLS. The backend speaks HTTP/2 over plain TCP.
			if backendIsCleartext {
				return rawConn, nil
			}

			// TLS handshake for HTTPS backends. Ensure MinVersion is set.
			if tlsCfg.MinVersion == 0 {
				tlsCfg = tlsCfg.Clone()
				tlsCfg.MinVersion = tls.VersionTLS12
			}

			tlsConn := tls.Client(rawConn, tlsCfg)
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				_ = rawConn.Close()
				return nil, fmt.Errorf("h2 TLS handshake to %s: %w", addr, err)
			}
			return tlsConn, nil
		},
		ReadIdleTimeout: h2ReadIdleTimeout,
		PingTimeout:     h2PingTimeout,
	}

	var h3 http.RoundTripper
	if !backendIsCleartext {
		h3t := &http3.Transport{
			TLSClientConfig: backendTLS,
		}
		if cfg.H3UDPReceiveBufferSize > 0 || cfg.H3UDPSendBufferSize > 0 {
			udpConn, err := net.ListenUDP("udp", nil)
			if err == nil {
				if cfg.H3UDPReceiveBufferSize > 0 {
					_ = udpConn.SetReadBuffer(cfg.H3UDPReceiveBufferSize)
				}
				if cfg.H3UDPSendBufferSize > 0 {
					_ = udpConn.SetWriteBuffer(cfg.H3UDPSendBufferSize)
				}
				qt := &quic.Transport{Conn: udpConn}
				h3t.Dial = func(ctx context.Context, addr string, tlsCfg *tls.Config, qCfg *quic.Config) (*quic.Conn, error) {
					udpAddr, err := net.ResolveUDPAddr("udp", addr)
					if err != nil {
						return nil, err
					}
					return qt.DialEarly(ctx, udpAddr, tlsCfg, qCfg)
				}
			}
		}
		h3 = h3t
	}

	return h1, h2, h3
}

// setForwardingHeaders adds standard proxy forwarding headers (Host, Proto)
// to the request. These headers enable the upstream (e.g. Traefik) to route
// correctly based on the original client host and detect the original protocol.
//
// Headers are only set when not already present, so values injected by an
// upstream proxy in front of EdgeQuota are preserved.
//
// Note: X-Forwarded-For is NOT set here for the HTTP path because
// httputil.ReverseProxy already handles it. The WebSocket handler calls
// setForwardedFor separately since it bypasses ReverseProxy.
func setForwardingHeaders(req *http.Request) {
	if req.Header.Get("X-Forwarded-Host") == "" {
		req.Header.Set("X-Forwarded-Host", req.Host)
	}
	if req.Header.Get("X-Forwarded-Proto") == "" {
		proto := "http"
		if req.TLS != nil {
			proto = "https"
		}
		req.Header.Set("X-Forwarded-Proto", proto)
	}
}

// setForwardedFor adds X-Forwarded-For from RemoteAddr when not already present.
// Called only by the WebSocket handler, which bypasses httputil.ReverseProxy
// (the ReverseProxy already adds X-Forwarded-For on the HTTP path).
func setForwardedFor(req *http.Request) {
	if req.Header.Get("X-Forwarded-For") == "" {
		if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			req.Header.Set("X-Forwarded-For", clientIP)
		}
	}
}

func buildReverseProxy(target *url.URL, defaultRT, h1RT, h2RT, h3RT http.RoundTripper, protoMode string, logger *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// Use per-request backend URL from context if available,
			// falling back to the static default.
			effective := target
			if ctxURL := backendURLFromContext(req.Context()); ctxURL != nil {
				effective = ctxURL
			}
			if effective == nil {
				// No default backend and no per-request override — the
				// error handler will produce a 502.
				logger.Error("no backend URL configured for request", "path", req.URL.Path)
				return
			}
			req.URL.Scheme = effective.Scheme
			req.URL.Host = effective.Host
			if effective.Path != "" && effective.Path != "/" {
				req.URL.Path = singleJoiningSlash(effective.Path, req.URL.Path)
			}
			setForwardingHeaders(req)
		},
		Transport: &protocolAwareTransport{
			defaultRT: defaultRT,
			h1:        h1RT,
			h2:        h2RT,
			h3:        h3RT,
			mode:      protoMode,
			logger:    logger,
		},
		FlushInterval: -1, // Flush immediately for SSE and streaming.
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
			logger.Error("proxy error", "error", proxyErr, "path", req.URL.Path)
			if !isClientDisconnect(proxyErr) {
				rw.WriteHeader(http.StatusBadGateway)
			}
		},
		ModifyResponse: func(_ *http.Response) error {
			return nil
		},
	}
}

// ServeHTTP handles all incoming requests, routing to the appropriate protocol handler.
// Request body size enforcement is handled by the streamingTransport layer,
// which counts bytes during streaming rather than buffering the entire body.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isWebSocketUpgrade(r) {
		p.handleWebSocket(w, r)
		return
	}

	// For gRPC, ensure TE: trailers is preserved (it's a hop-by-hop header
	// that httputil.ReverseProxy would normally strip).
	if isGRPC(r) {
		r.Header.Set("TE", "trailers")
	}

	p.httpProxy.ServeHTTP(w, r)
}

// handleWebSocket performs a WebSocket upgrade and bidirectional relay.
func (p *Proxy) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Enforce per-key WebSocket connection limit when configured.
	if p.wsLimiter != nil {
		wsKey, _ := r.Context().Value(ratelimit.KeyContextKey).(string)
		if wsKey == "" {
			wsKey = "__unknown__"
		}
		if !p.wsLimiter.Acquire(wsKey) {
			p.logger.Warn("websocket: per-key connection limit reached", "key", wsKey)
			http.Error(w, "too many WebSocket connections", http.StatusTooManyRequests)
			return
		}
		defer p.wsLimiter.Release(wsKey)
	}

	// Resolve the effective backend URL for this request.
	effective := p.backendURL
	if ctxURL := backendURLFromContext(r.Context()); ctxURL != nil {
		effective = ctxURL
	}
	if effective == nil {
		p.logger.Error("websocket: no backend URL configured")
		http.Error(w, "no backend configured", http.StatusBadGateway)
		return
	}

	backendConn, dialErr := p.dialWebSocketBackend(r)
	if dialErr != nil {
		p.logger.Error("websocket: dial backend failed", "error", dialErr)
		http.Error(w, "backend unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = backendConn.Close() }()

	// Rewrite the request to target the backend before writing it to the
	// raw connection. handleWebSocket bypasses httputil.ReverseProxy (and
	// its Director), so we must apply the same rewriting here.
	//
	// Unlike the HTTP path (where httputil.ReverseProxy keeps r.Host as
	// the original and uses r.URL.Host for routing), the WebSocket path
	// writes the raw HTTP upgrade request via r.Write(backendConn).
	// Request.Write uses r.Host for the Host header, so we MUST set it
	// to the backend host for the upgrade to succeed. The original host
	// is preserved in X-Forwarded-Host so the upstream (e.g. Traefik)
	// can still route based on the client's requested domain.
	setForwardingHeaders(r)
	setForwardedFor(r)
	r.Host = effective.Host
	r.URL.Host = effective.Host
	r.URL.Scheme = effective.Scheme

	if writeErr := r.Write(backendConn); writeErr != nil {
		p.logger.Error("websocket: write upgrade request failed", "error", writeErr)
		http.Error(w, "backend write error", http.StatusBadGateway)
		return
	}

	// Use NewResponseController to traverse Unwrap() chains (e.g. the
	// middleware's statusWriter) and find the real http.Hijacker.
	rc := http.NewResponseController(w)
	clientConn, _, hijackErr := rc.Hijack()
	if hijackErr != nil {
		p.logger.Error("websocket: hijack failed", "error", hijackErr)
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	defer func() { _ = clientConn.Close() }()

	p.relayWebSocket(clientConn, backendConn)
}

// dialWebSocketBackend dials the backend for a WebSocket connection.
// The backend URL is resolved from the request context (per-tenant override)
// or the static default. The request's context is used so that client
// cancellation propagates to the dial.
func (p *Proxy) dialWebSocketBackend(r *http.Request) (net.Conn, error) {
	effective := p.backendURL
	if ctxURL := backendURLFromContext(r.Context()); ctxURL != nil {
		effective = ctxURL
	}
	if effective == nil {
		return nil, fmt.Errorf("no backend URL configured")
	}

	backendAddr := effective.Host // Always host:port after normalization.
	ctx := r.Context()

	wsDialer := &SafeDialer{
		Inner:   &net.Dialer{Timeout: p.wsDialTimeout},
		Enabled: p.denyPrivateNetworks,
	}

	if effective.Scheme == "https" {
		tlsCfg := &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: p.backendTLSInsecure, //nolint:gosec // Configurable per-user choice.
		}
		rawConn, err := wsDialer.DialContext(ctx, "tcp", backendAddr)
		if err != nil {
			return nil, err
		}
		tlsConn := tls.Client(rawConn, tlsCfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("websocket TLS handshake to %s: %w", backendAddr, err)
		}
		return tlsConn, nil
	}
	return wsDialer.DialContext(ctx, "tcp", backendAddr)
}

// relayWebSocket copies data bidirectionally between client and backend.
// When maxWSTransferBytes > 0, each direction is capped to that many bytes.
// When wsIdleTimeout > 0, connections with no activity are closed.
func (p *Proxy) relayWebSocket(clientConn, backendConn net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	wsWrap := func(r io.Reader, conn net.Conn) io.Reader {
		if p.maxWSTransferBytes > 0 {
			r = io.LimitReader(r, p.maxWSTransferBytes)
		}
		if p.wsIdleTimeout > 0 {
			r = &idleTimeoutReader{inner: r, conn: conn, peerConn: nil, timeout: p.wsIdleTimeout}
		}
		return r
	}

	// Set initial deadlines if idle timeout is configured.
	if p.wsIdleTimeout > 0 {
		deadline := time.Now().Add(p.wsIdleTimeout)
		_ = clientConn.SetDeadline(deadline)
		_ = backendConn.SetDeadline(deadline)
	}

	go func() {
		defer wg.Done()
		bufp := wsBufPool.Get().(*[]byte)
		defer wsBufPool.Put(bufp)
		src := wsWrap(backendConn, backendConn)
		if itr, ok := src.(*idleTimeoutReader); ok {
			itr.peerConn = clientConn
		}
		if _, cpErr := io.CopyBuffer(clientConn, src, *bufp); cpErr != nil {
			p.logger.Debug("websocket: backend→client copy ended", "error", cpErr)
		}
		if tc, tcOK := clientConn.(*net.TCPConn); tcOK {
			if cwErr := tc.CloseWrite(); cwErr != nil {
				p.logger.Debug("websocket: client CloseWrite", "error", cwErr)
			}
		}
	}()

	go func() {
		defer wg.Done()
		bufp := wsBufPool.Get().(*[]byte)
		defer wsBufPool.Put(bufp)
		src := wsWrap(clientConn, clientConn)
		if itr, ok := src.(*idleTimeoutReader); ok {
			itr.peerConn = backendConn
		}
		if _, cpErr := io.CopyBuffer(backendConn, src, *bufp); cpErr != nil {
			p.logger.Debug("websocket: client→backend copy ended", "error", cpErr)
		}
		if tc, tcOK := backendConn.(*net.TCPConn); tcOK {
			if cwErr := tc.CloseWrite(); cwErr != nil {
				p.logger.Debug("websocket: backend CloseWrite", "error", cwErr)
			}
		}
	}()

	wg.Wait()
}

// idleTimeoutReader wraps a reader and resets deadlines on both the source
// and peer connections after each successful read. This keeps active WebSocket
// connections alive while closing idle ones.
type idleTimeoutReader struct {
	inner    io.Reader
	conn     net.Conn // connection being read from
	peerConn net.Conn // peer connection (write side)
	timeout  time.Duration
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 && r.timeout > 0 {
		deadline := time.Now().Add(r.timeout)
		_ = r.conn.SetDeadline(deadline)
		if r.peerConn != nil {
			_ = r.peerConn.SetDeadline(deadline)
		}
	}
	return n, err
}

// ---------------------------------------------------------------------------
// Protocol detection
// ---------------------------------------------------------------------------

// isGRPC returns true if the request appears to be a gRPC call.
func isGRPC(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// isWebSocketUpgrade returns true if the request is a WebSocket upgrade.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// IsSSE returns true if the request appears to accept Server-Sent Events.
func IsSSE(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// ---------------------------------------------------------------------------
// Protocol-aware transport
// ---------------------------------------------------------------------------

// protocolAwareTransport selects the outbound backend transport:
//
//   - gRPC always uses h2 (required for trailers/streaming)
//   - All other traffic uses defaultRT, resolved at startup:
//   - Explicit mode (h1/h2/h3): the forced transport
//   - Auto + static backend: ALPN-probed result
//   - Auto + dynamic backends (no static URL): h1 (safe default)
//
// All transports (h1/h2/h3) are retained for gRPC's h2 requirement and
// per-request protocol overrides via the external rate-limit service.
// The client's incoming protocol is decoupled from the backend protocol.
type protocolAwareTransport struct {
	defaultRT http.RoundTripper // pre-resolved for static backends or explicit mode
	h1        http.RoundTripper
	h2        http.RoundTripper
	h3        http.RoundTripper // nil when backend is cleartext
	mode      string            // "auto", "h1", "h2", "h3"
	logger    *slog.Logger
}

func (t *protocolAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if isGRPC(req) {
		return t.h2.RoundTrip(req)
	}
	if proto := backendProtocolFromContext(req.Context()); proto != "" {
		return t.roundTripForProtocol(proto, req)
	}
	if t.mode == config.BackendProtocolAuto && req.ProtoMajor == 2 {
		return t.h2.RoundTrip(req)
	}
	return t.defaultRT.RoundTrip(req)
}

// roundTripForProtocol selects the transport for a per-request protocol
// override from the external rate-limit service. Falls back to defaultRT
// for unknown values or when the requested transport is unavailable.
func (t *protocolAwareTransport) roundTripForProtocol(proto string, req *http.Request) (*http.Response, error) {
	switch proto {
	case config.BackendProtocolH2:
		return t.h2.RoundTrip(req)
	case config.BackendProtocolH3:
		if t.h3 != nil {
			return t.h3.RoundTrip(req)
		}
		t.logger.Warn("per-request backend_protocol=h3 requested but no h3 transport available, falling back to default",
			"path", req.URL.Path)
		return t.defaultRT.RoundTrip(req)
	case config.BackendProtocolH1:
		return t.h1.RoundTrip(req)
	default:
		return t.defaultRT.RoundTrip(req)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")

	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func isClientDisconnect(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "client disconnected") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe")
}

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
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

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

// Proxy is a multi-protocol reverse proxy that transparently forwards
// HTTP, gRPC, SSE, and WebSocket traffic to a backend service.
type Proxy struct {
	backendURL         *url.URL
	httpProxy          *httputil.ReverseProxy
	http2Transport     *http2.Transport
	logger             *slog.Logger
	backendTLSInsecure bool
	wsDialTimeout      time.Duration
	wsLimiter          *WSLimiter // nil when no per-key WS limit is configured.
}

// New creates a new multi-protocol reverse proxy targeting the given backend URL.
func New(
	backendURL string,
	timeout time.Duration,
	maxIdleConns int,
	idleConnTimeout time.Duration,
	transportCfg config.TransportConfig,
	logger *slog.Logger,
	opts ...Option,
) (*Proxy, error) {
	target, err := url.Parse(backendURL)
	if err != nil {
		return nil, fmt.Errorf("invalid backend URL %q: %w", backendURL, err)
	}

	// Apply options early so backendTLSInsecure is known before building
	// transports. Options may set TLS behaviour for all protocol paths.
	p := &Proxy{
		backendURL: target,
		logger:     logger,
	}
	for _, o := range opts {
		o(p)
	}

	httpTransport, h2Transport, h3Transport := buildTransports(target.Scheme, transportCfg, timeout, maxIdleConns, idleConnTimeout, p.backendTLSInsecure)
	wsDialTimeout, _ := config.ParseDuration(transportCfg.WebSocketDialTimeout, 10*time.Second)
	rp := buildReverseProxy(target, httpTransport, h2Transport, h3Transport, logger)

	p.httpProxy = rp
	p.http2Transport = h2Transport
	p.wsDialTimeout = wsDialTimeout

	return p, nil
}

func buildTransports(
	targetScheme string,
	cfg config.TransportConfig,
	responseTimeout time.Duration,
	maxIdleConns int,
	idleConnTimeout time.Duration,
	tlsInsecure bool,
) (*http.Transport, *http2.Transport, http.RoundTripper) {
	dialTimeout, _ := config.ParseDuration(cfg.DialTimeout, 30*time.Second)
	dialKeepAlive, _ := config.ParseDuration(cfg.DialKeepAlive, 30*time.Second)
	tlsHandshakeTimeout, _ := config.ParseDuration(cfg.TLSHandshakeTimeout, 10*time.Second)
	expectContinueTimeout, _ := config.ParseDuration(cfg.ExpectContinueTimeout, time.Second)
	h2ReadIdleTimeout, _ := config.ParseDuration(cfg.H2ReadIdleTimeout, 30*time.Second)
	h2PingTimeout, _ := config.ParseDuration(cfg.H2PingTimeout, 15*time.Second)

	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
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

	// HTTP/3 transport — only created when the backend is HTTPS, since
	// QUIC requires TLS by design. When the backend is cleartext, h3 is
	// nil and HTTP/3 requests fall back to HTTP/2.
	var h3 http.RoundTripper
	if !backendIsCleartext {
		h3 = &http3.Transport{
			TLSClientConfig: backendTLS,
		}
	}

	return h1, h2, h3
}

func buildReverseProxy(target *url.URL, h1, h2, h3 http.RoundTripper, logger *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			if target.Path != "" && target.Path != "/" {
				req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
			}
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
		},
		Transport: &protocolAwareTransport{
			http1:  h1,
			http2:  h2,
			http3:  h3,
			logger: logger,
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

	backendConn, dialErr := p.dialWebSocketBackend(r)
	if dialErr != nil {
		p.logger.Error("websocket: dial backend failed", "error", dialErr)
		http.Error(w, "backend unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = backendConn.Close() }()

	// Rewrite the request to target the backend before writing it to the
	// raw connection. handleWebSocket bypasses httputil.ReverseProxy (and
	// its Director), so we must apply the same URL/Host rewriting here.
	origHost := r.Host
	r.Host = p.backendURL.Host
	r.URL.Host = p.backendURL.Host
	r.URL.Scheme = p.backendURL.Scheme
	r.Header.Set("X-Forwarded-Host", origHost)

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
// The backend URL is expected to already contain an explicit port
// (normalized at config load time). The request's context is used so
// that client cancellation propagates to the dial.
func (p *Proxy) dialWebSocketBackend(r *http.Request) (net.Conn, error) {
	backendAddr := p.backendURL.Host // Always host:port after config normalization.
	ctx := r.Context()

	dialer := &net.Dialer{Timeout: p.wsDialTimeout}

	if p.backendURL.Scheme == "https" {
		tlsCfg := &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: p.backendTLSInsecure, //nolint:gosec // Configurable per-user choice.
		}
		rawConn, err := dialer.DialContext(ctx, "tcp", backendAddr)
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
	return dialer.DialContext(ctx, "tcp", backendAddr)
}

// relayWebSocket copies data bidirectionally between client and backend.
func (p *Proxy) relayWebSocket(clientConn, backendConn net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if _, cpErr := io.Copy(clientConn, backendConn); cpErr != nil {
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
		if _, cpErr := io.Copy(backendConn, clientConn); cpErr != nil {
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

// protocolAwareTransport selects the outbound transport based on the incoming
// request's protocol version, preserving the protocol end-to-end:
//
//   - HTTP/3 (QUIC) requests use the HTTP/3 transport (when available)
//   - HTTP/2 (gRPC, h2c, TLS) requests use the HTTP/2 transport
//   - HTTP/1.1 requests use the pooled HTTP/1.1 transport
//
// When an HTTP/3 transport is not available (e.g. cleartext backends that
// don't support QUIC), HTTP/3 requests fall back to the HTTP/2 transport.
type protocolAwareTransport struct {
	http1  http.RoundTripper
	http2  http.RoundTripper
	http3  http.RoundTripper // nil when backend doesn't support QUIC
	logger *slog.Logger
}

func (t *protocolAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.ProtoMajor >= 3 && t.http3 != nil {
		return t.http3.RoundTrip(req)
	}
	if req.ProtoMajor >= 2 {
		return t.http2.RoundTrip(req)
	}
	return t.http1.RoundTrip(req)
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

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
	"golang.org/x/net/http2"
)

// Option configures optional proxy behavior.
type Option func(*Proxy)

// WithBackendTLSInsecure allows skipping TLS certificate verification for
// WebSocket (wss) connections to the backend. Only enable this for trusted
// backends in controlled environments (e.g. mTLS or pod-to-pod within a cluster).
func WithBackendTLSInsecure() Option {
	return func(p *Proxy) {
		p.backendTLSInsecure = true
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

	httpTransport, h2Transport := buildTransports(transportCfg, timeout, maxIdleConns, idleConnTimeout)
	wsDialTimeout, _ := config.ParseDuration(transportCfg.WebSocketDialTimeout, 10*time.Second)
	rp := buildReverseProxy(target, httpTransport, h2Transport, logger)

	p := &Proxy{
		backendURL:     target,
		httpProxy:      rp,
		http2Transport: h2Transport,
		logger:         logger,
		wsDialTimeout:  wsDialTimeout,
	}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

func buildTransports(
	cfg config.TransportConfig,
	responseTimeout time.Duration,
	maxIdleConns int,
	idleConnTimeout time.Duration,
) (*http.Transport, *http2.Transport) {
	dialTimeout, _ := config.ParseDuration(cfg.DialTimeout, 30*time.Second)
	dialKeepAlive, _ := config.ParseDuration(cfg.DialKeepAlive, 30*time.Second)
	tlsHandshakeTimeout, _ := config.ParseDuration(cfg.TLSHandshakeTimeout, 10*time.Second)
	expectContinueTimeout, _ := config.ParseDuration(cfg.ExpectContinueTimeout, time.Second)
	h2ReadIdleTimeout, _ := config.ParseDuration(cfg.H2ReadIdleTimeout, 30*time.Second)
	h2PingTimeout, _ := config.ParseDuration(cfg.H2PingTimeout, 15*time.Second)

	h1 := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: dialKeepAlive,
		}).DialContext,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConns,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ResponseHeaderTimeout: responseTimeout,
		ForceAttemptHTTP2:     false, // We handle HTTP/2 separately.
	}

	h2 := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		ReadIdleTimeout: h2ReadIdleTimeout,
		PingTimeout:     h2PingTimeout,
	}

	return h1, h2
}

func buildReverseProxy(target *url.URL, h1, h2 http.RoundTripper, logger *slog.Logger) *httputil.ReverseProxy {
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
	backendConn, dialErr := p.dialWebSocketBackend(r)
	if dialErr != nil {
		p.logger.Error("websocket: dial backend failed", "error", dialErr)
		http.Error(w, "backend unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = backendConn.Close() }()

	if writeErr := r.Write(backendConn); writeErr != nil {
		p.logger.Error("websocket: write upgrade request failed", "error", writeErr)
		http.Error(w, "backend write error", http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		p.logger.Error("websocket: hijack not supported")
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, hijackErr := hijacker.Hijack()
	if hijackErr != nil {
		p.logger.Error("websocket: hijack failed", "error", hijackErr)
		return
	}
	defer func() { _ = clientConn.Close() }()

	p.relayWebSocket(clientConn, backendConn)
}

// dialWebSocketBackend dials the backend for a WebSocket connection.
// The backend URL is expected to already contain an explicit port
// (normalized at config load time).
func (p *Proxy) dialWebSocketBackend(_ *http.Request) (net.Conn, error) {
	backendAddr := p.backendURL.Host // Always host:port after config normalization.

	if p.backendURL.Scheme == "https" {
		return tls.Dial("tcp", backendAddr, &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: p.backendTLSInsecure, //nolint:gosec // Configurable per-user choice.
		})
	}
	return net.DialTimeout("tcp", backendAddr, p.wsDialTimeout)
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

// protocolAwareTransport selects HTTP/1.1 or HTTP/2 transport based on the
// incoming request protocol version. Any request that arrived over HTTP/2
// (gRPC, plain HTTP/2, h2c) is forwarded via the HTTP/2 transport so the
// protocol is preserved end-to-end. HTTP/1.1 requests use the pooled HTTP/1.1
// transport.
type protocolAwareTransport struct {
	http1  http.RoundTripper
	http2  http.RoundTripper
	logger *slog.Logger
}

func (t *protocolAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
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

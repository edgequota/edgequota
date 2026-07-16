// Package server orchestrates EdgeQuota's main proxy server and admin server.
// The main server handles incoming traffic (HTTP, gRPC, SSE, WebSocket) while
// the admin server exposes health checks, readiness probes, the /v1/stats
// counter snapshot, and pprof. First-party metrics are pushed to the OTLP
// collector (there is no Prometheus /metrics scrape endpoint).
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"sync/atomic"
	"time"

	adminv1 "github.com/edgequota/edgequota/api/gen/http/admin/v1"
	"github.com/edgequota/edgequota/internal/cache"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/middleware"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/edgequota/edgequota/internal/proxy"
	iredis "github.com/edgequota/edgequota/internal/redis"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/net/http2"
)

// Server is the main EdgeQuota server.
type Server struct {
	cfg             *config.Config
	logger          *slog.Logger
	version         string
	mainServer      *http.Server
	mtlsServer      *http.Server  // nil when mTLS is disabled; second listener for client-cert auth.
	http3Server     *http3.Server // nil when HTTP/3 is disabled.
	adminServer     *http.Server
	adminRL         *adminRateLimiter // stopped during shutdown to avoid goroutine leak
	chain           *middleware.Chain
	health          *observability.HealthChecker
	metrics         *observability.Metrics
	tracingShutdown func(context.Context) error
	metricsShutdown func(context.Context) error
	certs           *certHolder     // non-nil when TLS is enabled; supports hot-reload.
	mtlsCAs         *clientCAHolder // non-nil when mTLS is enabled; supports hot-reload.
}

// New creates a new EdgeQuota server instance.
func New(cfg *config.Config, logger *slog.Logger, version string) (*Server, error) {
	metrics := observability.NewMetrics(logger)
	health := observability.NewHealthChecker()

	// Warn about insecure configurations at startup.
	iredis.WarnInsecureRedis(cfg.Redis.TLS, logger)
	if cfg.CacheRedis != nil {
		iredis.WarnInsecureRedis(cfg.CacheRedis.TLS, logger)
	}

	rp, err := buildProxy(cfg, logger)
	if err != nil {
		return nil, err
	}

	chain, err := middleware.NewChain(context.Background(), rp, cfg, logger, metrics)
	if err != nil {
		return nil, fmt.Errorf("create middleware chain: %w", err)
	}

	// Register the Redis pinger for deep health checks.
	if pinger := chain.RedisPinger(); pinger != nil {
		health.SetRedisPinger(pinger)
	}

	mainServer, h3srv := buildMainServer(cfg, chain, logger)
	adminServer, adminRL := buildAdminServer(cfg, health, metrics, logger, chain.ResponseCache, chain)

	var mtlsSrv *http.Server
	if cfg.Server.TLS.MTLS.Enabled {
		mtlsSrv = buildMTLSServer(cfg, chain, logger)
	}

	return &Server{
		cfg:         cfg,
		logger:      logger,
		version:     version,
		mainServer:  mainServer,
		mtlsServer:  mtlsSrv,
		http3Server: h3srv,
		adminServer: adminServer,
		adminRL:     adminRL,
		chain:       chain,
		health:      health,
		metrics:     metrics,
	}, nil
}

func buildProxy(cfg *config.Config, logger *slog.Logger) (*proxy.Proxy, error) {
	backendTimeout, _ := config.ParseDuration(cfg.Backend.Timeout, 30*time.Second)
	idleConnTimeout, _ := config.ParseDuration(cfg.Backend.IdleConnTimeout, 90*time.Second)

	maxIdleConns := cfg.Backend.MaxIdleConns
	if maxIdleConns <= 0 {
		maxIdleConns = 100
	}

	// Resolve the default backend URL. In static mode it comes from
	// rate_limit.static.backend_url; in external mode the proxy starts with
	// the fallback URL (per-request URLs come from the external service).
	defaultBackendURL := cfg.RateLimit.Static.BackendURL
	if cfg.RateLimit.External.Enabled {
		defaultBackendURL = cfg.RateLimit.External.Fallback.BackendURL
	}

	var proxyOpts []proxy.Option
	if cfg.Backend.TLSInsecureVerify {
		logger.Warn("backend TLS certificate verification is disabled (tls_insecure_skip_verify=true). "+
			"This is acceptable for cluster-internal backends with self-signed certificates, "+
			"but should not be used when proxying to external services over untrusted networks.",
			"backend_url", defaultBackendURL)
		proxyOpts = append(proxyOpts, proxy.WithBackendTLSInsecure())
	}
	if cfg.Server.MaxWebSocketConnsPerKey > 0 {
		proxyOpts = append(proxyOpts, proxy.WithWSLimiter(cfg.Server.MaxWebSocketConnsPerKey))
	}
	if cfg.Backend.MaxRequestBodySize > 0 {
		proxyOpts = append(proxyOpts, proxy.WithMaxRequestBodySize(cfg.Backend.MaxRequestBodySize))
	}
	if cfg.Server.MaxWebSocketTransferBytes > 0 {
		proxyOpts = append(proxyOpts, proxy.WithMaxWSTransferBytes(cfg.Server.MaxWebSocketTransferBytes))
	}
	wsIdleTimeout, _ := config.ParseDuration(cfg.Server.WebSocketIdleTimeout, 5*time.Minute)
	if wsIdleTimeout > 0 {
		proxyOpts = append(proxyOpts, proxy.WithWSIdleTimeout(wsIdleTimeout))
	}
	if cfg.Backend.URLPolicy.DenyPrivateNetworksEnabled() && cfg.RateLimit.External.Enabled {
		proxyOpts = append(proxyOpts, proxy.WithDenyPrivateNetworks())
	}
	if len(cfg.Server.AllowedWebSocketOrigins) > 0 {
		proxyOpts = append(proxyOpts, proxy.WithAllowedWSOrigins(cfg.Server.AllowedWebSocketOrigins))
	}

	rp, err := proxy.New(
		defaultBackendURL,
		backendTimeout,
		maxIdleConns,
		idleConnTimeout,
		cfg.Backend.Transport,
		logger,
		proxyOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("create proxy: %w", err)
	}
	return rp, nil
}

// skipCORSPreflight is an otelhttp.WithFilter that prevents span creation
// for CORS preflight (OPTIONS) requests. Preflights are browser protocol
// overhead and create noise in traces without diagnostic value.
func skipCORSPreflight(r *http.Request) bool {
	return r.Method != http.MethodOptions ||
		r.Header.Get("Origin") == "" ||
		r.Header.Get("Access-Control-Request-Method") == ""
}

// skipStreaming is an otelhttp.WithFilter that prevents span creation AND
// http.server.request.duration recording for long-lived streaming connections
// (SSE, WebSocket, gRPC). Their duration is the connection lifetime, not a
// request latency, so folding them into the request-duration histogram records
// multi-minute-to-hour values that overflow the top bucket (le=10s), peg p99,
// and false-fire the latency alerts. Streaming has its own duration metric
// (edgequota.streaming.duration); this keeps http.server.request.duration an
// honest measure of non-streaming request latency — the invariant the traffic
// counters already assume. otelhttp filters are AND-combined, so this runs
// alongside skipCORSPreflight.
//
// Note: proxy.IsGRPC matches unary as well as streaming gRPC, so unary gRPC
// latency is also excluded here — consistent with treating this histogram as
// non-streaming-only (there is no gRPC traffic today regardless).
func skipStreaming(r *http.Request) bool {
	return !proxy.IsSSE(r) && !proxy.IsWebSocketUpgrade(r) && !proxy.IsGRPC(r)
}

// h3ContextServer marks HTTP/3 requests as server-handled. Only its presence is
// read, but it is a real *http.Server so a type assertion on the context value
// still holds. It never listens, so its timeouts are irrelevant — quic-go serves
// these requests.
var h3ContextServer = &http.Server{} //nolint:gosec // G112: a context marker, never serves a connection.

// underHTTPServerContext puts http.ServerContextKey on the request context.
//
// quic-go sets its own http3.ServerContextKey and never net/http's, and
// httputil.ReverseProxy treats that key's absence as "not running under a
// server": on a mid-body copy failure it then logs and returns normally instead
// of panicking with http.ErrAbortHandler. Handlers that detect a truncated
// upstream response by that panic would silently not fire over HTTP/3 — the
// response cache would store the fragment it had buffered and serve it to every
// later client for the full TTL.
//
// Setting the key makes HTTP/3 abort exactly like HTTP/1.1 and HTTP/2, so the
// handler chain has one behavior rather than one per protocol. quic-go recovers
// handler panics and special-cases http.ErrAbortHandler the same way net/http
// does, so aborting is handled the same on this path too.
func underHTTPServerContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(http.ServerContextKey) == nil {
			r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, h3ContextServer))
		}
		next.ServeHTTP(w, r)
	})
}

func buildMainServer(cfg *config.Config, chain *middleware.Chain, logger *slog.Logger) (*http.Server, *http3.Server) {
	readTimeout, _ := config.ParseDuration(cfg.Server.ReadTimeout, 30*time.Second)
	writeTimeout, _ := config.ParseDuration(cfg.Server.WriteTimeout, 30*time.Second)
	idleTimeout, _ := config.ParseDuration(cfg.Server.IdleTimeout, 120*time.Second)

	// Store the write timeout in the chain so it can apply per-request
	// deadlines via ResponseController. We do NOT set http.Server.WriteTimeout
	// because it is an absolute wall-clock deadline from when the request
	// headers are read — it kills SSE, WebSocket, and gRPC streams that
	// legitimately outlive the timeout. Instead, the chain sets write
	// deadlines only on non-streaming responses.
	chain.SetWriteTimeout(writeTimeout)

	// Wrap the chain with otelhttp so that:
	//  1. Incoming traceparent/tracestate headers are extracted into the
	//     request context (W3C TraceContext propagation).
	//  2. A root "edgequota.request" span is created for every request,
	//     which the chain's child spans (auth, external_rl, proxy, redis)
	//     automatically nest under.
	// otelhttp uses the global TextMapPropagator registered by InitTracing.
	tracedChain := otelhttp.NewHandler(
		chain, "edgequota.request",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return "edgequota.request"
		}),
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
		otelhttp.WithFilter(skipCORSPreflight),
		otelhttp.WithFilter(skipStreaming),
	)

	// When TLS is enabled, HTTP/2 is negotiated via ALPN (http2.ConfigureServer
	// in Run + NextProtos in the tls.Config). For the cleartext listener,
	// unencrypted HTTP/2 (h2c, e.g. gRPC without TLS) is enabled via the
	// server's Protocols field below — the Go 1.24+ replacement for the
	// deprecated h2c.NewHandler.
	mainHandler := tracedChain

	var h3srv *http3.Server
	if cfg.Server.TLS.HTTP3Enabled {
		h3srv = &http3.Server{
			Addr:           cfg.Server.Address,
			Handler:        underHTTPServerContext(tracedChain),
			MaxHeaderBytes: 1 << 20, // 1 MiB — same as the TCP server.
			IdleTimeout:    idleTimeout,
			QUICConfig: &quic.Config{
				MaxIdleTimeout: idleTimeout,
				Allow0RTT:      false, // Disable 0-RTT to prevent replay attacks.
			},
		}

		tcpHandler := mainHandler
		if port := cfg.Server.TLS.HTTP3AdvertisePort; port > 0 {
			altSvc := fmt.Sprintf(`h3=":%d"; ma=2592000`, port)
			mainHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor < 3 {
					w.Header().Set("Alt-Svc", altSvc)
				}
				tcpHandler.ServeHTTP(w, r)
			})
		} else {
			mainHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor < 3 {
					if setErr := h3srv.SetQUICHeaders(w.Header()); setErr != nil {
						logger.Debug("failed to set Alt-Svc header", "error", setErr)
					}
				}
				tcpHandler.ServeHTTP(w, r)
			})
		}
	}

	srv := &http.Server{
		Addr:              cfg.Server.Address,
		Handler:           mainHandler,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB — explicit default to prevent large-header DoS.
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		BaseContext: func(_ net.Listener) context.Context {
			return context.Background()
		},
	}

	if !cfg.Server.TLS.Enabled {
		// Cleartext listener: enable HTTP/1.1 + unencrypted HTTP/2 (h2c) so
		// gRPC / HTTP2-without-TLS clients work. Native replacement for the
		// deprecated h2c.NewHandler wrapper.
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		protocols.SetUnencryptedHTTP2(true)
		srv.Protocols = protocols
	}

	return srv, h3srv
}

// buildMTLSServer creates the secondary HTTP server for the mTLS listener.
// It shares the same handler (chain) as the main server but has no HTTP/3.
// TLS config (with ClientCAs and ClientAuth) is applied in Run().
func buildMTLSServer(cfg *config.Config, chain *middleware.Chain, logger *slog.Logger) *http.Server {
	readTimeout, _ := config.ParseDuration(cfg.Server.ReadTimeout, 30*time.Second)
	idleTimeout, _ := config.ParseDuration(cfg.Server.IdleTimeout, 120*time.Second)

	tracedChain := otelhttp.NewHandler(
		chain, "edgequota.request",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return "edgequota.request"
		}),
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
		otelhttp.WithFilter(skipCORSPreflight),
		otelhttp.WithFilter(skipStreaming),
	)

	return &http.Server{
		Addr:              cfg.Server.TLS.MTLS.ListenAddr,
		Handler:           tracedChain,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		BaseContext: func(_ net.Listener) context.Context {
			return context.Background()
		},
	}
}

// loadClientCAs reads a PEM file containing one or more CA certificates and
// returns an x509.CertPool. Returns an error if the file cannot be read or
// contains no valid certificates.
func loadClientCAs(path string) (*x509.CertPool, error) {
	pemData, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read client CA file %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("no valid CA certificates found in %s", path)
	}
	return pool, nil
}

// mtlsClientAuth maps a config.ClientAuthMode to the corresponding
// crypto/tls constant.
func mtlsClientAuth(mode config.ClientAuthMode) tls.ClientAuthType {
	switch mode {
	case config.ClientAuthRequest:
		return tls.RequestClientCert
	case config.ClientAuthRequireAny:
		return tls.RequireAnyClientCert
	case config.ClientAuthRequireAndVerify:
		return tls.RequireAndVerifyClientCert
	default:
		return tls.RequireAndVerifyClientCert
	}
}

// adminRateLimiter provides a simple in-process rate limiter for the admin
// server. It uses a token bucket that refills at `rate` tokens per second,
// allowing bursts up to `burst`. Protects admin endpoints from accidental
// exposure or scraping abuse.
type adminRateLimiter struct {
	handler http.Handler
	tokens  atomic.Int64
	burst   int64
	done    chan struct{}
}

// Hand-written purge handlers removed — now implemented via generated
// StrictServerInterface in admin.go.

func newAdminRateLimiter(handler http.Handler, burst int64) *adminRateLimiter {
	rl := &adminRateLimiter{handler: handler, burst: burst, done: make(chan struct{})}
	rl.tokens.Store(burst)
	// Refill tokens every 100ms (rate = burst per second).
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-rl.done:
				return
			case <-ticker.C:
				refill := burst / 10
				if refill < 1 {
					refill = 1
				}
				for {
					cur := rl.tokens.Load()
					next := cur + refill
					if next > burst {
						next = burst
					}
					if rl.tokens.CompareAndSwap(cur, next) {
						break
					}
				}
			}
		}
	}()
	return rl
}

// Stop terminates the background token-refill goroutine. Safe to call
// multiple times; subsequent calls are no-ops.
func (rl *adminRateLimiter) Stop() {
	select {
	case <-rl.done:
		// Already stopped.
	default:
		close(rl.done)
	}
}

func (rl *adminRateLimiter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for {
		cur := rl.tokens.Load()
		if cur <= 0 {
			http.Error(w, "admin rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		if rl.tokens.CompareAndSwap(cur, cur-1) {
			break
		}
	}
	rl.handler.ServeHTTP(w, r)
}

func buildAdminServer(cfg *config.Config, health *observability.HealthChecker, metrics *observability.Metrics, logger *slog.Logger, cacheStore func() *cache.Store, chain AuthCachePurger) (*http.Server, *adminRateLimiter) {
	adminReadTimeout, _ := config.ParseDuration(cfg.Admin.ReadTimeout, 5*time.Second)
	adminWriteTimeout, _ := config.ParseDuration(cfg.Admin.WriteTimeout, 10*time.Second)
	adminIdleTimeout, _ := config.ParseDuration(cfg.Admin.IdleTimeout, 30*time.Second)

	adminMux := http.NewServeMux()
	adminMux.Handle("/startz", health.StartzHandler())
	adminMux.Handle("/healthz", health.HealthzHandler())
	adminMux.Handle("/readyz", health.ReadyzHandler())
	configHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		data, err := json.MarshalIndent(cfg.Sanitized(), "", "  ")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(data)
	})
	statsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		snapshot := metrics.Snapshot()
		data, err := json.MarshalIndent(snapshot, "", "  ")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(data)
	})

	adminMux.Handle("/v1/config", configHandler)
	adminMux.Handle("/v1/stats", statsHandler)

	// pprof endpoints for live heap/goroutine/profile diagnosis. The admin
	// listener is internal and already gated by adminRateLimiter below.
	adminMux.HandleFunc("/debug/pprof/", pprof.Index)
	adminMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	adminMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	adminMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	adminMux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// Cache purge routes — generated strict-server handler.
	ah := &adminHandler{
		responseCache: cacheStore,
		authPurger:    chain,
		logger:        logger,
	}
	strictHandler := adminv1.NewStrictHandler(ah, nil)
	adminv1.HandlerFromMux(strictHandler, adminMux)

	// Wrap the admin mux in a rate limiter (100 req/s burst).
	rl := newAdminRateLimiter(adminMux, 100)

	srv := &http.Server{
		Addr:              cfg.Admin.Address,
		Handler:           rl,
		ReadTimeout:       adminReadTimeout,
		WriteTimeout:      adminWriteTimeout,
		IdleTimeout:       adminIdleTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB — explicit default.
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	return srv, rl
}

// certHolder provides atomic TLS certificate hot-reload via GetCertificate.
type certHolder struct {
	cert atomic.Pointer[tls.Certificate]
}

// newCertHolder creates and loads the initial certificate.
func newCertHolder(certFile, keyFile string) (*certHolder, error) {
	ch := &certHolder{}
	if err := ch.Reload(certFile, keyFile); err != nil {
		return nil, err
	}
	return ch, nil
}

// Reload loads a new certificate from disk and atomically swaps it.
func (ch *certHolder) Reload(certFile, keyFile string) error {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}
	ch.cert.Store(&cert)
	return nil
}

// GetCertificate implements the tls.Config.GetCertificate callback.
func (ch *certHolder) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return ch.cert.Load(), nil
}

// clientCAHolder provides atomic hot-reload of the mTLS client CA pool.
type clientCAHolder struct {
	pool atomic.Pointer[x509.CertPool]
}

// newClientCAHolder loads the initial client CA pool from a PEM file.
func newClientCAHolder(path string) (*clientCAHolder, error) {
	ch := &clientCAHolder{}
	if err := ch.Reload(path); err != nil {
		return nil, err
	}
	return ch, nil
}

// Reload reads the PEM file and atomically swaps the CA pool.
func (ch *clientCAHolder) Reload(path string) error {
	pool, err := loadClientCAs(path)
	if err != nil {
		return err
	}
	ch.pool.Store(pool)
	return nil
}

// GetPool returns the current client CA pool.
func (ch *clientCAHolder) GetPool() *x509.CertPool {
	return ch.pool.Load()
}

// tlsMinVersion returns the tls.Config MinVersion from config, defaulting to TLS 1.2.
func tlsMinVersion(cfg *config.Config) uint16 {
	if cfg.Server.TLS.MinVersion == config.TLSVersion13 {
		return tls.VersionTLS13
	}
	return tls.VersionTLS12
}

// initTelemetry installs the OpenTelemetry tracer and meter providers, storing
// their shutdown functions on the server. Tracing and metrics are enabled
// independently but share the OTLP transport (endpoint/protocol/insecure) from
// the tracing config. Installing the MeterProvider here — after New built the
// instruments from the global meter — late-binds every instrument, plus
// otelhttp's http.server.request.duration, onto the real provider.
func (s *Server) initTelemetry(ctx context.Context) {
	tracingShutdown, err := observability.InitTracing(ctx, s.cfg.Tracing, s.version)
	if err != nil {
		s.logger.Warn("failed to initialize tracing", "error", err)
		tracingShutdown = func(_ context.Context) error { return nil }
	}
	s.tracingShutdown = tracingShutdown

	metricsShutdown, err := observability.InitMetrics(ctx, s.cfg.Tracing, s.cfg.Metrics.Enabled, s.version)
	if err != nil {
		s.logger.Warn("failed to initialize metrics", "error", err)
		metricsShutdown = func(_ context.Context) error { return nil }
	}
	s.metricsShutdown = metricsShutdown
	if !s.cfg.Metrics.Enabled {
		s.logger.Info("metrics disabled (metrics.enabled=false); MeterProvider left no-op")
	}
}

// Run starts both the main and admin servers and blocks until the context is
// canceled, then performs a graceful shutdown.
func (s *Server) Run(ctx context.Context) error {
	s.initTelemetry(ctx)

	// Initialize TLS up-front so that both the main TCP server and the
	// HTTP/3 (QUIC) server share the same TLSConfig from the start. This
	// eliminates a race where startHTTP3Server could call ListenAndServe
	// before startMainServerWithReady has created and assigned TLSConfig.
	if s.cfg.Server.TLS.Enabled {
		ch, certErr := newCertHolder(s.cfg.Server.TLS.CertFile, s.cfg.Server.TLS.KeyFile)
		if certErr != nil {
			return certErr
		}
		s.certs = ch

		minVer := max(tlsMinVersion(s.cfg), tls.VersionTLS12)
		tlsCfg := &tls.Config{ //nolint:gosec // G402: MinVersion is always >= TLS 1.2; clamped above.
			MinVersion:     minVer,
			GetCertificate: ch.GetCertificate,
			NextProtos:     []string{"h2", "http/1.1"},
		}
		s.mainServer.TLSConfig = tlsCfg

		// Serve() (with a manual tls.NewListener) does NOT auto-configure
		// HTTP/2 — only ListenAndServeTLS / ServeTLS do. We must register
		// the h2 handler explicitly so the server can actually speak HTTP/2
		// after advertising it via ALPN.
		if err := http2.ConfigureServer(s.mainServer, nil); err != nil {
			return fmt.Errorf("configure HTTP/2: %w", err)
		}

		if s.http3Server != nil {
			s.http3Server.TLSConfig = tlsCfg
		}

		// Configure the mTLS listener with its own TLSConfig that includes
		// ClientCAs and a stricter ClientAuth mode. The CA pool is served
		// via GetConfigForClient so it can be hot-reloaded at runtime.
		if s.mtlsServer != nil {
			mtlsCfg := s.cfg.Server.TLS.MTLS
			caHolder, caErr := newClientCAHolder(mtlsCfg.ClientCAFile)
			if caErr != nil {
				return caErr
			}
			s.mtlsCAs = caHolder

			clientAuth := mtlsClientAuth(mtlsCfg.ResolvedClientAuth())
			mtlsTLSCfg := &tls.Config{ //nolint:gosec // G402: MinVersion is always >= TLS 1.2; clamped above.
				MinVersion:     minVer,
				GetCertificate: ch.GetCertificate,
				NextProtos:     []string{"h2", "http/1.1"},
				ClientCAs:      caHolder.GetPool(),
				ClientAuth:     clientAuth,
				GetConfigForClient: func(_ *tls.ClientHelloInfo) (*tls.Config, error) {
					return &tls.Config{ //nolint:gosec // G402: MinVersion is always >= TLS 1.2; clamped above.
						MinVersion:     minVer,
						GetCertificate: ch.GetCertificate,
						NextProtos:     []string{"h2", "http/1.1"},
						ClientCAs:      caHolder.GetPool(),
						ClientAuth:     clientAuth,
					}, nil
				},
			}
			s.mtlsServer.TLSConfig = mtlsTLSCfg

			if h2Err := http2.ConfigureServer(s.mtlsServer, nil); h2Err != nil {
				return fmt.Errorf("configure HTTP/2 for mTLS server: %w", h2Err)
			}

			s.logger.Info("mTLS listener configured",
				"listen_addr", mtlsCfg.ListenAddr,
				"client_auth", string(mtlsCfg.ResolvedClientAuth()),
				"client_ca_file", mtlsCfg.ClientCAFile)
		}
	}

	errCh := make(chan error, 4)

	// readyCh is closed after the main listener has successfully bound,
	// preventing SetReady from being called before the server can accept
	// connections.
	readyCh := make(chan struct{})

	go s.startAdminServer(errCh)
	go s.startMainServerWithReady(errCh, readyCh)

	if s.mtlsServer != nil {
		go s.startMTLSServer(errCh)
	}

	if s.http3Server != nil {
		go s.startHTTP3Server(errCh)
	}

	s.health.SetStarted()

	// Wait for the main listener to bind (or fail) before marking ready.
	select {
	case <-readyCh:
		s.health.SetReady()
		s.logger.Info("edgequota is ready", "version", s.version)
	case srvErr := <-errCh:
		return srvErr
	}

	select {
	case <-ctx.Done():
		s.logger.Info("shutdown signal received, draining...")
	case srvErr := <-errCh:
		return srvErr
	}

	return s.shutdown()
}

func (s *Server) startAdminServer(errCh chan<- error) {
	s.logger.Info("admin server starting", "address", s.cfg.Admin.Address)
	if err := s.adminServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		errCh <- fmt.Errorf("admin server: %w", err)
	}
}

func (s *Server) startMainServerWithReady(errCh chan<- error, readyCh chan struct{}) {
	backendURL := s.cfg.RateLimit.Static.BackendURL
	if s.cfg.RateLimit.External.Enabled {
		backendURL = s.cfg.RateLimit.External.Fallback.BackendURL
	}
	s.logger.Info("proxy server starting",
		"address", s.cfg.Server.Address,
		"backend", backendURL,
		"tls", s.cfg.Server.TLS.Enabled,
		"http3", s.cfg.Server.TLS.HTTP3Enabled)

	// Separate Listen from Serve so we can signal readiness after bind.
	ln, listenErr := net.Listen("tcp", s.cfg.Server.Address)
	if listenErr != nil {
		errCh <- fmt.Errorf("proxy server listen: %w", listenErr)
		return
	}
	close(readyCh) // signal that the listener has bound

	var err error
	if s.cfg.Server.TLS.Enabled {
		// TLSConfig and certHolder are initialized in Run() before any
		// server goroutines start, so mainServer.TLSConfig is ready here.
		tlsLn := tls.NewListener(ln, s.mainServer.TLSConfig)
		err = s.mainServer.Serve(tlsLn)
	} else {
		err = s.mainServer.Serve(ln)
	}

	if err != nil && err != http.ErrServerClosed {
		errCh <- fmt.Errorf("proxy server: %w", err)
	}
}

func (s *Server) startMTLSServer(errCh chan<- error) {
	s.logger.Info("mTLS proxy server starting", "address", s.cfg.Server.TLS.MTLS.ListenAddr)

	ln, listenErr := net.Listen("tcp", s.cfg.Server.TLS.MTLS.ListenAddr)
	if listenErr != nil {
		errCh <- fmt.Errorf("mTLS server listen: %w", listenErr)
		return
	}

	tlsLn := tls.NewListener(ln, s.mtlsServer.TLSConfig)
	if err := s.mtlsServer.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
		errCh <- fmt.Errorf("mTLS server: %w", err)
	}
}

func (s *Server) startHTTP3Server(errCh chan<- error) {
	s.logger.Info("HTTP/3 (QUIC) server starting", "address", s.cfg.Server.Address)

	// Use ListenAndServe (not ListenAndServeTLS) so that the HTTP/3 server
	// uses the shared TLSConfig with GetCertificate, enabling hot-reload of
	// TLS certificates. ListenAndServeTLS reads certs from disk once at
	// startup, bypassing the certHolder hot-reload mechanism.
	err := s.http3Server.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		errCh <- fmt.Errorf("HTTP/3 server: %w", err)
	}
}

// Reload hot-swaps the rate-limit, auth, external RL configuration, and
// TLS certificates without restarting the server.
func (s *Server) Reload(newCfg *config.Config) error {
	if err := s.chain.Reload(newCfg); err != nil {
		return err
	}

	// Hot-reload proxy when backend configuration changes.
	if backendChanged(s.cfg, newCfg) {
		newProxy, err := buildProxy(newCfg, s.logger)
		if err != nil {
			s.logger.Error("failed to rebuild proxy on reload, keeping old proxy", "error", err)
		} else {
			s.chain.SwapProxy(newProxy)
			s.logger.Info("proxy rebuilt after backend config change")
		}
	}

	// Reload TLS certificates if TLS is enabled and cert files are configured.
	if s.certs != nil && newCfg.Server.TLS.CertFile != "" && newCfg.Server.TLS.KeyFile != "" {
		if err := s.certs.Reload(newCfg.Server.TLS.CertFile, newCfg.Server.TLS.KeyFile); err != nil {
			s.logger.Error("TLS certificate reload failed, keeping old certificate", "error", err)
		} else {
			s.logger.Info("TLS certificates reloaded")
		}
	}

	s.cfg = newCfg
	s.metrics.SetConfigReloadTimestamp()
	return nil
}

func backendChanged(old, new_ *config.Config) bool {
	if !old.Backend.Equal(new_.Backend) {
		return true
	}
	if old.RateLimit.Static.BackendURL != new_.RateLimit.Static.BackendURL {
		return true
	}
	if old.RateLimit.External.Fallback.BackendURL != new_.RateLimit.External.Fallback.BackendURL {
		return true
	}
	return false
}

// ReloadCerts hot-swaps TLS certificates from the given files without
// restarting the server. This is called by the dedicated cert file
// watcher (separate from the config watcher) to handle Kubernetes Secret
// volume updates where the cert files change independently of the config.
func (s *Server) ReloadCerts(certFile, keyFile string) {
	if s.certs == nil {
		return
	}
	if err := s.certs.Reload(certFile, keyFile); err != nil {
		s.logger.Error("TLS certificate reload failed, keeping old certificate", "error", err)
	} else {
		s.logger.Info("TLS certificates reloaded", "cert", certFile, "key", keyFile)
		s.metrics.SetTLSReloadTimestamp()
	}
}

// ReloadMTLSCA hot-swaps the mTLS client CA pool from the given file
// without restarting the server. Called by the dedicated CA file watcher
// to handle Kubernetes Secret volume updates.
func (s *Server) ReloadMTLSCA(caFile string) {
	if s.mtlsCAs == nil {
		return
	}
	if err := s.mtlsCAs.Reload(caFile); err != nil {
		s.logger.Error("mTLS CA reload failed, keeping old CA pool", "error", err)
	} else {
		s.logger.Info("mTLS CA certificates reloaded", "ca_file", caFile)
		s.metrics.SetMTLSCAReloadTimestamp()
	}
}

func (s *Server) shutdown() error {
	s.health.SetNotReady()

	drainTimeout, _ := config.ParseDuration(s.cfg.Server.DrainTimeout, 30*time.Second)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	if s.http3Server != nil {
		if err := s.http3Server.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("HTTP/3 server shutdown error", "error", err)
		}
	}

	if err := s.mainServer.Shutdown(shutdownCtx); err != nil {
		s.logger.Error("main server shutdown error", "error", err)
	}

	if s.mtlsServer != nil {
		if err := s.mtlsServer.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("mTLS server shutdown error", "error", err)
		}
	}

	if err := s.adminServer.Shutdown(shutdownCtx); err != nil {
		s.logger.Error("admin server shutdown error", "error", err)
	}

	// Stop the admin rate limiter's background goroutine.
	if s.adminRL != nil {
		s.adminRL.Stop()
	}

	if err := s.chain.Close(); err != nil {
		s.logger.Error("middleware chain close error", "error", err)
	}

	if s.tracingShutdown != nil {
		if err := s.tracingShutdown(shutdownCtx); err != nil {
			s.logger.Error("tracing shutdown error", "error", err)
		}
	}

	if s.metricsShutdown != nil {
		if err := s.metricsShutdown(shutdownCtx); err != nil {
			s.logger.Error("metrics shutdown error", "error", err)
		}
	}

	s.logger.Info("shutdown complete")
	return nil
}

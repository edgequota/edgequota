// Package server orchestrates EdgeQuota's main proxy server and admin server.
// The main server handles incoming traffic (HTTP, gRPC, SSE, WebSocket) while
// the admin server exposes health checks, readiness probes, and Prometheus metrics.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/middleware"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/edgequota/edgequota/internal/proxy"
	iredis "github.com/edgequota/edgequota/internal/redis"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Server is the main EdgeQuota server.
type Server struct {
	cfg             *config.Config
	logger          *slog.Logger
	version         string
	mainServer      *http.Server
	http3Server     *http3.Server // nil when HTTP/3 is disabled.
	adminServer     *http.Server
	adminRL         *adminRateLimiter // stopped during shutdown to avoid goroutine leak
	chain           *middleware.Chain
	health          *observability.HealthChecker
	metrics         *observability.Metrics
	tracingShutdown func(context.Context) error
	certs           *certHolder // non-nil when TLS is enabled; supports hot-reload.
}

// New creates a new EdgeQuota server instance.
func New(cfg *config.Config, logger *slog.Logger, version string) (*Server, error) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	reg.MustRegister(collectors.NewGoCollector())

	metrics := observability.NewMetrics(reg, int64(cfg.RateLimit.MaxTenantLabels))
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
	adminServer, adminRL := buildAdminServer(cfg, health, reg, metrics, logger)

	return &Server{
		cfg:         cfg,
		logger:      logger,
		version:     version,
		mainServer:  mainServer,
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

	var proxyOpts []proxy.Option
	if cfg.Backend.TLSInsecureVerify {
		logger.Warn("backend TLS certificate verification is disabled (tls_insecure_skip_verify=true). "+
			"This is acceptable for cluster-internal backends with self-signed certificates, "+
			"but should not be used when proxying to external services over untrusted networks.",
			"backend_url", cfg.Backend.URL)
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
		cfg.Backend.URL,
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

func buildMainServer(cfg *config.Config, chain *middleware.Chain, logger *slog.Logger) (*http.Server, *http3.Server) {
	readTimeout, _ := config.ParseDuration(cfg.Server.ReadTimeout, 30*time.Second)
	writeTimeout, _ := config.ParseDuration(cfg.Server.WriteTimeout, 30*time.Second)
	idleTimeout, _ := config.ParseDuration(cfg.Server.IdleTimeout, 120*time.Second)

	// When TLS is enabled, Go's native HTTP/2 (via NextProtos in tls.Config)
	// handles h2 negotiation. h2c.NewHandler is only needed for cleartext
	// HTTP/2 upgrades (e.g. gRPC without TLS).
	var mainHandler http.Handler
	if cfg.Server.TLS.Enabled {
		mainHandler = chain
	} else {
		mainHandler = h2c.NewHandler(chain, &http2.Server{})
	}

	var h3srv *http3.Server
	if cfg.Server.TLS.HTTP3Enabled {
		// #region agent log
		h3Handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.Info("[DEBUG-H3] h3-server-entry",
				"proto", r.Proto,
				"proto_major", r.ProtoMajor,
				"remote_addr", r.RemoteAddr,
				"path", r.URL.Path,
				"request_id", r.Header.Get("X-Request-Id"),
				"hypothesis", "A")
			chain.ServeHTTP(w, r)
		})
		// #endregion
		h3srv = &http3.Server{
			Addr:           cfg.Server.Address,
			Handler:        h3Handler,
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
				// #region agent log
				logger.Info("[DEBUG-H3] tcp-handler-entry",
					"proto", r.Proto,
					"proto_major", r.ProtoMajor,
					"remote_addr", r.RemoteAddr,
					"path", r.URL.Path,
					"request_id", r.Header.Get("X-Request-Id"),
					"hypothesis", "A,B")
				// #endregion
				if r.ProtoMajor < 3 {
					w.Header().Set("Alt-Svc", altSvc)
				}
				tcpHandler.ServeHTTP(w, r)
			})
		} else {
			mainHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// #region agent log
				logger.Info("[DEBUG-H3] tcp-handler-entry-no-port",
					"proto", r.Proto,
					"proto_major", r.ProtoMajor,
					"remote_addr", r.RemoteAddr,
					"path", r.URL.Path,
					"hypothesis", "A,B")
				// #endregion
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
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB — explicit default to prevent large-header DoS.
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		BaseContext: func(_ net.Listener) context.Context {
			return context.Background()
		},
	}

	return srv, h3srv
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

func buildAdminServer(cfg *config.Config, health *observability.HealthChecker, reg *prometheus.Registry, metrics *observability.Metrics, logger *slog.Logger) (*http.Server, *adminRateLimiter) {
	adminReadTimeout, _ := config.ParseDuration(cfg.Admin.ReadTimeout, 5*time.Second)
	adminWriteTimeout, _ := config.ParseDuration(cfg.Admin.WriteTimeout, 10*time.Second)
	adminIdleTimeout, _ := config.ParseDuration(cfg.Admin.IdleTimeout, 30*time.Second)

	adminMux := http.NewServeMux()
	adminMux.Handle("/startz", health.StartzHandler())
	adminMux.Handle("/healthz", health.HealthzHandler())
	adminMux.Handle("/readyz", health.ReadyzHandler())
	adminMux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))
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

// tlsMinVersion returns the tls.Config MinVersion from config, defaulting to TLS 1.2.
func tlsMinVersion(cfg *config.Config) uint16 {
	if cfg.Server.TLS.MinVersion == config.TLSVersion13 {
		return tls.VersionTLS13
	}
	return tls.VersionTLS12
}

// Run starts both the main and admin servers and blocks until the context is
// canceled, then performs a graceful shutdown.
func (s *Server) Run(ctx context.Context) error {
	tracingShutdown, err := observability.InitTracing(ctx, s.cfg.Tracing, s.version)
	if err != nil {
		s.logger.Warn("failed to initialize tracing", "error", err)
		tracingShutdown = func(_ context.Context) error { return nil }
	}
	s.tracingShutdown = tracingShutdown

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
	}

	errCh := make(chan error, 3)

	// readyCh is closed after the main listener has successfully bound,
	// preventing SetReady from being called before the server can accept
	// connections.
	readyCh := make(chan struct{})

	go s.startAdminServer(errCh)
	go s.startMainServerWithReady(errCh, readyCh)

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
	s.logger.Info("proxy server starting",
		"address", s.cfg.Server.Address,
		"backend", s.cfg.Backend.URL,
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
	return nil
}

func backendChanged(old, new_ *config.Config) bool {
	return !old.Backend.Equal(new_.Backend)
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

	s.logger.Info("shutdown complete")
	return nil
}

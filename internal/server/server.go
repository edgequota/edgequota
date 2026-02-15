// Package server orchestrates EdgeQuota's main proxy server and admin server.
// The main server handles incoming traffic (HTTP, gRPC, SSE, WebSocket) while
// the admin server exposes health checks, readiness probes, and Prometheus metrics.
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/middleware"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/edgequota/edgequota/internal/proxy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
	chain           *middleware.Chain
	health          *observability.HealthChecker
	metrics         *observability.Metrics
	tracingShutdown func(context.Context) error
}

// New creates a new EdgeQuota server instance.
func New(cfg *config.Config, logger *slog.Logger, version string) (*Server, error) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	reg.MustRegister(collectors.NewGoCollector())

	metrics := observability.NewMetrics(reg)
	health := observability.NewHealthChecker()

	rp, err := buildProxy(cfg, logger)
	if err != nil {
		return nil, err
	}

	chain, err := middleware.NewChain(context.Background(), rp, cfg, logger, metrics)
	if err != nil {
		return nil, fmt.Errorf("create middleware chain: %w", err)
	}

	mainServer, h3srv := buildMainServer(cfg, chain, logger)
	adminServer := buildAdminServer(cfg, health, reg)

	return &Server{
		cfg:         cfg,
		logger:      logger,
		version:     version,
		mainServer:  mainServer,
		http3Server: h3srv,
		adminServer: adminServer,
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
		proxyOpts = append(proxyOpts, proxy.WithBackendTLSInsecure())
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

	h2s := &http2.Server{}
	mainHandler := h2c.NewHandler(chain, h2s)

	var h3srv *http3.Server
	if cfg.Server.TLS.HTTP3Enabled {
		h3srv = &http3.Server{
			Addr:    cfg.Server.Address,
			Handler: chain,
		}

		tcpHandler := mainHandler
		mainHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor < 3 {
				if setErr := h3srv.SetQUICHeaders(w.Header()); setErr != nil {
					logger.Debug("failed to set Alt-Svc header", "error", setErr)
				}
			}
			tcpHandler.ServeHTTP(w, r)
		})
	}

	srv := &http.Server{
		Addr:         cfg.Server.Address,
		Handler:      mainHandler,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
		BaseContext: func(_ net.Listener) context.Context {
			return context.Background()
		},
	}

	return srv, h3srv
}

func buildAdminServer(cfg *config.Config, health *observability.HealthChecker, reg *prometheus.Registry) *http.Server {
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

	return &http.Server{
		Addr:              cfg.Admin.Address,
		Handler:           adminMux,
		ReadTimeout:       adminReadTimeout,
		WriteTimeout:      adminWriteTimeout,
		IdleTimeout:       adminIdleTimeout,
		ReadHeaderTimeout: 5 * time.Second,
	}
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

	errCh := make(chan error, 3)

	go s.startAdminServer(errCh)
	go s.startMainServer(errCh)

	if s.http3Server != nil {
		go s.startHTTP3Server(errCh)
	}

	s.health.SetStarted()
	s.health.SetReady()
	s.logger.Info("edgequota is ready", "version", s.version)

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

func (s *Server) startMainServer(errCh chan<- error) {
	s.logger.Info("proxy server starting",
		"address", s.cfg.Server.Address,
		"backend", s.cfg.Backend.URL,
		"tls", s.cfg.Server.TLS.Enabled,
		"http3", s.cfg.Server.TLS.HTTP3Enabled)

	var err error
	if s.cfg.Server.TLS.Enabled {
		tlsCfg := &tls.Config{ //nolint:gosec // G402 — MinVersion is config-driven; minimum is TLS 1.2.
			MinVersion: tlsMinVersion(s.cfg),
		}
		s.mainServer.TLSConfig = tlsCfg
		err = s.mainServer.ListenAndServeTLS(s.cfg.Server.TLS.CertFile, s.cfg.Server.TLS.KeyFile)
	} else {
		err = s.mainServer.ListenAndServe()
	}

	if err != nil && err != http.ErrServerClosed {
		errCh <- fmt.Errorf("proxy server: %w", err)
	}
}

func (s *Server) startHTTP3Server(errCh chan<- error) {
	s.logger.Info("HTTP/3 (QUIC) server starting", "address", s.cfg.Server.Address)
	err := s.http3Server.ListenAndServeTLS(s.cfg.Server.TLS.CertFile, s.cfg.Server.TLS.KeyFile)
	if err != nil && err != http.ErrServerClosed {
		errCh <- fmt.Errorf("HTTP/3 server: %w", err)
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

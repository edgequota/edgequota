// Package main is the entry point for EdgeQuota, a high-performance standalone
// rate-limiting reverse proxy designed for Kubernetes edge deployments.
//
// EdgeQuota sits at the edge of a Kubernetes cluster (before the ingress
// controller / API gateway) and provides:
//   - Distributed rate limiting via Redis (token bucket algorithm)
//   - Optional external authentication (HTTP or gRPC)
//   - Optional dynamic rate limits from an external service (HTTP or gRPC)
//   - Multi-protocol proxying: HTTP, gRPC, SSE, WebSocket
//   - Full observability: Prometheus metrics, health checks, structured logging, OpenTelemetry tracing
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/edgequota/edgequota/internal/server"
)

// version is set at build time via ldflags: -ldflags "-X main.version=v1.0.0".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("edgequota %s\n", version)
		return
	}

	// Load configuration from YAML file + environment variable overrides.
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: configuration error: %v\n", err)
		os.Exit(1)
	}

	// Initialize structured logger.
	logger := observability.NewLogger(cfg.Logging.Level, cfg.Logging.Format)
	logger.Info("starting edgequota", "version", version)

	// Create root context with signal handling for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Create and start the server.
	srv, err := server.New(cfg, logger, version)
	if err != nil {
		logger.Error("failed to create server", "error", err)
		os.Exit(1)
	}

	// Start the config file watcher for hot-reload.
	watcher := config.NewWatcher(config.ConfigFilePath(), func(newCfg *config.Config) {
		if reloadErr := srv.Reload(newCfg); reloadErr != nil {
			logger.Error("config reload failed", "error", reloadErr)
		}
	}, logger)
	watcher.SetLastConfig(cfg)
	go func() {
		if watchErr := watcher.Start(ctx); watchErr != nil {
			logger.Error("config watcher error", "error", watchErr)
		}
	}()
	defer watcher.Stop()

	// Start a separate TLS cert watcher when TLS is enabled. The cert
	// files live in a different volume mount (Kubernetes Secret) than the
	// config file (ConfigMap), so the config watcher cannot detect cert
	// changes. This watcher polls the cert file hash and triggers a
	// reload when the certificate content changes on disk.
	if cfg.Server.TLS.Enabled && cfg.Server.TLS.CertFile != "" {
		certWatcher := config.NewCertWatcher(
			cfg.Server.TLS.CertFile,
			cfg.Server.TLS.KeyFile,
			func(certFile, keyFile string) {
				srv.ReloadCerts(certFile, keyFile)
			},
			logger,
		)
		go func() {
			if watchErr := certWatcher.Start(ctx); watchErr != nil {
				logger.Error("TLS cert watcher error", "error", watchErr)
			}
		}()
		defer certWatcher.Stop()
	}

	if err := srv.Run(ctx); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}

	logger.Info("edgequota shut down gracefully")
}

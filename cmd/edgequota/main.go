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
	go func() {
		if watchErr := watcher.Start(ctx); watchErr != nil {
			logger.Error("config watcher error", "error", watchErr)
		}
	}()
	defer watcher.Stop()

	if err := srv.Run(ctx); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}

	logger.Info("edgequota shut down gracefully")
}

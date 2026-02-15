package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerRunAndShutdown(t *testing.T) {
	t.Run("starts and stops gracefully", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://127.0.0.1:1" // won't actually connect
		cfg.Server.Address = ":0"              // random port
		cfg.Admin.Address = ":0"               // random port
		cfg.RateLimit.Average = 0
		cfg.Redis.Endpoints = []string{mr.Addr()}

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan error, 1)
		go func() {
			done <- srv.Run(ctx)
		}()

		// Give server time to start.
		time.Sleep(200 * time.Millisecond)

		// Cancel to trigger shutdown.
		cancel()

		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("server did not shut down within timeout")
		}
	})
}

func TestServerHealthEndpoints(t *testing.T) {
	t.Run("healthz and readyz are accessible", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://127.0.0.1:1"
		cfg.Server.Address = "127.0.0.1:18080"
		cfg.Admin.Address = "127.0.0.1:19090"
		cfg.RateLimit.Average = 0
		cfg.Redis.Endpoints = []string{mr.Addr()}

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- srv.Run(ctx)
		}()

		// Wait for server to be ready.
		time.Sleep(500 * time.Millisecond)

		client := &http.Client{Timeout: 2 * time.Second}

		// Test startz.
		respS, err := client.Get("http://127.0.0.1:19090/startz")
		require.NoError(t, err)
		defer respS.Body.Close()
		assert.Equal(t, http.StatusOK, respS.StatusCode)

		var startBody map[string]string
		require.NoError(t, json.NewDecoder(respS.Body).Decode(&startBody))
		assert.Equal(t, "started", startBody["status"])

		// Test healthz.
		resp, err := client.Get("http://127.0.0.1:19090/healthz")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var body map[string]string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		assert.Equal(t, "alive", body["status"])

		// Test readyz.
		resp2, err := client.Get("http://127.0.0.1:19090/readyz")
		require.NoError(t, err)
		defer resp2.Body.Close()
		assert.Equal(t, http.StatusOK, resp2.StatusCode)

		// Test metrics endpoint.
		resp3, err := client.Get("http://127.0.0.1:19090/metrics")
		require.NoError(t, err)
		defer resp3.Body.Close()
		assert.Equal(t, http.StatusOK, resp3.StatusCode)
		metricsBody, _ := io.ReadAll(resp3.Body)
		assert.Contains(t, string(metricsBody), "edgequota_requests_allowed_total")

		cancel()
		<-done
	})
}

func TestServerProxiesTraffic(t *testing.T) {
	t.Run("proxies HTTP request to backend", func(t *testing.T) {
		// Create a backend that responds.
		backendMux := http.NewServeMux()
		backendMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend", "true")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "hello from backend")
		})
		backendServer := &http.Server{Addr: "127.0.0.1:18081", Handler: backendMux}
		go backendServer.ListenAndServe()
		defer backendServer.Close()

		time.Sleep(100 * time.Millisecond)

		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://127.0.0.1:18081"
		cfg.Server.Address = "127.0.0.1:18082"
		cfg.Admin.Address = "127.0.0.1:19091"
		cfg.RateLimit.Average = 0
		cfg.Redis.Endpoints = []string{mr.Addr()}

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- srv.Run(ctx)
		}()

		time.Sleep(500 * time.Millisecond)

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("http://127.0.0.1:18082/")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "true", resp.Header.Get("X-Backend"))
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, "hello from backend", string(body))

		cancel()
		<-done
	})
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
		proxyAddr := freeAddr(t)
		adminAddr := freeAddr(t)

		cfg := config.Defaults()
		cfg.Backend.URL = "http://127.0.0.1:1"
		cfg.Server.Address = proxyAddr
		cfg.Admin.Address = adminAddr
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

		// Poll until the admin server is ready instead of a fixed sleep.
		require.Eventually(t, func() bool {
			resp, httpErr := http.Get("http://" + adminAddr + "/healthz")
			if httpErr != nil {
				return false
			}
			resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}, 5*time.Second, 50*time.Millisecond, "admin server did not become ready")

		client := &http.Client{Timeout: 2 * time.Second}

		// Test startz.
		respS, err := client.Get("http://" + adminAddr + "/startz")
		require.NoError(t, err)
		defer respS.Body.Close()
		assert.Equal(t, http.StatusOK, respS.StatusCode)

		var startBody map[string]string
		require.NoError(t, json.NewDecoder(respS.Body).Decode(&startBody))
		assert.Equal(t, "started", startBody["status"])

		// Test healthz.
		resp, err := client.Get("http://" + adminAddr + "/healthz")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var body map[string]string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		assert.Equal(t, "alive", body["status"])

		// Test readyz.
		resp2, err := client.Get("http://" + adminAddr + "/readyz")
		require.NoError(t, err)
		defer resp2.Body.Close()
		assert.Equal(t, http.StatusOK, resp2.StatusCode)

		// Test metrics endpoint.
		resp3, err := client.Get("http://" + adminAddr + "/metrics")
		require.NoError(t, err)
		defer resp3.Body.Close()
		assert.Equal(t, http.StatusOK, resp3.StatusCode)
		metricsBody, _ := io.ReadAll(resp3.Body)
		assert.Contains(t, string(metricsBody), "edgequota_requests_allowed_total")

		cancel()
		<-done
	})
}

// freeAddr returns a "host:port" string with a port the OS has confirmed is
// available. The listener is closed immediately so the port can be reused.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func TestServerProxiesTraffic(t *testing.T) {
	t.Run("proxies HTTP request to backend", func(t *testing.T) {
		// Use httptest.NewServer so the OS picks a free port and the
		// server is guaranteed to be listening before we proceed.
		backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend", "true")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "hello from backend")
		}))
		defer backendServer.Close()

		proxyAddr := freeAddr(t)
		adminAddr := freeAddr(t)

		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = backendServer.URL
		cfg.Server.Address = proxyAddr
		cfg.Admin.Address = adminAddr
		cfg.RateLimit.Average = 0
		cfg.Redis.Endpoints = []string{mr.Addr()}
		denyPrivate := false
		cfg.Backend.URLPolicy.DenyPrivateNetworks = &denyPrivate

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- srv.Run(ctx)
		}()

		// Poll the admin health endpoint until the server is ready.
		require.Eventually(t, func() bool {
			resp, httpErr := http.Get("http://" + adminAddr + "/healthz")
			if httpErr != nil {
				return false
			}
			resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}, 5*time.Second, 50*time.Millisecond, "server did not become ready")

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("http://" + proxyAddr + "/")
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

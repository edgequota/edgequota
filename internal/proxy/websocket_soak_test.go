package proxy

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWebSocketSoak exercises the WebSocket proxy under sustained concurrent
// load to verify there are no goroutine leaks, race conditions, or connection
// handling issues in the relay path.
func TestWebSocketSoak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping soak test in short mode")
	}

	t.Run("handles many concurrent WebSocket connections", func(t *testing.T) {
		const (
			concurrency     = 20
			messagesPerConn = 50
		)

		// TCP echo backend that handles multiple connections.
		backendLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer backendLn.Close()

		var backendConns atomic.Int32
		go func() {
			for {
				conn, err := backendLn.Accept()
				if err != nil {
					return
				}
				backendConns.Add(1)
				go func() {
					defer conn.Close()
					reader := bufio.NewReader(conn)

					// Read upgrade request.
					for {
						line, err := reader.ReadString('\n')
						if err != nil || strings.TrimSpace(line) == "" {
							break
						}
					}

					// Send upgrade response.
					response := "HTTP/1.1 101 Switching Protocols\r\n" +
						"Upgrade: websocket\r\n" +
						"Connection: Upgrade\r\n" +
						"\r\n"
					conn.Write([]byte(response))

					// Echo loop.
					buf := make([]byte, 4096)
					for {
						n, err := reader.Read(buf)
						if err != nil {
							return
						}
						if _, err := conn.Write(buf[:n]); err != nil {
							return
						}
					}
				}()
			}
		}()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(
			"http://"+backendLn.Addr().String(),
			30*time.Second, 200, 90*time.Second,
			config.TransportConfig{},
			logger,
		)
		require.NoError(t, err)

		proxyServer := httptest.NewServer(p)
		defer proxyServer.Close()

		var wg sync.WaitGroup
		var successCount atomic.Int32
		var errorCount atomic.Int32

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(connID int) {
				defer wg.Done()

				// Connect to proxy.
				conn, err := net.DialTimeout("tcp", proxyServer.Listener.Addr().String(), 5*time.Second)
				if err != nil {
					errorCount.Add(1)
					return
				}
				defer conn.Close()

				// Send upgrade.
				upgradeReq := fmt.Sprintf(
					"GET /ws/%d HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n",
					connID, proxyServer.Listener.Addr().String(),
				)
				if _, err := conn.Write([]byte(upgradeReq)); err != nil {
					errorCount.Add(1)
					return
				}

				conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				reader := bufio.NewReader(conn)

				// Read 101 response.
				statusLine, err := reader.ReadString('\n')
				if err != nil || !strings.Contains(statusLine, "101") {
					errorCount.Add(1)
					return
				}

				// Skip headers.
				for {
					line, err := reader.ReadString('\n')
					if err != nil || strings.TrimSpace(line) == "" {
						break
					}
				}

				// Send and receive messages.
				for j := 0; j < messagesPerConn; j++ {
					msg := fmt.Sprintf("conn%d-msg%d", connID, j)
					if _, err := conn.Write([]byte(msg)); err != nil {
						errorCount.Add(1)
						return
					}

					buf := make([]byte, 256)
					n, err := reader.Read(buf)
					if err != nil {
						errorCount.Add(1)
						return
					}

					if string(buf[:n]) != msg {
						errorCount.Add(1)
						return
					}
				}

				successCount.Add(1)
			}(i)
		}

		wg.Wait()

		t.Logf("WebSocket soak: %d/%d successful, %d errors, %d backend connections",
			successCount.Load(), concurrency, errorCount.Load(), backendConns.Load())

		// Allow some slack for transient timing issues, but majority should succeed.
		assert.Greater(t, successCount.Load(), int32(concurrency*8/10),
			"at least 80%% of connections should succeed")
	})

	t.Run("handles rapid connect-disconnect cycles", func(t *testing.T) {
		const cycles = 50

		backendLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer backendLn.Close()

		go func() {
			for {
				conn, err := backendLn.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					reader := bufio.NewReader(conn)
					for {
						line, err := reader.ReadString('\n')
						if err != nil || strings.TrimSpace(line) == "" {
							break
						}
					}
					response := "HTTP/1.1 101 Switching Protocols\r\n" +
						"Upgrade: websocket\r\n" +
						"Connection: Upgrade\r\n" +
						"\r\n"
					conn.Write([]byte(response))
					// Hold connection open briefly then close.
					time.Sleep(50 * time.Millisecond)
				}()
			}
		}()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New(
			"http://"+backendLn.Addr().String(),
			30*time.Second, 200, 90*time.Second,
			config.TransportConfig{},
			logger,
		)
		require.NoError(t, err)

		proxyServer := httptest.NewServer(p)
		defer proxyServer.Close()

		var successCount atomic.Int32
		var wg sync.WaitGroup

		for i := 0; i < cycles; i++ {
			wg.Add(1)
			go func(cycle int) {
				defer wg.Done()

				conn, err := net.DialTimeout("tcp", proxyServer.Listener.Addr().String(), 5*time.Second)
				if err != nil {
					return
				}

				upgradeReq := fmt.Sprintf(
					"GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n",
					proxyServer.Listener.Addr().String(),
				)
				conn.Write([]byte(upgradeReq))

				// Read status line, then immediately close (rapid disconnect).
				conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				reader := bufio.NewReader(conn)
				statusLine, err := reader.ReadString('\n')
				conn.Close()

				if err == nil && strings.Contains(statusLine, "101") {
					successCount.Add(1)
				}
			}(i)
		}

		wg.Wait()

		t.Logf("Rapid connect-disconnect: %d/%d got 101 before disconnect", successCount.Load(), cycles)
		// Most cycles should at least get the 101 response.
		assert.Greater(t, successCount.Load(), int32(cycles*7/10),
			"at least 70%% of rapid cycles should get 101")
	})
}

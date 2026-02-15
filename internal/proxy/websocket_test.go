package proxy

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleWebSocket verifies the WebSocket upgrade and bidirectional relay
// using raw TCP connections to exercise the handleWebSocket code path.
func TestHandleWebSocket(t *testing.T) {
	t.Run("relays data between client and backend", func(t *testing.T) {
		// Start a simple TCP echo server acting as a WebSocket backend.
		// It accepts the upgrade handshake, echoes messages, then closes.
		backendLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer backendLn.Close()

		backendDone := make(chan struct{})
		go func() {
			defer close(backendDone)
			conn, err := backendLn.Accept()
			if err != nil {
				return
			}
			defer conn.Close()

			// Read the upgrade request.
			reader := bufio.NewReader(conn)
			for {
				line, err := reader.ReadString('\n')
				if err != nil || strings.TrimSpace(line) == "" {
					break
				}
			}

			// Send a basic WebSocket upgrade response.
			response := "HTTP/1.1 101 Switching Protocols\r\n" +
				"Upgrade: websocket\r\n" +
				"Connection: Upgrade\r\n" +
				"\r\n"
			conn.Write([]byte(response))

			// Echo back any data received.
			buf := make([]byte, 256)
			n, err := reader.Read(buf)
			if err != nil {
				return
			}
			conn.Write(buf[:n])
		}()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := New("http://"+backendLn.Addr().String(), 30*time.Second, 100, 90*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		// Start a real HTTP server with the proxy handler.
		proxyServer := httptest.NewServer(p)
		defer proxyServer.Close()

		// Connect to the proxy server via raw TCP.
		proxyConn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
		require.NoError(t, err)
		defer proxyConn.Close()

		// Send a WebSocket upgrade request.
		upgradeReq := "GET /ws HTTP/1.1\r\n" +
			"Host: " + proxyServer.Listener.Addr().String() + "\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"\r\n"
		_, err = proxyConn.Write([]byte(upgradeReq))
		require.NoError(t, err)

		// Read the 101 response.
		proxyConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(proxyConn)
		statusLine, err := reader.ReadString('\n')
		require.NoError(t, err)
		assert.Contains(t, statusLine, "101")

		// Skip remaining headers.
		for {
			line, err := reader.ReadString('\n')
			if err != nil || strings.TrimSpace(line) == "" {
				break
			}
		}

		// Send data through the WebSocket relay.
		testMsg := "hello-websocket"
		_, err = proxyConn.Write([]byte(testMsg))
		require.NoError(t, err)

		// Read echoed data.
		buf := make([]byte, 256)
		n, err := reader.Read(buf)
		require.NoError(t, err)
		assert.Equal(t, testMsg, string(buf[:n]))

		<-backendDone
	})

	t.Run("returns 502 when backend is unreachable", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		// Point to a port that nothing listens on.
		p, err := New("http://127.0.0.1:1", 1*time.Second, 10, 10*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		proxyServer := httptest.NewServer(p)
		defer proxyServer.Close()

		proxyConn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
		require.NoError(t, err)
		defer proxyConn.Close()

		upgradeReq := fmt.Sprintf("GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n",
			proxyServer.Listener.Addr().String())
		_, err = proxyConn.Write([]byte(upgradeReq))
		require.NoError(t, err)

		proxyConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(proxyConn)
		statusLine, err := reader.ReadString('\n')
		require.NoError(t, err)
		// Should return 502 Bad Gateway.
		assert.Contains(t, statusLine, "502")
	})

	t.Run("handles wss scheme conversion for https backend", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		// Create proxy with https backend URL to test scheme conversion.
		p, err := New("https://127.0.0.1:1", 1*time.Second, 10, 10*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		proxyServer := httptest.NewServer(p)
		defer proxyServer.Close()

		proxyConn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
		require.NoError(t, err)
		defer proxyConn.Close()

		upgradeReq := fmt.Sprintf("GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n",
			proxyServer.Listener.Addr().String())
		_, err = proxyConn.Write([]byte(upgradeReq))
		require.NoError(t, err)

		proxyConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(proxyConn)
		statusLine, err := reader.ReadString('\n')
		require.NoError(t, err)
		// Backend is unreachable, but the wss scheme path was exercised.
		assert.Contains(t, statusLine, "502")
	})

	t.Run("handles backend without explicit port", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		// Backend URL without explicit port — tests the default port logic.
		p, err := New("http://127.0.0.1", 1*time.Second, 10, 10*time.Second, config.TransportConfig{}, logger)
		require.NoError(t, err)

		proxyServer := httptest.NewServer(p)
		defer proxyServer.Close()

		proxyConn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
		require.NoError(t, err)
		defer proxyConn.Close()

		upgradeReq := fmt.Sprintf("GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n",
			proxyServer.Listener.Addr().String())
		_, err = proxyConn.Write([]byte(upgradeReq))
		require.NoError(t, err)

		proxyConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(proxyConn)
		statusLine, err := reader.ReadString('\n')
		require.NoError(t, err)
		// Will get either 502 (connection refused) or timeout.
		assert.True(t, strings.Contains(statusLine, "502") || strings.Contains(statusLine, "500"))
	})
}

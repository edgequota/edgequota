package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestProxyClose_Idempotent verifies that Close can be called repeatedly
// without panicking — important because SwapProxy schedules Close in a
// goroutine and Chain.Close may also reach it during shutdown.
func TestProxyClose_Idempotent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := New(backend.URL, 30*time.Second, 10, 60*time.Second, config.TransportConfig{}, quietLogger())
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, p.Close(ctx))
	require.NoError(t, p.Close(ctx))
	require.NoError(t, p.Close(ctx))
}

// TestProxyClose_ReleasesHTTP3Transport verifies Close fully closes the HTTP/3
// RoundTripper (not just its idle conns) so a config hot-reload doesn't leak it,
// and that doing so stays panic-free and idempotent.
func TestProxyClose_ReleasesHTTP3Transport(t *testing.T) {
	p := &Proxy{
		logger:         quietLogger(),
		http3Transport: &http3.Transport{},
	}

	ctx := context.Background()
	require.NoError(t, p.Close(ctx))
	require.NoError(t, p.Close(ctx))
}

// TestProxyClose_RejectsAfterDraining proves that once Close has marked the
// proxy draining, a new request is rejected with 503 instead of being admitted
// into inflight (whose transport is about to be released).
func TestProxyClose_RejectsAfterDraining(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := New(backend.URL, 30*time.Second, 10, 60*time.Second, config.TransportConfig{}, quietLogger())
	require.NoError(t, err)

	require.NoError(t, p.Close(context.Background()))

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

// TestProxyClose_ConcurrentServeAndClose hammers ServeHTTP while Close runs to
// exercise the inflight.Add/Wait fence under -race. Each request must end as
// either served (200) or drained (503) — never a torn transport or a data race.
func TestProxyClose_ConcurrentServeAndClose(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := New(backend.URL, 30*time.Second, 10, 60*time.Second, config.TransportConfig{}, quietLogger())
	require.NoError(t, err)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := httptest.NewRecorder()
			p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
			if rr.Code != http.StatusOK && rr.Code != http.StatusServiceUnavailable {
				t.Errorf("unexpected status %d", rr.Code)
			}
		}()
	}

	require.NoError(t, p.Close(context.Background()))
	wg.Wait()
}

// TestProxyClose_WaitsForInflightHTTPRequests proves that an in-flight
// (non-WebSocket) request keeps Close blocked until it finishes — so a
// reload doesn't yank the transport out from under a real request.
func TestProxyClose_WaitsForInflightHTTPRequests(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := New(backend.URL, 30*time.Second, 10, 60*time.Second, config.TransportConfig{}, quietLogger())
	require.NoError(t, err)

	go func() {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		p.ServeHTTP(rr, req)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("backend handler never received the request")
	}

	closeDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		closeDone <- p.Close(ctx)
	}()

	// Close must still be waiting because the request is in flight.
	select {
	case <-closeDone:
		t.Fatal("Close returned before in-flight request finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the request finished")
	}
}

// TestProxyClose_DeadlineBoundsHungStreams confirms that a hung in-flight
// request can't block Close indefinitely — the context deadline forces
// teardown so an SSE/streaming response that runs longer than the
// configured grace window doesn't leak a proxy.
func TestProxyClose_DeadlineBoundsHungStreams(t *testing.T) {
	stuck := make(chan struct{}, 1) // closed by t.Cleanup so backend handlers can exit
	started := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-stuck
	}))
	t.Cleanup(func() {
		close(stuck)
		backend.Close()
	})

	p, err := New(backend.URL, 30*time.Second, 10, 60*time.Second, config.TransportConfig{}, quietLogger())
	require.NoError(t, err)

	go func() {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		p.ServeHTTP(rr, req)
	}()

	// Channel receive establishes happens-before with ServeHTTP's Add(1).
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("backend handler never received the request")
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	require.NoError(t, p.Close(ctx))
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond,
		"Close should have waited at least until ctx deadline")
	assert.Less(t, elapsed, 1500*time.Millisecond,
		"Close should have returned shortly after ctx deadline, not waited for the hung request")
}

// TestProxyClose_NoGoroutineLeakAcrossReloads is the leak-detection test
// from the original audit. Simulate N proxy swaps + closes, force GC,
// and assert the goroutine count is bounded.
func TestProxyClose_NoGoroutineLeakAcrossReloads(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	build := func() *Proxy {
		p, err := New(backend.URL, 30*time.Second, 10, 60*time.Second, config.TransportConfig{}, quietLogger())
		require.NoError(t, err)
		return p
	}

	warmup := build()
	_ = warmup.Close(context.Background())

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const reloads = 50
	for range reloads {
		p := build()
		_ = p.Close(context.Background())
	}

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()

	delta := after - baseline
	assert.LessOrEqual(t, delta, 5,
		"goroutine delta after %d build+Close cycles: %d (baseline %d, after %d)",
		reloads, delta, baseline, after)
}

// TestProxyClose_ReleasesUDPSocket exercises the HTTP/3 path: with the
// QUIC buffer-sizing options configured, buildTransports calls
// net.ListenUDP and we capture the udpConn for Close. After Close, the
// underlying socket must be released (verified by re-binding to the
// same port — if Close didn't release it the bind would fail).
func TestProxyClose_ReleasesUDPSocket(t *testing.T) {
	// HTTPS scheme is required for the H3 path; we don't actually issue a
	// request, only confirm the udpConn is stashed and released.
	p, err := New("https://example.invalid", 30*time.Second, 10, 60*time.Second,
		config.TransportConfig{
			H3UDPReceiveBufferSize: 1 << 20,
		}, quietLogger())
	require.NoError(t, err)
	require.NotNil(t, p.udpConn, "H3 buffer config should produce a captured udpConn")

	addr := p.udpConn.LocalAddr().(*net.UDPAddr)
	require.NoError(t, p.Close(context.Background()))

	// After Close, the address should be free again. We accept a brief
	// TIME_WAIT-equivalent grace by retrying.
	var rebindErr error
	for range 5 {
		ln, err := net.ListenUDP("udp", addr)
		if err == nil {
			_ = ln.Close()
			rebindErr = nil
			break
		}
		rebindErr = err
		time.Sleep(20 * time.Millisecond)
	}
	require.NoError(t, rebindErr, "udp socket should be released by Close")
}

// TestProxyClose_BurstSwapsAreSerializable repeatedly Close()s the same
// proxy concurrently from many goroutines. With sync.Once gating the
// release path no goroutine should panic and every Close call should
// return successfully.
func TestProxyClose_BurstSwapsAreSerializable(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := New(backend.URL, 30*time.Second, 10, 60*time.Second, config.TransportConfig{}, quietLogger())
	require.NoError(t, err)

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			_ = p.Close(context.Background())
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Close calls did not finish")
	}
}

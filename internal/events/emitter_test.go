package events

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
)

func testMetrics() *observability.Metrics {
	return observability.NewMetrics(prometheus.NewRegistry(), 0)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEmitter_DisabledReturnsNil(t *testing.T) {
	e := NewEmitter(config.EventsConfig{Enabled: false}, testLogger(), testMetrics())
	if e != nil {
		t.Fatal("expected nil emitter when disabled")
	}
}

func TestEmitter_BatchFlushing(t *testing.T) {
	var mu sync.Mutex
	var received []UsageEvent

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Events []UsageEvent `json:"events"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("unmarshal error: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, payload.Events...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"accepted":` + string(body[:1]) + `}`))
	}))
	defer srv.Close()

	e := NewEmitter(config.EventsConfig{
		Enabled:       true,
		HTTP:          config.EventsHTTPConfig{URL: srv.URL},
		BatchSize:     5,
		FlushInterval: "100ms",
		BufferSize:    100,
	}, testLogger(), testMetrics())

	for i := range 12 {
		e.Emit(UsageEvent{
			Key:       "test-key",
			Method:    "GET",
			Path:      "/test",
			Allowed:   i%2 == 0,
			Remaining: int64(10 - i),
			Limit:     10,
			Timestamp: time.Now().Format(time.RFC3339),
		})
	}

	// Wait for flush.
	time.Sleep(500 * time.Millisecond)

	if err := e.Close(); err != nil {
		t.Fatalf("close error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 12 {
		t.Errorf("expected 12 events, got %d", len(received))
	}
}

func TestEmitter_BufferOverflow(t *testing.T) {
	// Use a very small buffer to force overflow.
	e := NewEmitter(config.EventsConfig{
		Enabled:       true,
		HTTP:          config.EventsHTTPConfig{URL: "http://localhost:0/noop"},
		BatchSize:     1000, // larger than buffer to prevent flushing
		FlushInterval: "1h",
		BufferSize:    5,
	}, testLogger(), testMetrics())

	for range 10 {
		e.Emit(UsageEvent{Key: "overflow"})
	}

	e.ringMu.Lock()
	length := e.ringLen
	e.ringMu.Unlock()

	if length != 5 {
		t.Errorf("expected ring length 5 (capped), got %d", length)
	}

	// Don't bother flushing — close and move on.
	close(e.done)
	e.wg.Wait()
}

func TestEmitter_GracefulShutdownDrain(t *testing.T) {
	var mu sync.Mutex
	var received int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Events []UsageEvent `json:"events"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err == nil {
			mu.Lock()
			received += len(payload.Events)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := NewEmitter(config.EventsConfig{
		Enabled:       true,
		HTTP:          config.EventsHTTPConfig{URL: srv.URL},
		BatchSize:     100,
		FlushInterval: "1h", // long enough that only Close() will trigger drain
		BufferSize:    100,
	}, testLogger(), testMetrics())

	for range 7 {
		e.Emit(UsageEvent{Key: "drain-test"})
	}

	if err := e.Close(); err != nil {
		t.Fatalf("close error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if received != 7 {
		t.Errorf("expected 7 events drained on close, got %d", received)
	}
}

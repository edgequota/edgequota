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
	"github.com/prometheus/client_golang/prometheus/testutil"
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

func TestEmitter_CustomHeaders(t *testing.T) {
	t.Run("single Authorization header", func(t *testing.T) {
		var captured http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		e := NewEmitter(config.EventsConfig{
			Enabled: true,
			HTTP: config.EventsHTTPConfig{
				URL: srv.URL,
				Headers: map[string]config.RedactedString{
					"Authorization": "Bearer my-token",
				},
			},
			BatchSize: 1, FlushInterval: "50ms", BufferSize: 10,
		}, testLogger(), testMetrics())

		e.Emit(UsageEvent{Key: "auth-test"})
		time.Sleep(300 * time.Millisecond)
		e.Close()

		if captured.Get("Authorization") != "Bearer my-token" {
			t.Errorf("expected Authorization: Bearer my-token, got %q", captured.Get("Authorization"))
		}
		if captured.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type should always be application/json, got %q", captured.Get("Content-Type"))
		}
	})

	t.Run("multiple custom headers", func(t *testing.T) {
		var captured http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		e := NewEmitter(config.EventsConfig{
			Enabled: true,
			HTTP: config.EventsHTTPConfig{
				URL: srv.URL,
				Headers: map[string]config.RedactedString{
					"X-Api-Key":      "key-123",
					"X-Destination":  "analytics",
				},
			},
			BatchSize: 1, FlushInterval: "50ms", BufferSize: 10,
		}, testLogger(), testMetrics())

		e.Emit(UsageEvent{Key: "multi-header-test"})
		time.Sleep(300 * time.Millisecond)
		e.Close()

		if captured.Get("X-Api-Key") != "key-123" {
			t.Errorf("expected X-Api-Key: key-123, got %q", captured.Get("X-Api-Key"))
		}
		if captured.Get("X-Destination") != "analytics" {
			t.Errorf("expected X-Destination: analytics, got %q", captured.Get("X-Destination"))
		}
	})

	t.Run("no custom headers", func(t *testing.T) {
		var captured http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		e := NewEmitter(config.EventsConfig{
			Enabled:       true,
			HTTP:          config.EventsHTTPConfig{URL: srv.URL},
			BatchSize:     1,
			FlushInterval: "50ms",
			BufferSize:    10,
		}, testLogger(), testMetrics())

		e.Emit(UsageEvent{Key: "no-header-test"})
		time.Sleep(300 * time.Millisecond)
		e.Close()

		if captured.Get("Authorization") != "" {
			t.Errorf("expected no Authorization header, got %q", captured.Get("Authorization"))
		}
	})

	t.Run("deprecated auth_token is migrated to headers via Validate", func(t *testing.T) {
		var captured http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.Events.Enabled = true
		cfg.Events.HTTP.URL = srv.URL
		cfg.Events.HTTP.AuthToken = "legacy-token"
		cfg.Events.BatchSize = 1
		cfg.Events.FlushInterval = "50ms"
		cfg.Events.BufferSize = 10
		if err := config.Validate(cfg); err != nil {
			t.Fatalf("validate: %v", err)
		}

		e := NewEmitter(cfg.Events, testLogger(), testMetrics())
		e.Emit(UsageEvent{Key: "compat-test"})
		time.Sleep(300 * time.Millisecond)
		e.Close()

		if captured.Get("Authorization") != "Bearer legacy-token" {
			t.Errorf("expected Authorization: Bearer legacy-token, got %q", captured.Get("Authorization"))
		}
	})

	t.Run("deprecated auth_token with custom auth_header", func(t *testing.T) {
		var captured http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.Events.Enabled = true
		cfg.Events.HTTP.URL = srv.URL
		cfg.Events.HTTP.AuthToken = "api-key-val"
		cfg.Events.HTTP.AuthHeader = "X-Api-Key"
		cfg.Events.BatchSize = 1
		cfg.Events.FlushInterval = "50ms"
		cfg.Events.BufferSize = 10
		if err := config.Validate(cfg); err != nil {
			t.Fatalf("validate: %v", err)
		}

		e := NewEmitter(cfg.Events, testLogger(), testMetrics())
		e.Emit(UsageEvent{Key: "compat-custom-test"})
		time.Sleep(300 * time.Millisecond)
		e.Close()

		if captured.Get("X-Api-Key") != "Bearer api-key-val" {
			t.Errorf("expected X-Api-Key: Bearer api-key-val, got %q", captured.Get("X-Api-Key"))
		}
	})
}

func TestEmitter_RetriesOnServerError(t *testing.T) {
	var mu sync.Mutex
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		a := attempts
		mu.Unlock()
		if a < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := NewEmitter(config.EventsConfig{
		Enabled:       true,
		HTTP:          config.EventsHTTPConfig{URL: srv.URL},
		BatchSize:     1,
		FlushInterval: "50ms",
		BufferSize:    10,
		MaxRetries:    5,
		RetryBackoff:  "1ms",
	}, testLogger(), testMetrics())

	e.Emit(UsageEvent{Key: "retry-test"})
	time.Sleep(500 * time.Millisecond)
	e.Close()

	mu.Lock()
	defer mu.Unlock()
	if attempts < 3 {
		t.Errorf("expected at least 3 attempts, got %d", attempts)
	}
}

func TestEmitter_FailureMetricAfterExhaustedRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	m := testMetrics()
	e := NewEmitter(config.EventsConfig{
		Enabled:       true,
		HTTP:          config.EventsHTTPConfig{URL: srv.URL},
		BatchSize:     1,
		FlushInterval: "50ms",
		BufferSize:    10,
		MaxRetries:    2,
		RetryBackoff:  "1ms",
	}, testLogger(), m)

	e.Emit(UsageEvent{Key: "failure-metric-test"})
	time.Sleep(500 * time.Millisecond)
	e.Close()

	if got := testutil.ToFloat64(m.PromEventsSendFailures); got < 1 {
		t.Errorf("expected PromEventsSendFailures >= 1, got %v", got)
	}
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

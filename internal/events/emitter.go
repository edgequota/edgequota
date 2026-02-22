// Package events implements an async, buffered usage event emitter that sends
// rate-limit decisions to an external HTTP/gRPC service (webhook pattern).
// Events are batched and flushed at configurable intervals. The emitter is
// entirely optional and fire-and-forget — it never blocks the request hot path.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	eventsv1http "github.com/edgequota/edgequota/api/gen/http/events/v1"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/observability"
)

// UsageEvent represents a single rate-limit decision.
type UsageEvent struct {
	Key        string `json:"key"`
	TenantKey  string `json:"tenant_key,omitempty"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Allowed    bool   `json:"allowed"`
	Remaining  int64  `json:"remaining"`
	Limit      int64  `json:"limit"`
	Timestamp  string `json:"timestamp"` // RFC 3339
	StatusCode int    `json:"status_code"`
	RequestID  string `json:"request_id,omitempty"` // X-Request-Id for deduplication
	Reason     string `json:"reason,omitempty"`     // Non-empty for anomaly events (e.g. "tenant_key_rejected")
}

// Emitter is an async, buffered event emitter that batches usage events and
// flushes them to an external HTTP (or gRPC) service.
type Emitter struct {
	logger  *slog.Logger
	metrics *observability.Metrics

	httpURL     string
	httpHeaders http.Header
	httpClient  *http.Client

	batchSize     int
	flushInterval time.Duration
	bufferSize    int
	maxRetries    int
	retryBackoff  time.Duration

	ring     []UsageEvent
	ringMu   sync.Mutex
	ringHead int
	ringTail int
	ringLen  int

	lastDropWarn time.Time // rate-limits overflow warnings to once per flush interval

	flushCh chan struct{}
	done    chan struct{}
	wg      sync.WaitGroup
}

// NewEmitter creates a new usage event emitter. Returns nil if events are
// not enabled in the config.
func NewEmitter(cfg config.EventsConfig, logger *slog.Logger, metrics *observability.Metrics) *Emitter {
	if !cfg.Enabled {
		return nil
	}

	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 100
	}

	bufferSize := cfg.BufferSize
	if bufferSize <= 0 {
		bufferSize = 10000
	}

	flushInterval := 5 * time.Second
	if cfg.FlushInterval != "" {
		if d, err := time.ParseDuration(cfg.FlushInterval); err == nil && d > 0 {
			flushInterval = d
		}
	}

	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	retryBackoff := 100 * time.Millisecond
	if cfg.RetryBackoff != "" {
		if d, err := time.ParseDuration(cfg.RetryBackoff); err == nil && d > 0 {
			retryBackoff = d
		}
	}

	// Build the pre-validated header set from config. Validation already
	// happened in config.Validate, so we trust the names here.
	headers := make(http.Header, len(cfg.HTTP.Headers))
	for k, v := range cfg.HTTP.Headers {
		headers.Set(k, v.Value())
	}

	e := &Emitter{
		logger:        logger.With("component", "events"),
		metrics:       metrics,
		httpURL:       cfg.HTTP.URL,
		httpHeaders:   headers,
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		batchSize:     batchSize,
		flushInterval: flushInterval,
		bufferSize:    bufferSize,
		maxRetries:    maxRetries,
		retryBackoff:  retryBackoff,
		ring:          make([]UsageEvent, bufferSize),
		flushCh:       make(chan struct{}, 1),
		done:          make(chan struct{}),
	}

	e.wg.Add(1)
	go e.flushLoop()

	return e
}

// Emit enqueues a usage event into the ring buffer. This is fire-and-forget
// and never blocks. When the buffer is full, the oldest event is dropped.
func (e *Emitter) Emit(ev UsageEvent) {
	e.ringMu.Lock()
	e.ring[e.ringTail] = ev
	e.ringTail = (e.ringTail + 1) % e.bufferSize
	if e.ringLen == e.bufferSize {
		// Buffer full — drop oldest by advancing head.
		e.ringHead = (e.ringHead + 1) % e.bufferSize
		e.metrics.IncEventsDropped()
		now := time.Now()
		if now.Sub(e.lastDropWarn) >= e.flushInterval {
			e.lastDropWarn = now
			e.logger.Warn("event buffer overflow, dropping oldest events",
				"buffer_size", e.bufferSize)
		}
	} else {
		e.ringLen++
	}
	shouldFlush := e.ringLen >= e.batchSize
	e.ringMu.Unlock()

	if shouldFlush {
		select {
		case e.flushCh <- struct{}{}:
		default:
		}
	}
}

// Close flushes remaining events and stops the flush loop.
func (e *Emitter) Close() error {
	close(e.done)
	e.wg.Wait()

	// Final drain.
	e.flush()
	return nil
}

func (e *Emitter) flushLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(e.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-e.done:
			return
		case <-ticker.C:
			e.flush()
		case <-e.flushCh:
			e.flush()
		}
	}
}

func (e *Emitter) flush() {
	for {
		batch := e.drain()
		if len(batch) == 0 {
			return
		}
		e.send(batch)
	}
}

func (e *Emitter) drain() []UsageEvent {
	e.ringMu.Lock()
	defer e.ringMu.Unlock()

	if e.ringLen == 0 {
		return nil
	}

	n := e.ringLen
	if n > e.batchSize {
		n = e.batchSize
	}

	batch := make([]UsageEvent, n)
	for i := range n {
		batch[i] = e.ring[(e.ringHead+i)%e.bufferSize]
	}
	e.ringHead = (e.ringHead + n) % e.bufferSize
	e.ringLen -= n
	return batch
}

func (e *Emitter) send(batch []UsageEvent) {
	if e.httpURL != "" {
		e.sendHTTP(batch)
		return
	}
	e.logger.Warn("no events destination configured, dropping batch", "count", len(batch))
}

func (e *Emitter) sendHTTP(batch []UsageEvent) {
	wireEvents := make([]eventsv1http.UsageEvent, len(batch))
	for i, ev := range batch {
		wireEvents[i] = usageEventToHTTP(ev)
	}
	payload := eventsv1http.PublishEventsRequest{Events: wireEvents}

	body, err := json.Marshal(payload)
	if err != nil {
		e.logger.Error("failed to marshal events batch", "error", err)
		return
	}

	backoff := e.retryBackoff
	for attempt := 1; attempt <= e.maxRetries; attempt++ {
		if e.trySendHTTP(body, len(batch)) {
			return
		}
		if attempt < e.maxRetries {
			time.Sleep(backoff)
			backoff *= 2
		}
	}

	e.logger.Error("events batch send failed after all retries",
		"attempts", e.maxRetries, "count", len(batch))
	e.metrics.PromEventsSendFailures.Inc()
}

func (e *Emitter) trySendHTTP(body []byte, count int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.httpURL, bytes.NewReader(body))
	if err != nil {
		e.logger.Error("failed to create events HTTP request", "error", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vals := range e.httpHeaders {
		for _, v := range vals {
			req.Header.Set(k, v)
		}
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		e.logger.Warn("failed to send events batch", "error", err, "count", count)
		return false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 500 {
		e.logger.Warn("events receiver returned server error",
			"status", resp.StatusCode, "count", count)
		return false
	}
	if resp.StatusCode >= 400 {
		e.logger.Warn("events receiver returned client error",
			"status", resp.StatusCode, "count", count)
	}
	return true
}

// String implements fmt.Stringer for debug logging.
func (e *Emitter) String() string {
	return fmt.Sprintf("Emitter(http=%s, batch=%d, flush=%s, buf=%d)",
		e.httpURL, e.batchSize, e.flushInterval, e.bufferSize)
}

// usageEventToHTTP converts the internal UsageEvent to the generated OpenAPI
// wire type, promoting value-typed optional fields to pointers where needed.
func usageEventToHTTP(ev UsageEvent) eventsv1http.UsageEvent {
	wire := eventsv1http.UsageEvent{
		Key:        ev.Key,
		Method:     ev.Method,
		Path:       ev.Path,
		Allowed:    ev.Allowed,
		Remaining:  ev.Remaining,
		Limit:      ev.Limit,
		Timestamp:  ev.Timestamp,
		StatusCode: int32(min(ev.StatusCode, 599)), //nolint:gosec // HTTP status codes are bounded [0, 599].
	}
	if ev.TenantKey != "" {
		wire.TenantKey = &ev.TenantKey
	}
	if ev.RequestID != "" {
		wire.RequestId = &ev.RequestID
	}
	if ev.Reason != "" {
		wire.Reason = &ev.Reason
	}
	return wire
}

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

	httpURL    string
	httpClient *http.Client

	batchSize     int
	flushInterval time.Duration
	bufferSize    int

	ring     []UsageEvent
	ringMu   sync.Mutex
	ringHead int
	ringTail int
	ringLen  int

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

	e := &Emitter{
		logger:        logger.With("component", "events"),
		metrics:       metrics,
		httpURL:       cfg.HTTP.URL,
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		batchSize:     batchSize,
		flushInterval: flushInterval,
		bufferSize:    bufferSize,
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
	payload := struct {
		Events []UsageEvent `json:"events"`
	}{Events: batch}

	body, err := json.Marshal(payload)
	if err != nil {
		e.logger.Error("failed to marshal events batch", "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.httpURL, bytes.NewReader(body))
	if err != nil {
		e.logger.Error("failed to create events HTTP request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		e.logger.Warn("failed to send events batch", "error", err, "count", len(batch))
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		e.logger.Warn("events receiver returned error",
			"status", resp.StatusCode, "count", len(batch))
	}
}

// String implements fmt.Stringer for debug logging.
func (e *Emitter) String() string {
	return fmt.Sprintf("Emitter(http=%s, batch=%d, flush=%s, buf=%d)",
		e.httpURL, e.batchSize, e.flushInterval, e.bufferSize)
}

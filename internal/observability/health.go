package observability

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Pre-serialized JSON responses avoid runtime encoding errors entirely.
var (
	jsonAlive      = []byte(`{"status":"alive"}`)
	jsonReady      = []byte(`{"status":"ready"}`)
	jsonNotReady   = []byte(`{"status":"not_ready"}`)
	jsonStarted    = []byte(`{"status":"started"}`)
	jsonNotStarted = []byte(`{"status":"not_started"}`)
	jsonDeepOK     = []byte(`{"status":"ready","redis":"ok"}`)
	jsonDeepFail   = []byte(`{"status":"not_ready","redis":"unreachable"}`)
)

// Pinger is implemented by any type that can check connectivity (e.g. Redis client).
type Pinger interface {
	Ping(ctx context.Context) error
}

// HealthChecker provides startup, liveness, and readiness check endpoints.
type HealthChecker struct {
	started int32 // atomic: 0 = not started, 1 = started
	ready   int32 // atomic: 0 = not ready, 1 = ready

	mu          sync.RWMutex
	redisPinger Pinger // may be nil if no Redis is configured
}

// NewHealthChecker creates a new health checker (starts in not-ready state).
func NewHealthChecker() *HealthChecker {
	return &HealthChecker{}
}

// SetStarted marks the service as having completed startup.
// Kubernetes uses this via the startup probe to know when to begin
// sending liveness and readiness probes.
func (h *HealthChecker) SetStarted() {
	atomic.StoreInt32(&h.started, 1)
}

// IsStarted returns whether the service has completed startup.
func (h *HealthChecker) IsStarted() bool {
	return atomic.LoadInt32(&h.started) == 1
}

// SetReady marks the service as ready to receive traffic.
func (h *HealthChecker) SetReady() {
	atomic.StoreInt32(&h.ready, 1)
}

// SetNotReady marks the service as not ready (draining).
func (h *HealthChecker) SetNotReady() {
	atomic.StoreInt32(&h.ready, 0)
}

// IsReady returns whether the service is ready.
func (h *HealthChecker) IsReady() bool {
	return atomic.LoadInt32(&h.ready) == 1
}

// StartzHandler returns 200 once the service has completed startup, 503 otherwise.
// Kubernetes startup probes use this to gate liveness/readiness checks.
func (h *HealthChecker) StartzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if h.IsStarted() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(jsonStarted)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(jsonNotStarted)
		}
	}
}

// HealthzHandler returns 200 if the process is alive.
func (h *HealthChecker) HealthzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonAlive)
	}
}

// SetRedisPinger registers a Redis client for deep health checks.
// Pass nil to clear it (e.g. during Redis recovery when the client changes).
func (h *HealthChecker) SetRedisPinger(p Pinger) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.redisPinger = p
}

// ReadyzHandler returns 200 if the service is ready, 503 otherwise.
// When the query parameter `deep=true` is present and a Redis pinger has been
// registered, it actively PINGs Redis and returns 503 if unreachable.
func (h *HealthChecker) ReadyzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if !h.IsReady() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(jsonNotReady)
			return
		}

		// Deep health check: probe Redis connectivity.
		if r.URL.Query().Get("deep") == "true" {
			h.mu.RLock()
			pinger := h.redisPinger
			h.mu.RUnlock()

			if pinger != nil {
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancel()
				if err := pinger.Ping(ctx); err != nil {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write(jsonDeepFail)
					return
				}
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(jsonDeepOK)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonReady)
	}
}

package observability

import (
	"net/http"
	"sync/atomic"
)

// Pre-serialized JSON responses avoid runtime encoding errors entirely.
var (
	jsonAlive      = []byte(`{"status":"alive"}`)
	jsonReady      = []byte(`{"status":"ready"}`)
	jsonNotReady   = []byte(`{"status":"not_ready"}`)
	jsonStarted    = []byte(`{"status":"started"}`)
	jsonNotStarted = []byte(`{"status":"not_started"}`)
)

// HealthChecker provides startup, liveness, and readiness check endpoints.
type HealthChecker struct {
	started int32 // atomic: 0 = not started, 1 = started
	ready   int32 // atomic: 0 = not ready, 1 = ready
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

// ReadyzHandler returns 200 if the service is ready, 503 otherwise.
func (h *HealthChecker) ReadyzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if h.IsReady() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(jsonReady)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(jsonNotReady)
		}
	}
}

// Package main is a mock external rate limit service for E2E testing of
// the tenant-aware backend URL feature. It returns rate limit parameters
// and a per-tenant backend_url based on the request headers or path.
//
// Routing logic (tenant derived from headers or path):
//
//   - "tenant-a"          → backend_url = BACKEND_A_URL env var
//   - "tenant-b"          → backend_url = BACKEND_B_URL env var
//   - "tenant-proto-h1"   → backend_url = BACKEND_B_URL, backend_protocol = "h1"
//   - "tenant-rl-cached"  → response includes Cache-Control: max-age=30
//   - "tenant-rl-nocache" → response has no cache headers
//   - Any other           → no backend_url (use EdgeQuota's default)
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

type limitsRequest struct {
	Headers map[string]string `json:"headers"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
}

type limitsResponse struct {
	Average         int64  `json:"average"`
	Burst           int64  `json:"burst"`
	Period          string `json:"period"`
	BackendURL      string `json:"backend_url,omitempty"`
	BackendProtocol string `json:"backend_protocol,omitempty"`
}

// deriveTenant derives a tenant key from request headers or path.
func deriveTenant(req *limitsRequest) string {
	// X-Tenant-Id header (common for tenant identification)
	if v := req.Headers["X-Tenant-Id"]; v != "" {
		return v
	}
	// X-Forwarded-Host / Host: extract first label if it looks like tenant-*
	if v := req.Headers["Host"]; v != "" {
		host := strings.Split(v, ":")[0]
		if idx := strings.Index(host, "."); idx > 0 && strings.HasPrefix(host, "tenant-") {
			return host[:idx]
		}
		if strings.HasPrefix(host, "tenant-") {
			return host
		}
	}
	if v := req.Headers["X-Forwarded-Host"]; v != "" {
		host := strings.Split(v, ":")[0]
		if idx := strings.Index(host, "."); idx > 0 && strings.HasPrefix(host, "tenant-") {
			return host[:idx]
		}
		if strings.HasPrefix(host, "tenant-") {
			return host
		}
	}
	// Path: /tenant-a/... or /tenant-a
	if req.Path != "" {
		path := strings.Trim(req.Path, "/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) > 0 && strings.HasPrefix(parts[0], "tenant-") {
			return parts[0]
		}
	}
	return ""
}

var rlHitCounter atomic.Int64

func main() {
	backendA := os.Getenv("BACKEND_A_URL")
	backendB := os.Getenv("BACKEND_B_URL")

	if backendA == "" || backendB == "" {
		log.Fatal("BACKEND_A_URL and BACKEND_B_URL environment variables are required")
	}

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/hits", func(w http.ResponseWriter, r *http.Request) {
		n := rlHitCounter.Load()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int64{"hits": n})
	})

	http.HandleFunc("/hits/reset", func(w http.ResponseWriter, r *http.Request) {
		rlHitCounter.Store(0)
		w.WriteHeader(http.StatusNoContent)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}

		rlHitCounter.Add(1)

		var req limitsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		resp := limitsResponse{
			Average: 1000,
			Burst:   500,
			Period:  "1s",
		}

		key := deriveTenant(&req)

		switch key {
		case "tenant-a":
			resp.BackendURL = backendA
		case "tenant-b":
			resp.BackendURL = backendB
		case "tenant-proto-h1":
			resp.BackendURL = backendB
			resp.BackendProtocol = "h1"
		case "tenant-rl-cached":
			resp.BackendURL = backendA
			w.Header().Set("Cache-Control", "public, max-age=30")
		case "tenant-rl-nocache":
			resp.BackendURL = backendA
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	log.Println("Mock external RL service listening on :8090")
	log.Fatal(http.ListenAndServe(":8090", nil))
}

// Package main is a mock external rate limit service for E2E testing of
// the tenant-aware backend URL feature. It returns rate limit parameters
// and a per-tenant backend_url based on the request key or headers.
//
// Routing logic:
//
//   - Key "tenant-a"        → backend_url = BACKEND_A_URL env var
//   - Key "tenant-b"        → backend_url = BACKEND_B_URL env var
//   - Key "tenant-proto-h1" → backend_url = BACKEND_B_URL, backend_protocol = "h1"
//   - Any other key         → no backend_url (use EdgeQuota's default)
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

type limitsRequest struct {
	Key     string            `json:"key"`
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

func main() {
	backendA := os.Getenv("BACKEND_A_URL")
	backendB := os.Getenv("BACKEND_B_URL")

	if backendA == "" || backendB == "" {
		log.Fatal("BACKEND_A_URL and BACKEND_B_URL environment variables are required")
	}

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}

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

		switch req.Key {
		case "tenant-a":
			resp.BackendURL = backendA
		case "tenant-b":
			resp.BackendURL = backendB
		case "tenant-proto-h1":
			resp.BackendURL = backendB
			resp.BackendProtocol = "h1"
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	log.Println("Mock external RL service listening on :8090")
	log.Fatal(http.ListenAndServe(":8090", nil))
}

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzLoadFromYAML feeds random YAML through the config loader to find panics,
// unhandled errors, or unexpected behaviour in the parsing and validation logic.
func FuzzLoadFromYAML(f *testing.F) {
	// Seed corpus with a minimal valid config.
	f.Add([]byte(`
server:
  address: ":8080"
backend:
  url: "http://localhost:9090"
rate_limit:
  average: 100
  burst: 200
redis:
  addrs: ["localhost:6379"]
`))
	// Seed with empty YAML.
	f.Add([]byte(``))
	// Seed with deeply nested structure.
	f.Add([]byte(`
server:
  address: ":0"
  tls:
    enabled: true
    cert_file: /nonexistent
    key_file: /nonexistent
    min_version: "1.3"
    http3_enabled: true
  read_timeout: "1s"
  write_timeout: "1s"
  idle_timeout: "1s"
backend:
  url: "https://backend:443"
  timeout: "5s"
  max_idle_conns: 50
  idle_conn_timeout: "30s"
  max_request_body_size: 1048576
rate_limit:
  average: 10
  burst: 20
  key_strategy:
    type: composite
    header_name: X-Api-Key
    composite_parts: ["client_ip", "header"]
  external:
    enabled: true
    http:
      url: "http://ratelimit:8080"
    timeout: "2s"
    cache_ttl: "30s"
auth:
  enabled: true
  failure_policy: failopen
  http:
    url: "http://auth:8080"
    forward_original_headers: true
  timeout: "3s"
redis:
  addrs: ["redis:6379"]
  password: "secret"
`))

	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		// We don't care about errors — we're looking for panics.
		_, _ = LoadFromPath(path)
	})
}

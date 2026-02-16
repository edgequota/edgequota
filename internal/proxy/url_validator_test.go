package proxy

import (
	"net/url"
	"testing"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestValidateBackendURL_SchemeBlocking(t *testing.T) {
	policy := BackendURLPolicy{DenyPrivateNetworks: false}

	tests := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{"http allowed", "http://example.com:80/path", false},
		{"https allowed", "https://example.com:443/path", false},
		{"file blocked", "file:///etc/passwd", true},
		{"gopher blocked", "gopher://evil.com", true},
		{"ftp blocked", "ftp://ftp.example.com", true},
		{"unknown scheme blocked", "javascript://example.com", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := mustParseURL(t, tt.rawURL)
			err := ValidateBackendURL(u, policy)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateBackendURL(%q) error = %v, wantErr %v", tt.rawURL, err, tt.wantErr)
			}
		})
	}
}

func TestValidateBackendURL_PrivateNetworkBlocking(t *testing.T) {
	policy := BackendURLPolicy{DenyPrivateNetworks: true}

	tests := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{"loopback v4", "http://127.0.0.1:8080/", true},
		{"loopback v6", "http://[::1]:8080/", true},
		{"rfc1918 10.x", "http://10.0.0.1:80/", true},
		{"rfc1918 172.16.x", "http://172.16.0.1:80/", true},
		{"rfc1918 192.168.x", "http://192.168.1.1:80/", true},
		{"cloud metadata", "http://169.254.169.254:80/latest/meta-data/", true},
		{"link-local", "http://169.254.1.1:80/", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := mustParseURL(t, tt.rawURL)
			err := ValidateBackendURL(u, policy)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateBackendURL(%q) error = %v, wantErr %v", tt.rawURL, err, tt.wantErr)
			}
		})
	}
}

func TestValidateBackendURL_PrivateNetworkAllowed(t *testing.T) {
	policy := BackendURLPolicy{DenyPrivateNetworks: false}

	u := mustParseURL(t, "http://10.0.0.1:8080/")
	if err := ValidateBackendURL(u, policy); err != nil {
		t.Errorf("private networks should be allowed when DenyPrivateNetworks=false: %v", err)
	}
}

func TestValidateBackendURL_AllowedHosts(t *testing.T) {
	policy := BackendURLPolicy{
		DenyPrivateNetworks: true,
		AllowedHosts:        []string{"my-backend.internal", "api.example.com"},
	}

	tests := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{"allowed host", "http://my-backend.internal:8080/", false},
		{"allowed host case-insensitive", "http://API.EXAMPLE.COM:443/", false},
		{"disallowed host", "http://evil.com:8080/", true},
		// Allowlist bypasses private-network checks.
		{"private IP on allowlist would normally be blocked but host not in list", "http://10.0.0.1:80/", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := mustParseURL(t, tt.rawURL)
			err := ValidateBackendURL(u, policy)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateBackendURL(%q) error = %v, wantErr %v", tt.rawURL, err, tt.wantErr)
			}
		})
	}
}

func TestValidateBackendURL_EmptyHost(t *testing.T) {
	policy := BackendURLPolicy{DenyPrivateNetworks: false}
	u := mustParseURL(t, "http://")
	if err := ValidateBackendURL(u, policy); err == nil {
		t.Error("expected error for empty host")
	}
}

func TestValidateBackendURL_CustomSchemes(t *testing.T) {
	policy := BackendURLPolicy{
		AllowedSchemes:      []string{"grpc", "grpcs"},
		DenyPrivateNetworks: false,
	}

	u := mustParseURL(t, "grpc://my-service:50051/")
	if err := ValidateBackendURL(u, policy); err != nil {
		t.Errorf("grpc scheme should be allowed: %v", err)
	}

	u = mustParseURL(t, "http://my-service:8080/")
	if err := ValidateBackendURL(u, policy); err == nil {
		t.Error("http scheme should be blocked when custom schemes are set")
	}
}

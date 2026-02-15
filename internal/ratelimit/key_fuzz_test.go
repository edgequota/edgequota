package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/edgequota/edgequota/internal/config"
)

// FuzzClientIPExtract feeds adversarial RemoteAddr and X-Forwarded-For values
// through the ClientIP key extractor to find panics, out-of-bounds, or
// unexpected behaviour.
func FuzzClientIPExtract(f *testing.F) {
	f.Add("192.168.1.1:8080", "", "")
	f.Add("10.0.0.1:1234", "1.2.3.4, 5.6.7.8", "9.10.11.12")
	f.Add("[::1]:80", "::ffff:192.168.0.1", "")
	f.Add("not-an-ip", "also-not-an-ip, ,,,,", "")
	f.Add("", "", "")
	f.Add("127.0.0.1:0", "127.0.0.1, 10.0.0.1, 192.168.1.1", "")

	f.Fuzz(func(t *testing.T, remoteAddr, xff, xRealIP string) {
		strategy, err := NewKeyStrategy(config.KeyStrategyConfig{
			Type: config.KeyStrategyClientIP,
		})
		if err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodGet, "/fuzz", nil)
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		if xRealIP != "" {
			req.Header.Set("X-Real-Ip", xRealIP)
		}

		// We don't care about the error, only that it doesn't panic.
		_, _ = strategy.Extract(req)
	})
}

// FuzzCompositeExtract feeds adversarial input through the composite key
// extractor to validate robustness with arbitrary header values and paths.
func FuzzCompositeExtract(f *testing.F) {
	f.Add("192.168.1.1:8080", "/api/v1/users", "tenant-abc")
	f.Add("[::1]:80", "/", "")
	f.Add("", "/path/with/many/segments/and/more", "header\x00with\nnulls")
	f.Add("not:valid:addr:80", "/../../../etc/passwd", "X-Injected: value\r\n")

	f.Fuzz(func(t *testing.T, remoteAddr, path, headerVal string) {
		strategy, err := NewKeyStrategy(config.KeyStrategyConfig{
			Type:       config.KeyStrategyComposite,
			HeaderName: "X-Tenant-Id",
			PathPrefix: true,
		})
		if err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = remoteAddr
		if headerVal != "" {
			req.Header.Set("X-Tenant-Id", headerVal)
		}

		// We don't care about the error, only that it doesn't panic.
		_, _ = strategy.Extract(req)
	})
}

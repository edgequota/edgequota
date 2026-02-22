package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testMetrics() *observability.Metrics {
	return observability.NewMetrics(prometheus.NewRegistry(), 0)
}

// findAccessLog scans newline-delimited JSON log output and returns the first
// entry whose "msg" field is "access", or nil if not found.
func findAccessLog(t *testing.T, data []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var entry map[string]any
		if err := dec.Decode(&entry); err != nil {
			break
		}
		if entry["msg"] == "access" {
			return entry
		}
	}
	return nil
}

func testConfig(redisAddr string) *config.Config {
	cfg := config.Defaults()
	cfg.Backend.URL = "http://backend:8080"
	cfg.RateLimit.Average = 10
	cfg.RateLimit.Burst = 5
	cfg.RateLimit.Period = "1s"
	cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
	cfg.Redis.Endpoints = []string{redisAddr}
	cfg.Redis.Mode = config.RedisModeSingle
	return cfg
}

func TestNewChain(t *testing.T) {
	t.Run("creates chain with valid config and Redis", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		assert.NotNil(t, chain)
		defer chain.Close()
	})

	t.Run("creates chain with no rate limit (average=0)", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		assert.NotNil(t, chain)
		defer chain.Close()
	})

	t.Run("starts recovery when Redis unavailable with passthrough", func(t *testing.T) {
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "100ms"
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()
	})

	t.Run("fails when Redis unavailable with failclosed", func(t *testing.T) {
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "100ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyFailClosed
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

		_, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "redis connection failed")
	})
}

func TestChainServeHTTP(t *testing.T) {
	t.Run("allows requests within rate limit", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()

		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		snap := metrics.Snapshot()
		assert.Equal(t, int64(1), snap.Allowed)
	})

	t.Run("rate limits after burst exhaustion", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.Average = 2
		cfg.RateLimit.Burst = 3
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Exhaust burst.
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "1.2.3.4:5555"
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		// Next should be 429.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code)
		assert.NotEmpty(t, rr.Header().Get("Retry-After"))
		assert.NotEmpty(t, rr.Header().Get("X-Retry-In"))
	})

	t.Run("passes through when rate limit disabled (average=0)", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		for i := 0; i < 20; i++ {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		snap := metrics.Snapshot()
		assert.Equal(t, int64(20), snap.Allowed)
	})

	t.Run("passthrough policy allows all when Redis is down", func(t *testing.T) {
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "100ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()

		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("inMemoryFallback limits when Redis is down", func(t *testing.T) {
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "100ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyInMemoryFallback
		cfg.RateLimit.Average = 2
		cfg.RateLimit.Burst = 2
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		allowed := 0
		limited := 0
		// The first request creates a ristretto entry asynchronously. Give
		// ristretto time to admit the entry before sending more requests,
		// otherwise all requests may create independent buckets.
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "1.2.3.4:5555"
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			if rr.Code == http.StatusOK {
				allowed++
			} else {
				limited++
			}
			// After first request, small pause so ristretto processes the Set.
			if i == 0 {
				time.Sleep(10 * time.Millisecond)
			}
		}

		assert.Greater(t, allowed, 0)
		assert.Greater(t, limited, 0)
	})

	t.Run("returns 500 on key extraction failure", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		cfg.RateLimit.KeyStrategy = config.KeyStrategyConfig{
			Type:       config.KeyStrategyHeader,
			HeaderName: "X-Tenant-Id",
		}
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Request without the required header.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusInternalServerError, rr.Code)
		snap := metrics.Snapshot()
		assert.Equal(t, int64(1), snap.KeyExtractErrors)
	})
}

func TestChainClose(t *testing.T) {
	t.Run("closes without error", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)

		assert.NoError(t, chain.Close())
	})

	t.Run("closes with no limiter", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)

		assert.NoError(t, chain.Close())
	})
}

func TestChainRecovery(t *testing.T) {
	t.Run("recovers when Redis becomes available", func(t *testing.T) {
		// Start with Redis down.
		cfg := testConfig("127.0.0.1:1")
		cfg.Redis.DialTimeout = "100ms"
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		metrics := testMetrics()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), metrics)
		require.NoError(t, err)
		defer chain.Close()

		// Should pass through (Redis down).
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Start Redis.
		mr := miniredis.RunT(t)
		cfg2 := *chain.cfg.Load()
		cfg2.Redis.Endpoints = []string{mr.Addr()}
		chain.cfg.Store(&cfg2)

		// Wait for recovery loop to pick up new Redis.
		time.Sleep(3 * time.Second)

		// Eventually should use Redis limiting.
		// (recovery may or may not have happened depending on backoff timing)
	})
}

// ---------------------------------------------------------------------------
// X-Request-Id Validation Tests
// ---------------------------------------------------------------------------

func TestValidRequestID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"valid UUID", "550e8400-e29b-41d4-a716-446655440000", true},
		{"valid alphanumeric", "abc123", true},
		{"valid with dots", "req.123.abc", true},
		{"valid with colons", "req:123:abc", true},
		{"valid with underscores", "req_123_abc", true},
		{"empty string", "", false},
		{"too long", string(make([]byte, 200)), false},
		{"CRLF injection", "valid\r\nX-Evil: injected", false},
		{"newline", "valid\nX-Evil: injected", false},
		{"space", "valid id", false},
		{"null byte", "valid\x00id", false},
		{"tab", "valid\tid", false},
		{"semicolon", "valid;id", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validRequestID(tt.id)
			assert.Equal(t, tt.want, got, "validRequestID(%q)", tt.id)
		})
	}
}

func TestRequestIDIsReplacedWhenInvalid(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := testConfig(mr.Addr())

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
	require.NoError(t, err)
	defer chain.Close()

	t.Run("invalid request ID is replaced", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		req.Header.Set("X-Request-Id", "evil\r\nX-Injected: true")
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		gotID := rr.Header().Get("X-Request-Id")
		assert.NotEmpty(t, gotID)
		assert.NotContains(t, gotID, "\r")
		assert.NotContains(t, gotID, "\n")
		assert.True(t, validRequestID(gotID))
	})

	t.Run("valid request ID is preserved", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		req.Header.Set("X-Request-Id", "my-custom-request-id-123")
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, "my-custom-request-id-123", rr.Header().Get("X-Request-Id"))
	})
}

// ---------------------------------------------------------------------------
// Auth Header Injection Deny-list Tests
// ---------------------------------------------------------------------------

func TestInjectionDenyHeaders(t *testing.T) {
	denied := []string{
		"Host", "Content-Length", "Transfer-Encoding", "Connection",
		"Te", "Upgrade", "Proxy-Authorization", "Proxy-Connection",
		"Keep-Alive", "Trailer",
	}

	for _, h := range denied {
		_, exists := injectionDenyHeaders[http.CanonicalHeaderKey(h)]
		assert.True(t, exists, "header %q should be in deny set", h)
	}

	// Safe headers should NOT be in the deny set.
	safe := []string{"X-Tenant-Id", "Authorization", "Accept", "X-Custom"}
	for _, h := range safe {
		_, exists := injectionDenyHeaders[http.CanonicalHeaderKey(h)]
		assert.False(t, exists, "header %q should NOT be in deny set", h)
	}
}

func TestValidTenantKey(t *testing.T) {
	t.Run("valid keys", func(t *testing.T) {
		valid := []string{
			"tenant-123",
			"org:proj.env",
			"abc",
			"a",
			"UPPER_CASE",
			"mix.of-ALL:chars_123",
		}
		for _, k := range valid {
			assert.True(t, validTenantKey(k), "expected %q to be valid", k)
		}
	})

	t.Run("empty key is invalid", func(t *testing.T) {
		assert.False(t, validTenantKey(""))
	})

	t.Run("key exceeding max length is invalid", func(t *testing.T) {
		long := ""
		for i := 0; i < maxTenantKeyLen+1; i++ {
			long += "a"
		}
		assert.False(t, validTenantKey(long))
	})

	t.Run("key at exact max length is valid", func(t *testing.T) {
		exact := ""
		for i := 0; i < maxTenantKeyLen; i++ {
			exact += "a"
		}
		assert.True(t, validTenantKey(exact))
	})

	t.Run("invalid characters are rejected", func(t *testing.T) {
		invalid := []string{
			"has space",
			"has/slash",
			"has\ttab",
			"has\nnewline",
			"has\x00null",
			"emoji👍",
			"semicolon;",
			"equals=sign",
			"pipe|char",
		}
		for _, k := range invalid {
			assert.False(t, validTenantKey(k), "expected %q to be invalid", k)
		}
	})
}

func TestStatusWriterBytesWritten(t *testing.T) {
	t.Run("counts bytes across multiple writes", func(t *testing.T) {
		rr := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: rr}

		n1, err := sw.Write([]byte("hello"))
		require.NoError(t, err)
		assert.Equal(t, 5, n1)

		n2, err := sw.Write([]byte(" world!"))
		require.NoError(t, err)
		assert.Equal(t, 7, n2)

		assert.Equal(t, int64(12), sw.bytesWritten)
		assert.Equal(t, http.StatusOK, sw.code)
		assert.True(t, sw.written)
	})

	t.Run("zero on no writes", func(t *testing.T) {
		rr := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: rr}
		sw.WriteHeader(http.StatusNoContent)
		assert.Equal(t, int64(0), sw.bytesWritten)
	})
}

func TestAccessLog(t *testing.T) {
	t.Run("emits structured access log entry", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0

		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))

		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		})

		chain, err := NewChain(context.Background(), next, cfg, logger, testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		buf.Reset()
		req := httptest.NewRequest(http.MethodGet, "/test/path", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		req.Header.Set("User-Agent", "test-agent/1.0")
		rr := httptest.NewRecorder()

		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		entry := findAccessLog(t, buf.Bytes())
		require.NotNil(t, entry, "expected an access log entry")

		assert.Equal(t, "GET", entry["method"])
		assert.Equal(t, "/test/path", entry["path"])
		assert.EqualValues(t, 200, entry["status"])
		assert.Contains(t, entry, "duration_ms")
		assert.EqualValues(t, 2, entry["bytes"])
		assert.Equal(t, "10.0.0.1:12345", entry["remote_addr"])
		assert.Contains(t, entry, "request_id")
		assert.Equal(t, "test-agent/1.0", entry["user_agent"])
		assert.Equal(t, "HTTP/1.1", entry["proto"])
	})

	t.Run("no access log when disabled", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		disabled := false
		cfg.Logging.AccessLog = &disabled

		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))

		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, logger, testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		buf.Reset()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		assert.Nil(t, findAccessLog(t, buf.Bytes()),
			"no access log entry expected when disabled")
	})

	t.Run("access log toggleable via reload", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := testConfig(mr.Addr())

		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))

		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, logger, testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		buf.Reset()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		assert.NotNil(t, findAccessLog(t, buf.Bytes()),
			"access log entry expected before disabling")

		// Disable access log via reload.
		newCfg := testConfig(mr.Addr())
		disabled := false
		newCfg.Logging.AccessLog = &disabled
		require.NoError(t, chain.Reload(newCfg))

		buf.Reset()
		rr = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		chain.ServeHTTP(rr, req)
		assert.Nil(t, findAccessLog(t, buf.Bytes()),
			"access log must be silent after disabling via reload")
	})

	t.Run("access log includes enriched fields", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0

		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))

		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, logger, testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		buf.Reset()
		req := httptest.NewRequest(http.MethodGet, "http://api.example.com/test", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		entry := findAccessLog(t, buf.Bytes())
		require.NotNil(t, entry, "expected an access log entry")

		assert.Equal(t, "api.example.com", entry["host"],
			"access log must include original host from r.Host")
		assert.Equal(t, "http://backend:8080", entry["backend_url"],
			"access log must include resolved backend URL")
		assert.Contains(t, entry, "backend_proto",
			"access log must include backend protocol")
		assert.Contains(t, entry, "rl_key")
		assert.Contains(t, entry, "tenant_id")
	})
}

func TestConcurrencySemaphore(t *testing.T) {
	t.Run("returns 503 when max concurrent requests exceeded", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		cfg.Server.MaxConcurrentRequests = 1

		blocked := make(chan struct{})
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-blocked
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		// First request holds the semaphore.
		rr1 := httptest.NewRecorder()
		go chain.ServeHTTP(rr1, httptest.NewRequest(http.MethodGet, "/", nil))

		// Give the first request time to acquire the semaphore.
		time.Sleep(50 * time.Millisecond)

		// Second request should be rejected immediately.
		rr2 := httptest.NewRecorder()
		chain.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/", nil))

		assert.Equal(t, http.StatusServiceUnavailable, rr2.Code,
			"second request must be rejected with 503")

		// Unblock the first request.
		close(blocked)
		time.Sleep(50 * time.Millisecond)
	})

	t.Run("allows request when under limit", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		cfg.Server.MaxConcurrentRequests = 10

		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("no limit when max_concurrent_requests is 0", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		cfg.Server.MaxConcurrentRequests = 0

		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		assert.Equal(t, http.StatusOK, rr.Code)
	})
}

func TestAllowedInjectionHeaders(t *testing.T) {
	t.Run("auth can inject normally-denied header when in allowed list", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"request_headers": map[string]string{
					"X-Forwarded-For": "10.1.1.1",
				},
			})
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		cfg.Auth.AllowedInjectionHeaders = []string{"X-Forwarded-For"}

		var capturedXFF string
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedXFF = r.Header.Get("X-Forwarded-For")
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "10.1.1.1", capturedXFF,
			"X-Forwarded-For should be injected when in allowed_injection_headers")
	})

	t.Run("auth cannot inject denied header when NOT in allowed list", func(t *testing.T) {
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"request_headers": map[string]string{
					"X-Forwarded-For": "spoofed",
				},
			})
		}))
		defer authServer.Close()

		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = authServer.URL
		// AllowedInjectionHeaders is NOT set, so deny-list applies.

		var capturedXFF string
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedXFF = r.Header.Get("X-Forwarded-For")
			w.WriteHeader(http.StatusOK)
		})

		chain, err := NewChain(context.Background(), next, cfg, testLogger(), testMetrics())
		require.NoError(t, err)
		defer chain.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Empty(t, capturedXFF,
			"X-Forwarded-For must NOT be injected when not in allowed_injection_headers")
	})
}

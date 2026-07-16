package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCachingResponseWriterCacheable(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/static/bundle.js", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=120")
	cw.Header().Set("Content-Type", "application/javascript")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("var x = 1;"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	got, ok := store.Get(context.Background(), key)
	require.True(t, ok, "response should be cached")
	assert.Equal(t, 200, got.StatusCode)
	assert.Equal(t, []byte("var x = 1;"), got.Body)
	assert.Equal(t, "application/javascript", got.Headers.Get("Content-Type"))
}

func TestCachingResponseWriterNoStore(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "no-store")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("sensitive"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	_, ok := store.Get(context.Background(), key)
	assert.False(t, ok, "no-store response should not be cached")
}

func TestCachingResponseWriterNoCacheControl(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("dynamic"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	_, ok := store.Get(context.Background(), key)
	assert.False(t, ok, "response without Cache-Control should not be cached")
}

func TestCachingResponseWriterPrivate(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/user/profile", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "private, max-age=60")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("user data"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	_, ok := store.Get(context.Background(), key)
	assert.False(t, ok, "private response should not be cached")
}

func TestCachingResponseWriterBodyOverflow(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client, WithMaxBodySize(5))

	var skipped bool
	store.OnSkip = func() { skipped = true }

	req := httptest.NewRequest(http.MethodGet, "/large", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=300")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("this body is much too large"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	_, ok := store.Get(context.Background(), key)
	assert.False(t, ok, "oversized response should not be cached")
	assert.True(t, skipped, "OnSkip should be called for oversized body")
}

// Finish must classify the lookup from the RESPONSE, not the request. A
// response the cache was never eligible to serve is not a miss: counting it as
// one makes the hit rate track traffic composition instead of cache health.
func TestFinishClassifiesLookupOutcome(t *testing.T) {
	tests := []struct {
		name            string
		status          int
		header          map[string]string
		body            string
		maxBodySize     int64
		noUpstreamReply bool
		aborted         bool // upstream died part-way: MarkComplete never runs
		wantMiss        bool
		wantUncacheable bool
		wantSkip        bool
		wantStored      bool
	}{
		{
			name:       "cacheable response is an eligible miss",
			status:     http.StatusOK,
			header:     map[string]string{"Cache-Control": "max-age=120"},
			body:       "ok",
			wantMiss:   true,
			wantStored: true,
		},
		{
			name:     "aborted cacheable response is counted but not stored",
			status:   http.StatusOK,
			header:   map[string]string{"Cache-Control": "max-age=120", "Content-Length": "1000"},
			body:     "partial",
			aborted:  true,
			wantMiss: true,
		},
		{
			name:            "aborted uncacheable response is still counted",
			status:          http.StatusOK,
			body:            "partial",
			aborted:         true,
			wantUncacheable: true,
		},
		{
			name:       "301 with max-age is an eligible miss",
			status:     http.StatusMovedPermanently,
			header:     map[string]string{"Cache-Control": "max-age=60"},
			wantMiss:   true,
			wantStored: true,
		},
		{
			name:            "absent cache-control is uncacheable",
			status:          http.StatusOK,
			body:            "ok",
			wantUncacheable: true,
		},
		{
			name:            "no-store is uncacheable",
			status:          http.StatusOK,
			header:          map[string]string{"Cache-Control": "no-store"},
			wantUncacheable: true,
		},
		{
			name:            "private is uncacheable",
			status:          http.StatusOK,
			header:          map[string]string{"Cache-Control": "private, max-age=60"},
			wantUncacheable: true,
		},
		{
			name:            "zero max-age is uncacheable",
			status:          http.StatusOK,
			header:          map[string]string{"Cache-Control": "max-age=0"},
			wantUncacheable: true,
		},
		{
			name:            "error status is uncacheable",
			status:          http.StatusInternalServerError,
			header:          map[string]string{"Cache-Control": "max-age=120"},
			wantUncacheable: true,
		},
		{
			name:            "vary star is uncacheable",
			status:          http.StatusOK,
			header:          map[string]string{"Cache-Control": "max-age=60", "Vary": "*"},
			wantUncacheable: true,
		},
		{
			name:        "oversized cacheable body is a miss that skips the store",
			status:      http.StatusOK,
			header:      map[string]string{"Cache-Control": "max-age=300"},
			body:        "this body is much too large",
			maxBodySize: 5,
			wantMiss:    true,
			wantSkip:    true,
		},
		{
			name:            "no upstream response records no outcome",
			noUpstreamReply: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := newTestRedis(t)
			opts := []Option{}
			if tt.maxBodySize > 0 {
				opts = append(opts, WithMaxBodySize(tt.maxBodySize))
			}
			store := NewStore(client, opts...)

			var misses, uncacheables, skips int
			store.OnMiss = func() { misses++ }
			store.OnUncacheable = func() { uncacheables++ }
			store.OnSkip = func() { skips++ }

			req := httptest.NewRequest(http.MethodGet, "/resource", nil)
			cw := NewCachingResponseWriter(httptest.NewRecorder(), store, req)

			if !tt.noUpstreamReply {
				for k, v := range tt.header {
					cw.Header().Set(k, v)
				}
				cw.WriteHeader(tt.status)
				if tt.body != "" {
					_, _ = cw.Write([]byte(tt.body))
				}
				// An abort skips MarkComplete, exactly as the panicking
				// ReverseProxy path does.
				if !tt.aborted {
					cw.MarkComplete()
				}
			}

			key := store.KeyFromRequest(req, nil)
			cw.Finish(context.Background(), key)

			assert.Equal(t, boolToInt(tt.wantMiss), misses, "miss count")
			assert.Equal(t, boolToInt(tt.wantUncacheable), uncacheables, "uncacheable count")
			assert.Equal(t, boolToInt(tt.wantSkip), skips, "skip count")

			_, stored := store.Get(context.Background(), key)
			assert.Equal(t, tt.wantStored, stored, "stored in cache")

			// Every lookup is classified exactly once, so the three outcomes
			// never double-count a single request.
			assert.LessOrEqual(t, misses+uncacheables, 1, "outcome recorded more than once")
		})
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestCachingResponseWriterPassthrough(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/page", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Content-Type", "text/html")
	_, _ = cw.Write([]byte("<html>hi</html>"))

	assert.Equal(t, 200, rec.Code)
	assert.Equal(t, "<html>hi</html>", rec.Body.String())
}

func TestCachingResponseWriterWithVary(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Header.Set("X-Tenant-Id", "acme")
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=60")
	cw.Header().Set("Vary", "X-Tenant-Id")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("tenant data"))

	// Pass the base key (as production does in proxyWithCache).
	baseKey := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), baseKey)

	// Full entry stored under the vary-expanded key.
	varyKey := store.KeyFromRequest(req, []string{"X-Tenant-Id"})
	got, ok := store.Get(context.Background(), varyKey)
	require.True(t, ok, "full entry should exist under vary key")
	assert.Equal(t, []string{"X-Tenant-Id"}, got.Vary)
	assert.Equal(t, []byte("tenant data"), got.Body)

	// Pointer entry stored under base key for Vary discovery.
	pointer, ok := store.Get(context.Background(), baseKey)
	require.True(t, ok, "pointer entry should exist under base key")
	assert.Equal(t, []string{"X-Tenant-Id"}, pointer.Vary)
}

func TestCachingResponseWriterWithSurrogateKey(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/product/123", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=3600")
	cw.Header().Set("Surrogate-Key", "product-123 catalog")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte(`{"id":123}`))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	_, ok := store.Get(context.Background(), key)
	require.True(t, ok)

	n := store.DeleteByTag(context.Background(), "product-123")
	assert.Equal(t, 1, n)

	_, ok = store.Get(context.Background(), key)
	assert.False(t, ok, "should be purged by tag")
}

func TestCachingResponseWriterVaryStar(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/vary-star", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=60")
	cw.Header().Set("Vary", "*")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("data"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	_, ok := store.Get(context.Background(), key)
	assert.False(t, ok, "Vary: * should prevent caching")
}

func TestCachingResponseWriter500NotCached(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/error", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=60")
	cw.WriteHeader(http.StatusInternalServerError)
	_, _ = cw.Write([]byte("error"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	_, ok := store.Get(context.Background(), key)
	assert.False(t, ok, "500 responses should not be cached")
}

func TestCachingResponseWriterMultipleWrites(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/chunked", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=60")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("chunk1"))
	_, _ = cw.Write([]byte("chunk2"))
	_, _ = cw.Write([]byte("chunk3"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	got, ok := store.Get(context.Background(), key)
	require.True(t, ok)
	assert.Equal(t, "chunk1chunk2chunk3", string(got.Body))
}

func TestCachingResponseWriterETagAndLastModified(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/resource", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=60")
	cw.Header().Set("ETag", `"v1"`)
	cw.Header().Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("data"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	got, ok := store.Get(context.Background(), key)
	require.True(t, ok)
	// The validators survive in the stored headers, so a hit replays them to
	// the client even though nothing revalidates against them.
	assert.Equal(t, `"v1"`, got.Headers.Get("ETag"))
	assert.Equal(t, "Mon, 01 Jan 2024 00:00:00 GMT", got.Headers.Get("Last-Modified"))
}

func TestCachingResponseWriterSurrogateKeyStripped(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/tagged", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=60")
	cw.Header().Set("Surrogate-Key", "tag1 tag2")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("x"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	got, ok := store.Get(context.Background(), key)
	require.True(t, ok)
	assert.Empty(t, got.Headers.Get("Surrogate-Key"), "Surrogate-Key should be stripped from cached headers")
	assert.Equal(t, []string{"tag1", "tag2"}, got.Tags)
}

func TestCachingResponseWriterFlush(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/sse", nil)
	rec := httptest.NewRecorder()

	client, _ := newTestRedis(t)
	store := NewStore(client)
	cw := NewCachingResponseWriter(rec, store, req)

	cw.Flush()
	assert.True(t, rec.Flushed)
}

func TestCachingResponseWriterUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	client, _ := newTestRedis(t)
	store := NewStore(client)
	cw := NewCachingResponseWriter(rec, store, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, rec, cw.Unwrap())
}

func TestCachingResponseWriterImplicitWriteHeader(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/implicit", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=60")
	_, _ = cw.Write([]byte("implicit header"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	got, ok := store.Get(context.Background(), key)
	require.True(t, ok)
	assert.Equal(t, 200, got.StatusCode)
	assert.Equal(t, "implicit header", string(got.Body))
}

func TestCachingResponseWriterDoubleWriteHeader(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/double", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=60")
	cw.WriteHeader(http.StatusOK)
	cw.WriteHeader(http.StatusInternalServerError) // should be ignored
	_, _ = cw.Write([]byte("ok"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	got, ok := store.Get(context.Background(), key)
	require.True(t, ok)
	assert.Equal(t, 200, got.StatusCode, "second WriteHeader should be ignored")
}

func TestCachingResponseWriterLargeBodyOverflowMidStream(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client, WithMaxBodySize(10))

	req := httptest.NewRequest(http.MethodGet, "/grow", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=60")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("12345"))   // 5 bytes, under limit
	_, _ = cw.Write([]byte("67890AB")) // total 12, over limit

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	_, ok := store.Get(context.Background(), key)
	assert.False(t, ok, "body that grows past limit should not be cached")

	assert.Equal(t, "1234567890AB", rec.Body.String(), "client still gets full response")
}

func TestCachingResponseWriterXCacheMissNonCacheable(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/no-cc", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("dynamic"))

	assert.Equal(t, "MISS", rec.Header().Get("X-Cache"), "non-cacheable response should have X-Cache: MISS")
}

func TestCachingResponseWriterXCacheMissCacheable(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/cached", nil)
	rec := httptest.NewRecorder()

	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "max-age=60")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("will be cached"))

	assert.Equal(t, "MISS", rec.Header().Get("X-Cache"), "cacheable response on first request should have X-Cache: MISS")
}

func TestKeyFromRequestLongQuery(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	r := httptest.NewRequest(http.MethodPost, "/search?q="+strings.Repeat("x", 100), nil)
	key := store.KeyFromRequest(r, nil)
	assert.Contains(t, key, "POST|/search?q=")
}

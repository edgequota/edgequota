package cache

import (
	"bytes"
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

// A Vary'd response is written twice -- a pointer under the base key, then the
// entry under the vary-qualified key -- but it is still ONE cached response, and
// only the entry carries a body.
func TestFinishCountsVariedResponseOnce(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	var stores int
	var bodySizes []float64
	store.OnStore = func() { stores++ }
	store.OnBodySize = func(n float64) { bodySizes = append(bodySizes, n) }

	req := httptest.NewRequest(http.MethodGet, "/resource", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	cw := NewCachingResponseWriter(httptest.NewRecorder(), store, req)

	cw.Header().Set("Cache-Control", "max-age=60")
	cw.Header().Set("Vary", "Accept-Encoding")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("body"))
	cw.MarkComplete()

	baseKey := store.KeyFromRequest(req, nil)
	cw.Finish(context.Background(), baseKey)

	// Both writes must land: the pointer is what lets a later lookup discover
	// the Vary list and recompute the real key.
	_, pointerStored := store.Get(context.Background(), baseKey)
	require.True(t, pointerStored, "vary pointer must be stored under the base key")
	_, entryStored := store.Get(context.Background(), store.KeyFromRequest(req, []string{"Accept-Encoding"}))
	require.True(t, entryStored, "entry must be stored under the vary-qualified key")

	assert.Equal(t, 1, stores, "one response must count as one store")
	// The pointer has no body, so counting it would drag the histogram to zero.
	assert.Equal(t, []float64{4}, bodySizes, "only the real body is measured")
}

func TestKeyFromRequestLongQuery(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	r := httptest.NewRequest(http.MethodPost, "/search?q="+strings.Repeat("x", 100), nil)
	key := store.KeyFromRequest(r, nil)
	assert.Contains(t, key, "POST|/search?q=")
}

// interimRecordingWriter faithfully models net/http's 1xx contract: a WriteHeader
// with a 1xx code is an interim response that is recorded but does NOT commit,
// so a later final WriteHeader still fixes the committed status (and a bare
// Write commits an implicit 200). httptest.ResponseRecorder does not model
// interim responses, so bug D needs this.
type interimRecordingWriter struct {
	header    http.Header
	codes     []int // every WriteHeader code, in order
	committed int   // the final committed status
	wrote     bool
	body      bytes.Buffer
}

func (w *interimRecordingWriter) Header() http.Header { return w.header }

func (w *interimRecordingWriter) WriteHeader(code int) {
	w.codes = append(w.codes, code)
	if code >= 100 && code < 200 {
		return // interim: relayed but does not commit
	}
	if w.wrote {
		return
	}
	w.committed = code
	w.wrote = true
}

func (w *interimRecordingWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.committed = http.StatusOK
		w.wrote = true
	}
	return w.body.Write(b)
}

// Bug D: a 1xx informational response (103 Early Hints) relayed through the
// writer must not latch headerSent, or it swallows the real final status and
// disables caching for the response.
func TestCachingResponseWriter1xxIsRelayedNotLatched(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/moved", nil)
	rw := &interimRecordingWriter{header: http.Header{}}
	cw := NewCachingResponseWriter(rw, store, req)

	// 103 Early Hints, exactly as httputil.ReverseProxy relays it.
	cw.Header().Set("Link", "</style.css>; rel=preload")
	cw.WriteHeader(http.StatusEarlyHints)

	// The real response: a cacheable 301.
	cw.Header().Set("Cache-Control", "max-age=60")
	cw.Header().Set("Location", "/new")
	cw.WriteHeader(http.StatusMovedPermanently)
	_, _ = cw.Write([]byte("moved"))

	cw.MarkComplete()
	key := store.KeyFromRequest(req, nil)
	cw.Finish(context.Background(), key)

	assert.Equal(t, []int{http.StatusEarlyHints, http.StatusMovedPermanently}, rw.codes,
		"both the 103 and the final 301 must reach the client")
	assert.Equal(t, http.StatusMovedPermanently, rw.committed,
		"a latched 1xx would corrupt the final status into an implicit 200")

	got, ok := store.Get(context.Background(), key)
	require.True(t, ok, "a cacheable 301 after a 103 must still be cached")
	assert.Equal(t, http.StatusMovedPermanently, got.StatusCode)
}

// Bug A: the writer threads its request into eligibility, so a response to an
// Authorization'd request is cached only with an explicit shared-reuse permit.
func TestCachingResponseWriterAuthorizedResponseNeedsPermit(t *testing.T) {
	t.Run("bare max-age is not cached", func(t *testing.T) {
		client, _ := newTestRedis(t)
		store := NewStore(client)

		req := httptest.NewRequest(http.MethodGet, "/account", nil)
		req.Header.Set("Authorization", "Bearer token")
		cw := NewCachingResponseWriter(httptest.NewRecorder(), store, req)
		cw.Header().Set("Cache-Control", "max-age=60")
		cw.WriteHeader(http.StatusOK)
		_, _ = cw.Write([]byte("private data"))

		key := store.KeyFromRequest(req, nil)
		cw.MarkComplete()
		cw.Finish(context.Background(), key)

		_, ok := store.Get(context.Background(), key)
		assert.False(t, ok, "an authenticated response without a permit must not be cached")
	})

	t.Run("public is cached", func(t *testing.T) {
		client, _ := newTestRedis(t)
		store := NewStore(client)

		req := httptest.NewRequest(http.MethodGet, "/catalog", nil)
		req.Header.Set("Authorization", "Bearer token")
		cw := NewCachingResponseWriter(httptest.NewRecorder(), store, req)
		cw.Header().Set("Cache-Control", "public, max-age=60")
		cw.WriteHeader(http.StatusOK)
		_, _ = cw.Write([]byte("shared data"))

		key := store.KeyFromRequest(req, nil)
		cw.MarkComplete()
		cw.Finish(context.Background(), key)

		_, ok := store.Get(context.Background(), key)
		assert.True(t, ok, "public authorizes shared caching of an authenticated response")
	})
}

// Bug E: a response carrying Set-Cookie is never cached, so its per-client
// cookie cannot replay from cache to a later client.
func TestCachingResponseWriterSetCookieNotCached(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/login-landing", nil)
	rec := httptest.NewRecorder()
	cw := NewCachingResponseWriter(rec, store, req)
	cw.Header().Set("Cache-Control", "public, max-age=60")
	cw.Header().Set("Set-Cookie", "session=abc123; Path=/")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("landing"))

	key := store.KeyFromRequest(req, nil)
	cw.MarkComplete()
	cw.Finish(context.Background(), key)

	_, ok := store.Get(context.Background(), key)
	assert.False(t, ok, "a Set-Cookie response must not be cached")
	assert.Equal(t, "MISS", rec.Header().Get("X-Cache"))
	// The client still receives its cookie -- the veto only stops caching.
	assert.Equal(t, "session=abc123; Path=/", rec.Header().Get("Set-Cookie"))
}

// Bug E, store-time: a Set-Cookie delivered AFTER WriteHeader -- as an HTTP
// trailer the reverse proxy copies into the response headers -- evades the
// WriteHeader-time veto. Finish must re-assert the veto on the final headers, or
// the cookie is stored under the shared key and replayed to later clients.
func TestCachingResponseWriterLateSetCookieNotCached(t *testing.T) {
	cases := []struct {
		name string
		key  string // header key set after WriteHeader
	}{
		{"announced trailer (canonical key)", "Set-Cookie"},
		{"unannounced trailer (TrailerPrefix key)", http.TrailerPrefix + "Set-Cookie"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newTestRedis(t)
			store := NewStore(client)

			req := httptest.NewRequest(http.MethodGet, "/page", nil)
			cw := NewCachingResponseWriter(httptest.NewRecorder(), store, req)
			// At head-commit there is no Set-Cookie: the response is deemed cacheable.
			cw.Header().Set("Cache-Control", "public, max-age=60")
			cw.WriteHeader(http.StatusOK)
			_, _ = cw.Write([]byte("<html>hi</html>"))
			// The cookie arrives later, as a trailer, exactly as ReverseProxy deposits it.
			cw.Header()[tc.key] = []string{"session=abc123; Path=/"}
			cw.MarkComplete()

			key := store.KeyFromRequest(req, nil)
			cw.Finish(context.Background(), key)

			_, ok := store.Get(context.Background(), key)
			assert.False(t, ok, "a trailer-delivered Set-Cookie must not be stored")
		})
	}
}

// Bug D lower bound: a 100 Continue is also a 1xx and must be relayed without
// latching, so the eventual final status still commits. Pins the >=100 edge of
// the 1xx branch (a `code > 100` mutant would latch a 100 Continue).
func TestCachingResponseWriter100ContinueDoesNotLatch(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	req := httptest.NewRequest(http.MethodGet, "/resource", nil)
	rw := &interimRecordingWriter{header: http.Header{}}
	cw := NewCachingResponseWriter(rw, store, req)

	cw.WriteHeader(http.StatusContinue) // 100

	cw.Header().Set("Cache-Control", "max-age=60")
	cw.WriteHeader(http.StatusOK) // final
	_, _ = cw.Write([]byte("body"))

	cw.MarkComplete()
	key := store.KeyFromRequest(req, nil)
	cw.Finish(context.Background(), key)

	assert.Equal(t, []int{http.StatusContinue, http.StatusOK}, rw.codes,
		"the 100 and the final 200 must both reach the client")
	assert.Equal(t, http.StatusOK, rw.committed,
		"a latched 100 Continue would corrupt the committed status")
	got, ok := store.Get(context.Background(), key)
	require.True(t, ok, "a cacheable 200 after a 100 Continue must still be cached")
	assert.Equal(t, http.StatusOK, got.StatusCode)
}

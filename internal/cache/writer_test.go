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
	cw.Finish(context.Background(), key)

	_, ok := store.Get(context.Background(), key)
	assert.False(t, ok, "oversized response should not be cached")
	assert.True(t, skipped, "OnSkip should be called for oversized body")
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

	key := store.KeyFromRequest(req, []string{"X-Tenant-Id"})
	cw.Finish(context.Background(), key)

	got, ok := store.Get(context.Background(), key)
	require.True(t, ok)
	assert.Equal(t, []string{"X-Tenant-Id"}, got.Vary)
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
	cw.Finish(context.Background(), key)

	got, ok := store.Get(context.Background(), key)
	require.True(t, ok)
	assert.Equal(t, `"v1"`, got.ETag)
	assert.Equal(t, "Mon, 01 Jan 2024 00:00:00 GMT", got.LastModified)
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
	cw.Finish(context.Background(), key)

	_, ok := store.Get(context.Background(), key)
	assert.False(t, ok, "body that grows past limit should not be cached")

	assert.Equal(t, "1234567890AB", rec.Body.String(), "client still gets full response")
}

func TestKeyFromRequestLongQuery(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	r := httptest.NewRequest(http.MethodPost, "/search?q="+strings.Repeat("x", 100), nil)
	key := store.KeyFromRequest(r, nil)
	assert.Contains(t, key, "POST|/search?q=")
}

package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRedis(t *testing.T) (redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := redis.NewClient(config.RedisConfig{
		Endpoints: []string{mr.Addr()},
		Mode:      config.RedisModeSingle,
	})
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	return client, mr
}

func TestStoreGetSetRoundTrip(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	entry := &Entry{
		StatusCode: 200,
		Headers:    http.Header{"Content-Type": []string{"text/html"}},
		Body:       []byte("<html>hello</html>"),
		ETag:       `"abc123"`,
		CreatedAt:  time.Now(),
	}

	ctx := context.Background()
	store.Set(ctx, "test-key", entry, 60*time.Second)

	got, ok := store.Get(ctx, "test-key")
	require.True(t, ok)
	assert.Equal(t, 200, got.StatusCode)
	assert.Equal(t, []byte("<html>hello</html>"), got.Body)
	assert.Equal(t, `"abc123"`, got.ETag)
	assert.Equal(t, "text/html", got.Headers.Get("Content-Type"))
}

func TestStoreGetMiss(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	got, ok := store.Get(context.Background(), "nonexistent")
	assert.False(t, ok)
	assert.Nil(t, got)
}

func TestStoreTTLExpiry(t *testing.T) {
	client, mr := newTestRedis(t)
	store := NewStore(client)

	ctx := context.Background()
	store.Set(ctx, "expires", &Entry{StatusCode: 200, Body: []byte("data")}, 1*time.Second)

	got, ok := store.Get(ctx, "expires")
	require.True(t, ok)
	assert.Equal(t, 200, got.StatusCode)

	mr.FastForward(2 * time.Second)

	_, ok = store.Get(ctx, "expires")
	assert.False(t, ok, "entry should have expired")
}

func TestStoreZeroTTLSkipsCache(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)
	ctx := context.Background()

	store.Set(ctx, "no-cache", &Entry{StatusCode: 200, Body: []byte("x")}, 0)

	_, ok := store.Get(ctx, "no-cache")
	assert.False(t, ok, "TTL=0 should not store anything")
}

func TestStoreDelete(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)
	ctx := context.Background()

	store.Set(ctx, "purge-me", &Entry{StatusCode: 200, Body: []byte("x")}, time.Minute)
	_, ok := store.Get(ctx, "purge-me")
	require.True(t, ok)

	deleted := store.Delete(ctx, "purge-me")
	assert.True(t, deleted)

	_, ok = store.Get(ctx, "purge-me")
	assert.False(t, ok)
}

func TestStoreDeleteNonexistent(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	deleted := store.Delete(context.Background(), "nope")
	assert.False(t, deleted)
}

func TestStoreDeleteByTag(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)
	ctx := context.Background()

	store.Set(ctx, "product-page", &Entry{
		StatusCode: 200,
		Body:       []byte("page1"),
		Tags:       []string{"product-123", "homepage"},
	}, time.Minute)
	store.Set(ctx, "product-api", &Entry{
		StatusCode: 200,
		Body:       []byte("api1"),
		Tags:       []string{"product-123"},
	}, time.Minute)
	store.Set(ctx, "other", &Entry{
		StatusCode: 200,
		Body:       []byte("other"),
		Tags:       []string{"homepage"},
	}, time.Minute)

	n := store.DeleteByTag(ctx, "product-123")
	assert.Equal(t, 2, n)

	_, ok := store.Get(ctx, "product-page")
	assert.False(t, ok)
	_, ok = store.Get(ctx, "product-api")
	assert.False(t, ok)

	got, ok := store.Get(ctx, "other")
	assert.True(t, ok, "unrelated entry should survive")
	assert.Equal(t, []byte("other"), got.Body)
}

func TestStoreDeleteByTagEmpty(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)
	n := store.DeleteByTag(context.Background(), "nonexistent-tag")
	assert.Equal(t, 0, n)
}

func TestKeyFromRequestBasic(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	r := httptest.NewRequest(http.MethodGet, "/static/bundle.js", nil)
	key := store.KeyFromRequest(r, nil)
	assert.Equal(t, "GET|/static/bundle.js", key)
}

func TestKeyFromRequestWithQuery(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	r := httptest.NewRequest(http.MethodGet, "/api/users?page=2&limit=10", nil)
	key := store.KeyFromRequest(r, nil)
	assert.Equal(t, "GET|/api/users?page=2&limit=10", key)
}

func TestKeyFromRequestWithVary(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	r := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	r.Header.Set("X-Tenant-Id", "acme")
	r.Header.Set("Authorization", "Bearer token")
	r.Header.Set("X-Request-Id", "unique-per-request")

	key := store.KeyFromRequest(r, []string{"X-Tenant-Id"})
	assert.Equal(t, "GET|/api/data|X-Tenant-Id=acme", key)
	assert.NotContains(t, key, "Authorization")
	assert.NotContains(t, key, "X-Request-Id")
}

func TestKeyFromRequestNoHeadersWithoutVary(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	r := httptest.NewRequest(http.MethodGet, "/page", nil)
	r.Header.Set("X-Tenant-Id", "acme")
	r.Header.Set("Traceparent", "00-trace-span-01")

	key := store.KeyFromRequest(r, nil)
	assert.Equal(t, "GET|/page", key, "without Vary, headers should not be in the key")
}

func TestKeyFromRequestVarySelectsHeaders(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	r := httptest.NewRequest(http.MethodGet, "/page", nil)
	r.Header.Set("X-Tenant-Id", "acme")
	r.Header.Set("Traceparent", "00-trace-span-01")
	r.Header.Set("X-Request-Id", "abc")

	key := store.KeyFromRequest(r, []string{"X-Tenant-Id"})
	assert.Contains(t, key, "X-Tenant-Id=acme")
	assert.NotContains(t, key, "Traceparent")
	assert.NotContains(t, key, "X-Request-Id")
}

func TestKeyFromRequestDeterministic(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client)

	t.Run("without Vary", func(t *testing.T) {
		r1 := httptest.NewRequest(http.MethodGet, "/api", nil)
		r1.Header.Set("B-Header", "b")
		r2 := httptest.NewRequest(http.MethodGet, "/api", nil)
		r2.Header.Set("A-Header", "a")

		assert.Equal(t, store.KeyFromRequest(r1, nil), store.KeyFromRequest(r2, nil),
			"without Vary, headers should not affect the key")
	})

	t.Run("with Vary headers in different order", func(t *testing.T) {
		r1 := httptest.NewRequest(http.MethodGet, "/api", nil)
		r1.Header.Set("B-Header", "b")
		r1.Header.Set("A-Header", "a")

		r2 := httptest.NewRequest(http.MethodGet, "/api", nil)
		r2.Header.Set("A-Header", "a")
		r2.Header.Set("B-Header", "b")

		vary := []string{"B-Header", "A-Header"}
		assert.Equal(t, store.KeyFromRequest(r1, vary), store.KeyFromRequest(r2, vary),
			"Vary headers should be sorted for deterministic keys")
	})
}

func TestMaxBodySize(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewStore(client, WithMaxBodySize(10))

	assert.Equal(t, int64(10), store.MaxBodySize())
}

func TestStoreMetricHooks(t *testing.T) {
	client, _ := newTestRedis(t)
	var stored, purged int
	store := NewStore(client)
	store.OnStore = func() { stored++ }
	store.OnPurge = func() { purged++ }

	ctx := context.Background()
	store.Set(ctx, "k", &Entry{StatusCode: 200, Body: []byte("x")}, time.Minute)
	assert.Equal(t, 1, stored)

	store.Delete(ctx, "k")
	assert.Equal(t, 1, purged)
}

func TestParseCacheControl(t *testing.T) {
	t.Run("max-age", func(t *testing.T) {
		maxAge, noStore, noCache, private, public := ParseCacheControl("max-age=300")
		assert.Equal(t, 300*time.Second, maxAge)
		assert.False(t, noStore)
		assert.False(t, noCache)
		assert.False(t, private)
		assert.False(t, public)
	})

	t.Run("no-store", func(t *testing.T) {
		_, noStore, _, _, _ := ParseCacheControl("no-store")
		assert.True(t, noStore)
	})

	t.Run("no-cache", func(t *testing.T) {
		_, _, noCache, _, _ := ParseCacheControl("no-cache")
		assert.True(t, noCache)
	})

	t.Run("private", func(t *testing.T) {
		_, _, _, private, _ := ParseCacheControl("private, max-age=60")
		assert.True(t, private)
	})

	t.Run("public with max-age", func(t *testing.T) {
		maxAge, _, _, _, public := ParseCacheControl("public, max-age=3600")
		assert.True(t, public)
		assert.Equal(t, 3600*time.Second, maxAge)
	})

	t.Run("case insensitive", func(t *testing.T) {
		maxAge, _, _, _, _ := ParseCacheControl("Max-Age=120")
		assert.Equal(t, 120*time.Second, maxAge)
	})

	t.Run("empty string", func(t *testing.T) {
		maxAge, noStore, noCache, private, public := ParseCacheControl("")
		assert.Zero(t, maxAge)
		assert.False(t, noStore)
		assert.False(t, noCache)
		assert.False(t, private)
		assert.False(t, public)
	})
}

func TestIsCacheable(t *testing.T) {
	t.Run("200 with max-age is cacheable", func(t *testing.T) {
		h := http.Header{}
		h.Set("Cache-Control", "max-age=60")
		ttl, ok := IsCacheable(200, h)
		assert.True(t, ok)
		assert.Equal(t, 60*time.Second, ttl)
	})

	t.Run("200 with no-store is not cacheable", func(t *testing.T) {
		h := http.Header{}
		h.Set("Cache-Control", "no-store")
		_, ok := IsCacheable(200, h)
		assert.False(t, ok)
	})

	t.Run("200 with private is not cacheable", func(t *testing.T) {
		h := http.Header{}
		h.Set("Cache-Control", "private, max-age=300")
		_, ok := IsCacheable(200, h)
		assert.False(t, ok)
	})

	t.Run("no Cache-Control is not cacheable", func(t *testing.T) {
		_, ok := IsCacheable(200, http.Header{})
		assert.False(t, ok)
	})

	t.Run("500 is not cacheable", func(t *testing.T) {
		h := http.Header{}
		h.Set("Cache-Control", "max-age=60")
		_, ok := IsCacheable(500, h)
		assert.False(t, ok)
	})

	t.Run("301 with max-age is cacheable", func(t *testing.T) {
		h := http.Header{}
		h.Set("Cache-Control", "max-age=86400")
		ttl, ok := IsCacheable(301, h)
		assert.True(t, ok)
		assert.Equal(t, 86400*time.Second, ttl)
	})
}

func TestParseVary(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		h := http.Header{}
		headers, ok := ParseVary(h)
		assert.True(t, ok)
		assert.Nil(t, headers)
	})

	t.Run("single header", func(t *testing.T) {
		h := http.Header{}
		h.Set("Vary", "Accept-Encoding")
		headers, ok := ParseVary(h)
		assert.True(t, ok)
		assert.Equal(t, []string{"Accept-Encoding"}, headers)
	})

	t.Run("multiple headers", func(t *testing.T) {
		h := http.Header{}
		h.Set("Vary", "Accept-Encoding, X-Tenant-Id")
		headers, ok := ParseVary(h)
		assert.True(t, ok)
		assert.Equal(t, []string{"Accept-Encoding", "X-Tenant-Id"}, headers)
	})

	t.Run("star is uncacheable", func(t *testing.T) {
		h := http.Header{}
		h.Set("Vary", "*")
		_, ok := ParseVary(h)
		assert.False(t, ok)
	})
}

func TestParseSurrogateKey(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		tags := ParseSurrogateKey(http.Header{})
		assert.Nil(t, tags)
	})

	t.Run("single tag", func(t *testing.T) {
		h := http.Header{}
		h.Set("Surrogate-Key", "product-123")
		tags := ParseSurrogateKey(h)
		assert.Equal(t, []string{"product-123"}, tags)
	})

	t.Run("multiple tags", func(t *testing.T) {
		h := http.Header{}
		h.Set("Surrogate-Key", "product-123 homepage featured")
		tags := ParseSurrogateKey(h)
		assert.Equal(t, []string{"product-123", "homepage", "featured"}, tags)
	})

	t.Run("uses Cache-Tag as fallback", func(t *testing.T) {
		h := http.Header{}
		h.Set("Cache-Tag", "api-v2")
		tags := ParseSurrogateKey(h)
		assert.Equal(t, []string{"api-v2"}, tags)
	})
}

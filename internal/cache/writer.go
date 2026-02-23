package cache

import (
	"bytes"
	"context"
	"net/http"
	"time"
)

// CachingResponseWriter wraps an http.ResponseWriter to capture the response
// for caching. It inspects Cache-Control on WriteHeader to decide whether to
// buffer the body. Non-cacheable responses pass through with zero overhead.
type CachingResponseWriter struct {
	http.ResponseWriter
	store *Store
	req   *http.Request

	statusCode int
	buf        *bytes.Buffer
	cacheable  bool
	ttl        time.Duration
	vary       []string
	tags       []string
	overflow   bool
	headerSent bool
}

// NewCachingResponseWriter creates a writer that inspects upstream responses
// for cache eligibility and buffers cacheable bodies.
func NewCachingResponseWriter(w http.ResponseWriter, store *Store, req *http.Request) *CachingResponseWriter {
	return &CachingResponseWriter{
		ResponseWriter: w,
		store:          store,
		req:            req,
		statusCode:     http.StatusOK,
	}
}

// WriteHeader captures the status code and decides cache eligibility.
func (cw *CachingResponseWriter) WriteHeader(code int) {
	if cw.headerSent {
		return
	}
	cw.headerSent = true
	cw.statusCode = code

	ttl, ok := IsCacheable(code, cw.Header())
	if !ok {
		cw.ResponseWriter.WriteHeader(code)
		return
	}

	varyHeaders, varyOK := ParseVary(cw.Header())
	if !varyOK {
		cw.ResponseWriter.WriteHeader(code)
		return
	}

	cw.cacheable = true
	cw.ttl = ttl
	cw.vary = varyHeaders
	cw.tags = ParseSurrogateKey(cw.Header())
	cw.buf = &bytes.Buffer{}

	cw.ResponseWriter.WriteHeader(code)
}

// Write captures body bytes for cacheable responses, passing through
// to the underlying writer simultaneously.
func (cw *CachingResponseWriter) Write(b []byte) (int, error) {
	if !cw.headerSent {
		cw.WriteHeader(http.StatusOK)
	}

	n, err := cw.ResponseWriter.Write(b)

	if cw.cacheable && !cw.overflow && cw.buf != nil {
		cw.buf.Write(b[:n])
		if int64(cw.buf.Len()) > cw.store.MaxBodySize() {
			cw.overflow = true
			cw.buf = nil
		}
	}

	return n, err
}

// Unwrap returns the underlying ResponseWriter for interface assertions
// (http.Hijacker, http.Flusher, etc.).
func (cw *CachingResponseWriter) Unwrap() http.ResponseWriter {
	return cw.ResponseWriter
}

// Flush implements http.Flusher.
func (cw *CachingResponseWriter) Flush() {
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Finish must be called after the response is fully written. It stores
// the response in the cache if it was deemed cacheable. When the response
// has a Vary header, the cache key is recomputed to include the Vary'd
// request header values.
func (cw *CachingResponseWriter) Finish(ctx context.Context, cacheKey string) {
	if !cw.cacheable || cw.overflow || cw.buf == nil {
		if cw.overflow && cw.store.OnSkip != nil {
			cw.store.OnSkip()
		}
		return
	}

	storeKey := cacheKey
	if len(cw.vary) > 0 {
		storeKey = cw.store.KeyFromRequest(cw.req, cw.vary)
	}

	headers := cw.Header().Clone()
	headers.Del("Surrogate-Key")
	headers.Del("Cache-Tag")

	entry := &Entry{
		StatusCode:   cw.statusCode,
		Headers:      headers,
		Body:         cw.buf.Bytes(),
		ETag:         headers.Get("ETag"),
		LastModified: headers.Get("Last-Modified"),
		Vary:         cw.vary,
		Tags:         cw.tags,
		CreatedAt:    time.Now(),
	}

	cw.store.Set(ctx, storeKey, entry, cw.ttl)
}

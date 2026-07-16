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
	complete   bool
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
		cw.ResponseWriter.Header().Set("X-Cache", "MISS")
		cw.ResponseWriter.WriteHeader(code)
		return
	}

	varyHeaders, varyOK := ParseVary(cw.Header())
	if !varyOK {
		cw.ResponseWriter.Header().Set("X-Cache", "MISS")
		cw.ResponseWriter.WriteHeader(code)
		return
	}

	cw.cacheable = true
	cw.ttl = ttl
	cw.vary = varyHeaders
	cw.tags = ParseSurrogateKey(cw.Header())
	cw.buf = &bytes.Buffer{}

	cw.ResponseWriter.Header().Set("X-Cache", "MISS")
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

// MarkComplete records that the upstream response was relayed in full. Callers
// must call it only after the upstream handler returns normally: if the copy
// fails part-way, httputil.ReverseProxy panics with http.ErrAbortHandler, so
// this is skipped and Finish knows the buffered body is a fragment.
//
// That panic is conditional — ReverseProxy raises it only when the request
// context carries http.ServerContextKey — so a server that does not set the key
// returns normally on a copy failure and a caller would wrongly mark a fragment
// complete. quic-go is such a server; see underHTTPServerContext in
// internal/server.
func (cw *CachingResponseWriter) MarkComplete() {
	cw.complete = true
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

// Finish records the lookup outcome and stores the response in the cache if it
// was deemed cacheable and arrived in full. When the response has a Vary header,
// the cache key is recomputed to include the Vary'd request header values.
//
// Call it deferred: a copy failure part-way through the upstream response makes
// httputil.ReverseProxy panic with http.ErrAbortHandler, and the lookup still
// happened, so it must still be counted.
//
// The outcome is classified here rather than at lookup time because cache
// eligibility is a property of the response: a request whose response carries
// no positive max-age could never have been served from cache, so counting it
// as a miss would make the hit rate track traffic composition instead of cache
// health.
func (cw *CachingResponseWriter) Finish(ctx context.Context, cacheKey string) {
	// The upstream never responded (e.g. the client disconnected first), so
	// the cache was neither used nor bypassed — record nothing either way.
	if !cw.headerSent {
		return
	}

	if !cw.cacheable {
		if cw.store.OnUncacheable != nil {
			cw.store.OnUncacheable()
		}
		return
	}

	// Cache-eligible but not already cached: a genuine miss.
	if cw.store.OnMiss != nil {
		cw.store.OnMiss()
	}

	// An aborted response leaves a fragment in the buffer that would be served
	// to every later hit as if it were the whole body, so count the lookup but
	// never store it.
	if !cw.complete {
		return
	}

	if cw.overflow || cw.buf == nil {
		if cw.overflow && cw.store.OnSkip != nil {
			cw.store.OnSkip()
		}
		return
	}

	storeKey := cacheKey
	if len(cw.vary) > 0 {
		storeKey = cw.store.KeyFromRequest(cw.req, cw.vary)
		if storeKey != cacheKey {
			// Store a lightweight pointer under the base key so that
			// serveCached() can discover the Vary headers and recompute
			// the full cache key for lookup.
			pointer := &Entry{
				StatusCode: cw.statusCode,
				Vary:       cw.vary,
				CreatedAt:  time.Now(),
			}
			cw.store.Set(ctx, cacheKey, pointer, cw.ttl)
		}
	}

	headers := cw.Header().Clone()
	headers.Del("Surrogate-Key")
	headers.Del("Cache-Tag")

	entry := &Entry{
		StatusCode: cw.statusCode,
		Headers:    headers,
		Body:       cw.buf.Bytes(),
		Vary:       cw.vary,
		Tags:       cw.tags,
		CreatedAt:  time.Now(),
	}

	cw.store.Set(ctx, storeKey, entry, cw.ttl)
}

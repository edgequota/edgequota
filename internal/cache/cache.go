// Package cache provides a CDN-style response cache backed by Redis.
// It honors Cache-Control semantics from any upstream origin (backend,
// external rate-limit service, or external auth service). The cache
// decision is entirely response-driven: origins opt in by returning
// Cache-Control headers or body-level cache fields.
package cache

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edgequota/edgequota/internal/redis"
)

const (
	keyPrefix = "eq:cache:"
	tagPrefix = "eq:tags:"
)

// Entry is a cached HTTP response. The backend's validators (ETag,
// Last-Modified) need no dedicated fields: they travel in Headers and are
// replayed to the client on a hit, and nothing revalidates against them.
type Entry struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body"`
	Vary       []string    `json:"vary,omitempty"`
	Tags       []string    `json:"tags,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
}

// Store is a response cache backed by Redis.
type Store struct {
	client      redis.Client
	maxBodySize int64
	logger      *slog.Logger

	// OnHit fires when a request is served from cache. OnMiss and
	// OnUncacheable are decided only once the upstream response is known
	// (see CachingResponseWriter.Finish), because cache eligibility is a
	// property of the response, not of the request.
	OnHit         func()
	OnMiss        func()
	OnUncacheable func()
	OnStore       func()
	OnSkip        func()
	OnPurge       func()
	OnBodySize    func(float64)

	// OnRedisError fires for every Redis operation that fails for a reason
	// other than a missing key, which is an ordinary negative lookup.
	// OnHealthy fires only when reachability changes, so a healthy pool
	// serving traffic does not re-report on every operation.
	OnRedisError func()
	OnHealthy    func(healthy bool)

	// healthy tracks whether Redis is currently reachable. The store owns this
	// because it owns the client: nothing else sees these operations fail.
	// healthMu serializes a reachability transition with its OnHealthy emission
	// so the reported gauge can never latch to the opposite of healthy after a
	// flap; the atomic lets the common no-transition case skip the lock.
	healthy  atomic.Bool
	healthMu sync.Mutex
}

// Option configures a Store.
type Option func(*Store)

// WithMaxBodySize sets the maximum cacheable response body in bytes.
// Responses larger than this are not cached. Default: 1MB.
func WithMaxBodySize(n int64) Option {
	return func(s *Store) { s.maxBodySize = n }
}

// WithLogger sets the logger for debug/error messages.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.logger = l }
}

const defaultMaxBodySize = 1 << 20 // 1 MB

// NewStore creates a response cache backed by the given Redis client.
func NewStore(client redis.Client, opts ...Option) *Store {
	s := &Store{
		client:      client,
		maxBodySize: defaultMaxBodySize,
		logger:      slog.Default(),
	}
	for _, o := range opts {
		o(s)
	}
	s.healthy.Store(true)
	return s
}

// noteRedis records the outcome of a Redis operation. A missing key is a normal
// negative lookup, so it counts as neither an error nor unreachability.
//
// A caller-canceled context means the client disconnected before the operation
// finished; it reveals nothing about Redis, so it is neither counted nor a
// health transition. Counting it would page EdgeQuotaRedisErrors for a burst of
// client cancellations while Redis is perfectly healthy.
//
// Only a connectivity-class failure flips the pool unhealthy: a malformed
// command or a READONLY replica means Redis answered, so reporting it as
// unreachable would page for the wrong thing.
func (s *Store) noteRedis(err error) {
	if err == nil || redis.IsNilErr(err) {
		s.setHealthy(true)
		return
	}
	if redis.IsCanceledErr(err) {
		return
	}

	if s.OnRedisError != nil {
		s.OnRedisError()
	}
	if redis.IsConnectivityErr(err) {
		s.setHealthy(false)
	}
}

// setHealthy records a reachability transition and emits it exactly once. The
// atomic read is a lock-free fast path for the overwhelmingly common case where
// nothing changes; a genuine flip takes the lock so the OnHealthy emission is
// ordered with the state change. Because every flip writes the state and emits
// under the same lock, the reported gauge stays in lockstep with healthy even
// when concurrent goroutines see opposite outcomes during a Redis flap.
func (s *Store) setHealthy(healthy bool) {
	if s.healthy.Load() == healthy {
		return
	}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if s.healthy.Load() == healthy {
		return
	}
	s.healthy.Store(healthy)
	if s.OnHealthy != nil {
		s.OnHealthy(healthy)
	}
}

// ReportHealth emits the store's current reachability through OnHealthy. Call it
// once at install, before the store is published to request goroutines, so the
// reported gauge reconciles with this store: a boot-time outage flips the gauge
// unhealthy while no store exists, and the store that finally connects must
// clear it. A freshly installed store is healthy (its client connected), so
// this also keeps the normal path a harmless no-op.
func (s *Store) ReportHealth() {
	if s.OnHealthy != nil {
		s.OnHealthy(s.healthy.Load())
	}
}

// MaxBodySize returns the configured maximum cacheable body size.
func (s *Store) MaxBodySize() int64 { return s.maxBodySize }

// Get retrieves a cached entry by key. Returns nil, false on miss.
func (s *Store) Get(ctx context.Context, key string) (*Entry, bool) {
	data, err := s.client.Get(ctx, keyPrefix+key).Bytes()
	s.noteRedis(err)
	if err != nil {
		return nil, false
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		s.logger.Debug("cache: unmarshal error", "key", key, "error", err)
		return nil, false
	}
	return &e, true
}

// Set stores a cached response with the given TTL and counts it. Tags are
// indexed for tag-based purging. Entries with TTL <= 0 are skipped.
func (s *Store) Set(ctx context.Context, key string, entry *Entry, ttl time.Duration) {
	if !s.write(ctx, key, entry, ttl) {
		return
	}
	if s.OnStore != nil {
		s.OnStore()
	}
	if s.OnBodySize != nil {
		s.OnBodySize(float64(len(entry.Body)))
	}
}

// setIndex stores an internal index entry -- a Vary pointer, which carries no
// body and stands for no response of its own. It deliberately skips OnStore and
// OnBodySize: those count cached responses, and a Vary'd response is written
// twice (pointer plus the vary-qualified entry), so counting both would double
// every Vary'd response and feed a bodyless 0-byte observation into the
// body-size histogram.
func (s *Store) setIndex(ctx context.Context, key string, entry *Entry, ttl time.Duration) {
	s.write(ctx, key, entry, ttl)
}

// write persists the entry and its tag index, reporting whether the entry
// reached Redis. A write that failed is not a store: reporting it as one would
// hold the store metric at full rate straight through a Redis outage.
func (s *Store) write(ctx context.Context, key string, entry *Entry, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	data, err := json.Marshal(entry)
	if err != nil {
		s.logger.Debug("cache: marshal error", "key", key, "error", err)
		return false
	}

	err = s.client.Set(ctx, keyPrefix+key, data, ttl).Err()
	s.noteRedis(err)
	if err != nil {
		s.logger.Debug("cache: store error", "key", key, "error", err)
		return false
	}

	// The entry is cached either way; a failed tag index only costs the entry
	// its tag-based purge, so it must not discard the store.
	for _, tag := range entry.Tags {
		tagErr := s.client.SAdd(ctx, tagPrefix+tag, key).Err()
		s.noteRedis(tagErr)
		if tagErr != nil {
			s.logger.Debug("cache: tag index error", "key", key, "tag", tag, "error", tagErr)
		}
	}

	s.logger.Debug("cache: stored", "key", key, "ttl", ttl, "body_size", len(entry.Body), "tags", entry.Tags)
	return true
}

// Delete removes a single cache entry by key.
func (s *Store) Delete(ctx context.Context, key string) bool {
	n, err := s.client.Del(ctx, keyPrefix+key).Result()
	s.noteRedis(err)
	if err != nil || n == 0 {
		return false
	}
	if s.OnPurge != nil {
		s.OnPurge()
	}
	s.logger.Debug("cache: purged", "key", key)
	return true
}

// DeleteByTag removes all cache entries associated with a tag.
// Returns the number of entries deleted.
func (s *Store) DeleteByTag(ctx context.Context, tag string) int {
	members, err := s.client.SMembers(ctx, tagPrefix+tag).Result()
	s.noteRedis(err)
	if err != nil || len(members) == 0 {
		return 0
	}

	redisKeys := make([]string, 0, len(members))
	for _, m := range members {
		redisKeys = append(redisKeys, keyPrefix+m)
	}
	n, delErr := s.client.Del(ctx, redisKeys...).Result()
	s.noteRedis(delErr)
	_ = s.client.Del(ctx, tagPrefix+tag)

	if s.OnPurge != nil {
		for i := int64(0); i < n; i++ {
			s.OnPurge()
		}
	}
	s.logger.Debug("cache: purged by tag", "tag", tag, "count", n)
	return int(n)
}

// KeyFromRequest computes a deterministic cache key from an HTTP request.
// The base key is always method|path?query. When varyHeaders is non-empty,
// the listed request header values are appended to the key. Without Vary
// headers, the key is just method|path?query — request headers do not
// participate unless the origin explicitly lists them via Vary.
func (s *Store) KeyFromRequest(r *http.Request, varyHeaders []string) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('|')
	b.WriteString(r.URL.Path)
	if r.URL.RawQuery != "" {
		b.WriteByte('?')
		b.WriteString(r.URL.RawQuery)
	}

	if len(varyHeaders) > 0 {
		sorted := make([]string, len(varyHeaders))
		copy(sorted, varyHeaders)
		sort.Strings(sorted)
		for _, h := range sorted {
			b.WriteByte('|')
			b.WriteString(h)
			b.WriteByte('=')
			b.WriteString(r.Header.Get(h))
		}
	}

	return b.String()
}

// ParseCacheControl extracts cache directives from a Cache-Control header value.
func ParseCacheControl(cc string) (maxAge time.Duration, noStore, noCache, isPrivate, isPublic bool) {
	for _, directive := range strings.Split(cc, ",") {
		directive = strings.TrimSpace(directive)
		lower := strings.ToLower(directive)

		switch {
		case strings.HasPrefix(lower, "max-age="):
			if after, found := strings.CutPrefix(lower, "max-age="); found {
				seconds, err := strconv.Atoi(strings.TrimSpace(after))
				if err == nil && seconds > 0 {
					maxAge = time.Duration(seconds) * time.Second
				}
			}
		case lower == "no-store":
			noStore = true
		case lower == "no-cache":
			noCache = true
		case lower == "private":
			isPrivate = true
		case lower == "public":
			isPublic = true
		}
	}

	return maxAge, noStore, noCache, isPrivate, isPublic
}

// IsCacheable returns true if the response should be cached based on its
// status code and Cache-Control header. Only 200 and 301 are cached.
func IsCacheable(statusCode int, header http.Header) (ttl time.Duration, ok bool) {
	if statusCode != http.StatusOK && statusCode != http.StatusMovedPermanently {
		return 0, false
	}
	cc := header.Get("Cache-Control")
	if cc == "" {
		return 0, false
	}
	maxAge, noStore, noCache, private, _ := ParseCacheControl(cc)
	if noStore || noCache || private {
		return 0, false
	}
	if maxAge <= 0 {
		return 0, false
	}
	return maxAge, true
}

// ParseVary extracts header names from a Vary response header.
// Returns nil for "Vary: *" (uncacheable).
func ParseVary(header http.Header) ([]string, bool) {
	vary := header.Get("Vary")
	if vary == "" {
		return nil, true
	}
	if vary == "*" {
		return nil, false
	}
	var headers []string
	for _, h := range strings.Split(vary, ",") {
		h = strings.TrimSpace(h)
		if h != "" {
			headers = append(headers, http.CanonicalHeaderKey(h))
		}
	}
	return headers, true
}

// ParseSurrogateKey extracts cache tags from the Surrogate-Key header.
func ParseSurrogateKey(header http.Header) []string {
	sk := header.Get("Surrogate-Key")
	if sk == "" {
		sk = header.Get("Cache-Tag")
	}
	if sk == "" {
		return nil
	}
	var tags []string
	for _, t := range strings.Split(sk, " ") {
		t = strings.TrimSpace(t)
		if t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

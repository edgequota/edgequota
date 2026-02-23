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
	"time"

	"github.com/edgequota/edgequota/internal/redis"
)

const (
	keyPrefix = "eq:cache:"
	tagPrefix = "eq:tags:"
)

// Entry is a cached HTTP response.
type Entry struct {
	StatusCode   int         `json:"status_code"`
	Headers      http.Header `json:"headers"`
	Body         []byte      `json:"body"`
	ETag         string      `json:"etag,omitempty"`
	LastModified string      `json:"last_modified,omitempty"`
	Vary         []string    `json:"vary,omitempty"`
	Tags         []string    `json:"tags,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
}

// Store is a response cache backed by Redis.
type Store struct {
	client      redis.Client
	maxBodySize int64
	logger      *slog.Logger

	OnHit      func()
	OnMiss     func()
	OnStaleHit func()
	OnStore    func()
	OnSkip     func()
	OnPurge    func()
	OnBodySize func(float64)
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
	return s
}

// MaxBodySize returns the configured maximum cacheable body size.
func (s *Store) MaxBodySize() int64 { return s.maxBodySize }

// Get retrieves a cached entry by key. Returns nil, false on miss.
func (s *Store) Get(ctx context.Context, key string) (*Entry, bool) {
	data, err := s.client.Get(ctx, keyPrefix+key).Bytes()
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

// Set stores an entry with the given TTL. Tags are indexed for
// tag-based purging. Entries with empty body or TTL <= 0 are skipped.
func (s *Store) Set(ctx context.Context, key string, entry *Entry, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		s.logger.Debug("cache: marshal error", "key", key, "error", err)
		return
	}
	_ = s.client.Set(ctx, keyPrefix+key, data, ttl).Err()

	for _, tag := range entry.Tags {
		_ = s.client.SAdd(ctx, tagPrefix+tag, key).Err()
	}

	if s.OnStore != nil {
		s.OnStore()
	}
	if s.OnBodySize != nil {
		s.OnBodySize(float64(len(entry.Body)))
	}
	s.logger.Debug("cache: stored", "key", key, "ttl", ttl, "body_size", len(entry.Body), "tags", entry.Tags)
}

// Delete removes a single cache entry by key.
func (s *Store) Delete(ctx context.Context, key string) bool {
	n, err := s.client.Del(ctx, keyPrefix+key).Result()
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
	if err != nil || len(members) == 0 {
		return 0
	}

	redisKeys := make([]string, 0, len(members))
	for _, m := range members {
		redisKeys = append(redisKeys, keyPrefix+m)
	}
	n, _ := s.client.Del(ctx, redisKeys...).Result()
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

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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edgequota/edgequota/internal/httphdr"
	"github.com/edgequota/edgequota/internal/redis"
)

const (
	keyPrefix = "eq:cache:"
	tagPrefix = "eq:tags:"
)

// growOnlyExpireLua bounds a tag set's lifetime without ever shrinking it: it
// sets the TTL when the key has none (PTTL < 0) and otherwise only raises it
// (new > current). One atomic round trip, and portable across Redis versions —
// EXPIRE's NX/GT flags need Redis 7, but PTTL/PEXPIRE are ancient.
// Keys: KEYS[1] = tag set key. Args: ARGV[1] = ttl in milliseconds.
const growOnlyExpireLua = `
local cur = redis.call('PTTL', KEYS[1])
local ms = tonumber(ARGV[1])
if cur < 0 or ms > cur then
  redis.call('PEXPIRE', KEYS[1], ms)
end
return 1
`

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
		tagKey := tagPrefix + tag
		tagErr := s.client.SAdd(ctx, tagKey, key).Err()
		s.noteRedis(tagErr)
		if tagErr != nil {
			s.logger.Debug("cache: tag index error", "key", key, "tag", tag, "error", tagErr)
			continue
		}
		// Bound the tag set's lifetime, GROW-ONLY. Without a TTL the set outlives
		// every entry it indexes and grows forever (members are added but never
		// removed). But the TTL must never shrink: entries sharing a tag can have
		// different TTLs, and if a shorter-lived one shortened the set, the set
		// would expire while a longer-lived member is still cached, silently
		// dropping that member from tag-based purge. growOnlyExpireLua sets the
		// TTL only if the set has none and otherwise only raises it, atomically
		// in one round trip, so TTL == the longest-lived member's. A failed
		// expire only risks the set living longer, so it must not discard the store.
		expErr := s.client.Eval(ctx, growOnlyExpireLua, []string{tagKey}, ttl.Milliseconds()).Err()
		s.noteRedis(expErr)
		if expErr != nil {
			s.logger.Debug("cache: tag expire error", "key", key, "tag", tag, "error", expErr)
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

// baseKey is the identity-free part of a cache key: method|EscapedPath?RawQuery.
// The path comes from EscapedPath, not the decoded Path: the reverse proxy
// forwards the escaped form, so distinct on-the-wire targets that decode to the
// same Path must not share a slot (an encoded "?" in "/a%3Fx=1" decodes to Path
// "/a?x=1", colliding with the genuine "/a?x=1"). KeyFromRequest and KeyFromURL
// both derive it here, so an admin purge-by-URL keys identically to the request
// that cached the entry.
func baseKey(method string, u *url.URL) string {
	var b strings.Builder
	b.WriteString(method)
	b.WriteByte('|')
	b.WriteString(u.EscapedPath())
	if u.RawQuery != "" {
		b.WriteByte('?')
		b.WriteString(u.RawQuery)
	}
	return b.String()
}

// KeyFromRequest computes a deterministic cache key from an HTTP request. The
// base key is method|EscapedPath?query (see baseKey); when varyHeaders is
// non-empty the listed request header values are appended, so a Vary'd response
// keys separately per variant. Without Vary headers the key is just the base key
// — request headers do not participate unless the origin lists them via Vary.
func (s *Store) KeyFromRequest(r *http.Request, varyHeaders []string) string {
	key := baseKey(r.Method, r.URL)
	if len(varyHeaders) == 0 {
		return key
	}
	var b strings.Builder
	b.WriteString(key)
	sorted := make([]string, len(varyHeaders))
	copy(sorted, varyHeaders)
	sort.Strings(sorted)
	for _, h := range sorted {
		b.WriteByte('|')
		b.WriteString(h)
		b.WriteByte('=')
		b.WriteString(r.Header.Get(h))
	}
	return b.String()
}

// KeyFromURL computes the base cache key (no Vary) for a method and request
// target, deriving it exactly as KeyFromRequest so an admin purge-by-URL matches
// the key a request produced. It uses url.ParseRequestURI — the same parser the
// net/http server applies to the request-target — because url.Parse disagrees on
// exactly the inputs this must unify: a "//host/path" target (url.Parse strips
// "//host" as an authority) and a literal "#" (url.Parse drops it as a fragment).
// rawURL must be the on-the-wire (escaped) target the reverse proxy forwards.
func (s *Store) KeyFromURL(method, rawURL string) (string, error) {
	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return "", err
	}
	return baseKey(method, u), nil
}

// CacheControl holds the response Cache-Control directives edgequota acts on.
// A directive with an argument (RFC 9111's qualified forms no-cache="field" and
// private="field") is treated exactly like its bare form: edgequota does not
// revalidate or strip named fields, so the safe reading is the restrictive one.
type CacheControl struct {
	MaxAge         time.Duration // max-age (0 when absent or non-positive)
	SMaxAge        time.Duration // s-maxage value; only meaningful when HasSMaxAge
	HasSMaxAge     bool          // s-maxage was present (its value may be 0)
	NoStore        bool
	NoCache        bool
	Private        bool
	Public         bool
	MustRevalidate bool
}

// splitDirectives splits a Cache-Control value into directives on commas that
// are not inside a quoted-string argument (RFC 9110 §5.6.4). A bare comma split
// would tear an opaque quoted value such as ext="a, max-age=99, b" into pieces
// and re-read its interior as real directives — caching a response that carries
// no valid directive at all.
func splitDirectives(cc string) []string {
	var out []string
	var b strings.Builder
	inQuotes := false
	escaped := false
	for _, r := range cc {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\' && inQuotes:
			b.WriteRune(r)
			escaped = true
		case r == '"':
			inQuotes = !inQuotes
			b.WriteRune(r)
		case r == ',' && !inQuotes:
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	out = append(out, b.String())
	return out
}

// ParseCacheControl parses a Cache-Control header value. Pass every field line
// joined with commas (see IsCacheable): RFC 9110 makes repeated Cache-Control
// lines one combined list, so a restrictive directive on a second line must not
// be dropped. Directives are split quoted-string-aware, so a comma inside a
// quoted argument stays with its directive.
func ParseCacheControl(cc string) CacheControl {
	var c CacheControl
	for _, directive := range splitDirectives(cc) {
		name, value, _ := strings.Cut(directive, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		// An argument may be a token or a quoted-string; we only ever look at
		// the numeric deltas, so strip surrounding quotes and ignore the rest.
		value = strings.Trim(strings.TrimSpace(value), `"`)

		switch name {
		case "max-age":
			if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
				c.MaxAge = time.Duration(seconds) * time.Second
			}
		case "s-maxage":
			// A shared cache honors s-maxage over max-age, and s-maxage=0 is a
			// valid instruction not to store, so record even a zero value.
			if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
				c.SMaxAge = time.Duration(seconds) * time.Second
				c.HasSMaxAge = true
			}
		case "no-store":
			c.NoStore = true
		case "no-cache":
			c.NoCache = true
		case "private":
			c.Private = true
		case "public":
			c.Public = true
		case "must-revalidate":
			c.MustRevalidate = true
		}
	}

	return c
}

// IsCacheable reports whether a response may be stored by this shared cache,
// given the request that produced it, its status code, and its headers. Only
// 200 and 301 are cached. A nil request means no request-identity constraints
// apply (the response-only rules still do).
//
// All Cache-Control field lines are considered, not just the first: an origin
// (or a middleware layered on it) may emit "public, max-age=N" from one layer
// and "no-store" from another, and the restrictive directive must win. Being a
// shared cache, edgequota honors s-maxage over max-age when present, so
// s-maxage=0 makes a response uncacheable even alongside a positive max-age.
//
// Two request/response-coupled rules keep one client's data from reaching
// another under edgequota's identity-free key:
//
//   - A response carrying Set-Cookie is never stored: the cookie is per-client
//     state, and replaying it from cache to every later hit is session
//     fixation. This holds whatever the Cache-Control says.
//   - RFC 9111 §3.5: a response to a request that carried an Authorization
//     header is stored only if the response explicitly permits shared reuse via
//     public, s-maxage, or must-revalidate. Otherwise an authenticated response
//     would be keyed without identity and served to other (even anonymous)
//     clients.
func IsCacheable(r *http.Request, statusCode int, header http.Header) (ttl time.Duration, ok bool) {
	if statusCode != http.StatusOK && statusCode != http.StatusMovedPermanently {
		return 0, false
	}
	if responseHasSetCookie(header) {
		return 0, false
	}
	lines := header.Values("Cache-Control")
	if len(lines) == 0 {
		return 0, false
	}
	cc := ParseCacheControl(strings.Join(lines, ","))
	if cc.NoStore || cc.NoCache || cc.Private {
		return 0, false
	}
	if r != nil && r.Header.Get(httphdr.Authorization) != "" &&
		!cc.Public && !cc.HasSMaxAge && !cc.MustRevalidate {
		return 0, false
	}
	ttl = cc.MaxAge
	if cc.HasSMaxAge {
		ttl = cc.SMaxAge
	}
	if ttl <= 0 {
		return 0, false
	}
	return ttl, true
}

// responseHasSetCookie reports whether the response carries a Set-Cookie header,
// including one delivered as an HTTP trailer. A trailer arrives after the
// response head is committed, so the eligibility veto in WriteHeader cannot see
// it; the store-time check in Finish uses this to keep the "never cache a
// Set-Cookie response" invariant true. httputil.ReverseProxy deposits an
// announced trailer under its canonical name and an unannounced one under an
// http.TrailerPrefix-prefixed key, so both forms are checked.
func responseHasSetCookie(header http.Header) bool {
	for name := range header {
		key := name
		if trimmed, ok := strings.CutPrefix(key, http.TrailerPrefix); ok {
			key = trimmed
		}
		if http.CanonicalHeaderKey(key) == "Set-Cookie" {
			return true
		}
	}
	return false
}

// ParseVary extracts header names from a response's Vary header(s). Every field
// line is read (RFC 9110 makes repeated Vary lines one combined list), and a "*"
// appearing anywhere makes the response uncacheable — dropping a second Vary
// line, or missing a "*" mixed with real names, would collapse variants and
// serve one client's response to another.
func ParseVary(header http.Header) ([]string, bool) {
	var headers []string
	for _, line := range header.Values("Vary") {
		for _, h := range strings.Split(line, ",") {
			h = strings.TrimSpace(h)
			if h == "" {
				continue
			}
			if h == "*" {
				return nil, false
			}
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

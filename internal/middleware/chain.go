// Package middleware implements the request processing pipeline for EdgeQuota.
// The middleware chain handles: authentication → rate limiting → proxying.
// Each stage is optional and configurable.
package middleware

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edgequota/edgequota/internal/auth"
	"github.com/edgequota/edgequota/internal/cache"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/events"
	"github.com/edgequota/edgequota/internal/mtls"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/edgequota/edgequota/internal/proxy"
	"github.com/edgequota/edgequota/internal/ratelimit"
	"github.com/edgequota/edgequota/internal/redis"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/semaphore"
)

var (
	tracer      = otel.Tracer("edgequota.middleware")
	redisTracer = otel.Tracer("edgequota.redis")
)

// requestIDHeader is the canonical HTTP header for request correlation.
const requestIDHeader = "X-Request-Id"

// maxRequestIDLen is the maximum allowed length for a client-supplied X-Request-Id.
const maxRequestIDLen = 128

// requestIDRng is a goroutine-safe CSPRNG seeded from crypto/rand.
// rand.New wraps ChaCha8 with internal synchronization so concurrent
// calls to Uint64 are safe without external locking.
var requestIDRng = func() *rand.Rand {
	var seed [32]byte
	if _, err := cryptorand.Read(seed[:]); err != nil {
		panic("failed to seed ChaCha8: " + err.Error())
	}
	return rand.New(rand.NewChaCha8(seed)) //nolint:gosec // ChaCha8 seeded from crypto/rand is cryptographically strong.
}()

// generateRequestID creates a 16-byte hex-encoded random ID (128 bits).
func generateRequestID() string {
	var buf [16]byte
	for i := 0; i < len(buf); i += 8 {
		v := requestIDRng.Uint64()
		binary.LittleEndian.PutUint64(buf[i:], v)
	}
	return hex.EncodeToString(buf[:])
}

// validRequestID checks that a client-supplied request ID is safe to propagate.
// Rejects IDs that are too long or contain non-printable / injection characters.
// Allowed characters: alphanumeric, hyphens, underscores, dots, colons.
func validRequestID(s string) bool {
	if len(s) == 0 || len(s) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == ':':
		default:
			return false
		}
	}
	return true
}

// injectionDenyHeaders is the set of hop-by-hop, forwarding, and security-
// sensitive headers that the auth service is NOT allowed to inject into
// upstream requests. This prevents request smuggling and IP/identity
// spoofing via a compromised or misconfigured auth service.
var injectionDenyHeaders = map[string]struct{}{
	"Host":                {},
	"Content-Length":      {},
	"Transfer-Encoding":   {},
	"Connection":          {},
	"Te":                  {},
	"Upgrade":             {},
	"Proxy-Authorization": {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Trailer":             {},
	"X-Forwarded-For":     {},
	"X-Forwarded-Host":    {},
	"X-Forwarded-Proto":   {},
	"X-Real-Ip":           {},
	"X-Request-Id":        {},

	// mTLS identity headers — always set from TLS state in the middleware chain.
	"X-Edgequota-Mtls":                      {},
	"X-Edgequota-Client-Fingerprint-Sha256": {},
	"X-Edgequota-Client-Serial":             {},
	"X-Edgequota-Client-Subject":            {},
}

// jsonErrorResponse is the structured error body returned by EdgeQuota.
type jsonErrorResponse struct {
	Error      string  `json:"error"`
	Message    string  `json:"message"`
	RetryAfter float64 `json:"retry_after,omitempty"`
	RequestID  string  `json:"request_id,omitempty"`
}

// writeJSONError writes a structured JSON error response. The Content-Type
// is set to application/json. Any existing rate-limit headers are preserved.
func writeJSONError(w http.ResponseWriter, code int, errType, message string, retryAfter float64) {
	resp := jsonErrorResponse{
		Error:      errType,
		Message:    message,
		RetryAfter: retryAfter,
		RequestID:  w.Header().Get(requestIDHeader),
	}
	body, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// cryptoRandFloat64 returns a cryptographically random float64 in [0, 1).
func cryptoRandFloat64() float64 {
	var buf [8]byte
	if _, err := cryptorand.Read(buf[:]); err != nil {
		return 0.5
	}
	return float64(binary.BigEndian.Uint64(buf[:])>>11) / (1 << 53)
}

// defaultProxyCloseTimeout caps how long SwapProxy waits for the old
// proxy's in-flight non-WebSocket requests to drain before forcibly
// releasing its backend transports. Long-lived chunked/SSE streams
// inside ServeHTTP are bounded by this value across a reload — five
// minutes covers the vast majority of legitimate long-poll / SSE
// sessions while preventing a hung backend from leaking transports
// indefinitely.
const defaultProxyCloseTimeout = 5 * time.Minute

// Default recovery backoff configuration.
var (
	defaultRecoveryBackoffBase = time.Second
	defaultRecoveryBackoffMax  = 60 * time.Second

	defaultBackoffJitter = func(d time.Duration) time.Duration {
		factor := 0.8 + cryptoRandFloat64()*0.4
		return time.Duration(float64(d) * factor)
	}
)

// maxTTL caps Redis key TTL to 7 days.
const maxTTL = 7 * 24 * 3600

// authCacheKeyPrefix is the Redis key prefix for cached auth responses.
const authCacheKeyPrefix = "auth:cache:"

// authCacheTagPrefix is the Redis key prefix for auth cache tag-to-keys sets.
const authCacheTagPrefix = "auth:tag:"

// Failure policy constants — re-exported from config for local readability.
const (
	policyPassthrough      = config.FailurePolicyPassThrough
	policyFailClosed       = config.FailurePolicyFailClosed
	policyInMemoryFallback = config.FailurePolicyInMemoryFallback
)

// Chain is the main request processing middleware. It chains authentication,
// rate limiting, and proxying into a single http.Handler.
type Chain struct {
	next    atomic.Pointer[http.Handler]  // swappable proxy for backend hot-reload
	cfg     atomic.Pointer[config.Config] // atomic: read by recoveryLoop without lock, written by Reload under mu
	logger  *slog.Logger
	metrics *observability.Metrics

	// authClient and externalRL are accessed from the hot path (ServeHTTP)
	// without holding mu, and swapped atomically during Reload. Using
	// atomic.Pointer avoids a data race between concurrent readers in
	// ServeHTTP and the writer in Reload.
	authClient atomic.Pointer[auth.Client]
	externalRL atomic.Pointer[ratelimit.ExternalClient]

	keyStrategy         atomic.Pointer[ratelimit.KeyStrategy]
	fallbackKeyStrategy atomic.Pointer[ratelimit.KeyStrategy]

	mu             sync.RWMutex
	limiter        *ratelimit.Limiter
	redisUnhealthy bool

	// cacheRedis is the Redis client used for caching external rate limit
	// responses. It is either a dedicated client (when cache_redis is
	// configured) or an alias to the main limiter's client (otherwise).
	// cacheRedisOwned distinguishes the two — only owned clients may be
	// Closed by the cache code paths; the alias must not be closed because
	// the limiter still owns it.
	cacheRedis      redis.Client
	cacheRedisOwned bool

	// authCacheRedis is the Redis client used to cache auth responses. It
	// reuses cacheRedis when available, falling back to the main limiter client.
	// Nil when no Redis is configured (auth caching silently disabled).
	//
	// Stored as atomic.Pointer because the auth cache is read on every
	// authenticated request and written by Reload — without atomicity those
	// can race and tear the interface header, causing a nil-deref panic.
	authCacheRedis atomic.Pointer[redis.Client]

	// responseCache is the CDN-style response cache store, backed by Redis.
	// When nil, response caching is disabled. Read on every cacheable
	// request and re-pointed by installResponseCache (NewChain + recovery
	// loop), so an atomic.Pointer keeps the swap race-free.
	responseCache atomic.Pointer[cache.Store]

	// responseCacheRedis is only touched by installResponseCache (under
	// c.mu) and Chain.Close (after cancel), so c.mu is sufficient.
	responseCacheRedis redis.Client

	// proxyRef holds a typed reference to the proxy for cache injection.
	proxyRef *proxy.Proxy

	globalBackstop *ratelimit.InMemoryLimiter // optional global RPS safety valve for passthrough mode

	// staticRL is the immutable static-rate-limit snapshot. Reload builds a
	// new value and atomically Stores it; hot-path readers Load the pointer
	// once and read all fields from the same snapshot, guaranteeing tuple
	// consistency (e.g. burst and ratePerSecond from the same generation).
	staticRL atomic.Pointer[staticParams]

	// externalFallback is the immutable fallback snapshot used when the
	// external rate-limit service is unavailable. Same atomic-swap pattern
	// as staticRL.
	externalFallback atomic.Pointer[fallbackParams]

	// requestTimeout, urlPolicy, and accessLog are read from ServeHTTP
	// (hot path) without holding mu, and written by Reload. Using atomic
	// types avoids a data race without adding lock contention.
	requestTimeout atomic.Value // time.Duration
	writeTimeout   atomic.Value // time.Duration — per-request write deadline for non-streaming responses
	urlPolicy      atomic.Value // proxy.BackendURLPolicy
	accessLog      atomic.Bool

	bypassPreflight atomic.Bool

	emitter        atomic.Pointer[events.Emitter]
	concurrencySem *semaphore.Weighted

	allowedInjectionHeaders map[string]struct{}

	ctx          context.Context
	cancel       context.CancelFunc
	reconnectMu  sync.Mutex
	reconnecting bool

	// tracingLevel controls the depth of OTel instrumentation:
	//   basic    – root span only (created by the otelhttp wrapper in server.go)
	//   external – + spans for auth and external RL HTTP calls (default)
	//   full     – + spans for every Redis operation
	tracingLevel config.TracingLevel

	// Per-instance backoff config for the recovery loop. Copied from
	// package-level defaults at construction; tests override these on
	// individual Chain instances to avoid data races with goroutines
	// from other tests reading the same values.
	// proxyCloseTimeout bounds how long SwapProxy waits for the old proxy
	// to drain in-flight requests before releasing its transports.
	// Defaults to defaultProxyCloseTimeout.
	proxyCloseTimeout time.Duration

	recoveryBackoffBase time.Duration
	recoveryBackoffMax  time.Duration
	backoffJitter       func(time.Duration) time.Duration
}

// rateLimitParams holds parsed rate limit configuration.
type rateLimitParams struct {
	ratePerSecond float64
	burst         int64
	ttl           int
	period        time.Duration
	failurePolicy config.FailurePolicy
	failureCode   int
}

// staticParams is an immutable snapshot of the static rate-limit configuration
// plus its dependent in-memory fallback limiter. Reload constructs a new
// value and atomically Stores it; readers Load once and treat the result as
// a coherent tuple — never mix-and-match fields across snapshots.
type staticParams struct {
	ratePerSecond float64
	burst         int64
	ttl           int
	prefix        string
	failurePolicy config.FailurePolicy
	failureCode   int
	average       int64

	// fallback is the in-memory limiter used when Redis is unavailable
	// during static rate limiting. Bundled with the scalars above so that
	// the limiter you call always matches the limits you log/return.
	fallback *ratelimit.InMemoryLimiter
}

// fallbackParams is an immutable snapshot of the external-RL fallback
// configuration and its in-memory limiter, used when the external rate-limit
// service is unavailable. Same atomic-swap discipline as staticParams.
type fallbackParams struct {
	ratePerSecond float64
	burst         int64
	ttl           int
	average       int64
	limiter       *ratelimit.InMemoryLimiter
}

// parseBucketParams computes rate-per-second, burst, TTL, and period from a
// set of average/burst/period values and an optional minTTL override.
func parseBucketParams(average, burst int64, periodStr, minTTL string) (ratePerSecond float64, resolvedBurst int64, ttl int, period time.Duration) {
	period, _ = time.ParseDuration(periodStr)
	if period <= 0 {
		period = time.Second
	}

	resolvedBurst = max(burst, 1)

	if average > 0 {
		ratePerSecond = float64(average) * float64(time.Second) / float64(period)
	}

	periodSec := int(math.Ceil(period.Seconds()))
	ttl = max(2, periodSec*2)
	if ratePerSecond > 0 && ratePerSecond < 1 {
		ttl = min(int(1/ratePerSecond)+2, maxTTL)
	}

	if minTTL != "" {
		if minTTLDur, err := time.ParseDuration(minTTL); err == nil && minTTLDur > 0 {
			ttl = max(ttl, int(math.Ceil(minTTLDur.Seconds())))
		}
	}

	return ratePerSecond, resolvedBurst, ttl, period
}

// parseRateLimitParams normalizes and computes derived rate limit parameters
// from the static bucket config and the top-level failure policy settings.
func parseRateLimitParams(cfg *config.RateLimitConfig) rateLimitParams {
	fp := cfg.FailurePolicy
	if fp == "" {
		fp = policyPassthrough
	}

	fc := cfg.FailureCode
	if fc == 0 {
		fc = 429
	}

	ratePerSecond, burst, ttl, period := parseBucketParams(
		cfg.Static.Average, cfg.Static.Burst, cfg.Static.Period, cfg.MinTTL,
	)

	return rateLimitParams{
		ratePerSecond: ratePerSecond,
		burst:         burst,
		ttl:           ttl,
		period:        period,
		failurePolicy: fp,
		failureCode:   fc,
	}
}

// NewChain creates the middleware chain with auth, rate limiting, and the given next handler.
// ChainOption configures optional Chain behavior. Used in tests to override
// defaults before any background goroutines are started.
type ChainOption func(*Chain)

// WithRecoveryBackoff overrides the recovery loop backoff parameters.
// This is intended for testing; production callers should use the defaults.
func WithRecoveryBackoff(base, maxBackoff time.Duration, jitter func(time.Duration) time.Duration) ChainOption {
	return func(c *Chain) {
		c.recoveryBackoffBase = base
		c.recoveryBackoffMax = maxBackoff
		c.backoffJitter = jitter
	}
}

func NewChain(
	parentCtx context.Context,
	next http.Handler,
	cfg *config.Config,
	logger *slog.Logger,
	metrics *observability.Metrics,
	opts ...ChainOption,
) (*Chain, error) {
	ctx, cancel := context.WithCancel(parentCtx)

	ks, err := ratelimit.NewKeyStrategy(cfg.RateLimit.Static.KeyStrategy)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("key strategy: %w", err)
	}

	p := parseRateLimitParams(&cfg.RateLimit)
	prefix := "rl:edgequota:"
	if cfg.RateLimit.KeyPrefix != "" {
		prefix = "rl:" + cfg.RateLimit.KeyPrefix + ":"
	}

	reqTimeout, _ := time.ParseDuration(cfg.Server.RequestTimeout)

	chain := &Chain{
		logger:              logger,
		metrics:             metrics,
		ctx:                 ctx,
		cancel:              cancel,
		proxyCloseTimeout:   defaultProxyCloseTimeout,
		recoveryBackoffBase: defaultRecoveryBackoffBase,
		recoveryBackoffMax:  defaultRecoveryBackoffMax,
		backoffJitter:       defaultBackoffJitter,
		tracingLevel:        cfg.Tracing.ResolvedLevel(),
	}
	chain.staticRL.Store(&staticParams{
		ratePerSecond: p.ratePerSecond,
		burst:         p.burst,
		ttl:           p.ttl,
		prefix:        prefix,
		failurePolicy: p.failurePolicy,
		failureCode:   p.failureCode,
		average:       cfg.RateLimit.Static.Average,
		fallback:      ratelimit.NewInMemoryLimiter(p.ratePerSecond, p.burst, time.Duration(p.ttl)*time.Second),
	})
	for _, o := range opts {
		o(chain)
	}
	chain.next.Store(&next)
	if pp, ok := next.(*proxy.Proxy); ok {
		chain.proxyRef = pp
	}
	chain.requestTimeout.Store(reqTimeout)
	chain.writeTimeout.Store(time.Duration(0))
	chain.urlPolicy.Store(proxy.BackendURLPolicy{
		AllowedSchemes:      cfg.Backend.URLPolicy.AllowedSchemes,
		DenyPrivateNetworks: cfg.Backend.URLPolicy.DenyPrivateNetworksEnabled(),
		AllowedHosts:        cfg.Backend.URLPolicy.AllowedHosts,
	})
	chain.keyStrategy.Store(&ks)
	if cfg.RateLimit.External.Enabled {
		fbKS, fbErr := ratelimit.NewKeyStrategy(cfg.RateLimit.External.Fallback.KeyStrategy)
		if fbErr != nil {
			cancel()
			return nil, fmt.Errorf("fallback key strategy: %w", fbErr)
		}
		chain.fallbackKeyStrategy.Store(&fbKS)
		fb := cfg.RateLimit.External.Fallback
		fbRPS, fbBurst, fbTTL, _ := parseBucketParams(fb.Average, fb.Burst, fb.Period, "")
		chain.externalFallback.Store(&fallbackParams{
			ratePerSecond: fbRPS,
			burst:         fbBurst,
			ttl:           fbTTL,
			average:       fb.Average,
			limiter:       ratelimit.NewInMemoryLimiter(fbRPS, fbBurst, time.Duration(fbTTL)*time.Second),
		})
		logger.Info("external RL fallback config ready",
			"fallback_average", fb.Average, "fallback_burst", fbBurst,
			"fallback_period", fb.Period, "fallback_backend_url", fb.BackendURL,
			"fallback_key_strategy", fb.KeyStrategy.Type)
	}
	chain.bypassPreflight.Store(cfg.Server.BypassPreflightAuth)
	chain.accessLog.Store(cfg.Logging.AccessLogEnabled())
	chain.cfg.Store(cfg)
	if cfg.RateLimit.GlobalPassthroughRPS > 0 {
		chain.globalBackstop = ratelimit.NewInMemoryLimiter(
			cfg.RateLimit.GlobalPassthroughRPS,
			max(int64(cfg.RateLimit.GlobalPassthroughRPS), 1),
			time.Minute,
		)
	}
	if cfg.Server.MaxConcurrentRequests > 0 {
		chain.concurrencySem = semaphore.NewWeighted(cfg.Server.MaxConcurrentRequests)
	}
	if len(cfg.Auth.AllowedInjectionHeaders) > 0 {
		chain.allowedInjectionHeaders = make(map[string]struct{}, len(cfg.Auth.AllowedInjectionHeaders))
		for _, h := range cfg.Auth.AllowedInjectionHeaders {
			chain.allowedInjectionHeaders[http.CanonicalHeaderKey(h)] = struct{}{}
		}
	}
	chain.emitter.Store(events.NewEmitter(cfg.Events, logger, metrics))

	if err := chain.initRedis(cfg, logger, p); err != nil {
		cancel()
		return nil, err
	}

	if err := chain.initClients(cfg, logger); err != nil {
		cancel()
		return nil, err
	}

	logger.Info("middleware chain ready",
		"average", cfg.RateLimit.Static.Average, "burst", p.burst,
		"period", p.period, "policy", p.failurePolicy)

	if p.failurePolicy == policyInMemoryFallback {
		logger.Warn("failure_policy is inmemoryfallback: rate limits are per-instance, "+
			"not globally consistent. With N replicas, effective burst is N * configured_burst",
			"burst", p.burst)
	}

	return chain, nil
}

func (c *Chain) initClients(cfg *config.Config, logger *slog.Logger) error {
	if cfg.Auth.Enabled {
		authClient, authErr := auth.NewClient(cfg.Auth, c.tracingLevel)
		if authErr != nil {
			return fmt.Errorf("auth client: %w", authErr)
		}
		c.authClient.Store(authClient)
		authCache := c.resolveCacheRedis(cfg, logger)
		c.storeAuthCacheRedis(authCache)
		if authCache != nil {
			logger.Info("authentication enabled with response caching",
				"cache_backend", cacheBackendName(authCache))
		} else {
			logger.Info("authentication enabled")
		}
	}

	if cfg.RateLimit.External.Enabled {
		cacheClient := c.resolveCacheRedis(cfg, logger)

		extClient, extErr := ratelimit.NewExternalClient(cfg.RateLimit.External, cacheClient, logger, c.tracingLevel)
		if extErr != nil {
			_ = c.releaseCacheRedis()
			return fmt.Errorf("external ratelimit client: %w", extErr)
		}
		extClient.SetMetricHooks(
			c.metrics.PromExtRLSemaphoreRejected.Inc,
			c.metrics.PromExtRLSingleflightShared.Inc,
		)
		extClient.SetCacheMetricHooks(
			c.metrics.PromExtRLCacheHit.Inc,
			c.metrics.PromExtRLCacheMiss.Inc,
			c.metrics.PromExtRLCacheStaleHit.Inc,
		)
		c.externalRL.Store(extClient)
		logger.Info("external rate limit service enabled",
			"cache_backend", cacheBackendName(cacheClient),
			"dedicated_cache_redis", cfg.CacheRedis != nil && len(cfg.CacheRedis.Endpoints) > 0)
	}

	if cfg.Cache.Enabled {
		c.initResponseCache(cfg, logger)
	}

	return nil
}

// initResponseCache creates the CDN-style response cache store backed by Redis.
func (c *Chain) initResponseCache(cfg *config.Config, logger *slog.Logger) {
	redisCfg := cfg.ResponseCacheRedisConfig()
	client, err := redis.NewClient(redisCfg)
	if err != nil {
		logger.Warn("response cache redis unavailable, retrying in background", "error", err)
		go c.responseCacheRecoveryLoop(cfg, logger, redisCfg)
		return
	}
	c.installResponseCache(cfg, logger, client)
}

func (c *Chain) installResponseCache(cfg *config.Config, logger *slog.Logger, client redis.Client) {
	c.mu.Lock()
	old := c.responseCacheRedis
	c.responseCacheRedis = client
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	maxBody := cfg.Cache.ParseMaxBodySize()
	store := cache.NewStore(client,
		cache.WithMaxBodySize(maxBody),
		cache.WithLogger(logger),
	)
	store.OnHit = c.metrics.PromRespCacheHit.Inc
	store.OnMiss = c.metrics.PromRespCacheMiss.Inc
	store.OnStaleHit = c.metrics.PromRespCacheStaleHit.Inc
	store.OnStore = c.metrics.PromRespCacheStore.Inc
	store.OnSkip = c.metrics.PromRespCacheSkip.Inc
	store.OnPurge = c.metrics.PromRespCachePurge.Inc
	store.OnBodySize = c.metrics.PromRespCacheBodySize.Observe

	c.responseCache.Store(store)
	if c.proxyRef != nil {
		c.proxyRef.SetCache(store)
	}
	logger.Info("response cache enabled",
		"max_body_size", maxBody,
		"dedicated_redis", cfg.ResponseCacheRedis != nil && len(cfg.ResponseCacheRedis.Endpoints) > 0)
}

func (c *Chain) responseCacheRecoveryLoop(
	cfg *config.Config, logger *slog.Logger, redisCfg config.RedisConfig,
) {
	backoff := c.recoveryBackoffBase
	attempt := 0
	for {
		if c.ctx.Err() != nil {
			return
		}

		sleep := c.backoffJitter(backoff)
		timer := time.NewTimer(sleep)
		select {
		case <-c.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, c.recoveryBackoffMax)

		attempt++
		client, err := redis.NewClient(redisCfg)
		if err != nil {
			if attempt <= 5 || attempt%10 == 0 {
				logger.Warn("response cache redis recovery attempt failed",
					"attempt", attempt, "error", err, "next_in", sleep)
			}
			continue
		}

		c.installResponseCache(cfg, logger, client)
		logger.Info("response cache redis recovered", "attempt", attempt)
		return
	}
}

// resolveCacheRedis returns a Redis client for caching external rate limit
// responses. If cache_redis is explicitly configured, a dedicated client is
// created and stored in c.cacheRedis with cacheRedisOwned=true so it can be
// closed separately. Otherwise, the main rate-limit Redis client is aliased
// with cacheRedisOwned=false — the alias must NOT be closed via c.cacheRedis,
// because the limiter still owns it.
//
// resolveCacheRedis is idempotent: the first call connects to (or falls back
// from) the dedicated cache_redis and stores the result in c.cacheRedis;
// every subsequent call returns that same client immediately.
func (c *Chain) resolveCacheRedis(cfg *config.Config, logger *slog.Logger) redis.Client {
	c.mu.RLock()
	existing := c.cacheRedis
	c.mu.RUnlock()
	if existing != nil {
		return existing
	}

	// Dedicated cache_redis configured — create a separate connection.
	if cfg.CacheRedis != nil && len(cfg.CacheRedis.Endpoints) > 0 {
		cacheClient, err := redis.NewClient(*cfg.CacheRedis)
		if err != nil {
			logger.Warn("dedicated cache redis unavailable, falling back to main redis",
				"error", err)
		} else {
			c.mu.Lock()
			c.cacheRedis = cacheClient
			c.cacheRedisOwned = true
			c.mu.Unlock()
			logger.Info("dedicated cache redis connected",
				"mode", cfg.CacheRedis.Mode,
				"endpoints", cfg.CacheRedis.Endpoints)
			return cacheClient
		}
	}

	// Fall back to the main rate-limit Redis client. This is a borrowed
	// reference; the limiter retains ownership and is responsible for Close.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limiter != nil {
		c.cacheRedis = c.limiter.Client()
		c.cacheRedisOwned = false
	}
	return c.cacheRedis
}

// releaseCacheRedis closes c.cacheRedis if and only if it is an owned
// dedicated client, then clears the pointer so resolveCacheRedis can
// re-establish it. Calling this on a borrowed alias is a no-op apart
// from clearing the pointer — the limiter retains ownership of the
// underlying client and is responsible for its Close.
//
// The pointer swap is done under c.mu to be race-free against
// recoveryInstall reading c.cacheRedis. The Close call itself is done
// outside the lock to avoid holding mu across a network round-trip.
func (c *Chain) releaseCacheRedis() error {
	c.mu.Lock()
	owned := c.cacheRedisOwned
	client := c.cacheRedis
	c.cacheRedis = nil
	c.cacheRedisOwned = false
	c.mu.Unlock()

	if owned && client != nil {
		return client.Close()
	}
	return nil
}

// loadAuthCacheRedis atomically reads the auth-cache redis client. Returns
// nil when auth caching is disabled or temporarily released across a
// reload.
func (c *Chain) loadAuthCacheRedis() redis.Client {
	ptr := c.authCacheRedis.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}

// storeAuthCacheRedis atomically replaces the auth-cache redis client.
// Pass nil to clear (auth caching becomes a no-op until a fresh client
// is stored). The Chain does not own this client — it is either an
// alias to a dedicated cache_redis or to the limiter's client, both of
// which are closed by their owners.
func (c *Chain) storeAuthCacheRedis(client redis.Client) {
	if client == nil {
		c.authCacheRedis.Store(nil)
		return
	}
	c.authCacheRedis.Store(&client)
}

func cacheBackendName(c redis.Client) string {
	if c != nil {
		return "redis"
	}
	return "none"
}

// authCacheKeyEphemeral is the set of headers that must not contribute to the
// auth cache key even though they pass through the auth header filter. These
// change on every request (tracing IDs, X-Request-Id, etc.) and would produce
// a unique cache key per request, defeating caching entirely.
var authCacheKeyEphemeral = func() map[string]struct{} {
	m := make(map[string]struct{}, len(config.DefaultEphemeralHeaders))
	for _, h := range config.DefaultEphemeralHeaders {
		m[strings.ToLower(h)] = struct{}{}
	}
	return m
}()

// authCacheKey returns a Redis key derived from the filtered CheckRequest —
// the same view of the request the auth service actually receives. Using the
// filtered headers (not r.Header) means:
//   - Headers stripped by DefaultAuthDenyHeaders (Cookie, etc.) do not affect
//     the key: requests that differ only in those headers share a cache entry.
//   - Method and path are included so path-specific auth decisions are isolated
//     (consistent with how the external RL service builds its lookup key).
//   - Ephemeral per-request headers (X-Request-Id, trace IDs, etc.) are
//     excluded so they don't produce a unique key on every request.
//
// Headers are SHA-256 hashed rather than stored plain because the filtered set
// still contains raw credential values (Authorization, X-Api-Key, etc.).
func authCacheKey(req *auth.CheckRequest) string {
	keys := make([]string, 0, len(req.Headers))
	for k := range req.Headers {
		if _, skip := authCacheKeyEphemeral[strings.ToLower(k)]; !skip {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	h := sha256.New()
	h.Write([]byte(req.Method))
	h.Write([]byte("|"))
	h.Write([]byte(req.Path))
	for _, k := range keys {
		h.Write([]byte("|"))
		h.Write([]byte(k))
		h.Write([]byte("="))
		h.Write([]byte(req.Headers[k]))
	}
	return authCacheKeyPrefix + hex.EncodeToString(h.Sum(nil))
}

// getAuthFromCache retrieves a cached auth response from Redis.
// Returns nil on cache miss or when Redis is unavailable.
// When tracingLevel is "full", the Redis GET is wrapped in an OTel span.
func (c *Chain) getAuthFromCache(ctx context.Context, key string) *auth.CheckResponse {
	client := c.loadAuthCacheRedis()
	if client == nil {
		return nil
	}
	if c.tracingLevel == config.TracingLevelFull {
		var span oteltrace.Span
		ctx, span = redisTracer.Start(ctx, "edgequota.redis.auth_cache_get",
			oteltrace.WithSpanKind(oteltrace.SpanKindClient),
			oteltrace.WithAttributes(attribute.String("db.system", "redis")),
		)
		defer span.End()
	}
	data, err := client.Get(ctx, key).Bytes()
	if err != nil {
		return nil
	}
	var resp auth.CheckResponse
	if json.Unmarshal(data, &resp) != nil {
		return nil
	}
	return &resp
}

// setAuthInCache stores an auth response in Redis with the given TTL.
// No-ops when Redis is unavailable or TTL is zero.
// When tracingLevel is "full", the Redis SET is wrapped in an OTel span.
func (c *Chain) setAuthInCache(ctx context.Context, key string, resp *auth.CheckResponse, ttl time.Duration) {
	client := c.loadAuthCacheRedis()
	if client == nil || ttl <= 0 {
		return
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if c.tracingLevel == config.TracingLevelFull {
		var span oteltrace.Span
		ctx, span = redisTracer.Start(ctx, "edgequota.redis.auth_cache_set",
			oteltrace.WithSpanKind(oteltrace.SpanKindClient),
			oteltrace.WithAttributes(attribute.String("db.system", "redis")),
		)
		defer span.End()
	}
	_ = client.Set(ctx, key, data, ttl).Err()

	// Index by surrogate-key tags for targeted purge.
	for _, tag := range resp.CacheTags {
		_ = client.SAdd(ctx, authCacheTagPrefix+tag, key).Err()
	}
}

// DeleteAuthByTag evicts all cached auth decisions tagged with the given tag.
// Returns the number of entries deleted.
func (c *Chain) DeleteAuthByTag(ctx context.Context, tag string) int {
	client := c.loadAuthCacheRedis()
	if client == nil {
		return 0
	}
	members, err := client.SMembers(ctx, authCacheTagPrefix+tag).Result()
	if err != nil || len(members) == 0 {
		return 0
	}
	n, _ := client.Del(ctx, members...).Result()
	_ = client.Del(ctx, authCacheTagPrefix+tag)
	return int(n)
}

func (c *Chain) initRedis(cfg *config.Config, logger *slog.Logger, p rateLimitParams) error {
	if cfg.RateLimit.Static.Average == 0 && !cfg.RateLimit.External.Enabled {
		logger.Info("rate limiting disabled (average=0)")
		return nil
	}

	client, redisErr := redis.NewClient(cfg.Redis)
	if redisErr != nil {
		return c.handleRedisStartupFailure(redisErr, p.failurePolicy, logger)
	}

	// Keep the Redis client and create a limiter whenever either static rate limiting
	// or external RL is active. In external RL mode (static.Average=0), the limiter
	// is created with ratePerSecond=0 so the Lua script's "rate <= 0 → always allow"
	// path is taken for the base Allow call, while AllowWithOverrides is used for
	// per-request limits supplied by the external service.
	if p.ratePerSecond > 0 || cfg.RateLimit.External.Enabled {
		c.limiter = ratelimit.NewLimiter(client, p.ratePerSecond, p.burst, p.ttl, c.staticRL.Load().prefix, logger, c.tracingLevel == config.TracingLevelFull)
	} else if client != nil {
		_ = client.Close()
	}

	return nil
}

func (c *Chain) handleRedisStartupFailure(err error, fp config.FailurePolicy, logger *slog.Logger) error {
	switch fp {
	case policyPassthrough, policyInMemoryFallback:
		logger.Warn("redis unavailable at startup, operating in fallback mode",
			"error", err, "policy", fp)
		c.startRecoveryIfNeeded()
		return nil
	default:
		return fmt.Errorf("redis connection failed: %w", err)
	}
}

// statusWriter captures the HTTP status code and response size written by
// downstream handlers.
type statusWriter struct {
	http.ResponseWriter
	code         int
	written      bool
	bytesWritten int64
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.written {
		sw.code = code
		sw.written = true
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.written {
		sw.code = http.StatusOK
		sw.written = true
	}
	n, err := sw.ResponseWriter.Write(b)
	sw.bytesWritten += int64(n)
	return n, err
}

// Unwrap supports http.ResponseController and middleware that check for
// underlying interfaces (http.Hijacker, http.Flusher, etc.).
func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

// Flush implements http.Flusher so that SSE streaming works even with
// middleware or handlers that assert w.(http.Flusher) directly instead of
// using Unwrap().
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// statusWriterPool amortizes statusWriter allocations on the hot path.
var statusWriterPool = sync.Pool{
	New: func() any { return &statusWriter{} },
}

// emitAccessLog writes a structured access log entry if access logging is enabled.
func (c *Chain) emitAccessLog(r *http.Request, sw *statusWriter, originalHost, reqID, rlKey, tenantID string, elapsed time.Duration) {
	if !c.accessLog.Load() {
		return
	}
	cfg := c.cfg.Load()
	resolvedBackendURL := cfg.RateLimit.Static.BackendURL
	if cfg.RateLimit.External.Enabled {
		resolvedBackendURL = cfg.RateLimit.External.Fallback.BackendURL
	}
	resolvedBackendProto := cfg.Backend.Transport.ResolvedBackendProtocol()
	if u := proxy.BackendURLFromContext(r.Context()); u != nil {
		resolvedBackendURL = u.String()
	}
	if p := proxy.BackendProtocolFromContext(r.Context()); p != "" {
		resolvedBackendProto = p
	}
	c.logger.Info("access",
		"method", r.Method,
		"host", originalHost,
		"path", r.URL.Path,
		"status", sw.code,
		"duration_ms", elapsed.Milliseconds(),
		"bytes", sw.bytesWritten,
		"remote_addr", r.RemoteAddr,
		"request_id", reqID,
		"user_agent", r.UserAgent(),
		"proto", r.Proto,
		"backend_url", resolvedBackendURL,
		"backend_proto", resolvedBackendProto,
		"rl_key", rlKey,
		"tenant_id", tenantID,
	)
}

// fetchExternalLimitsWithSpan wraps fetchExternalLimits with an OTel span
// when an external rate-limit service is configured. The span context is
// propagated into the request so that outgoing HTTP/gRPC calls to the
// external service appear as children of edgequota.external_rl.
func (c *Chain) fetchExternalLimitsWithSpan(r *http.Request, key string) (*ratelimit.ExternalLimits, string) {
	if c.externalRL.Load() != nil {
		ctx, extSpan := tracer.Start(r.Context(), "edgequota.external_rl")
		extStart := time.Now()
		limits, resolved := c.fetchExternalLimits(r.WithContext(ctx), key)
		c.metrics.PromExternalRLDuration.Observe(time.Since(extStart).Seconds())
		extSpan.End()
		return limits, resolved
	}
	return c.fetchExternalLimits(r, key)
}

// acquireConcurrency attempts to acquire the concurrency semaphore. Returns
// true if the request may proceed, false if the server is at capacity (503
// already written to sw).
func (c *Chain) acquireConcurrency(sw *statusWriter) bool {
	if c.concurrencySem == nil || c.concurrencySem.TryAcquire(1) {
		return true
	}
	c.metrics.PromConcurrencyRejected.Inc()
	sw.code = http.StatusServiceUnavailable
	writeJSONError(sw, http.StatusServiceUnavailable, "overloaded", "server at capacity", 0)
	return false
}

// extractTenantID returns the tenant portion of a resolved rate-limit key
// that uses the "t:<tenant>" convention.
func extractTenantID(resolvedKey string) string {
	if len(resolvedKey) > 2 && resolvedKey[:2] == "t:" {
		return resolvedKey[2:]
	}
	return ""
}

// externalLimitsFailClosed checks whether the external rate-limit service
// returned a fail-closed signal (Average < 0) and writes the error response.
func (c *Chain) externalLimitsFailClosed(sw *statusWriter, extLimits *ratelimit.ExternalLimits) bool {
	if extLimits == nil || extLimits.Average >= 0 {
		return false
	}
	code := c.staticRL.Load().failureCode
	if code == 0 {
		code = http.StatusServiceUnavailable
	}
	writeJSONError(sw, code, "rate_limited", "service unavailable (external RL fail-closed)", 0)
	return true
}

// runAuth executes the auth check with tracing if an auth client is
// configured. Returns false if the request was denied (response already
// written). The request is updated with the auth span's context so that
// downstream calls (gRPC/HTTP to the auth service) appear as children of
// the edgequota.auth span rather than siblings of it.
func (c *Chain) runAuth(sw *statusWriter, r *http.Request) bool {
	ac := c.authClient.Load()
	if ac == nil {
		return true
	}
	ctx, authSpan := tracer.Start(r.Context(), "edgequota.auth")
	authStart := time.Now()
	allowed := c.checkAuth(sw, r.WithContext(ctx), ac)
	c.metrics.PromAuthDuration.Observe(time.Since(authStart).Seconds())
	authSpan.End()
	return allowed
}

// ensureRequestID validates or generates an X-Request-Id for the request.
func ensureRequestID(r *http.Request) string {
	reqID := r.Header.Get(requestIDHeader)
	if !validRequestID(reqID) {
		reqID = generateRequestID()
		r.Header.Set(requestIDHeader, reqID)
	}
	return reqID
}

// isCORSPreflight returns true when r is a CORS preflight request as defined
// by the Fetch specification: an OPTIONS request carrying both Origin and
// Access-Control-Request-Method headers.
func isCORSPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions &&
		r.Header.Get("Origin") != "" &&
		r.Header.Get("Access-Control-Request-Method") != ""
}

// handlePreflightBypass skips auth and rate limiting for CORS preflight
// requests when bypass_preflight_auth is enabled. Returns true if the
// request was handled (caller should return early).
func (c *Chain) handlePreflightBypass(sw *statusWriter, r *http.Request, originalHost, reqID string, start time.Time) bool {
	if !c.bypassPreflight.Load() || !isCORSPreflight(r) {
		return false
	}
	c.metrics.IncAllowed()
	c.metrics.PromPreflightBypassed.Inc()
	backendStart := time.Now()
	(*c.next.Load()).ServeHTTP(sw, r)
	c.metrics.PromBackendDuration.Observe(time.Since(backendStart).Seconds())
	elapsed := time.Since(start)
	c.metrics.PromRequestsTotal.WithLabelValues(r.Method, observability.StatusFamily(sw.code)).Inc()
	c.metrics.PromRequestDuration.Observe(elapsed.Seconds())
	c.emitAccessLog(r, sw, originalHost, reqID, "", "", elapsed)
	sw.ResponseWriter = nil
	statusWriterPool.Put(sw)
	return true
}

// ServeHTTP processes the request through auth → rate limit → proxy.
// prepareRequest sets up the statusWriter, injects mTLS headers, records
// transport metrics, and returns the request ID and original host.
func (c *Chain) prepareRequest(w http.ResponseWriter, r *http.Request) (sw *statusWriter, reqID, originalHost string) {
	c.applyWriteDeadline(w, r)

	sw = statusWriterPool.Get().(*statusWriter)
	sw.ResponseWriter = w
	sw.code = http.StatusOK
	sw.written = false
	sw.bytesWritten = 0

	reqID = ensureRequestID(r)
	sw.Header().Set(requestIDHeader, reqID)

	mtls.InjectHeaders(r)
	c.metrics.PromRequestsByTransport.WithLabelValues(r.Proto, tlsMode(r)).Inc()

	originalHost = r.Host
	return sw, reqID, originalHost
}

func (c *Chain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.metrics.PromRequestsInFlight.Inc()
	defer c.metrics.PromRequestsInFlight.Dec()

	start := time.Now()
	sw, reqID, originalHost := c.prepareRequest(w, r)
	var logRLKey, logTenantID string

	if c.handlePreflightBypass(sw, r, originalHost, reqID, start) {
		return
	}

	if !c.acquireConcurrency(sw) {
		sw.ResponseWriter = nil
		statusWriterPool.Put(sw)
		return
	}

	proto := streamingProtocol(r)
	streaming := proto != ""
	if streaming {
		c.metrics.PromStreamingTotal.WithLabelValues(proto).Inc()
		c.metrics.PromStreamingInFlight.WithLabelValues(proto).Inc()
	}

	defer func() {
		if c.concurrencySem != nil {
			c.concurrencySem.Release(1)
		}
		elapsed := time.Since(start)
		c.metrics.PromRequestsTotal.WithLabelValues(r.Method, observability.StatusFamily(sw.code)).Inc()
		if streaming {
			c.metrics.PromStreamingInFlight.WithLabelValues(proto).Dec()
			c.metrics.PromStreamingDuration.WithLabelValues(proto).Observe(elapsed.Seconds())
		} else {
			c.metrics.PromRequestDuration.Observe(elapsed.Seconds())
		}
		c.emitAccessLog(r, sw, originalHost, reqID, logRLKey, logTenantID, elapsed)
		sw.ResponseWriter = nil
		statusWriterPool.Put(sw)
	}()

	if !c.runAuth(sw, r) {
		return
	}

	if c.staticRL.Load().average == 0 && c.externalRL.Load() == nil {
		c.metrics.IncAllowed()
		backendStart := time.Now()
		(*c.next.Load()).ServeHTTP(sw, r)
		if !streaming {
			c.metrics.PromBackendDuration.Observe(time.Since(backendStart).Seconds())
		}
		return
	}

	var key string
	if c.externalRL.Load() != nil {
		key = ""
	} else {
		var err error
		key, err = (*c.keyStrategy.Load()).Extract(r)
		if err != nil {
			c.metrics.IncKeyExtractErrors()
			c.logger.Warn("key extraction failed", "error", err)
			writeJSONError(sw, http.StatusInternalServerError, "key_extraction_failed", "could not extract rate-limit key", 0)
			return
		}
	}

	ctx := context.WithValue(r.Context(), ratelimit.KeyContextKey, key)
	r = r.WithContext(ctx)

	extLimits, resolvedKey := c.fetchExternalLimitsWithSpan(r, key)

	logRLKey = resolvedKey
	logTenantID = extractTenantID(resolvedKey)

	ctx, cancel := c.applyRequestTimeout(ctx, r, extLimits)
	if cancel != nil {
		defer cancel()
	}
	r = r.WithContext(ctx)

	r = c.injectBackendURL(r, extLimits)
	r = c.injectBackendProtocol(r, extLimits)

	if c.externalLimitsFailClosed(sw, extLimits) {
		return
	}

	if c.tryRedisLimit(sw, r, resolvedKey, extLimits) {
		return
	}

	c.handleRedisFailurePolicy(sw, r, resolvedKey, extLimits)
}

// SetWriteTimeout stores the per-request write deadline applied to
// non-streaming responses via http.ResponseController. Called by
// buildMainServer after parsing the config.
func (c *Chain) SetWriteTimeout(d time.Duration) {
	c.writeTimeout.Store(d)
}

// isStreamingRequest returns true for SSE, WebSocket upgrades, and gRPC
// streams — protocols that are inherently long-lived and must not be
// subject to the server's write timeout or per-request context deadline.
func isStreamingRequest(r *http.Request) bool {
	return proxy.IsSSE(r) || proxy.IsWebSocketUpgrade(r) || proxy.IsGRPC(r)
}

// tlsMode returns a low-cardinality label for the connection's TLS state.
func tlsMode(r *http.Request) string {
	if r.TLS == nil {
		return "plain"
	}
	if len(r.TLS.VerifiedChains) > 0 {
		return "mtls"
	}
	return "tls"
}

// streamingProtocol returns the protocol label for a streaming request,
// or "" if the request is not a streaming request.
func streamingProtocol(r *http.Request) string {
	switch {
	case proxy.IsSSE(r):
		return "sse"
	case proxy.IsWebSocketUpgrade(r):
		return "websocket"
	case proxy.IsGRPC(r):
		return "grpc"
	default:
		return ""
	}
}

// applyRequestTimeout returns a context with a timeout derived from the global
// config and optionally overridden by the external rate-limit response. The
// caller must defer the returned cancel func when non-nil.
// Streaming requests (SSE, WebSocket, gRPC) are exempt — they are long-lived
// by design and must not be killed by a request-scoped deadline.
func (c *Chain) applyRequestTimeout(ctx context.Context, r *http.Request, extLimits *ratelimit.ExternalLimits) (context.Context, context.CancelFunc) {
	if isStreamingRequest(r) {
		return ctx, nil
	}
	timeout := c.requestTimeout.Load().(time.Duration)
	if extLimits != nil && extLimits.RequestTimeout != "" {
		if d, parseErr := time.ParseDuration(extLimits.RequestTimeout); parseErr == nil && d > 0 {
			timeout = d
		}
	}
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return ctx, nil
}

// applyWriteDeadline sets a per-request write deadline on non-streaming
// responses using http.ResponseController. Streaming requests (SSE,
// WebSocket, gRPC) get an unlimited deadline so the server's idle timeout
// (not a hard wall-clock cap) governs their lifetime.
func (c *Chain) applyWriteDeadline(w http.ResponseWriter, r *http.Request) {
	wt, _ := c.writeTimeout.Load().(time.Duration)
	if wt <= 0 {
		return
	}
	rc := http.NewResponseController(w)
	if isStreamingRequest(r) {
		// Extend the deadline to effectively unlimited; the connection's
		// idle timeout and client-side close still apply.
		_ = rc.SetWriteDeadline(time.Time{})
	} else {
		_ = rc.SetWriteDeadline(time.Now().Add(wt))
	}
}

// injectBackendURL sets the per-request backend URL from the external service
// response into the request context. When extLimits is nil (static mode or
// fallback), no override is applied and the proxy uses the static/fallback URL.
// Parse or policy validation failures are logged and the request proceeds
// without a URL override (the proxy uses its startup default).
func (c *Chain) injectBackendURL(r *http.Request, extLimits *ratelimit.ExternalLimits) *http.Request {
	if extLimits == nil || extLimits.BackendURL == "" {
		return r
	}
	backendURL, parseErr := url.Parse(extLimits.BackendURL)
	if parseErr != nil {
		c.logger.Warn("invalid backend_url from external service, using default",
			"backend_url", extLimits.BackendURL, "error", parseErr)
		return r
	}
	if validErr := c.validateBackendURL(backendURL); validErr != nil {
		c.logger.Warn("backend_url from external service blocked by policy, using default",
			"backend_url", extLimits.BackendURL, "error", validErr)
		return r
	}
	return proxy.WithBackendURL(r, backendURL)
}

// injectBackendProtocol sets a per-request backend protocol in the request
// context when the external rate-limit service returns one. Invalid values
// are logged and ignored (the static config default is used instead).
func (c *Chain) injectBackendProtocol(r *http.Request, extLimits *ratelimit.ExternalLimits) *http.Request {
	if extLimits == nil || extLimits.BackendProtocol == "" {
		return r
	}
	proto := extLimits.BackendProtocol
	switch proto {
	case config.BackendProtocolH1, config.BackendProtocolH2, config.BackendProtocolH3:
		return proxy.WithBackendProtocol(r, proto)
	default:
		c.logger.Warn("invalid backend_protocol from external service, using default",
			"backend_protocol", proto)
		return r
	}
}

// checkAuth verifies the request with the external auth service.
// Returns true if allowed, false if denied (response already written).
// The auth client is passed explicitly to avoid a redundant atomic load
// (the caller already loaded it to check for nil).
// When allowed, any request_headers from the auth response are injected into
// the request so that downstream stages (rate limiting, backend) can read them.
// Responses with cache_max_age_seconds > 0 are stored in Redis keyed on a
// SHA-256 hash of the forwarded headers so subsequent requests carrying the
// same credential skip the external auth call entirely.
func (c *Chain) checkAuth(w http.ResponseWriter, r *http.Request, ac *auth.Client) bool {
	// When body propagation is enabled, buffer the body up to the configured
	// limit so it can be included in the auth request AND still forwarded to
	// the backend. The original r.Body is replaced with a re-readable reader.
	var bodyBytes []byte
	if ac.PropagateBody() && r.Body != nil && r.Body != http.NoBody {
		limited := io.LimitReader(r.Body, ac.MaxBodySize())
		var err error
		bodyBytes, err = io.ReadAll(limited)
		if err != nil {
			c.logger.Warn("failed to read request body for auth propagation", "error", err)
		}
		// Replace the body with ONLY the bytes that were buffered. The
		// remainder of the original body (beyond MaxBodySize) is discarded
		// to prevent forwarding an unbounded tail to the backend.
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	// Build the check request first so the cache key is derived from the same
	// filtered header set that the auth service actually receives. This ensures
	// that headers stripped by DefaultAuthDenyHeaders (Cookie, etc.) do not
	// pollute the key, and that method + path are included for path-specific
	// auth decisions.
	authReq := ac.BuildCheckRequest(r, bodyBytes)
	cacheKey := authCacheKey(authReq)

	// Cache hit: serve the previously cached auth decision without calling
	// the external service.
	if cached := c.getAuthFromCache(r.Context(), cacheKey); cached != nil {
		c.logger.Debug("auth cache hit, skipping external auth call")
		return c.applyAuthResponse(w, r, cached)
	}
	resp, err := ac.Check(r.Context(), authReq)
	if err != nil {
		c.logger.Error("auth service error", "error", err)
		c.metrics.IncAuthErrors()

		// Apply the configured auth failure policy.
		if c.cfg.Load().Auth.FailurePolicy == config.AuthFailurePolicyFailOpen {
			c.logger.Warn("auth service unreachable, allowing request (failure_policy=failopen)")
			return true
		}
		// Default: failclosed — reject the request.
		writeJSONError(w, http.StatusServiceUnavailable, "auth_unavailable", "authentication service unavailable", 0)
		return false
	}

	// Cache the response before applying it so subsequent requests carrying
	// the same credential skip the upstream auth call within the TTL.
	ttl := resp.ResolveCacheTTL()
	switch {
	case ttl > 0:
		c.logger.Debug("auth response cached",
			"ttl", ttl.String(),
			"source", resp.CacheTTLSource,
		)
		c.setAuthInCache(r.Context(), cacheKey, resp, ttl)
	case resp.CacheTTLSource != "":
		c.logger.Debug("auth response not cached",
			"source", resp.CacheTTLSource,
		)
	}

	return c.applyAuthResponse(w, r, resp)
}

// applyAuthResponse applies a (possibly cached) auth decision to the request.
// Writes the deny response and returns false when the request is denied,
// injects request_headers and returns true when the request is allowed.
func (c *Chain) applyAuthResponse(w http.ResponseWriter, r *http.Request, resp *auth.CheckResponse) bool {
	if !resp.Allowed {
		c.metrics.IncAuthDenied()
		for k, v := range resp.ResponseHeaders {
			w.Header().Set(k, v)
		}
		code := resp.StatusCode
		if code == 0 {
			code = http.StatusForbidden
		}
		denyMsg := resp.DenyBody
		if denyMsg == "" {
			denyMsg = http.StatusText(code)
		}
		writeJSONError(w, code, "auth_denied", denyMsg, 0)
		return false
	}

	// Inject auth-derived headers into the request. These overwrite any
	// client-sent headers with the same name, so the auth service is the
	// source of truth (e.g. for tenant ID decoded from a token).
	// Hop-by-hop and security-sensitive headers are blocked to prevent
	// request smuggling via a compromised or misconfigured auth service.
	for k, v := range resp.RequestHeaders {
		canonical := http.CanonicalHeaderKey(k)
		if _, denied := injectionDenyHeaders[canonical]; denied {
			if _, allowed := c.allowedInjectionHeaders[canonical]; !allowed {
				c.logger.Warn("auth service tried to inject denied header, skipping",
					"header", k)
				continue
			}
		}
		r.Header.Set(k, v)
	}

	return true
}

// maxTenantKeyLen is the maximum allowed length for a TenantKey from the
// external rate limit service. Keys exceeding this are rejected and the
// extracted key is used as fallback.
const maxTenantKeyLen = 256

// validTenantKey checks that a TenantKey from the external service is safe
// to use as a Redis key component and in log/metric labels. Allowed characters:
// alphanumeric, hyphens, underscores, dots, colons. Control characters and
// excessively long keys are rejected.
func validTenantKey(s string) bool {
	if len(s) == 0 || len(s) > maxTenantKeyLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == ':':
		default:
			return false
		}
	}
	return true
}

// fetchExternalLimits queries the external rate limit service if configured.
// Returns the dynamic limits and (optionally overridden) key. When the external
// service returns a tenant_key, it replaces the extracted key so that each tenant
// gets its own isolated Redis bucket. When limits are returned, they override the
// static config for this request.
func (c *Chain) fetchExternalLimits(r *http.Request, key string) (limits *ratelimit.ExternalLimits, resolvedKey string) {
	resolvedKey = key

	ext := c.externalRL.Load()
	if ext == nil {
		return nil, resolvedKey
	}

	headers := ext.BuildFilteredHeaders(r.Header)
	// Go promotes the Host header (and HTTP/2 :authority) to r.Host and
	// removes it from r.Header. Re-inject it so external rate-limit
	// services can make per-host decisions.
	if r.Host != "" {
		headers["Host"] = r.Host
	}

	extReq := &ratelimit.ExternalRequest{
		Headers: headers,
		Method:  r.Method,
		Path:    r.URL.Path,
	}

	extLimits, extErr := ext.GetLimits(r.Context(), extReq)
	if extErr != nil {
		cfg := c.cfg.Load()
		if cfg.RateLimit.External.FailurePolicy == config.ExternalRLFailurePolicyFailClosed {
			c.logger.Error("external rate limit service error, fail-closed", "error", extErr)
			return &ratelimit.ExternalLimits{Average: -1}, resolvedKey
		}
		// Fallback: use fallback key strategy and fallback limits.
		if fbKS := c.fallbackKeyStrategy.Load(); fbKS != nil {
			fbKey, fbErr := (*fbKS).Extract(r)
			if fbErr != nil {
				c.logger.Warn("fallback key extraction failed, using static config",
					"error", fbErr, "ext_error", extErr)
				return nil, resolvedKey
			}
			fb := c.cfg.Load().RateLimit.External.Fallback
			fbSnap := c.externalFallback.Load()
			c.logger.Warn("external rate limit service error, using fallback config",
				"error", extErr,
				"fallback_key", fbKey,
				"fallback_average", fbSnap.average,
				"fallback_burst", fbSnap.burst,
				"fallback_backend_url", fb.BackendURL)
			return &ratelimit.ExternalLimits{
				Average: fbSnap.average,
				Burst:   fbSnap.burst,
			}, fbKey
		}
		c.logger.Warn("external rate limit service error, using static config", "error", extErr)
		return nil, resolvedKey
	}

	// When external RL is active, the external service MUST return
	// backend_url — there is no implicit fallback to a static URL.
	// A missing backend_url is treated as a malformed response.
	if extLimits.BackendURL == "" {
		c.logger.Error("external service response missing required backend_url")
		cfg := c.cfg.Load()
		if cfg.RateLimit.External.FailurePolicy == config.ExternalRLFailurePolicyFailClosed {
			return &ratelimit.ExternalLimits{Average: -1}, resolvedKey
		}
		if fbKS := c.fallbackKeyStrategy.Load(); fbKS != nil {
			fbKey, fbErr := (*fbKS).Extract(r)
			if fbErr != nil {
				c.logger.Warn("fallback key extraction failed after missing backend_url", "error", fbErr)
				return nil, resolvedKey
			}
			fb := c.cfg.Load().RateLimit.External.Fallback
			fbSnap := c.externalFallback.Load()
			c.logger.Warn("external service response missing backend_url, using fallback config",
				"fallback_key", fbKey,
				"fallback_average", fbSnap.average,
				"fallback_burst", fbSnap.burst,
				"fallback_backend_url", fb.BackendURL)
			return &ratelimit.ExternalLimits{
				Average: fbSnap.average,
				Burst:   fbSnap.burst,
			}, fbKey
		}
		return nil, resolvedKey
	}

	// Use tenant key from external service for Redis bucket isolation.
	// The "t:" prefix ensures tenant-provided keys cannot collide with
	// the default "rl:edgequota:" prefixed keys.
	// TenantKey is validated: max 256 chars, alphanumeric + -_.:
	// Invalid keys are logged and the extracted key is used as fallback.
	if extLimits.TenantKey != "" {
		if validTenantKey(extLimits.TenantKey) {
			resolvedKey = "t:" + extLimits.TenantKey
		} else {
			c.logger.Warn("invalid tenant_key from external service, using extracted key as fallback",
				"tenant_key_len", len(extLimits.TenantKey),
				"max_len", maxTenantKeyLen)
			c.metrics.IncTenantKeyRejected()
			if em := c.emitter.Load(); em != nil {
				em.Emit(events.UsageEvent{
					Key:       resolvedKey,
					Method:    r.Method,
					Path:      r.URL.Path,
					Allowed:   true, // request still proceeds
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					RequestID: r.Header.Get(requestIDHeader),
					Reason:    "tenant_key_rejected",
				})
			}
		}
	}

	if extLimits.Average > 0 {
		return extLimits, resolvedKey
	}

	return nil, resolvedKey
}

// tryRedisLimit attempts to enforce rate limit via Redis.
// Returns true if the request was fully handled (allowed or denied), false if Redis is unavailable.
// When extLimits is non-nil, the dynamic per-tenant/per-key limits override the static config.
// The Redis check is wrapped in an edgequota.ratelimit span; edgequota.proxy
// then starts as a sibling (both children of edgequota.request).
func (c *Chain) tryRedisLimit(w http.ResponseWriter, r *http.Request, key string, extLimits *ratelimit.ExternalLimits) bool {
	c.mu.RLock()
	lim := c.limiter
	c.mu.RUnlock()

	if lim == nil {
		return false
	}

	// parentCtx holds the span context one level above (edgequota.request).
	// The ratelimit span is a child of it; the proxy span will also be a
	// child of it (sibling of ratelimit), not a child of ratelimit.
	parentCtx := r.Context()

	rlCtx, rlSpan := tracer.Start(parentCtx, "edgequota.ratelimit")

	var result *ratelimit.Result
	var limErr error

	if extLimits != nil && extLimits.Average > 0 {
		// Apply dynamic limits from external service.
		period, _ := time.ParseDuration(extLimits.Period)
		if period <= 0 {
			period = time.Second
		}
		ratePerSecond := float64(extLimits.Average) * float64(time.Second) / float64(period)
		burst := max(extLimits.Burst, 1)
		result, limErr = lim.AllowWithOverrides(rlCtx, key, ratePerSecond, burst, 0)
	} else {
		result, limErr = lim.Allow(rlCtx, key)
	}
	rlSpan.End()

	if limErr == nil {
		// Pass parentCtx so edgequota.proxy is a sibling of edgequota.ratelimit.
		c.serveResult(w, r.WithContext(parentCtx), key, result)
		return true
	}

	c.handleLimiterError(limErr)
	return false
}

func (c *Chain) handleLimiterError(limErr error) {
	c.metrics.IncRedisErrors("ratelimit")
	if redis.IsReadOnlyErr(limErr) {
		c.metrics.IncReadOnlyRetries()
	}

	if redis.IsConnectivityErr(limErr) {
		c.mu.Lock()
		old := c.swapLimiterLocked(nil)
		shouldLog := c.markUnhealthyLocked()
		c.mu.Unlock()

		if old != nil {
			_ = old.Close()
		}

		if shouldLog {
			c.metrics.PromRedisHealthy.WithLabelValues("ratelimit").Set(0)
			c.logger.Warn("redis became unhealthy, switching to fallback",
				"error", limErr, "policy", c.staticRL.Load().failurePolicy)
		}
		c.startRecoveryIfNeeded()
	}
}

func (c *Chain) serveResult(w http.ResponseWriter, r *http.Request, resolvedKey string, result *ratelimit.Result) {
	// Always attach rate limit headroom headers (allowed and denied).
	setRateLimitHeaders(w, result)

	// Record remaining tokens distribution for observability.
	c.metrics.ObserveRemaining(result.Remaining)

	// Emit per-tenant metrics when a tenant-namespaced key is in use
	// (indicated by the "t:" prefix applied in fetchExternalLimits).
	isTenantKey := len(resolvedKey) > 2 && resolvedKey[:2] == "t:"
	tenantID := ""
	if isTenantKey {
		tenantID = resolvedKey[2:]
	}

	if !result.Allowed {
		c.metrics.IncLimited()
		if isTenantKey {
			c.metrics.IncTenantLimited(tenantID)
		}
		c.emitUsageEvent(r, resolvedKey, tenantID, result, http.StatusTooManyRequests)
		c.serveRateLimited(w, result)
		return
	}
	c.metrics.IncAllowed()
	if isTenantKey {
		c.metrics.IncTenantAllowed(tenantID)
	}
	c.emitUsageEvent(r, resolvedKey, tenantID, result, http.StatusOK)
	ctx, proxySpan := tracer.Start(r.Context(), "edgequota.proxy")
	proxySpan.SetAttributes(
		attribute.String("tenant_key", tenantID),
		attribute.Bool("rate_limit.allowed", result.Allowed),
		attribute.Int64("rate_limit.remaining", result.Remaining),
	)
	backendStart := time.Now()
	(*c.next.Load()).ServeHTTP(w, r.WithContext(ctx))
	if !isStreamingRequest(r) {
		c.metrics.PromBackendDuration.Observe(time.Since(backendStart).Seconds())
	}
	proxySpan.End()
}

// emitUsageEvent enqueues a usage event to the events emitter (if configured).
// This is fire-and-forget and never blocks the request hot path.
func (c *Chain) emitUsageEvent(r *http.Request, key, tenantID string, result *ratelimit.Result, statusCode int) {
	em := c.emitter.Load()
	if em == nil {
		return
	}
	em.Emit(events.UsageEvent{
		Key:        key,
		TenantKey:  tenantID,
		Method:     r.Method,
		Path:       r.URL.Path,
		Allowed:    result.Allowed,
		Remaining:  result.Remaining,
		Limit:      result.Limit,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		StatusCode: statusCode,
		RequestID:  r.Header.Get(requestIDHeader),
	})
}

// setRateLimitHeaders writes standard rate-limit headers to every response.
// See https://datatracker.ietf.org/doc/draft-ietf-httpapi-ratelimit-headers/
func setRateLimitHeaders(w http.ResponseWriter, result *ratelimit.Result) {
	w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(result.Limit, 10))
	w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(result.Remaining, 10))

	resetSeconds := int64(math.Ceil(result.ResetAfter.Seconds()))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetSeconds, 10))
}

func (c *Chain) serveRateLimited(w http.ResponseWriter, result *ratelimit.Result) {
	// Apply +/-10% jitter to retry timing to prevent thundering herd and
	// avoid leaking precise token-bucket refill timing.
	jitterFactor := 0.9 + cryptoRandFloat64()*0.2 // [0.9, 1.1)
	retryDuration := time.Duration(float64(result.RetryAfter) * jitterFactor)
	retrySeconds := math.Ceil(retryDuration.Seconds())
	if retrySeconds < 1 {
		retrySeconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%.0f", retrySeconds))
	w.Header().Set("X-Retry-In", retryDuration.String())
	writeJSONError(w, http.StatusTooManyRequests, "rate_limited", "Too Many Requests", retrySeconds)
}

// resolveFailurePolicy returns the effective failure policy and failure code
// for this request. The external rate limit service can override both on a
// per-request basis (e.g. "passthrough" during planned maintenance,
// "failclosed" during an active attack). When no override is present, the
// static config values are used.
func (c *Chain) resolveFailurePolicy(extLimits *ratelimit.ExternalLimits) (config.FailurePolicy, int) {
	snap := c.staticRL.Load()
	fp := snap.failurePolicy
	fc := snap.failureCode

	if extLimits == nil {
		return fp, fc
	}

	if extLimits.FailurePolicy != "" {
		if extLimits.FailurePolicy.Valid() {
			fp = extLimits.FailurePolicy
			c.logger.Debug("failure policy overridden by external service",
				"static", snap.failurePolicy, "override", fp)
		} else {
			c.logger.Warn("external service returned invalid failure_policy, ignoring",
				"value", extLimits.FailurePolicy)
		}
	}

	if extLimits.FailureCode > 0 {
		fc = extLimits.FailureCode
	}

	return fp, fc
}

func (c *Chain) handleRedisFailurePolicy(w http.ResponseWriter, r *http.Request, key string, extLimits *ratelimit.ExternalLimits) {
	fp, fc := c.resolveFailurePolicy(extLimits)
	snap := c.staticRL.Load()

	switch fp {
	case policyPassthrough:
		if c.globalBackstop != nil && !c.globalBackstop.Allow("__global__") {
			c.metrics.IncLimited()
			c.serveRateLimited(w, &ratelimit.Result{RetryAfter: time.Second, Limit: snap.burst})
			return
		}
		c.metrics.IncAllowed()
		backendStart := time.Now()
		(*c.next.Load()).ServeHTTP(w, r)
		if !isStreamingRequest(r) {
			c.metrics.PromBackendDuration.Observe(time.Since(backendStart).Seconds())
		}

	case policyFailClosed:
		c.metrics.IncLimited()
		writeJSONError(w, fc, "service_unavailable", http.StatusText(fc), 0)

	case policyInMemoryFallback:
		c.metrics.IncFallbackUsed("ratelimit")
		if snap.fallback.Allow(key) {
			c.metrics.IncAllowed()
			c.logger.Debug("in-memory fallback: request allowed",
				"key", key, "average", snap.ratePerSecond, "burst", snap.burst)
			backendStart := time.Now()
			(*c.next.Load()).ServeHTTP(w, r)
			if !isStreamingRequest(r) {
				c.metrics.PromBackendDuration.Observe(time.Since(backendStart).Seconds())
			}
		} else {
			c.metrics.IncLimited()
			c.logger.Debug("in-memory fallback: request rate-limited",
				"key", key, "average", snap.ratePerSecond, "burst", snap.burst)
			c.serveRateLimited(w, &ratelimit.Result{
				RetryAfter: time.Second,
				Limit:      snap.burst,
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

func (c *Chain) startRecoveryIfNeeded() {
	if c.ctx.Err() != nil {
		return
	}

	c.reconnectMu.Lock()
	if c.reconnecting {
		c.reconnectMu.Unlock()
		return
	}
	c.reconnecting = true
	c.reconnectMu.Unlock()

	go func() {
		c.recoveryLoop()
		c.reconnectMu.Lock()
		c.reconnecting = false
		c.reconnectMu.Unlock()
	}()
}

func (c *Chain) recoveryLoop() {
	backoff := c.recoveryBackoffBase
	attempt := 0
	maxAttempts := c.cfg.Load().RateLimit.MaxRecoveryAttempts //nolint:staticcheck // Intentionally reading deprecated field for backward compatibility.

	if maxAttempts > 0 {
		c.logger.Warn("max_recovery_attempts is deprecated and will be removed; "+
			"Redis recovery now retries indefinitely with capped exponential backoff",
			"max_recovery_attempts", maxAttempts)
	}

	lastCfg := c.cfg.Load().Redis
	client, err := redis.NewClientWithoutPing(lastCfg)
	if err != nil {
		c.logger.Error("redis recovery: failed to create client", "error", err)
		return
	}

	for {
		if c.ctx.Err() != nil {
			_ = client.Close()
			return
		}

		// If Redis config changed (e.g. endpoints swapped during hot-reload),
		// recreate the client so it targets the new address.
		curCfg := c.cfg.Load().Redis
		if !sameRedisEndpoints(lastCfg, curCfg) {
			_ = client.Close()
			lastCfg = curCfg
			client, err = redis.NewClientWithoutPing(lastCfg)
			if err != nil {
				c.logger.Error("redis recovery: failed to recreate client after config change", "error", err)
				return
			}
			backoff = c.recoveryBackoffBase
		}

		pingCtx, pingCancel := context.WithTimeout(c.ctx, 5*time.Second)
		err := client.Ping(pingCtx).Err()
		pingCancel()

		if err == nil {
			c.recoveryInstall(client)
			return
		}

		attempt++
		if done := c.recoveryRetry(&backoff, attempt, maxAttempts, err); done {
			_ = client.Close()
			return
		}
	}
}

func sameRedisEndpoints(a, b config.RedisConfig) bool {
	if len(a.Endpoints) != len(b.Endpoints) {
		return false
	}
	for i, e := range a.Endpoints {
		if e != b.Endpoints[i] {
			return false
		}
	}
	return a.Mode == b.Mode && a.MasterName == b.MasterName
}

func (c *Chain) recoveryRetry(backoff *time.Duration, attempt, maxAttempts int, err error) (done bool) {
	if maxAttempts > 0 && attempt >= maxAttempts {
		c.logger.Error("redis recovery exhausted max attempts, giving up",
			"attempts", attempt, "max", maxAttempts, "last_error", err)
		return true
	}

	sleep := c.backoffJitter(*backoff)

	if attempt <= 5 || attempt%10 == 0 {
		c.logger.Warn("redis recovery attempt failed",
			"attempt", attempt, "error", err, "next_in", sleep)
	}

	timer := time.NewTimer(sleep)
	select {
	case <-c.ctx.Done():
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		return true
	case <-timer.C:
	}

	*backoff = min(*backoff*2, c.recoveryBackoffMax)
	return false
}

func (c *Chain) recoveryInstall(client redis.Client) {
	snap := c.staticRL.Load()
	// In external RL mode, a limiter is required even when ratePerSecond=0 so that
	// tryRedisLimit can apply per-request limits from the external service. Only skip
	// limiter creation when neither static rate limiting nor external RL is active.
	if snap.ratePerSecond <= 0 && !c.cfg.Load().RateLimit.External.Enabled {
		c.mu.Lock()
		old := c.swapLimiterLocked(nil)
		c.redisUnhealthy = false
		c.mu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		_ = client.Close()
		c.logger.Info("redis reachable (no limit configured)")
		return
	}

	limiter := ratelimit.NewLimiter(client, snap.ratePerSecond, snap.burst, snap.ttl, snap.prefix, c.logger, c.tracingLevel == config.TracingLevelFull)

	c.mu.Lock()
	old := c.swapLimiterLocked(limiter)
	c.redisUnhealthy = false
	c.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}

	// If the cache client is a borrowed alias to the limiter's redis (no
	// dedicated cache_redis), the swap above just closed the alias's
	// underlying connection. Re-point cacheRedis, authCacheRedis, and the
	// external RL client at the fresh limiter.Client() so cache reads
	// survive the recovery.
	newClient := limiter.Client()
	c.mu.Lock()
	rebindBorrowed := !c.cacheRedisOwned
	if rebindBorrowed {
		c.cacheRedis = newClient
	}
	c.mu.Unlock()
	if rebindBorrowed {
		c.storeAuthCacheRedis(newClient)
		if ext := c.externalRL.Load(); ext != nil {
			ext.SetRedisClient(newClient)
			c.logger.Info("external RL cache redis updated via main recovery")
		}
	}

	c.metrics.PromRedisHealthy.WithLabelValues("ratelimit").Set(1)
	c.logger.Info("redis connection recovered")
}

func (c *Chain) swapLimiterLocked(newLim *ratelimit.Limiter) redis.Client {
	var old redis.Client
	if c.limiter != nil {
		old = c.limiter.Client()
	}
	c.limiter = newLim
	return old
}

func (c *Chain) markUnhealthyLocked() bool {
	if c.redisUnhealthy {
		return false
	}
	c.redisUnhealthy = true
	return true
}

// redisPingerAdapter wraps a redis.Client to satisfy the observability.Pinger interface.
type redisPingerAdapter struct {
	client redis.Client
}

func (a *redisPingerAdapter) Ping(ctx context.Context) error {
	return a.client.Ping(ctx).Err()
}

// RedisPinger returns a Pinger that can probe the current Redis connection.
// Returns nil if no Redis limiter is configured. The pinger delegates to the
// underlying Redis client's Ping command. It's safe to call concurrently.
func (c *Chain) RedisPinger() observability.Pinger {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.limiter == nil {
		return nil
	}
	return &redisPingerAdapter{client: c.limiter.Client()}
}

// ResponseCache returns the current response cache store, or nil if
// response caching is disabled. Used by the admin PURGE endpoints.
func (c *Chain) ResponseCache() *cache.Store {
	return c.responseCache.Load()
}

// Close shuts down the middleware chain and releases all resources.
// Reload hot-swaps the rate-limit parameters, key strategy, failure policy,
// auth client, and external RL client from a new config. Components that
// haven't changed are not replaced.
func (c *Chain) Reload(newCfg *config.Config) error {
	p := parseRateLimitParams(&newCfg.RateLimit)
	prefix := "rl:edgequota:"
	if newCfg.RateLimit.KeyPrefix != "" {
		prefix = "rl:" + newCfg.RateLimit.KeyPrefix + ":"
	}

	// Rebuild key strategy if it changed.
	newKS, err := ratelimit.NewKeyStrategy(newCfg.RateLimit.Static.KeyStrategy)
	if err != nil {
		return fmt.Errorf("reload key strategy: %w", err)
	}

	c.keyStrategy.Store(&newKS) // Atomic store — read by ServeHTTP without holding mu.

	// Rebuild fallback key strategy and external-fallback snapshot when
	// external RL is enabled.
	var oldExternalFB *fallbackParams
	if newCfg.RateLimit.External.Enabled {
		fbKS, fbErr := ratelimit.NewKeyStrategy(newCfg.RateLimit.External.Fallback.KeyStrategy)
		if fbErr != nil {
			return fmt.Errorf("reload fallback key strategy: %w", fbErr)
		}
		c.fallbackKeyStrategy.Store(&fbKS)
		fb := newCfg.RateLimit.External.Fallback
		fbRPS, fbBurst, fbTTL, _ := parseBucketParams(fb.Average, fb.Burst, fb.Period, "")
		oldExternalFB = c.externalFallback.Swap(&fallbackParams{
			ratePerSecond: fbRPS,
			burst:         fbBurst,
			ttl:           fbTTL,
			average:       fb.Average,
			limiter:       ratelimit.NewInMemoryLimiter(fbRPS, fbBurst, time.Duration(fbTTL)*time.Second),
		})
	}

	// Build the new static-RL snapshot, including a fresh fallback limiter
	// sized to the new params. Swap it in atomically; Close the prior
	// fallback limiter after the swap and outside any lock.
	newStatic := &staticParams{
		ratePerSecond: p.ratePerSecond,
		burst:         p.burst,
		ttl:           p.ttl,
		prefix:        prefix,
		failurePolicy: p.failurePolicy,
		failureCode:   p.failureCode,
		average:       newCfg.RateLimit.Static.Average,
		fallback:      ratelimit.NewInMemoryLimiter(p.ratePerSecond, p.burst, time.Duration(p.ttl)*time.Second),
	}
	oldStatic := c.staticRL.Swap(newStatic)

	c.mu.Lock()
	prevCfg := c.cfg.Load() // Capture previous config before overwriting.
	c.cfg.Store(newCfg)     // Atomic store — recoveryLoop reads c.cfg without the mutex.

	// Rebuild Redis limiter with new params but same Redis client if available.
	if c.limiter != nil {
		oldLim := c.limiter
		c.limiter = ratelimit.NewLimiter(oldLim.Client(), p.ratePerSecond, p.burst, p.ttl, prefix, c.logger, c.tracingLevel == config.TracingLevelFull)
	}
	c.mu.Unlock()

	if oldStatic != nil && oldStatic.fallback != nil {
		oldStatic.fallback.Close()
	}
	if oldExternalFB != nil && oldExternalFB.limiter != nil {
		oldExternalFB.limiter.Close()
	}

	c.reloadExternalRL(newCfg)
	c.reloadAuth(prevCfg, newCfg)
	// Keep the auth cache pointed at the (possibly rebuilt) cache client.
	// reloadExternalRL closes and replaces c.cacheRedis when external RL is
	// enabled, so authCacheRedis must be re-read regardless of whether the
	// auth config itself changed. Atomic store: every authenticated request
	// reads authCacheRedis on the hot path.
	c.mu.RLock()
	cur := c.cacheRedis
	c.mu.RUnlock()
	c.storeAuthCacheRedis(cur)

	// Update URL policy (atomic — read from ServeHTTP without holding mu).
	c.urlPolicy.Store(proxy.BackendURLPolicy{
		AllowedSchemes:      newCfg.Backend.URLPolicy.AllowedSchemes,
		DenyPrivateNetworks: newCfg.Backend.URLPolicy.DenyPrivateNetworksEnabled(),
		AllowedHosts:        newCfg.Backend.URLPolicy.AllowedHosts,
	})

	// Update per-request timeout (atomic — read from ServeHTTP without holding mu).
	if d, parseErr := time.ParseDuration(newCfg.Server.RequestTimeout); parseErr == nil {
		c.requestTimeout.Store(d)
	}

	// Update write timeout for per-request deadline enforcement.
	if d, parseErr := config.ParseDuration(newCfg.Server.WriteTimeout, 30*time.Second); parseErr == nil {
		c.writeTimeout.Store(d)
	}

	c.bypassPreflight.Store(newCfg.Server.BypassPreflightAuth)
	c.accessLog.Store(newCfg.Logging.AccessLogEnabled())

	// Recreate emitter if events config changed.
	oldEmitter := c.emitter.Swap(events.NewEmitter(newCfg.Events, c.logger, c.metrics))
	if oldEmitter != nil {
		_ = oldEmitter.Close()
	}

	c.logger.Info("middleware chain reloaded",
		"average", newCfg.RateLimit.Static.Average, "burst", p.burst,
		"period", p.period, "policy", p.failurePolicy)

	return nil
}

// reloadAuth rebuilds the auth client only if the auth config actually
// changed. This avoids destroying the HTTP transport's connection pool on
// rate-limit-only reloads.
func (c *Chain) reloadAuth(prevCfg, newCfg *config.Config) {
	authChanged := prevCfg == nil ||
		newCfg.Auth.Enabled != prevCfg.Auth.Enabled ||
		newCfg.Auth.HTTP.URL != prevCfg.Auth.HTTP.URL ||
		newCfg.Auth.GRPC.Address != prevCfg.Auth.GRPC.Address ||
		newCfg.Auth.Timeout != prevCfg.Auth.Timeout ||
		newCfg.Auth.FailurePolicy != prevCfg.Auth.FailurePolicy

	if !newCfg.Auth.Enabled || !authChanged {
		return
	}
	newAuth, authErr := auth.NewClient(newCfg.Auth, c.tracingLevel)
	if authErr != nil {
		c.logger.Error("reload auth client failed, keeping old client", "error", authErr)
		return
	}
	oldAuth := c.authClient.Swap(newAuth)
	if oldAuth != nil {
		_ = oldAuth.Close()
	}
}

// reloadExternalRL rebuilds the external rate-limit client if enabled.
func (c *Chain) reloadExternalRL(newCfg *config.Config) {
	if !newCfg.RateLimit.External.Enabled {
		return
	}

	// Release the previous cache client before re-resolving. releaseCacheRedis
	// only Closes when the client was owned (dedicated cache_redis); when it
	// was a borrowed alias to the limiter's client the underlying connection
	// stays alive — the limiter retains ownership.
	_ = c.releaseCacheRedis()

	cacheClient := c.resolveCacheRedis(newCfg, c.logger)
	newExt, extErr := ratelimit.NewExternalClient(newCfg.RateLimit.External, cacheClient, c.logger, c.tracingLevel)
	if extErr != nil {
		c.logger.Error("reload external RL client failed, keeping old client", "error", extErr)
		return
	}
	newExt.SetMetricHooks(
		c.metrics.PromExtRLSemaphoreRejected.Inc,
		c.metrics.PromExtRLSingleflightShared.Inc,
	)
	newExt.SetCacheMetricHooks(
		c.metrics.PromExtRLCacheHit.Inc,
		c.metrics.PromExtRLCacheMiss.Inc,
		c.metrics.PromExtRLCacheStaleHit.Inc,
	)
	oldExt := c.externalRL.Swap(newExt)
	if oldExt != nil {
		_ = oldExt.Close()
	}
}

// validateBackendURL checks a dynamic backend URL against the configured policy.
func (c *Chain) validateBackendURL(u *url.URL) error {
	return proxy.ValidateBackendURL(u, c.urlPolicy.Load().(proxy.BackendURLPolicy))
}

// SwapProxy atomically replaces the downstream proxy handler. The
// previous proxy is released asynchronously: a background goroutine
// waits for its in-flight non-WebSocket requests to drain (up to
// c.proxyCloseTimeout) and then calls Close on its backend transports.
// Hijacked WebSocket sessions on the old proxy are not tracked and
// survive across the swap.
//
// Reload returns immediately — proxy teardown happens in the background.
func (c *Chain) SwapProxy(next *proxy.Proxy) {
	var handler http.Handler = next
	oldHandlerPtr := c.next.Swap(&handler)
	if oldHandlerPtr == nil {
		return
	}
	oldProxy, ok := (*oldHandlerPtr).(*proxy.Proxy)
	if !ok || oldProxy == nil {
		return
	}
	timeout := c.proxyCloseTimeout
	if timeout <= 0 {
		timeout = defaultProxyCloseTimeout
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, timeout)
		defer cancel()
		_ = oldProxy.Close(ctx)
	}()
}

func (c *Chain) Close() error {
	c.cancel()

	c.mu.Lock()
	old := c.swapLimiterLocked(nil)
	c.redisUnhealthy = true
	c.mu.Unlock()

	return c.closeResources(old)
}

func (c *Chain) closeResources(redisClient redis.Client) error {
	var firstErr error
	collect := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if redisClient != nil {
		collect(redisClient.Close())
	}
	// releaseCacheRedis is a no-op when the cache client is a borrowed
	// alias to the limiter — that connection is already closed via
	// redisClient above.
	collect(c.releaseCacheRedis())
	if c.responseCacheRedis != nil {
		collect(c.responseCacheRedis.Close())
	}
	if ac := c.authClient.Load(); ac != nil {
		collect(ac.Close())
	}
	if ext := c.externalRL.Load(); ext != nil {
		collect(ext.Close())
	}
	if snap := c.staticRL.Load(); snap != nil && snap.fallback != nil {
		snap.fallback.Close()
	}
	if fb := c.externalFallback.Load(); fb != nil && fb.limiter != nil {
		fb.limiter.Close()
	}
	if c.globalBackstop != nil {
		c.globalBackstop.Close()
	}
	if em := c.emitter.Load(); em != nil {
		collect(em.Close())
	}

	return firstErr
}

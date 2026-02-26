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
	"strings"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edgequota/edgequota/internal/auth"
	"github.com/edgequota/edgequota/internal/cache"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/events"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/edgequota/edgequota/internal/proxy"
	"github.com/edgequota/edgequota/internal/ratelimit"
	"github.com/edgequota/edgequota/internal/redis"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/semaphore"
)

var tracer = otel.Tracer("edgequota.middleware")

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

	// cacheRedis is a dedicated Redis client for caching external rate limit
	// responses. When cache_redis is configured, this is a separate connection;
	// otherwise it is nil and the main rate-limit Redis client is reused.
	cacheRedis redis.Client

	// authCacheRedis is the Redis client used to cache auth responses. It
	// reuses cacheRedis when available, falling back to the main limiter client.
	// Nil when no Redis is configured (auth caching silently disabled).
	authCacheRedis redis.Client

	// responseCache is the CDN-style response cache store, backed by Redis.
	// When nil, response caching is disabled.
	responseCache      *cache.Store
	responseCacheRedis redis.Client

	// proxyRef holds a typed reference to the proxy for cache injection.
	proxyRef *proxy.Proxy

	fallback       *ratelimit.InMemoryLimiter
	globalBackstop *ratelimit.InMemoryLimiter // optional global RPS safety valve for passthrough mode

	ratePerSecond float64
	burst         int64
	ttl           int
	prefix        string
	failurePolicy config.FailurePolicy
	failureCode   int
	average       int64

	fallbackRatePerSec float64
	fallbackBurst      int64
	fallbackTTL        int
	fallbackAverage    int64
	fallbackLimiter    *ratelimit.InMemoryLimiter

	// requestTimeout, urlPolicy, and accessLog are read from ServeHTTP
	// (hot path) without holding mu, and written by Reload. Using atomic
	// types avoids a data race without adding lock contention.
	requestTimeout atomic.Value // time.Duration
	writeTimeout   atomic.Value // time.Duration — per-request write deadline for non-streaming responses
	urlPolicy      atomic.Value // proxy.BackendURLPolicy
	accessLog      atomic.Bool

	emitter        *events.Emitter
	concurrencySem *semaphore.Weighted

	allowedInjectionHeaders map[string]struct{}

	ctx          context.Context
	cancel       context.CancelFunc
	reconnectMu  sync.Mutex
	reconnecting bool

	// Per-instance backoff config for the recovery loop. Copied from
	// package-level defaults at construction; tests override these on
	// individual Chain instances to avoid data races with goroutines
	// from other tests reading the same values.
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
		fallback:            ratelimit.NewInMemoryLimiter(p.ratePerSecond, p.burst, time.Duration(p.ttl)*time.Second),
		ratePerSecond:       p.ratePerSecond,
		burst:               p.burst,
		ttl:                 p.ttl,
		prefix:              prefix,
		failurePolicy:       p.failurePolicy,
		failureCode:         p.failureCode,
		average:             cfg.RateLimit.Static.Average,
		ctx:                 ctx,
		cancel:              cancel,
		recoveryBackoffBase: defaultRecoveryBackoffBase,
		recoveryBackoffMax:  defaultRecoveryBackoffMax,
		backoffJitter:       defaultBackoffJitter,
	}
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
		chain.fallbackRatePerSec = fbRPS
		chain.fallbackBurst = fbBurst
		chain.fallbackTTL = fbTTL
		chain.fallbackAverage = fb.Average
		chain.fallbackLimiter = ratelimit.NewInMemoryLimiter(fbRPS, fbBurst, time.Duration(fbTTL)*time.Second)
		logger.Info("external RL fallback config ready",
			"fallback_average", fb.Average, "fallback_burst", fbBurst,
			"fallback_period", fb.Period, "fallback_backend_url", fb.BackendURL,
			"fallback_key_strategy", fb.KeyStrategy.Type)
	}
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
	chain.emitter = events.NewEmitter(cfg.Events, logger, metrics)

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
		authClient, authErr := auth.NewClient(cfg.Auth)
		if authErr != nil {
			return fmt.Errorf("auth client: %w", authErr)
		}
		c.authClient.Store(authClient)
		c.authCacheRedis = c.resolveCacheRedis(cfg, logger)
		if c.authCacheRedis != nil {
			logger.Info("authentication enabled with response caching",
				"cache_backend", cacheBackendName(c.authCacheRedis))
		} else {
			logger.Info("authentication enabled")
		}
	}

	if cfg.RateLimit.External.Enabled {
		cacheClient := c.resolveCacheRedis(cfg, logger)

		extClient, extErr := ratelimit.NewExternalClient(cfg.RateLimit.External, cacheClient, logger)
		if extErr != nil {
			if c.cacheRedis != nil {
				_ = c.cacheRedis.Close()
				c.cacheRedis = nil
			}
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
	c.responseCacheRedis = client

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

	c.responseCache = store
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
// created and stored in c.cacheRedis so it can be closed separately. Otherwise,
// the main rate-limit Redis client is reused.
// resolveCacheRedis is idempotent: the first call connects to (or falls back
// from) the dedicated cache_redis and stores the result in c.cacheRedis;
// every subsequent call returns that same client immediately.
func (c *Chain) resolveCacheRedis(cfg *config.Config, logger *slog.Logger) redis.Client {
	if c.cacheRedis != nil {
		return c.cacheRedis
	}

	// Dedicated cache_redis configured — create a separate connection.
	if cfg.CacheRedis != nil && len(cfg.CacheRedis.Endpoints) > 0 {
		cacheClient, err := redis.NewClient(*cfg.CacheRedis)
		if err != nil {
			logger.Warn("dedicated cache redis unavailable, falling back to main redis",
				"error", err)
		} else {
			c.cacheRedis = cacheClient
			logger.Info("dedicated cache redis connected",
				"mode", cfg.CacheRedis.Mode,
				"endpoints", cfg.CacheRedis.Endpoints)
			return c.cacheRedis
		}
	}

	// Fall back to the main rate-limit Redis client.
	c.mu.RLock()
	if c.limiter != nil {
		c.cacheRedis = c.limiter.Client()
	}
	c.mu.RUnlock()

	return c.cacheRedis
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
func (c *Chain) getAuthFromCache(ctx context.Context, key string) *auth.CheckResponse {
	if c.authCacheRedis == nil {
		return nil
	}
	data, err := c.authCacheRedis.Get(ctx, key).Bytes()
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
func (c *Chain) setAuthInCache(ctx context.Context, key string, resp *auth.CheckResponse, ttl time.Duration) {
	if c.authCacheRedis == nil || ttl <= 0 {
		return
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_ = c.authCacheRedis.Set(ctx, key, data, ttl).Err()
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
		c.limiter = ratelimit.NewLimiter(client, p.ratePerSecond, p.burst, p.ttl, c.prefix, logger)
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
// when an external rate-limit service is configured.
func (c *Chain) fetchExternalLimitsWithSpan(r *http.Request, key string) (*ratelimit.ExternalLimits, string) {
	if c.externalRL.Load() != nil {
		_, extSpan := tracer.Start(r.Context(), "edgequota.external_rl")
		extStart := time.Now()
		limits, resolved := c.fetchExternalLimits(r, key)
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
	code := c.failureCode
	if code == 0 {
		code = http.StatusServiceUnavailable
	}
	writeJSONError(sw, code, "rate_limited", "service unavailable (external RL fail-closed)", 0)
	return true
}

// runAuth executes the auth check with tracing if an auth client is
// configured. Returns false if the request was denied (response already
// written).
func (c *Chain) runAuth(sw *statusWriter, r *http.Request) bool {
	ac := c.authClient.Load()
	if ac == nil {
		return true
	}
	_, authSpan := tracer.Start(r.Context(), "edgequota.auth")
	authStart := time.Now()
	allowed := c.checkAuth(sw, r, ac)
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

// ServeHTTP processes the request through auth → rate limit → proxy.
func (c *Chain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Apply per-request write deadline early, before any response bytes.
	// Streaming requests (SSE, WebSocket, gRPC) get an unlimited deadline;
	// regular requests get the configured write_timeout.
	c.applyWriteDeadline(w, r)

	sw := statusWriterPool.Get().(*statusWriter)
	sw.ResponseWriter = w
	sw.code = http.StatusOK
	sw.written = false
	sw.bytesWritten = 0

	reqID := ensureRequestID(r)
	sw.Header().Set(requestIDHeader, reqID)

	originalHost := r.Host
	var logRLKey, logTenantID string

	if !c.acquireConcurrency(sw) {
		sw.ResponseWriter = nil
		statusWriterPool.Put(sw)
		return
	}

	defer func() {
		if c.concurrencySem != nil {
			c.concurrencySem.Release(1)
		}
		elapsed := time.Since(start)
		c.metrics.PromRequestDuration.WithLabelValues(
			r.Method,
			strconv.Itoa(sw.code),
		).Observe(elapsed.Seconds())
		c.emitAccessLog(r, sw, originalHost, reqID, logRLKey, logTenantID, elapsed)
		sw.ResponseWriter = nil
		statusWriterPool.Put(sw)
	}()

	if !c.runAuth(sw, r) {
		return
	}

	if c.average == 0 && c.externalRL.Load() == nil {
		c.metrics.IncAllowed()
		backendStart := time.Now()
		(*c.next.Load()).ServeHTTP(sw, r)
		c.metrics.PromBackendDuration.Observe(time.Since(backendStart).Seconds())
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
			c.logger.Warn("external rate limit service error, using fallback config",
				"error", extErr,
				"fallback_key", fbKey,
				"fallback_average", c.fallbackAverage,
				"fallback_burst", c.fallbackBurst,
				"fallback_backend_url", fb.BackendURL)
			return &ratelimit.ExternalLimits{
				Average: c.fallbackAverage,
				Burst:   c.fallbackBurst,
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
			c.logger.Warn("external service response missing backend_url, using fallback config",
				"fallback_key", fbKey,
				"fallback_average", c.fallbackAverage,
				"fallback_burst", c.fallbackBurst,
				"fallback_backend_url", fb.BackendURL)
			return &ratelimit.ExternalLimits{
				Average: c.fallbackAverage,
				Burst:   c.fallbackBurst,
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
			if c.emitter != nil {
				c.emitter.Emit(events.UsageEvent{
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
func (c *Chain) tryRedisLimit(w http.ResponseWriter, r *http.Request, key string, extLimits *ratelimit.ExternalLimits) bool {
	c.mu.RLock()
	lim := c.limiter
	c.mu.RUnlock()

	if lim == nil {
		return false
	}

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
		result, limErr = lim.AllowWithOverrides(r.Context(), key, ratePerSecond, burst, 0)
	} else {
		result, limErr = lim.Allow(r.Context(), key)
	}

	if limErr == nil {
		c.serveResult(w, r, key, result)
		return true
	}

	c.handleLimiterError(limErr)
	return false
}

func (c *Chain) handleLimiterError(limErr error) {
	c.metrics.IncRedisErrors()
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
			c.metrics.PromRedisHealthy.Set(0)
			c.logger.Warn("redis became unhealthy, switching to fallback",
				"error", limErr, "policy", c.failurePolicy)
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
	c.metrics.PromBackendDuration.Observe(time.Since(backendStart).Seconds())
	proxySpan.End()
}

// emitUsageEvent enqueues a usage event to the events emitter (if configured).
// This is fire-and-forget and never blocks the request hot path.
func (c *Chain) emitUsageEvent(r *http.Request, key, tenantID string, result *ratelimit.Result, statusCode int) {
	if c.emitter == nil {
		return
	}
	c.emitter.Emit(events.UsageEvent{
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
	fp := c.failurePolicy
	fc := c.failureCode

	if extLimits == nil {
		return fp, fc
	}

	if extLimits.FailurePolicy != "" {
		if extLimits.FailurePolicy.Valid() {
			fp = extLimits.FailurePolicy
			c.logger.Debug("failure policy overridden by external service",
				"static", c.failurePolicy, "override", fp)
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

	switch fp {
	case policyPassthrough:
		if c.globalBackstop != nil && !c.globalBackstop.Allow("__global__") {
			c.metrics.IncLimited()
			c.serveRateLimited(w, &ratelimit.Result{RetryAfter: time.Second, Limit: c.burst})
			return
		}
		c.metrics.IncAllowed()
		backendStart := time.Now()
		(*c.next.Load()).ServeHTTP(w, r)
		c.metrics.PromBackendDuration.Observe(time.Since(backendStart).Seconds())

	case policyFailClosed:
		c.metrics.IncLimited()
		writeJSONError(w, fc, "service_unavailable", http.StatusText(fc), 0)

	case policyInMemoryFallback:
		c.metrics.IncFallbackUsed()
		if c.fallback.Allow(key) {
			c.metrics.IncAllowed()
			c.logger.Debug("in-memory fallback: request allowed",
				"key", key, "average", c.ratePerSecond, "burst", c.burst)
			backendStart := time.Now()
			(*c.next.Load()).ServeHTTP(w, r)
			c.metrics.PromBackendDuration.Observe(time.Since(backendStart).Seconds())
		} else {
			c.metrics.IncLimited()
			c.logger.Debug("in-memory fallback: request rate-limited",
				"key", key, "average", c.ratePerSecond, "burst", c.burst)
			c.serveRateLimited(w, &ratelimit.Result{
				RetryAfter: time.Second,
				Limit:      c.burst,
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
	// In external RL mode, a limiter is required even when ratePerSecond=0 so that
	// tryRedisLimit can apply per-request limits from the external service. Only skip
	// limiter creation when neither static rate limiting nor external RL is active.
	if c.ratePerSecond <= 0 && !c.cfg.Load().RateLimit.External.Enabled {
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

	limiter := ratelimit.NewLimiter(client, c.ratePerSecond, c.burst, c.ttl, c.prefix, c.logger)

	c.mu.Lock()
	old := c.swapLimiterLocked(limiter)
	c.redisUnhealthy = false
	c.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}

	// When the external RL client uses the main Redis for caching (no
	// dedicated cache_redis), inject the recovered client so ext RL caching
	// starts working even if Redis was unavailable at startup.
	if c.cacheRedis == nil {
		if ext := c.externalRL.Load(); ext != nil {
			ext.SetRedisClient(limiter.Client())
			c.logger.Info("external RL cache redis updated via main recovery")
		}
	}

	c.metrics.PromRedisHealthy.Set(1)
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
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.responseCache
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

	// Rebuild fallback key strategy when external RL is enabled.
	if newCfg.RateLimit.External.Enabled {
		fbKS, fbErr := ratelimit.NewKeyStrategy(newCfg.RateLimit.External.Fallback.KeyStrategy)
		if fbErr != nil {
			return fmt.Errorf("reload fallback key strategy: %w", fbErr)
		}
		c.fallbackKeyStrategy.Store(&fbKS)
		fb := newCfg.RateLimit.External.Fallback
		fbRPS, fbBurst, fbTTL, _ := parseBucketParams(fb.Average, fb.Burst, fb.Period, "")
		c.fallbackRatePerSec = fbRPS
		c.fallbackBurst = fbBurst
		c.fallbackTTL = fbTTL
		c.fallbackAverage = fb.Average
		oldFBL := c.fallbackLimiter
		c.fallbackLimiter = ratelimit.NewInMemoryLimiter(fbRPS, fbBurst, time.Duration(fbTTL)*time.Second)
		if oldFBL != nil {
			oldFBL.Close()
		}
	}

	c.mu.Lock()
	prevCfg := c.cfg.Load() // Capture previous config before overwriting.
	c.ratePerSecond = p.ratePerSecond
	c.burst = p.burst
	c.ttl = p.ttl
	c.prefix = prefix
	c.failurePolicy = p.failurePolicy
	c.failureCode = p.failureCode
	c.average = newCfg.RateLimit.Static.Average
	c.cfg.Store(newCfg) // Atomic store — recoveryLoop reads c.cfg without the mutex.

	// Rebuild limiter with new params but same Redis client if available.
	if c.limiter != nil {
		oldLim := c.limiter
		c.limiter = ratelimit.NewLimiter(oldLim.Client(), p.ratePerSecond, p.burst, p.ttl, prefix, c.logger)
	}

	// Rebuild fallback limiter with new parameters.
	oldFB := c.fallback
	c.fallback = ratelimit.NewInMemoryLimiter(p.ratePerSecond, p.burst, time.Duration(p.ttl)*time.Second)
	c.mu.Unlock()

	if oldFB != nil {
		oldFB.Close()
	}

	c.reloadAuth(prevCfg, newCfg)
	c.reloadExternalRL(newCfg)

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

	c.accessLog.Store(newCfg.Logging.AccessLogEnabled())

	// Recreate emitter if events config changed.
	oldEmitter := c.emitter
	c.emitter = events.NewEmitter(newCfg.Events, c.logger, c.metrics)
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
	newAuth, authErr := auth.NewClient(newCfg.Auth)
	if authErr != nil {
		c.logger.Error("reload auth client failed, keeping old client", "error", authErr)
		return
	}
	oldAuth := c.authClient.Swap(newAuth)
	if oldAuth != nil {
		_ = oldAuth.Close()
	}
	c.authCacheRedis = c.cacheRedis
}

// reloadExternalRL rebuilds the external rate-limit client if enabled.
func (c *Chain) reloadExternalRL(newCfg *config.Config) {
	if !newCfg.RateLimit.External.Enabled {
		return
	}

	// Close the previous dedicated cache Redis client (if any) before
	// creating a new one. This prevents connection leaks across reloads.
	// The dedicated client is only set when cache_redis is explicitly
	// configured, so closing it does not affect the main limiter's client.
	if c.cacheRedis != nil {
		_ = c.cacheRedis.Close()
		c.cacheRedis = nil
	}

	cacheClient := c.resolveCacheRedis(newCfg, c.logger)
	newExt, extErr := ratelimit.NewExternalClient(newCfg.RateLimit.External, cacheClient, c.logger)
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

// SwapProxy atomically replaces the downstream proxy handler.
// Used for hot-reloading backend configuration changes.
func (c *Chain) SwapProxy(next http.Handler) {
	c.next.Store(&next)
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
	if c.cacheRedis != nil {
		collect(c.cacheRedis.Close())
	}
	if c.responseCacheRedis != nil {
		collect(c.responseCacheRedis.Close())
	}
	if ac := c.authClient.Load(); ac != nil {
		collect(ac.Close())
	}
	if ext := c.externalRL.Load(); ext != nil {
		collect(ext.Close())
	}
	if c.fallback != nil {
		c.fallback.Close()
	}
	if c.globalBackstop != nil {
		c.globalBackstop.Close()
	}
	if c.emitter != nil {
		collect(c.emitter.Close())
	}

	return firstErr
}

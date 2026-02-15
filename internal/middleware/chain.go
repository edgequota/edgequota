// Package middleware implements the request processing pipeline for EdgeQuota.
// The middleware chain handles: authentication → rate limiting → proxying.
// Each stage is optional and configurable.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/edgequota/edgequota/internal/auth"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/edgequota/edgequota/internal/ratelimit"
	"github.com/edgequota/edgequota/internal/redis"
)

// cryptoRandFloat64 returns a cryptographically random float64 in [0, 1).
func cryptoRandFloat64() float64 {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Fallback: zero jitter is safe — it just removes randomness.
		return 0.5
	}
	// Use top 53 bits for a uniform float64 in [0, 1).
	return float64(binary.BigEndian.Uint64(buf[:])>>11) / (1 << 53)
}

// Recovery backoff configuration.
var (
	recoveryBackoffBase = time.Second
	recoveryBackoffMax  = 30 * time.Second

	backoffJitter = func(d time.Duration) time.Duration {
		factor := 0.8 + cryptoRandFloat64()*0.4
		return time.Duration(float64(d) * factor)
	}
)

// maxTTL caps Redis key TTL to 7 days.
const maxTTL = 7 * 24 * 3600

// Failure policy constants — re-exported from config for local readability.
const (
	policyPassthrough      = config.FailurePolicyPassThrough
	policyFailClosed       = config.FailurePolicyFailClosed
	policyInMemoryFallback = config.FailurePolicyInMemoryFallback
)

// Chain is the main request processing middleware. It chains authentication,
// rate limiting, and proxying into a single http.Handler.
type Chain struct {
	next        http.Handler
	cfg         *config.Config
	logger      *slog.Logger
	metrics     *observability.Metrics
	keyStrategy ratelimit.KeyStrategy
	authClient  *auth.Client
	externalRL  *ratelimit.ExternalClient

	mu             sync.RWMutex
	limiter        *ratelimit.Limiter
	redisUnhealthy bool

	// cacheRedis is a dedicated Redis client for caching external rate limit
	// responses. When cache_redis is configured, this is a separate connection;
	// otherwise it is nil and the main rate-limit Redis client is reused.
	cacheRedis redis.Client

	fallback *ratelimit.InMemoryLimiter

	ratePerSecond float64
	burst         int64
	ttl           int
	prefix        string
	failurePolicy config.FailurePolicy
	failureCode   int
	average       int64

	ctx          context.Context
	cancel       context.CancelFunc
	reconnectMu  sync.Mutex
	reconnecting bool
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

// parseRateLimitParams normalizes and computes derived rate limit parameters.
func parseRateLimitParams(cfg *config.RateLimitConfig) rateLimitParams {
	fp := cfg.FailurePolicy
	if fp == "" {
		fp = policyPassthrough
	}

	fc := cfg.FailureCode
	if fc == 0 {
		fc = 429
	}

	period, _ := time.ParseDuration(cfg.Period)
	if period <= 0 {
		period = time.Second
	}

	burst := max(cfg.Burst, 1)

	var ratePerSecond float64
	if cfg.Average > 0 {
		ratePerSecond = float64(cfg.Average) * float64(time.Second) / float64(period)
	}

	ttl := 2
	if ratePerSecond > 0 && ratePerSecond < 1 {
		ttl = min(int(1/ratePerSecond)+2, maxTTL)
	}

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
func NewChain(
	parentCtx context.Context,
	next http.Handler,
	cfg *config.Config,
	logger *slog.Logger,
	metrics *observability.Metrics,
) (*Chain, error) {
	ctx, cancel := context.WithCancel(parentCtx)

	ks, err := ratelimit.NewKeyStrategy(cfg.RateLimit.KeyStrategy)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("key strategy: %w", err)
	}

	p := parseRateLimitParams(&cfg.RateLimit)
	prefix := "rl:edgequota:"

	chain := &Chain{
		next:          next,
		cfg:           cfg,
		logger:        logger,
		metrics:       metrics,
		keyStrategy:   ks,
		fallback:      ratelimit.NewInMemoryLimiter(p.ratePerSecond, p.burst, time.Duration(p.ttl)*time.Second),
		ratePerSecond: p.ratePerSecond,
		burst:         p.burst,
		ttl:           p.ttl,
		prefix:        prefix,
		failurePolicy: p.failurePolicy,
		failureCode:   p.failureCode,
		average:       cfg.RateLimit.Average,
		ctx:           ctx,
		cancel:        cancel,
	}

	if err := chain.initRedis(cfg, logger, p); err != nil {
		cancel()
		return nil, err
	}

	if err := chain.initClients(cfg, logger); err != nil {
		cancel()
		return nil, err
	}

	logger.Info("middleware chain ready",
		"average", cfg.RateLimit.Average, "burst", p.burst,
		"period", p.period, "policy", p.failurePolicy)

	return chain, nil
}

func (c *Chain) initClients(cfg *config.Config, logger *slog.Logger) error {
	if cfg.Auth.Enabled {
		authClient, authErr := auth.NewClient(cfg.Auth)
		if authErr != nil {
			return fmt.Errorf("auth client: %w", authErr)
		}
		c.authClient = authClient
		logger.Info("authentication enabled")
	}

	if cfg.RateLimit.External.Enabled {
		cacheClient := c.resolveCacheRedis(cfg, logger)

		extClient, extErr := ratelimit.NewExternalClient(cfg.RateLimit.External, cacheClient)
		if extErr != nil {
			if c.cacheRedis != nil {
				_ = c.cacheRedis.Close()
				c.cacheRedis = nil
			}
			return fmt.Errorf("external ratelimit client: %w", extErr)
		}
		c.externalRL = extClient
		logger.Info("external rate limit service enabled",
			"cache_backend", cacheBackendName(cacheClient),
			"dedicated_cache_redis", cfg.CacheRedis != nil && len(cfg.CacheRedis.Endpoints) > 0)
	}

	return nil
}

// resolveCacheRedis returns a Redis client for caching external rate limit
// responses. If cache_redis is explicitly configured, a dedicated client is
// created and stored in c.cacheRedis so it can be closed separately. Otherwise,
// the main rate-limit Redis client is reused.
func (c *Chain) resolveCacheRedis(cfg *config.Config, logger *slog.Logger) redis.Client {
	// Dedicated cache_redis configured — create a separate connection.
	if cfg.CacheRedis != nil && len(cfg.CacheRedis.Endpoints) > 0 {
		cacheClient, err := redis.NewClient(*cfg.CacheRedis)
		if err != nil {
			logger.Warn("dedicated cache redis unavailable, falling back to main redis",
				"error", err)
			// Fall through to reuse main redis.
		} else {
			c.cacheRedis = cacheClient
			logger.Info("dedicated cache redis connected",
				"mode", cfg.CacheRedis.Mode,
				"endpoints", cfg.CacheRedis.Endpoints)
			return cacheClient
		}
	}

	// Reuse the main rate-limit Redis client.
	c.mu.RLock()
	var client redis.Client
	if c.limiter != nil {
		client = c.limiter.Client()
	}
	c.mu.RUnlock()

	return client
}

func cacheBackendName(c redis.Client) string {
	if c != nil {
		return "redis"
	}
	return "none"
}

func (c *Chain) initRedis(cfg *config.Config, logger *slog.Logger, p rateLimitParams) error {
	if cfg.RateLimit.Average == 0 && !cfg.RateLimit.External.Enabled {
		logger.Info("rate limiting disabled (average=0)")
		return nil
	}

	client, redisErr := redis.NewClient(cfg.Redis)
	if redisErr != nil {
		return c.handleRedisStartupFailure(redisErr, p.failurePolicy, logger)
	}

	if p.ratePerSecond > 0 {
		c.limiter = ratelimit.NewLimiter(client, p.ratePerSecond, p.burst, p.ttl, c.prefix)
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

// ServeHTTP processes the request through auth → rate limit → proxy.
func (c *Chain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if c.authClient != nil && !c.checkAuth(w, r) {
		return
	}

	if c.average == 0 && c.externalRL == nil {
		c.metrics.IncAllowed()
		c.next.ServeHTTP(w, r)
		return
	}

	key, err := c.keyStrategy.Extract(r)
	if err != nil {
		c.metrics.IncKeyExtractErrors()
		c.logger.Warn("key extraction failed", "error", err)
		http.Error(w, "could not extract rate-limit key", http.StatusInternalServerError)
		return
	}

	c.fetchExternalLimits(r, key)

	if c.tryRedisLimit(w, r, key) {
		return
	}

	c.handleRedisFailurePolicy(w, r, key)
}

// checkAuth verifies the request with the external auth service.
// Returns true if allowed, false if denied (response already written).
// When allowed, any request_headers from the auth response are injected into
// the request so that downstream stages (rate limiting, backend) can read them.
func (c *Chain) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	authReq := auth.BuildCheckRequest(r)
	resp, err := c.authClient.Check(r.Context(), authReq)
	if err != nil {
		c.logger.Error("auth service error", "error", err)
		c.metrics.IncAuthErrors()
		http.Error(w, "authentication service unavailable", http.StatusServiceUnavailable)
		return false
	}

	if !resp.Allowed {
		c.metrics.IncAuthDenied()
		for k, v := range resp.ResponseHeaders {
			w.Header().Set(k, v)
		}
		code := resp.StatusCode
		if code == 0 {
			code = http.StatusForbidden
		}
		http.Error(w, resp.DenyBody, code)
		return false
	}

	// Inject auth-derived headers into the request. These overwrite any
	// client-sent headers with the same name, so the auth service is the
	// source of truth (e.g. for tenant ID decoded from a token).
	for k, v := range resp.RequestHeaders {
		r.Header.Set(k, v)
	}

	return true
}

// fetchExternalLimits queries the external rate limit service if configured.
func (c *Chain) fetchExternalLimits(r *http.Request, key string) {
	if c.externalRL == nil {
		return
	}

	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	extReq := &ratelimit.ExternalRequest{
		Key:     key,
		Headers: headers,
		Method:  r.Method,
		Path:    r.URL.Path,
	}

	limits, extErr := c.externalRL.GetLimits(r.Context(), extReq)
	if extErr != nil {
		c.logger.Warn("external rate limit service error, using static config", "error", extErr)
	} else if limits.Average > 0 {
		_ = limits // Dynamic limits are applied by the external service deciding allow/deny.
	}
}

// tryRedisLimit attempts to enforce rate limit via Redis.
// Returns true if the request was fully handled (allowed or denied), false if Redis is unavailable.
func (c *Chain) tryRedisLimit(w http.ResponseWriter, r *http.Request, key string) bool {
	c.mu.RLock()
	lim := c.limiter
	c.mu.RUnlock()

	if lim == nil {
		return false
	}

	result, limErr := lim.Allow(r.Context(), key)
	if limErr == nil {
		c.serveResult(w, r, result)
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
			c.logger.Warn("redis became unhealthy, switching to fallback",
				"error", limErr, "policy", c.failurePolicy)
		}
		c.startRecoveryIfNeeded()
	}
}

func (c *Chain) serveResult(w http.ResponseWriter, r *http.Request, result *ratelimit.Result) {
	if !result.Allowed {
		c.metrics.IncLimited()
		c.serveRateLimited(w, result.RetryAfter)
		return
	}
	c.metrics.IncAllowed()
	c.next.ServeHTTP(w, r)
}

func (c *Chain) serveRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	retrySeconds := math.Ceil(retryAfter.Seconds())
	if retrySeconds < 1 {
		retrySeconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%.0f", retrySeconds))
	w.Header().Set("X-Retry-In", retryAfter.String())
	http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
}

func (c *Chain) handleRedisFailurePolicy(w http.ResponseWriter, r *http.Request, key string) {
	switch c.failurePolicy {
	case policyPassthrough:
		c.metrics.IncAllowed()
		c.next.ServeHTTP(w, r)

	case policyFailClosed:
		c.metrics.IncLimited()
		http.Error(w, http.StatusText(c.failureCode), c.failureCode)

	case policyInMemoryFallback:
		c.metrics.IncFallbackUsed()
		if c.fallback.Allow(key) {
			c.metrics.IncAllowed()
			c.next.ServeHTTP(w, r)
		} else {
			c.metrics.IncLimited()
			c.serveRateLimited(w, time.Second)
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
	backoff := recoveryBackoffBase
	attempt := 0

	for {
		if c.ctx.Err() != nil {
			return
		}

		client, err := redis.NewClient(c.cfg.Redis)
		if err != nil {
			attempt++
			sleep := backoffJitter(backoff)

			if attempt <= 5 || attempt%10 == 0 {
				c.logger.Warn("redis recovery attempt failed",
					"attempt", attempt, "error", err, "next_in", sleep)
			}

			timer := time.NewTimer(sleep)
			select {
			case <-c.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}

			backoff = min(backoff*2, recoveryBackoffMax)
			continue
		}

		if c.ctx.Err() != nil {
			_ = client.Close()
			return
		}

		if c.ratePerSecond <= 0 {
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

		limiter := ratelimit.NewLimiter(client, c.ratePerSecond, c.burst, c.ttl, c.prefix)

		c.mu.Lock()
		old := c.swapLimiterLocked(limiter)
		c.redisUnhealthy = false
		c.mu.Unlock()

		if old != nil {
			_ = old.Close()
		}

		c.logger.Info("redis connection recovered")
		return
	}
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

// Close shuts down the middleware chain and releases all resources.
func (c *Chain) Close() error {
	c.cancel()

	c.mu.Lock()
	old := c.swapLimiterLocked(nil)
	c.redisUnhealthy = true
	c.mu.Unlock()

	var firstErr error
	if old != nil {
		firstErr = old.Close()
	}
	if c.cacheRedis != nil {
		if err := c.cacheRedis.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if c.authClient != nil {
		if err := c.authClient.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if c.externalRL != nil {
		if err := c.externalRL.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if c.fallback != nil {
		c.fallback.Close()
	}

	return firstErr
}

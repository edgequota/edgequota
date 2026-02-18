// Package middleware implements the request processing pipeline for EdgeQuota.
// The middleware chain handles: authentication → rate limiting → proxying.
// Each stage is optional and configurable.
package middleware

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
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
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edgequota/edgequota/internal/auth"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/events"
	"github.com/edgequota/edgequota/internal/observability"
	"github.com/edgequota/edgequota/internal/proxy"
	"github.com/edgequota/edgequota/internal/ratelimit"
	"github.com/edgequota/edgequota/internal/redis"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var tracer = otel.Tracer("edgequota.middleware")

// requestIDHeader is the canonical HTTP header for request correlation.
const requestIDHeader = "X-Request-Id"

// maxRequestIDLen is the maximum allowed length for a client-supplied X-Request-Id.
const maxRequestIDLen = 128

// requestIDRng is a per-goroutine-safe CSPRNG seeded from crypto/rand.
// ChaCha8 is cryptographically strong and avoids a syscall per ID
// (unlike crypto/rand.Read), which reduces latency under high concurrency.
var requestIDRng = func() *rand.ChaCha8 {
	var seed [32]byte
	if _, err := cryptorand.Read(seed[:]); err != nil {
		panic("failed to seed ChaCha8: " + err.Error())
	}
	return rand.NewChaCha8(seed)
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

// injectionDenyHeaders is the set of hop-by-hop and security-sensitive headers
// that the auth service is NOT allowed to inject into upstream requests. This
// prevents request smuggling via a compromised or misconfigured auth service.
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
	defaultRecoveryBackoffMax  = 30 * time.Second

	defaultBackoffJitter = func(d time.Duration) time.Duration {
		factor := 0.8 + cryptoRandFloat64()*0.4
		return time.Duration(float64(d) * factor)
	}
)

// maxTTL caps Redis key TTL to 7 days.
const maxTTL = 7 * 24 * 3600

// getHeaderMap returns a fresh map populated with the first value of each header.
// Uses plain allocation instead of sync.Pool — Go 1.25's GC handles short-lived
// maps efficiently, and the pool adds contention under high concurrency with no
// measurable benefit (validated by BenchmarkGetHeaderMap in chain_bench_test.go).
func getHeaderMap(r *http.Request) map[string]string {
	m := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 {
			m[k] = v[0]
		}
	}
	return m
}

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

	keyStrategy atomic.Pointer[ratelimit.KeyStrategy]

	mu             sync.RWMutex
	limiter        *ratelimit.Limiter
	redisUnhealthy bool

	// cacheRedis is a dedicated Redis client for caching external rate limit
	// responses. When cache_redis is configured, this is a separate connection;
	// otherwise it is nil and the main rate-limit Redis client is reused.
	cacheRedis redis.Client

	fallback       *ratelimit.InMemoryLimiter
	globalBackstop *ratelimit.InMemoryLimiter // optional global RPS safety valve for passthrough mode

	ratePerSecond float64
	burst         int64
	ttl           int
	prefix        string
	failurePolicy config.FailurePolicy
	failureCode   int
	average       int64

	// requestTimeout and urlPolicy are read from ServeHTTP (hot path)
	// without holding mu, and written by Reload. Using atomic.Value
	// avoids a data race without adding lock contention.
	requestTimeout atomic.Value // time.Duration
	urlPolicy      atomic.Value // proxy.BackendURLPolicy

	emitter *events.Emitter

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

	// Compute TTL for Redis keys. The TTL is tied to the configured period
	// to avoid excessive EXPIRE churn for high-cardinality keys at high rates.
	// For sub-1-rps rates, TTL is scaled to the replenishment time instead.
	periodSec := int(math.Ceil(period.Seconds()))
	ttl := max(2, periodSec*2)
	if ratePerSecond > 0 && ratePerSecond < 1 {
		ttl = min(int(1/ratePerSecond)+2, maxTTL)
	}

	// Apply optional minimum TTL floor from config.
	if cfg.MinTTL != "" {
		if minTTLDur, err := time.ParseDuration(cfg.MinTTL); err == nil && minTTLDur > 0 {
			ttl = max(ttl, int(math.Ceil(minTTLDur.Seconds())))
		}
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

	ks, err := ratelimit.NewKeyStrategy(cfg.RateLimit.KeyStrategy)
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
		average:             cfg.RateLimit.Average,
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
	chain.requestTimeout.Store(reqTimeout)
	chain.urlPolicy.Store(proxy.BackendURLPolicy{
		AllowedSchemes:      cfg.Backend.URLPolicy.AllowedSchemes,
		DenyPrivateNetworks: cfg.Backend.URLPolicy.DenyPrivateNetworksEnabled(),
		AllowedHosts:        cfg.Backend.URLPolicy.AllowedHosts,
	})
	chain.keyStrategy.Store(&ks)
	chain.cfg.Store(cfg)
	if cfg.RateLimit.GlobalPassthroughRPS > 0 {
		chain.globalBackstop = ratelimit.NewInMemoryLimiter(
			cfg.RateLimit.GlobalPassthroughRPS,
			max(int64(cfg.RateLimit.GlobalPassthroughRPS), 1),
			time.Minute,
		)
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
		c.authClient.Store(authClient)
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
		extClient.SetMetricHooks(
			c.metrics.PromExtRLSemaphoreRejected.Inc,
			c.metrics.PromExtRLSingleflightShared.Inc,
		)
		c.externalRL.Store(extClient)
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

// statusWriter captures the HTTP status code written by downstream handlers.
type statusWriter struct {
	http.ResponseWriter
	code    int
	written bool
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
	return sw.ResponseWriter.Write(b)
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

// ServeHTTP processes the request through auth → rate limit → proxy.
func (c *Chain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := statusWriterPool.Get().(*statusWriter)
	sw.ResponseWriter = w
	sw.code = http.StatusOK
	sw.written = false

	// Propagate or generate X-Request-Id for request correlation.
	// Validate client-supplied IDs to prevent CRLF injection and log pollution.
	reqID := r.Header.Get(requestIDHeader)
	if !validRequestID(reqID) {
		reqID = generateRequestID()
		r.Header.Set(requestIDHeader, reqID)
	}
	sw.Header().Set(requestIDHeader, reqID)

	defer func() {
		duration := time.Since(start).Seconds()
		c.metrics.PromRequestDuration.WithLabelValues(
			r.Method,
			strconv.Itoa(sw.code),
		).Observe(duration)
		sw.ResponseWriter = nil // prevent dangling reference
		statusWriterPool.Put(sw)
	}()

	if ac := c.authClient.Load(); ac != nil {
		_, authSpan := tracer.Start(r.Context(), "edgequota.auth")
		authStart := time.Now()
		allowed := c.checkAuth(sw, r, ac)
		c.metrics.PromAuthDuration.Observe(time.Since(authStart).Seconds())
		authSpan.End()
		if !allowed {
			return
		}
	}

	if c.average == 0 && c.externalRL.Load() == nil {
		c.metrics.IncAllowed()
		backendStart := time.Now()
		(*c.next.Load()).ServeHTTP(sw, r)
		c.metrics.PromBackendDuration.Observe(time.Since(backendStart).Seconds())
		return
	}

	key, err := (*c.keyStrategy.Load()).Extract(r)
	if err != nil {
		c.metrics.IncKeyExtractErrors()
		c.logger.Warn("key extraction failed", "error", err)
		writeJSONError(sw, http.StatusInternalServerError, "key_extraction_failed", "could not extract rate-limit key", 0)
		return
	}

	// Store the rate-limit key in context for downstream handlers (e.g.
	// the per-key WebSocket connection limiter in the proxy).
	ctx := context.WithValue(r.Context(), ratelimit.KeyContextKey, key)
	r = r.WithContext(ctx)

	var extLimits *ratelimit.ExternalLimits
	var resolvedKey string
	if c.externalRL.Load() != nil {
		_, extSpan := tracer.Start(r.Context(), "edgequota.external_rl")
		extStart := time.Now()
		extLimits, resolvedKey = c.fetchExternalLimits(r, key)
		c.metrics.PromExternalRLDuration.Observe(time.Since(extStart).Seconds())
		extSpan.End()
	} else {
		extLimits, resolvedKey = c.fetchExternalLimits(r, key)
	}

	ctx, cancel := c.applyRequestTimeout(ctx, extLimits)
	if cancel != nil {
		defer cancel()
	}
	r = r.WithContext(ctx)

	r = c.injectBackendURL(r, extLimits)

	if c.tryRedisLimit(sw, r, resolvedKey, extLimits) {
		return
	}

	c.handleRedisFailurePolicy(sw, r, resolvedKey, extLimits)
}

// applyRequestTimeout returns a context with a timeout derived from the global
// config and optionally overridden by the external rate-limit response. The
// caller must defer the returned cancel func when non-nil.
func (c *Chain) applyRequestTimeout(ctx context.Context, extLimits *ratelimit.ExternalLimits) (context.Context, context.CancelFunc) {
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

// injectBackendURL sets a per-tenant backend URL in the request context when
// the external service provides one. Falls back to the static backend URL on
// parse or policy validation failure.
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

// checkAuth verifies the request with the external auth service.
// Returns true if allowed, false if denied (response already written).
// The auth client is passed explicitly to avoid a redundant atomic load
// (the caller already loaded it to check for nil).
// When allowed, any request_headers from the auth response are injected into
// the request so that downstream stages (rate limiting, backend) can read them.
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

	authReq := ac.BuildCheckRequest(r, bodyBytes)
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
		if _, denied := injectionDenyHeaders[http.CanonicalHeaderKey(k)]; denied {
			c.logger.Warn("auth service tried to inject denied header, skipping",
				"header", k)
			continue
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

	extReq := &ratelimit.ExternalRequest{
		Key:     key,
		Headers: headers,
		Method:  r.Method,
		Path:    r.URL.Path,
	}

	extLimits, extErr := ext.GetLimits(r.Context(), extReq)
	if extErr != nil {
		c.logger.Warn("external rate limit service error, using static config", "error", extErr)
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
			backendStart := time.Now()
			(*c.next.Load()).ServeHTTP(w, r)
			c.metrics.PromBackendDuration.Observe(time.Since(backendStart).Seconds())
		} else {
			c.metrics.IncLimited()
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
	maxAttempts := c.cfg.Load().RateLimit.MaxRecoveryAttempts

	for {
		if c.ctx.Err() != nil {
			return
		}

		client, err := redis.NewClient(c.cfg.Load().Redis)
		if err != nil {
			attempt++
			if done := c.recoveryRetry(&backoff, attempt, maxAttempts, err); done {
				return
			}
			continue
		}

		if c.ctx.Err() != nil {
			_ = client.Close()
			return
		}

		c.recoveryInstall(client)
		return
	}
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

	limiter := ratelimit.NewLimiter(client, c.ratePerSecond, c.burst, c.ttl, c.prefix, c.logger)

	c.mu.Lock()
	old := c.swapLimiterLocked(limiter)
	c.redisUnhealthy = false
	c.mu.Unlock()

	if old != nil {
		_ = old.Close()
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
	newKS, err := ratelimit.NewKeyStrategy(newCfg.RateLimit.KeyStrategy)
	if err != nil {
		return fmt.Errorf("reload key strategy: %w", err)
	}

	c.keyStrategy.Store(&newKS) // Atomic store — read by ServeHTTP without holding mu.

	c.mu.Lock()
	prevCfg := c.cfg.Load() // Capture previous config before overwriting.
	c.ratePerSecond = p.ratePerSecond
	c.burst = p.burst
	c.ttl = p.ttl
	c.prefix = prefix
	c.failurePolicy = p.failurePolicy
	c.failureCode = p.failureCode
	c.average = newCfg.RateLimit.Average
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

	// Recreate emitter if events config changed.
	oldEmitter := c.emitter
	c.emitter = events.NewEmitter(newCfg.Events, c.logger, c.metrics)
	if oldEmitter != nil {
		_ = oldEmitter.Close()
	}

	c.logger.Info("middleware chain reloaded",
		"average", newCfg.RateLimit.Average, "burst", p.burst,
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
	newExt, extErr := ratelimit.NewExternalClient(newCfg.RateLimit.External, cacheClient)
	if extErr != nil {
		c.logger.Error("reload external RL client failed, keeping old client", "error", extErr)
		return
	}
	newExt.SetMetricHooks(
		c.metrics.PromExtRLSemaphoreRejected.Inc,
		c.metrics.PromExtRLSingleflightShared.Inc,
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

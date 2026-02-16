package ratelimit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ratelimitv1 "github.com/edgequota/edgequota/api/gen/edgequota/ratelimit/v1"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/redis"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// maxResponseBodyBytes limits the size of HTTP response bodies read from
// external services to prevent unbounded memory consumption.
const maxResponseBodyBytes = 64 * 1024 // 64 KiB

// cacheKeyPrefix is the Redis key prefix for cached external rate limit responses.
const cacheKeyPrefix = "rl:extcache:"

// ExternalLimits holds rate limit parameters returned by an external service.
// The cache control fields are optional and consistent across HTTP and gRPC.
type ExternalLimits struct {
	Average int64  `json:"average"`
	Burst   int64  `json:"burst"`
	Period  string `json:"period"`

	// TenantKey is an optional key returned by the external service for tenant
	// isolation. When non-empty, it replaces the extracted key so that each
	// tenant gets its own Redis bucket (e.g. "tenant:acme-corp").
	TenantKey string `json:"tenant_key,omitempty"`

	// FailurePolicy overrides the static failure_policy for this request when
	// Redis is unavailable. This allows an external service to dynamically
	// switch behavior — e.g. "passthrough" during planned maintenance, or
	// "failclosed" during an active attack. Zero-value ("") means no override.
	FailurePolicy config.FailurePolicy `json:"failure_policy,omitempty"`

	// FailureCode overrides the HTTP status code returned when the failure
	// policy is "failclosed". 0 means "use the static config" (no override).
	FailureCode int `json:"failure_code,omitempty"`

	// BackendURL overrides the static backend.url for this request. When
	// non-empty, the proxy forwards this request to the given URL instead of
	// the default backend. This enables per-tenant backend routing.
	BackendURL string `json:"backend_url,omitempty"`

	// RequestTimeout overrides the global server.request_timeout for this
	// request. Duration string (e.g. "10s", "30s"). Empty means no override.
	RequestTimeout string `json:"request_timeout,omitempty"`

	// CacheMaxAgeSec controls how long this response is cached (in seconds).
	// nil = not set (use default TTL or HTTP headers); > 0 = cache for N seconds.
	CacheMaxAgeSec *int64 `json:"cache_max_age_seconds,omitempty"`
	// CacheNoStore, when true, prevents this response from being cached.
	CacheNoStore bool `json:"cache_no_store,omitempty"`
}

// ExternalRequest is sent to the external rate limit service.
type ExternalRequest struct {
	Key     string            `json:"key"`
	Headers map[string]string `json:"headers"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
}

// ExternalClient fetches rate limit configuration from an external service.
// It supports both HTTP and gRPC backends. Responses are cached in Redis,
// using the TTL from the response's Cache-Control / Expires headers. When
// no cache headers are present, the configured default TTL is used.
//
// A circuit breaker protects the external service from cascading failures.
// When the external service is unhealthy, cached (potentially stale) responses
// are served while background refresh attempts are made.
type ExternalClient struct {
	httpURL      string
	grpcClient   ratelimitv1.RateLimitServiceClient // generated typed client
	grpcConn     *grpc.ClientConn
	httpClient   *http.Client
	timeout      time.Duration
	cacheTTL     time.Duration // default TTL when response has no cache headers
	headerFilter *config.HeaderFilter

	redisClient redis.Client // may be nil (no caching)
	maxLatency  time.Duration

	// singleflight collapses concurrent cache misses for the same key into
	// one external call, preventing thundering herd on the control plane.
	sfGroup singleflight.Group

	// fetchSem limits the number of concurrent in-flight requests to the
	// external rate limit service. Singleflight collapses duplicate keys,
	// but a cache flush can trigger thousands of distinct keys simultaneously.
	// The semaphore caps total concurrency to protect the control plane.
	fetchSem *semaphore.Weighted

	// Per-tenant circuit breakers. Each tenant key gets its own breaker so
	// that one misbehaving tenant config doesn't trip the circuit for all.
	breakers       sync.Map // map[string]*tenantCircuitBreaker
	breakerCount   atomic.Int64
	maxBreakers    int64
	cbThreshold    int
	cbResetTimeout time.Duration

	// done is closed by Close() to stop the background eviction goroutine.
	done chan struct{}
}

// Circuit breaker and concurrency defaults.
const (
	defaultCBThreshold    = 5
	defaultCBResetTimeout = 30 * time.Second
	staleCachePrefix      = "rl:extcache:stale:"
	staleCacheTTL         = 5 * time.Minute

	// defaultMaxConcurrentFetches caps the number of simultaneous in-flight
	// requests to the external rate limit service across all keys. This
	// prevents a thundering herd of distinct-key cache misses (e.g. after a
	// Redis flush) from overwhelming the control plane.
	defaultMaxConcurrentFetches = 50

	// defaultMaxCircuitBreakers caps the number of per-tenant circuit breaker
	// entries in the sync.Map. Prevents unbounded memory growth under attack
	// with millions of distinct keys.
	defaultMaxCircuitBreakers = 10000
)

// tenantCircuitBreaker holds per-tenant circuit breaker state. One bad tenant
// config shouldn't trip the circuit for everyone.
type tenantCircuitBreaker struct {
	mu           sync.Mutex
	failures     int
	lastFailure  time.Time
	open         bool
	openUntil    time.Time
	lastAccess   time.Time
	threshold    int
	resetTimeout time.Duration
}

// closedBreakerStub is a permanently-closed (healthy) circuit breaker returned
// when the per-tenant breaker map is at capacity. It's safe for concurrent use
// because isOpen always returns false and record* are no-ops.
var closedBreakerStub = &tenantCircuitBreaker{
	threshold:    1<<31 - 1, // effectively infinite
	resetTimeout: time.Hour,
}

func newTenantCB(threshold int, resetTimeout time.Duration) *tenantCircuitBreaker {
	return &tenantCircuitBreaker{
		threshold:    threshold,
		resetTimeout: resetTimeout,
		lastAccess:   time.Now(),
	}
}

func (cb *tenantCircuitBreaker) isOpen() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.lastAccess = time.Now()
	if !cb.open {
		return false
	}
	// Allow a probe if enough time has passed (half-open).
	return time.Now().Before(cb.openUntil)
}

func (cb *tenantCircuitBreaker) recordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures++
	cb.lastFailure = time.Now()
	cb.lastAccess = time.Now()
	if cb.failures >= cb.threshold {
		cb.open = true
		cb.openUntil = time.Now().Add(cb.resetTimeout)
	}
}

func (cb *tenantCircuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.open = false
	cb.lastAccess = time.Now()
}

func (cb *tenantCircuitBreaker) isStale(now time.Time, maxIdle time.Duration) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return now.Sub(cb.lastAccess) > maxIdle
}

// NewExternalClient creates a client for the external rate limit service.
// The redisClient is used for distributed caching; pass nil to disable caching.
func NewExternalClient(cfg config.ExternalRLConfig, redisClient redis.Client) (*ExternalClient, error) {
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil {
		timeout = 5 * time.Second
	}

	cacheTTL, err := time.ParseDuration(cfg.CacheTTL)
	if err != nil {
		cacheTTL = 60 * time.Second
	}

	// Tuned HTTP transport with high per-host connection pool for
	// low-latency, high-concurrency external rate limit calls.
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100, // External RL is typically a single host.
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	maxConcurrent := int64(cfg.MaxConcurrentRequests)
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrentFetches
	}

	maxBreakers := int64(cfg.MaxCircuitBreakers)
	if maxBreakers <= 0 {
		maxBreakers = defaultMaxCircuitBreakers
	}

	var maxLatency time.Duration
	if cfg.MaxLatency != "" {
		maxLatency, _ = time.ParseDuration(cfg.MaxLatency)
	}

	ec := &ExternalClient{
		httpURL:        cfg.HTTP.URL,
		httpClient:     &http.Client{Timeout: timeout, Transport: transport},
		timeout:        timeout,
		cacheTTL:       cacheTTL,
		headerFilter:   config.NewHeaderFilter(cfg.HeaderFilter),
		redisClient:    redisClient,
		maxLatency:     maxLatency,
		fetchSem:       semaphore.NewWeighted(maxConcurrent),
		maxBreakers:    maxBreakers,
		cbThreshold:    defaultCBThreshold,
		cbResetTimeout: defaultCBResetTimeout,
		done:           make(chan struct{}),
	}

	// Establish gRPC connection if configured.
	if cfg.GRPC.Address != "" {
		var creds credentials.TransportCredentials
		if cfg.GRPC.TLS.Enabled {
			serverName := cfg.GRPC.TLS.ResolveServerName(cfg.GRPC.Address)
			tlsCreds, tlsErr := credentials.NewClientTLSFromFile(cfg.GRPC.TLS.CAFile, serverName)
			if tlsErr != nil {
				return nil, fmt.Errorf("external ratelimit grpc tls: %w", tlsErr)
			}
			creds = tlsCreds
		} else {
			creds = insecure.NewCredentials()
		}

		conn, dialErr := grpc.NewClient(cfg.GRPC.Address,
			grpc.WithTransportCredentials(creds),
			grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64*1024)), // 64 KiB — bound response size from external service.
		)
		if dialErr != nil {
			return nil, fmt.Errorf("external ratelimit grpc dial: %w", dialErr)
		}
		ec.grpcConn = conn
		ec.grpcClient = ratelimitv1.NewRateLimitServiceClient(conn)
	}

	// Background eviction of stale per-tenant circuit breakers to prevent
	// unbounded growth of the sync.Map. Stopped by Close() via done channel.
	go func() {
		ticker := time.NewTicker(ec.cbResetTimeout)
		defer ticker.Stop()
		for {
			select {
			case <-ec.done:
				return
			case <-ticker.C:
				ec.evictStaleBreakers()
			}
		}
	}()

	return ec, nil
}

// GetLimits fetches rate limits for the given key. Returns cached results from
// Redis if available. On cache miss, calls the external service and caches the
// response. Cache TTL is resolved as follows:
//
// HTTP: Cache-Control/Expires headers > body cache fields > default TTL.
// gRPC: body cache fields > default TTL.
//
// Concurrency control:
//  1. Semaphore is checked BEFORE singleflight to cap total concurrency across
//     all keys. If the semaphore is full, stale cache is used when available.
//  2. Singleflight collapses concurrent misses for the SAME key into one call.
//
// When the circuit breaker is open (external service unhealthy), stale cached
// data is served if available, avoiding latency spikes on the hot path.
func (ec *ExternalClient) GetLimits(ctx context.Context, req *ExternalRequest) (*ExternalLimits, error) {
	// Try Redis cache first.
	if cached := ec.getFromCache(ctx, req.Key); cached != nil {
		return cached, nil
	}

	// If per-tenant circuit breaker is open, try stale cache before failing.
	if ec.isCircuitOpen(req.Key) {
		if stale := ec.getStaleFromCache(ctx, req.Key); stale != nil {
			return stale, nil
		}
		return nil, fmt.Errorf("external rate limit service circuit breaker open for key %q", req.Key)
	}

	// Acquire concurrency permit BEFORE singleflight. If the semaphore is
	// full, fall back to stale cache instead of queuing — this prevents
	// cascading latency when thousands of distinct keys miss simultaneously
	// (e.g. after a Redis flush).
	if !ec.fetchSem.TryAcquire(1) {
		if stale := ec.getStaleFromCache(ctx, req.Key); stale != nil {
			return stale, nil
		}
		return nil, fmt.Errorf("external rate limit service semaphore full for key %q", req.Key)
	}
	defer ec.fetchSem.Release(1)

	// Use singleflight to collapse concurrent cache misses for the same key
	// into one external call, preventing thundering herd on the control plane.
	type sfResult struct {
		limits *ExternalLimits
		ttl    time.Duration
	}

	v, err, _ := ec.sfGroup.Do(req.Key, func() (any, error) {
		// Double-check cache inside singleflight — another goroutine may
		// have populated it while we were waiting.
		if cached := ec.getFromCache(ctx, req.Key); cached != nil {
			return &sfResult{limits: cached}, nil
		}

		limits, ttl, fetchErr := ec.fetchFromService(ctx, req)
		if fetchErr != nil {
			ec.recordFailure(req.Key)
			if stale := ec.getStaleFromCache(ctx, req.Key); stale != nil {
				return &sfResult{limits: stale}, nil
			}
			return nil, fetchErr
		}
		ec.recordSuccess(req.Key)
		ec.setInCache(ctx, req.Key, limits, ttl)
		ec.setStaleInCache(ctx, req.Key, limits)
		return &sfResult{limits: limits, ttl: ttl}, nil
	})

	if err != nil {
		return nil, err
	}

	return v.(*sfResult).limits, nil
}

// fetchFromService calls the external rate limit service (gRPC or HTTP).
// When maxLatency is configured, the call is abandoned if it exceeds the
// latency budget and an error is returned (allowing stale cache fallback).
func (ec *ExternalClient) fetchFromService(ctx context.Context, req *ExternalRequest) (*ExternalLimits, time.Duration, error) {
	if ec.maxLatency > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ec.maxLatency)
		defer cancel()
	}

	if ec.grpcClient != nil {
		limits, err := ec.getLimitsGRPC(ctx, req)
		if err != nil {
			return nil, 0, err
		}
		return limits, ec.resolveBodyCacheTTL(limits), nil
	}
	if ec.httpURL != "" {
		return ec.getLimitsHTTP(ctx, req)
	}
	return nil, 0, fmt.Errorf("no external rate limit service configured")
}

// ---------------------------------------------------------------------------
// Circuit breaker
// ---------------------------------------------------------------------------

// getOrCreateCB returns the per-tenant circuit breaker, creating one if needed.
// When the breaker map exceeds maxBreakers, new keys get a permanently-closed
// (healthy) stub to prevent unbounded memory growth under high-cardinality attacks.
func (ec *ExternalClient) getOrCreateCB(key string) *tenantCircuitBreaker {
	if v, ok := ec.breakers.Load(key); ok {
		return v.(*tenantCircuitBreaker)
	}

	// Cap check: if at capacity, return a passthrough stub (circuit always closed).
	if ec.breakerCount.Load() >= ec.maxBreakers {
		return closedBreakerStub
	}

	cb := newTenantCB(ec.cbThreshold, ec.cbResetTimeout)
	actual, loaded := ec.breakers.LoadOrStore(key, cb)
	if !loaded {
		ec.breakerCount.Add(1)
	}
	return actual.(*tenantCircuitBreaker)
}

// isCircuitOpen checks the per-tenant circuit breaker for the given key.
func (ec *ExternalClient) isCircuitOpen(key string) bool {
	return ec.getOrCreateCB(key).isOpen()
}

func (ec *ExternalClient) recordFailure(key string) {
	ec.getOrCreateCB(key).recordFailure()
}

func (ec *ExternalClient) recordSuccess(key string) {
	ec.getOrCreateCB(key).recordSuccess()
}

// evictStaleBreakers removes per-tenant circuit breakers that haven't been
// accessed for 2x the reset timeout. This prevents unbounded growth.
func (ec *ExternalClient) evictStaleBreakers() {
	now := time.Now()
	maxIdle := 2 * ec.cbResetTimeout
	ec.breakers.Range(func(key, value any) bool {
		cb := value.(*tenantCircuitBreaker)
		if cb.isStale(now, maxIdle) {
			ec.breakers.Delete(key)
			// CAS loop with floor of 0 to prevent breakerCount from going
			// negative due to races between eviction and LoadOrStore.
			for {
				old := ec.breakerCount.Load()
				if old <= 0 {
					break
				}
				if ec.breakerCount.CompareAndSwap(old, old-1) {
					break
				}
			}
		}
		return true
	})
}

// ---------------------------------------------------------------------------
// Stale cache (for circuit breaker fallback)
// ---------------------------------------------------------------------------

func (ec *ExternalClient) getStaleFromCache(ctx context.Context, key string) *ExternalLimits {
	if ec.redisClient == nil {
		return nil
	}
	data, err := ec.redisClient.Get(ctx, staleCachePrefix+key).Bytes()
	if err != nil {
		return nil
	}
	var limits ExternalLimits
	if err := json.Unmarshal(data, &limits); err != nil {
		return nil
	}
	return &limits
}

func (ec *ExternalClient) setStaleInCache(ctx context.Context, key string, limits *ExternalLimits) {
	if ec.redisClient == nil {
		return
	}
	data, err := json.Marshal(limits)
	if err != nil {
		return
	}
	_ = ec.redisClient.Set(ctx, staleCachePrefix+key, data, staleCacheTTL).Err()
}

// getFromCache attempts to read cached limits from Redis.
func (ec *ExternalClient) getFromCache(ctx context.Context, key string) *ExternalLimits {
	if ec.redisClient == nil {
		return nil
	}

	data, err := ec.redisClient.Get(ctx, cacheKeyPrefix+key).Bytes()
	if err != nil {
		return nil // cache miss or Redis error — not fatal
	}

	var limits ExternalLimits
	if err := json.Unmarshal(data, &limits); err != nil {
		return nil // corrupt entry — treat as miss
	}

	return &limits
}

// setInCache stores limits in Redis with the given TTL.
func (ec *ExternalClient) setInCache(ctx context.Context, key string, limits *ExternalLimits, ttl time.Duration) {
	if ec.redisClient == nil || ttl <= 0 {
		return
	}

	data, err := json.Marshal(limits)
	if err != nil {
		return
	}

	_ = ec.redisClient.Set(ctx, cacheKeyPrefix+key, data, ttl).Err()
}

// getLimitsHTTP fetches limits via HTTP.
// TTL priority: HTTP cache headers > body cache fields > default TTL.
func (ec *ExternalClient) getLimitsHTTP(ctx context.Context, req *ExternalRequest) (*ExternalLimits, time.Duration, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ec.httpURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := ec.httpClient.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("external service returned status %d", resp.StatusCode)
	}

	var limits ExternalLimits
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodyBytes)).Decode(&limits); err != nil {
		return nil, 0, fmt.Errorf("decode response: %w", err)
	}

	ttl := ec.resolveHTTPCacheTTL(resp.Header, &limits)

	return &limits, ttl, nil
}

// resolveHTTPCacheTTL determines the cache TTL for an HTTP response.
// Priority: HTTP Cache-Control/Expires headers > body cache fields > default TTL.
func (ec *ExternalClient) resolveHTTPCacheTTL(h http.Header, limits *ExternalLimits) time.Duration {
	// 1. HTTP Cache-Control header takes highest priority.
	if cc := h.Get("Cache-Control"); cc != "" {
		if ttl := parseMaxAge(cc); ttl > 0 {
			return ttl
		}
		lower := strings.ToLower(cc)
		if strings.Contains(lower, "no-cache") || strings.Contains(lower, "no-store") {
			return 0
		}
	}

	// 2. Expires header.
	if expires := h.Get("Expires"); expires != "" {
		expTime, err := http.ParseTime(expires)
		if err == nil {
			ttl := time.Until(expTime)
			if ttl > 0 {
				return ttl
			}
			return 0 // already expired
		}
	}

	// 3. No HTTP cache headers — fall back to body fields (consistent with gRPC).
	return ec.resolveBodyCacheTTL(limits)
}

// resolveBodyCacheTTL determines the cache TTL from the response body's cache
// control fields. This is the primary resolution path for gRPC and the fallback
// path for HTTP (when no cache headers are present).
func (ec *ExternalClient) resolveBodyCacheTTL(limits *ExternalLimits) time.Duration {
	if limits.CacheNoStore {
		return 0
	}
	if limits.CacheMaxAgeSec != nil && *limits.CacheMaxAgeSec > 0 {
		return time.Duration(*limits.CacheMaxAgeSec) * time.Second
	}
	return ec.cacheTTL
}

// parseMaxAge extracts the max-age value from a Cache-Control header.
// Returns 0 if not found or invalid. Case-insensitive per RFC 7234.
func parseMaxAge(cc string) time.Duration {
	for _, directive := range strings.Split(cc, ",") {
		directive = strings.TrimSpace(directive)
		lower := strings.ToLower(directive)
		if after, found := strings.CutPrefix(lower, "max-age="); found {
			seconds, err := strconv.Atoi(after)
			if err == nil && seconds > 0 {
				return time.Duration(seconds) * time.Second
			}
		}
	}
	return 0
}

func (ec *ExternalClient) getLimitsGRPC(ctx context.Context, req *ExternalRequest) (*ExternalLimits, error) {
	ctx, cancel := context.WithTimeout(ctx, ec.timeout)
	defer cancel()

	pbReq := &ratelimitv1.GetLimitsRequest{
		Key:     req.Key,
		Headers: req.Headers,
		Method:  req.Method,
		Path:    req.Path,
	}

	pbResp, err := ec.grpcClient.GetLimits(ctx, pbReq)
	if err != nil {
		return nil, fmt.Errorf("grpc get limits: %w", err)
	}

	limits := &ExternalLimits{
		Average:        pbResp.GetAverage(),
		Burst:          pbResp.GetBurst(),
		Period:         pbResp.GetPeriod(),
		CacheNoStore:   pbResp.GetCacheNoStore(),
		TenantKey:      pbResp.GetTenantKey(),
		FailurePolicy:  failurePolicyFromProto(int32(pbResp.GetFailurePolicy())),
		FailureCode:    int(pbResp.GetFailureCode()),
		BackendURL:     pbResp.GetBackendUrl(),
		RequestTimeout: pbResp.GetRequestTimeout(),
	}

	if pbResp.CacheMaxAgeSeconds != nil {
		v := pbResp.GetCacheMaxAgeSeconds()
		limits.CacheMaxAgeSec = &v
	}

	return limits, nil
}

// failurePolicyFromProto maps a proto FailurePolicy enum value (int32) to the
// internal config.FailurePolicy type. Returns "" for UNSPECIFIED (no override).
//
// Proto enum values:
//
//	0 = FAILURE_POLICY_UNSPECIFIED  → "" (no override)
//	1 = FAILURE_POLICY_PASSTHROUGH  → "passthrough"
//	2 = FAILURE_POLICY_FAIL_CLOSED  → "failclosed"
//	3 = FAILURE_POLICY_IN_MEMORY_FALLBACK → "inmemoryfallback"
func failurePolicyFromProto(v int32) config.FailurePolicy {
	switch v {
	case 1:
		return config.FailurePolicyPassThrough
	case 2:
		return config.FailurePolicyFailClosed
	case 3:
		return config.FailurePolicyInMemoryFallback
	default:
		return "" // Unspecified or unknown → no override.
	}
}

// FilterHeaders applies the configured header allow/deny list to the given
// map in place, removing entries that should not be forwarded to the external
// rate-limit service.
func (ec *ExternalClient) FilterHeaders(headers map[string]string) {
	for k := range headers {
		if !ec.headerFilter.Allowed(k) {
			delete(headers, k)
		}
	}
}

// Close shuts down the external client and releases resources, including
// the background circuit breaker eviction goroutine.
func (ec *ExternalClient) Close() error {
	// Stop the background eviction goroutine.
	if ec.done != nil {
		select {
		case <-ec.done:
			// Already closed.
		default:
			close(ec.done)
		}
	}

	if ec.httpClient != nil {
		ec.httpClient.CloseIdleConnections()
	}

	if ec.grpcConn != nil {
		return ec.grpcConn.Close()
	}
	return nil
}

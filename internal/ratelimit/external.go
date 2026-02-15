package ratelimit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	ratelimitv1 "github.com/edgequota/edgequota/api/gen/edgequota/ratelimit/v1"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/redis"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
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
	// switch behaviour — e.g. "passthrough" during planned maintenance, or
	// "failclosed" during an active attack. Zero-value ("") means no override.
	FailurePolicy config.FailurePolicy `json:"failure_policy,omitempty"`

	// FailureCode overrides the HTTP status code returned when the failure
	// policy is "failclosed". 0 means "use the static config" (no override).
	FailureCode int `json:"failure_code,omitempty"`

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

	// Circuit breaker state.
	cbMu           sync.RWMutex
	cbFailures     int
	cbLastFailure  time.Time
	cbOpen         bool
	cbOpenUntil    time.Time
	cbThreshold    int           // failures before opening
	cbResetTimeout time.Duration // how long to keep circuit open
}

// Circuit breaker defaults.
const (
	defaultCBThreshold    = 5
	defaultCBResetTimeout = 30 * time.Second
	staleCachePrefix      = "rl:extcache:stale:"
	staleCacheTTL         = 5 * time.Minute
)

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

	ec := &ExternalClient{
		httpURL:        cfg.HTTP.URL,
		httpClient:     &http.Client{Timeout: timeout},
		timeout:        timeout,
		cacheTTL:       cacheTTL,
		headerFilter:   config.NewHeaderFilter(cfg.HeaderFilter),
		redisClient:    redisClient,
		cbThreshold:    defaultCBThreshold,
		cbResetTimeout: defaultCBResetTimeout,
	}

	// Establish gRPC connection if configured.
	if cfg.GRPC.Address != "" {
		var creds credentials.TransportCredentials
		if cfg.GRPC.TLS.Enabled {
			tlsCreds, tlsErr := credentials.NewClientTLSFromFile(cfg.GRPC.TLS.CAFile, "")
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
		)
		if dialErr != nil {
			return nil, fmt.Errorf("external ratelimit grpc dial: %w", dialErr)
		}
		ec.grpcConn = conn
		ec.grpcClient = ratelimitv1.NewRateLimitServiceClient(conn)
	}

	return ec, nil
}

// GetLimits fetches rate limits for the given key. Returns cached results from
// Redis if available. On cache miss, calls the external service and caches the
// response. Cache TTL is resolved as follows:
//
// HTTP: Cache-Control/Expires headers > body cache fields > default TTL.
// gRPC: body cache fields > default TTL.
//
// When the circuit breaker is open (external service unhealthy), stale cached
// data is served if available, avoiding latency spikes on the hot path.
func (ec *ExternalClient) GetLimits(ctx context.Context, req *ExternalRequest) (*ExternalLimits, error) {
	// Try Redis cache first.
	if cached := ec.getFromCache(ctx, req.Key); cached != nil {
		return cached, nil
	}

	// If circuit breaker is open, try stale cache before failing.
	if ec.isCircuitOpen() {
		if stale := ec.getStaleFromCache(ctx, req.Key); stale != nil {
			return stale, nil
		}
		return nil, fmt.Errorf("external rate limit service circuit breaker open")
	}

	// Fetch from external service.
	limits, ttl, err := ec.fetchFromService(ctx, req)
	if err != nil {
		ec.recordFailure()
		// Try stale cache as fallback on fetch failure.
		if stale := ec.getStaleFromCache(ctx, req.Key); stale != nil {
			return stale, nil
		}
		return nil, err
	}

	ec.recordSuccess()

	// Store in both primary and stale cache.
	ec.setInCache(ctx, req.Key, limits, ttl)
	ec.setStaleInCache(ctx, req.Key, limits)

	return limits, nil
}

// fetchFromService calls the external rate limit service (gRPC or HTTP).
func (ec *ExternalClient) fetchFromService(ctx context.Context, req *ExternalRequest) (*ExternalLimits, time.Duration, error) {
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

func (ec *ExternalClient) isCircuitOpen() bool {
	ec.cbMu.RLock()
	defer ec.cbMu.RUnlock()
	if !ec.cbOpen {
		return false
	}
	// Allow a probe if enough time has passed (half-open).
	return time.Now().Before(ec.cbOpenUntil)
}

func (ec *ExternalClient) recordFailure() {
	ec.cbMu.Lock()
	defer ec.cbMu.Unlock()
	ec.cbFailures++
	ec.cbLastFailure = time.Now()
	if ec.cbFailures >= ec.cbThreshold {
		ec.cbOpen = true
		ec.cbOpenUntil = time.Now().Add(ec.cbResetTimeout)
	}
}

func (ec *ExternalClient) recordSuccess() {
	ec.cbMu.Lock()
	defer ec.cbMu.Unlock()
	ec.cbFailures = 0
	ec.cbOpen = false
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
		Average:      pbResp.GetAverage(),
		Burst:        pbResp.GetBurst(),
		Period:       pbResp.GetPeriod(),
		CacheNoStore: pbResp.GetCacheNoStore(),
		TenantKey:    pbResp.GetTenantKey(),
		// TODO(proto): uncomment after running `buf generate` to regenerate stubs
		// from the updated ratelimit.proto which adds the FailurePolicy enum
		// (field 7) and failure_code (field 8). Map the proto enum to
		// config.FailurePolicy using failurePolicyFromProto().
		// FailurePolicy: failurePolicyFromProto(pbResp.GetFailurePolicy()),
		// FailureCode:   int(pbResp.GetFailureCode()),
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

// Close shuts down the external client and releases resources.
func (ec *ExternalClient) Close() error {
	if ec.grpcConn != nil {
		return ec.grpcConn.Close()
	}
	return nil
}

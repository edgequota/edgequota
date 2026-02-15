package ratelimit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	ratelimitv1 "github.com/edgequota/edgequota/api/gen/edgequota/ratelimit/v1"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/redis"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// cacheKeyPrefix is the Redis key prefix for cached external rate limit responses.
const cacheKeyPrefix = "rl:extcache:"

// ExternalLimits holds rate limit parameters returned by an external service.
// The cache control fields are optional and consistent across HTTP and gRPC.
type ExternalLimits struct {
	Average int64  `json:"average"`
	Burst   int64  `json:"burst"`
	Period  string `json:"period"`

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
type ExternalClient struct {
	httpURL    string
	grpcClient ratelimitv1.RateLimitServiceClient // generated typed client
	grpcConn   *grpc.ClientConn
	httpClient *http.Client
	timeout    time.Duration
	cacheTTL   time.Duration // default TTL when response has no cache headers

	redisClient redis.Client // may be nil (no caching)
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

	ec := &ExternalClient{
		httpURL:     cfg.HTTP.URL,
		httpClient:  &http.Client{Timeout: timeout},
		timeout:     timeout,
		cacheTTL:    cacheTTL,
		redisClient: redisClient,
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
func (ec *ExternalClient) GetLimits(ctx context.Context, req *ExternalRequest) (*ExternalLimits, error) {
	// Try Redis cache first.
	if cached := ec.getFromCache(ctx, req.Key); cached != nil {
		return cached, nil
	}

	// Fetch from external service.
	var limits *ExternalLimits
	var ttl time.Duration
	var err error

	if ec.grpcClient != nil {
		limits, err = ec.getLimitsGRPC(ctx, req)
		if err == nil {
			ttl = ec.resolveBodyCacheTTL(limits)
		}
	} else if ec.httpURL != "" {
		limits, ttl, err = ec.getLimitsHTTP(ctx, req)
	} else {
		return nil, fmt.Errorf("no external rate limit service configured")
	}

	if err != nil {
		return nil, err
	}

	// Store in Redis cache.
	ec.setInCache(ctx, req.Key, limits, ttl)

	return limits, nil
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
	if err := json.NewDecoder(resp.Body).Decode(&limits); err != nil {
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
// Returns 0 if not found or invalid.
func parseMaxAge(cc string) time.Duration {
	for _, directive := range strings.Split(cc, ",") {
		directive = strings.TrimSpace(directive)
		if strings.HasPrefix(strings.ToLower(directive), "max-age=") {
			valStr := strings.TrimPrefix(directive, directive[:len("max-age=")])
			seconds, err := strconv.Atoi(valStr)
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
	}
	if pbResp.CacheMaxAgeSeconds != nil {
		v := pbResp.GetCacheMaxAgeSeconds()
		limits.CacheMaxAgeSec = &v
	}

	return limits, nil
}

// Close shuts down the external client and releases resources.
func (ec *ExternalClient) Close() error {
	if ec.grpcConn != nil {
		return ec.grpcConn.Close()
	}
	return nil
}

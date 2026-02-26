// Package auth provides clients for calling an external authentication service.
// Both HTTP and gRPC backends are supported. When authentication is enabled,
// every incoming request is checked before rate limiting and proxying.
//
// HTTP auth works like nginx auth_request: the original request headers, method,
// path, and remote address are forwarded. A 200 response means allow; any other
// status means deny.
//
// gRPC auth uses the generated edgequota.auth.v1.AuthService client with proper
// protobuf serialization and OpenTelemetry trace propagation.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	authv1 "github.com/edgequota/edgequota/api/gen/grpc/edgequota/auth/v1"
	authv1http "github.com/edgequota/edgequota/api/gen/http/auth/v1"
	"github.com/edgequota/edgequota/internal/config"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// ErrCircuitOpen is returned when the auth circuit breaker is open and the
// call is short-circuited without contacting the auth service.
var ErrCircuitOpen = errors.New("auth circuit breaker is open")

// Circuit breaker defaults for the auth service.
const (
	defaultAuthCBThreshold    = 5
	defaultAuthCBResetTimeout = 30 * time.Second
)

// circuitBreaker protects the auth service from cascading failures. When the
// auth service is down, the breaker opens after `threshold` consecutive
// failures and short-circuits all calls for `resetTimeout`, avoiding the full
// auth timeout on every request. After the reset timeout, one probe request
// is allowed through (half-open state).
type circuitBreaker struct {
	mu           sync.Mutex
	failures     int
	open         bool
	openUntil    time.Time
	threshold    int
	resetTimeout time.Duration
}

func newCircuitBreaker(threshold int, resetTimeout time.Duration) *circuitBreaker {
	if threshold <= 0 {
		threshold = defaultAuthCBThreshold
	}
	if resetTimeout <= 0 {
		resetTimeout = defaultAuthCBResetTimeout
	}
	return &circuitBreaker{
		threshold:    threshold,
		resetTimeout: resetTimeout,
	}
}

// isOpen returns true when the circuit is open and the reset timeout has not
// yet elapsed. Once the timeout passes, the circuit enters half-open state
// (returns false) to allow a single probe request through.
func (cb *circuitBreaker) isOpen() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if !cb.open {
		return false
	}
	// Half-open: allow a probe if enough time has passed.
	return time.Now().Before(cb.openUntil)
}

func (cb *circuitBreaker) recordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures++
	if cb.failures >= cb.threshold {
		cb.open = true
		cb.openUntil = time.Now().Add(cb.resetTimeout)
	}
}

func (cb *circuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.open = false
}

// CheckRequest represents the data sent to the external auth service.
// Used as the canonical internal type for both HTTP and gRPC paths.
type CheckRequest struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Headers    map[string]string `json:"headers"`
	RemoteAddr string            `json:"remote_addr"`
	Body       []byte            `json:"body,omitempty"` // optional; included when propagate_request_body is enabled
}

// CheckResponse represents the response from the external auth service.
// Used as the canonical internal type for both HTTP and gRPC paths.
type CheckResponse struct {
	Allowed         bool              `json:"allowed"`
	StatusCode      int               `json:"status_code"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	DenyBody        string            `json:"deny_body,omitempty"`
	// RequestHeaders are injected into the upstream request when allowed.
	// Auth services use this to pass decoded metadata (e.g. tenant ID) to
	// downstream stages like rate limiting and the backend.
	RequestHeaders map[string]string `json:"request_headers,omitempty"`

	// CacheMaxAgeSec controls how long EdgeQuota caches this auth response.
	// nil = not set (no caching); > 0 = cache for N seconds. Typically set
	// to the remaining token TTL so auth is not re-checked until expiry.
	// For HTTP responses the field is normalised to reflect the winning TTL
	// source (HTTP headers take precedence over body values).
	CacheMaxAgeSec *int64 `json:"cache_max_age_seconds,omitempty"`
	// CacheNoStore prevents caching when true, even if CacheMaxAgeSec is set.
	CacheNoStore bool `json:"cache_no_store,omitempty"`

	// CacheTTLSource describes where the cache TTL decision came from.
	// Set during TTL resolution; not serialized (json:"-") because it is only
	// needed for logging at the time of the initial response before storing.
	CacheTTLSource string `json:"-"`
}

// ResolveCacheTTL returns the duration the auth response should be cached.
// Returns 0 when caching is disabled (CacheNoStore=true or no TTL set).
func (r *CheckResponse) ResolveCacheTTL() time.Duration {
	if r.CacheNoStore {
		return 0
	}
	if r.CacheMaxAgeSec != nil && *r.CacheMaxAgeSec > 0 {
		return time.Duration(*r.CacheMaxAgeSec) * time.Second
	}
	return 0
}

// resolveHTTPCacheTTL applies the same precedence rules as the external rate-
// limit service: HTTP response headers (Cache-Control, Expires) take priority
// over the body fields (cache_max_age_seconds, cache_no_store).  The winning
// directive is normalised back into resp so that ResolveCacheTTL() always
// returns the correct value regardless of transport.  CacheTTLSource is set
// to a human-readable description of the winning source for debug logging.
//
// Priority order:
//  1. Cache-Control: max-age=N  →  cache for N seconds
//  2. Cache-Control: no-cache | no-store  →  do not cache
//  3. Expires: <RFC1123>  →  cache until that date
//  4. Body: cache_max_age_seconds / cache_no_store  →  body-driven caching
func resolveHTTPCacheTTL(h http.Header, resp *CheckResponse) {
	// 1. Cache-Control header.
	if cc := h.Get("Cache-Control"); cc != "" {
		if secs := parseAuthMaxAge(cc); secs > 0 {
			resp.CacheMaxAgeSec = &secs
			resp.CacheNoStore = false
			resp.CacheTTLSource = fmt.Sprintf("Cache-Control: max-age=%d", secs)
			return
		}
		lower := strings.ToLower(cc)
		if strings.Contains(lower, "no-cache") || strings.Contains(lower, "no-store") {
			resp.CacheMaxAgeSec = nil
			resp.CacheNoStore = true
			resp.CacheTTLSource = "Cache-Control: " + lower
			return
		}
	}

	// 2. Expires header.
	if expires := h.Get("Expires"); expires != "" {
		expTime, err := http.ParseTime(expires)
		if err == nil {
			ttl := time.Until(expTime)
			if ttl > 0 {
				secs := int64(ttl.Seconds())
				resp.CacheMaxAgeSec = &secs
				resp.CacheNoStore = false
				resp.CacheTTLSource = fmt.Sprintf("Expires: %s", expires)
				return
			}
			// Already expired — treat as no-store.
			resp.CacheMaxAgeSec = nil
			resp.CacheNoStore = true
			resp.CacheTTLSource = fmt.Sprintf("Expires: %s (already expired)", expires)
			return
		}
	}

	// 3. Body fields (fallback, consistent with gRPC path).
	if resp.CacheNoStore {
		resp.CacheTTLSource = "body: cache_no_store=true"
		return
	}
	if resp.CacheMaxAgeSec != nil && *resp.CacheMaxAgeSec > 0 {
		resp.CacheTTLSource = fmt.Sprintf("body: cache_max_age_seconds=%d", *resp.CacheMaxAgeSec)
		return
	}
	// No directives — source remains empty ("" means no caching requested).
}

// parseAuthMaxAge extracts max-age seconds from a Cache-Control header value.
// Returns 0 when not found or invalid. Case-insensitive per RFC 7234.
func parseAuthMaxAge(cc string) int64 {
	for _, directive := range strings.Split(cc, ",") {
		directive = strings.TrimSpace(directive)
		lower := strings.ToLower(directive)
		if after, found := strings.CutPrefix(lower, "max-age="); found {
			if n, err := strconv.ParseInt(strings.TrimSpace(after), 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// defaultMaxAuthBodySize is the default maximum request body size (64 KiB)
// forwarded to the auth service when propagate_request_body is enabled.
const defaultMaxAuthBodySize = 64 * 1024

// Client calls an external authentication service to verify requests.
type Client struct {
	httpURL                string
	grpcClient             authv1.AuthServiceClient // generated typed client
	grpcConn               *grpc.ClientConn
	httpClient             *http.Client
	timeout                time.Duration
	headerFilter           *config.HeaderFilter
	forwardOriginalHeaders bool // when true, forward X-Original-* HTTP headers
	propagateBody          bool // when true, include request body in auth check
	maxBodySize            int64
	cb                     *circuitBreaker
}

// NewClient creates an auth client from the configuration.
func NewClient(cfg config.AuthConfig) (*Client, error) {
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil {
		timeout = 5 * time.Second
	}

	// Custom transport with tuned connection pool for high-concurrency auth checks.
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100, // Auth is typically a single host.
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	maxBodySize := cfg.MaxAuthBodySize
	if maxBodySize <= 0 {
		maxBodySize = defaultMaxAuthBodySize
	}

	// When no header filter is configured, default to a deny-list that strips
	// session/CSRF tokens but keeps Authorization and X-Api-Key (unlike
	// DefaultSensitiveHeaders which strips them). This preserves full request
	// context for the auth service while still preventing cookie-based session
	// tokens from leaking to an external service unnecessarily.
	hfCfg := cfg.HeaderFilter
	if len(hfCfg.AllowList) == 0 && len(hfCfg.DenyList) == 0 {
		hfCfg.DenyList = config.DefaultAuthDenyHeaders
	}

	c := &Client{
		httpURL:                cfg.HTTP.URL,
		httpClient:             &http.Client{Timeout: timeout, Transport: transport},
		timeout:                timeout,
		headerFilter:           config.NewHeaderFilter(hfCfg),
		forwardOriginalHeaders: cfg.HTTP.ForwardOriginalHeaders,
		propagateBody:          cfg.PropagateRequestBody,
		maxBodySize:            maxBodySize,
		cb: newCircuitBreaker(
			cfg.CircuitBreaker.Threshold,
			config.MustParseDuration(cfg.CircuitBreaker.ResetTimeout, 0),
		),
	}

	if cfg.GRPC.Address != "" {
		var creds credentials.TransportCredentials
		if cfg.GRPC.TLS.Enabled {
			serverName := cfg.GRPC.TLS.ResolveServerName(cfg.GRPC.Address)
			tlsCreds, tlsErr := credentials.NewClientTLSFromFile(cfg.GRPC.TLS.CAFile, serverName)
			if tlsErr != nil {
				return nil, fmt.Errorf("auth grpc tls: %w", tlsErr)
			}
			creds = tlsCreds
		} else {
			creds = insecure.NewCredentials()
		}

		conn, dialErr := grpc.NewClient(cfg.GRPC.Address,
			grpc.WithTransportCredentials(creds),
			grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64*1024)), // 64 KiB — bound response size from auth service.
		)
		if dialErr != nil {
			return nil, fmt.Errorf("auth grpc dial: %w", dialErr)
		}
		c.grpcConn = conn
		c.grpcClient = authv1.NewAuthServiceClient(conn)
	}

	return c, nil
}

// Check verifies the request with the external auth service.
// Returns the CheckResponse, or an error if the service is unreachable.
// When the circuit breaker is open, returns ErrCircuitOpen immediately
// without contacting the auth service, avoiding the full auth timeout.
func (c *Client) Check(ctx context.Context, req *CheckRequest) (*CheckResponse, error) {
	if c.cb.isOpen() {
		return nil, ErrCircuitOpen
	}

	var resp *CheckResponse
	var err error
	if c.grpcClient != nil {
		resp, err = c.checkGRPC(ctx, req)
	} else {
		resp, err = c.checkHTTP(ctx, req)
	}

	if err != nil {
		c.cb.recordFailure()
		return nil, err
	}
	c.cb.recordSuccess()
	return resp, nil
}

func (c *Client) checkHTTP(ctx context.Context, req *CheckRequest) (*CheckResponse, error) {
	wireReq := authv1http.CheckRequest{
		Method:     req.Method,
		Path:       req.Path,
		Headers:    req.Headers,
		RemoteAddr: req.RemoteAddr,
	}
	if len(req.Body) > 0 {
		wireReq.Body = &req.Body
	}

	body, err := json.Marshal(wireReq)
	if err != nil {
		return nil, fmt.Errorf("marshal auth request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.httpURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create auth request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	if c.forwardOriginalHeaders {
		for k, v := range req.Headers {
			httpReq.Header.Set("X-Original-"+k, v)
		}
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("auth http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode == http.StatusOK {
		result := &CheckResponse{Allowed: true, StatusCode: 200}
		var parsed authv1http.CheckResponse
		if len(respBody) > 0 && json.Unmarshal(respBody, &parsed) == nil {
			result.RequestHeaders = derefStringMap(parsed.RequestHeaders)
			if parsed.CacheMaxAgeSeconds != nil {
				v := *parsed.CacheMaxAgeSeconds
				result.CacheMaxAgeSec = &v
			}
			if parsed.CacheNoStore != nil {
				result.CacheNoStore = *parsed.CacheNoStore
			}
		}
		// HTTP headers take precedence over body fields; source is tracked for logging.
		resolveHTTPCacheTTL(resp.Header, result)
		return result, nil
	}

	var wireResp authv1http.CheckResponse
	if err := json.Unmarshal(respBody, &wireResp); err == nil && wireResp.StatusCode != 0 {
		result := checkResponseFromHTTP(&wireResp)
		resolveHTTPCacheTTL(resp.Header, result)
		return result, nil
	}

	return &CheckResponse{
		Allowed:    false,
		StatusCode: resp.StatusCode,
		DenyBody:   string(respBody),
	}, nil
}

// checkResponseFromHTTP converts the generated OpenAPI response type to the
// internal CheckResponse domain type.
func checkResponseFromHTTP(r *authv1http.CheckResponse) *CheckResponse {
	resp := &CheckResponse{
		Allowed:    r.Allowed,
		StatusCode: int(r.StatusCode),
	}
	if r.DenyBody != nil {
		resp.DenyBody = *r.DenyBody
	}
	resp.ResponseHeaders = derefStringMap(r.ResponseHeaders)
	resp.RequestHeaders = derefStringMap(r.RequestHeaders)
	if r.CacheMaxAgeSeconds != nil {
		v := *r.CacheMaxAgeSeconds
		resp.CacheMaxAgeSec = &v
	}
	if r.CacheNoStore != nil {
		resp.CacheNoStore = *r.CacheNoStore
	}
	return resp
}

func derefStringMap(p *map[string]string) map[string]string {
	if p == nil {
		return nil
	}
	return *p
}

func (c *Client) checkGRPC(ctx context.Context, req *CheckRequest) (*CheckResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	pbReq := &authv1.CheckRequest{
		Method:     req.Method,
		Path:       req.Path,
		Headers:    req.Headers,
		RemoteAddr: req.RemoteAddr,
		Body:       req.Body,
	}

	pbResp, err := c.grpcClient.Check(ctx, pbReq)
	if err != nil {
		return nil, fmt.Errorf("auth grpc check: %w", err)
	}

	resp := &CheckResponse{
		Allowed:      pbResp.GetAllowed(),
		StatusCode:   int(pbResp.GetStatusCode()),
		DenyBody:     pbResp.GetDenyBody(),
		CacheNoStore: pbResp.GetCacheNoStore(),
	}
	if len(pbResp.GetResponseHeaders()) > 0 {
		resp.ResponseHeaders = pbResp.GetResponseHeaders()
	}
	if len(pbResp.GetRequestHeaders()) > 0 {
		resp.RequestHeaders = pbResp.GetRequestHeaders()
	}
	if pbResp.CacheMaxAgeSeconds != nil {
		v := pbResp.GetCacheMaxAgeSeconds()
		resp.CacheMaxAgeSec = &v
	}

	// gRPC has no HTTP headers — set the source from body fields for logging.
	if resp.CacheNoStore {
		resp.CacheTTLSource = "body: cache_no_store=true"
	} else if resp.CacheMaxAgeSec != nil && *resp.CacheMaxAgeSec > 0 {
		resp.CacheTTLSource = fmt.Sprintf("body: cache_max_age_seconds=%d", *resp.CacheMaxAgeSec)
	}

	return resp, nil
}

// Close releases resources held by the auth client.
func (c *Client) Close() error {
	if c.httpClient != nil {
		c.httpClient.CloseIdleConnections()
	}
	if c.grpcConn != nil {
		return c.grpcConn.Close()
	}
	return nil
}

// PropagateBody returns whether request body propagation is enabled.
func (c *Client) PropagateBody() bool { return c.propagateBody }

// MaxBodySize returns the maximum body size for auth body propagation.
func (c *Client) MaxBodySize() int64 { return c.maxBodySize }

// BuildCheckRequest creates a CheckRequest from an http.Request, applying
// the client's header filter to strip sensitive headers before forwarding.
// When body is non-nil, it is included in the request for auth services that
// need to inspect the request payload (e.g. for request signing or content-based auth).
func (c *Client) BuildCheckRequest(r *http.Request, body []byte) *CheckRequest {
	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 && c.headerFilter.Allowed(k) {
			headers[k] = v[0]
		}
	}
	// Go promotes the Host header (and HTTP/2 :authority) to r.Host and
	// removes it from r.Header. Re-inject it so auth services can make
	// per-host decisions.
	if r.Host != "" {
		headers["Host"] = r.Host
	}

	return &CheckRequest{
		Method:     r.Method,
		Path:       r.URL.Path,
		Headers:    headers,
		RemoteAddr: r.RemoteAddr,
		Body:       body,
	}
}

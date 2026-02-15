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
	"sync"
	"time"

	authv1 "github.com/edgequota/edgequota/api/gen/edgequota/auth/v1"
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

func newCircuitBreaker() *circuitBreaker {
	return &circuitBreaker{
		threshold:    defaultAuthCBThreshold,
		resetTimeout: defaultAuthCBResetTimeout,
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
}

// Client calls an external authentication service to verify requests.
type Client struct {
	httpURL                string
	grpcClient             authv1.AuthServiceClient // generated typed client
	grpcConn               *grpc.ClientConn
	httpClient             *http.Client
	timeout                time.Duration
	headerFilter           *config.HeaderFilter
	forwardOriginalHeaders bool // when true, forward X-Original-* HTTP headers
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

	c := &Client{
		httpURL:                cfg.HTTP.URL,
		httpClient:             &http.Client{Timeout: timeout, Transport: transport},
		timeout:                timeout,
		headerFilter:           config.NewHeaderFilter(cfg.HeaderFilter),
		forwardOriginalHeaders: cfg.HTTP.ForwardOriginalHeaders,
		cb:                     newCircuitBreaker(),
	}

	if cfg.GRPC.Address != "" {
		var creds credentials.TransportCredentials
		if cfg.GRPC.TLS.Enabled {
			tlsCreds, tlsErr := credentials.NewClientTLSFromFile(cfg.GRPC.TLS.CAFile, "")
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
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal auth request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.httpURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create auth request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	// Optionally forward original request headers as X-Original-* HTTP headers.
	// Disabled by default to prevent credential leakage via a secondary channel;
	// the request body already contains the filtered headers as JSON.
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

	// Read response body — used for deny message or allow metadata (request_headers).
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode == http.StatusOK {
		result := &CheckResponse{Allowed: true, StatusCode: 200}
		// Try to parse a structured response to extract request_headers.
		var parsed CheckResponse
		if len(respBody) > 0 && json.Unmarshal(respBody, &parsed) == nil {
			result.RequestHeaders = parsed.RequestHeaders
		}
		return result, nil
	}

	// Try to parse structured response.
	var checkResp CheckResponse
	if err := json.Unmarshal(respBody, &checkResp); err == nil && checkResp.StatusCode != 0 {
		return &checkResp, nil
	}

	// Fall back to status-code based response.
	return &CheckResponse{
		Allowed:    false,
		StatusCode: resp.StatusCode,
		DenyBody:   string(respBody),
	}, nil
}

func (c *Client) checkGRPC(ctx context.Context, req *CheckRequest) (*CheckResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	pbReq := &authv1.CheckRequest{
		Method:     req.Method,
		Path:       req.Path,
		Headers:    req.Headers,
		RemoteAddr: req.RemoteAddr,
	}

	pbResp, err := c.grpcClient.Check(ctx, pbReq)
	if err != nil {
		return nil, fmt.Errorf("auth grpc check: %w", err)
	}

	resp := &CheckResponse{
		Allowed:    pbResp.GetAllowed(),
		StatusCode: int(pbResp.GetStatusCode()),
		DenyBody:   pbResp.GetDenyBody(),
	}
	if len(pbResp.GetResponseHeaders()) > 0 {
		resp.ResponseHeaders = pbResp.GetResponseHeaders()
	}
	if len(pbResp.GetRequestHeaders()) > 0 {
		resp.RequestHeaders = pbResp.GetRequestHeaders()
	}

	return resp, nil
}

// Close releases resources held by the auth client.
func (c *Client) Close() error {
	if c.grpcConn != nil {
		return c.grpcConn.Close()
	}
	return nil
}

// BuildCheckRequest creates a CheckRequest from an http.Request, applying
// the client's header filter to strip sensitive headers before forwarding.
func (c *Client) BuildCheckRequest(r *http.Request) *CheckRequest {
	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 && c.headerFilter.Allowed(k) {
			headers[k] = v[0]
		}
	}

	return &CheckRequest{
		Method:     r.Method,
		Path:       r.URL.Path,
		Headers:    headers,
		RemoteAddr: r.RemoteAddr,
	}
}

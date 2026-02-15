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
	"fmt"
	"io"
	"net/http"
	"time"

	authv1 "github.com/edgequota/edgequota/api/gen/edgequota/auth/v1"
	"github.com/edgequota/edgequota/internal/config"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

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
	httpURL    string
	grpcClient authv1.AuthServiceClient // generated typed client
	grpcConn   *grpc.ClientConn
	httpClient *http.Client
	timeout    time.Duration
}

// NewClient creates an auth client from the configuration.
func NewClient(cfg config.AuthConfig) (*Client, error) {
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil {
		timeout = 5 * time.Second
	}

	c := &Client{
		httpURL:    cfg.HTTP.URL,
		httpClient: &http.Client{Timeout: timeout},
		timeout:    timeout,
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
func (c *Client) Check(ctx context.Context, req *CheckRequest) (*CheckResponse, error) {
	if c.grpcClient != nil {
		return c.checkGRPC(ctx, req)
	}
	return c.checkHTTP(ctx, req)
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

	// Forward original request headers for context.
	for k, v := range req.Headers {
		httpReq.Header.Set("X-Original-"+k, v)
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

// BuildCheckRequest creates a CheckRequest from an http.Request.
func BuildCheckRequest(r *http.Request) *CheckRequest {
	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 {
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

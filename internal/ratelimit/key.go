package ratelimit

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/edgequota/edgequota/internal/config"
)

// Key strategy type aliases for local readability.
const (
	ksClientIP  = config.KeyStrategyClientIP
	ksHeader    = config.KeyStrategyHeader
	ksComposite = config.KeyStrategyComposite
)

// KeyStrategy extracts a rate-limit key from an HTTP request.
type KeyStrategy interface {
	Extract(req *http.Request) (string, error)
}

// ClientIPStrategy extracts the client IP from standard proxy headers or RemoteAddr.
type ClientIPStrategy struct{}

// Extract returns the client IP, checking X-Forwarded-For, X-Real-IP, then RemoteAddr.
func (s *ClientIPStrategy) Extract(req *http.Request) (string, error) {
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.SplitN(xff, ",", 2)
		if ip := strings.TrimSpace(parts[0]); ip != "" {
			return ip, nil
		}
	}

	if xri := req.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri), nil
	}

	ip, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr, nil
	}

	return ip, nil
}

// HeaderStrategy extracts a value from a specific request header.
type HeaderStrategy struct {
	HeaderName string
}

// Extract returns the header value, or an error if the header is missing/empty.
func (s *HeaderStrategy) Extract(req *http.Request) (string, error) {
	v := req.Header.Get(s.HeaderName)
	if v == "" {
		return "", fmt.Errorf("header %q is empty or missing", s.HeaderName)
	}
	return v, nil
}

// CompositeStrategy combines a header value with an optional first path segment.
type CompositeStrategy struct {
	HeaderName string
	PathPrefix bool
}

// Extract returns the composite key: header value, optionally suffixed with the
// first path segment (e.g. "tenant-123:api").
func (s *CompositeStrategy) Extract(req *http.Request) (string, error) {
	hv := req.Header.Get(s.HeaderName)
	if hv == "" {
		return "", fmt.Errorf("header %q is empty or missing", s.HeaderName)
	}

	if s.PathPrefix {
		prefix := FirstPathSegment(req.URL.Path)
		return hv + ":" + prefix, nil
	}

	return hv, nil
}

// FirstPathSegment returns the first non-empty path segment.
// e.g. "/api/v1/users" → "api", "/" → "root".
func FirstPathSegment(path string) string {
	path = strings.TrimPrefix(path, "/")
	if idx := strings.IndexByte(path, '/'); idx > 0 {
		return path[:idx]
	}
	if path == "" {
		return "root"
	}
	return path
}

// NewKeyStrategy creates a KeyStrategy from the configuration.
func NewKeyStrategy(cfg config.KeyStrategyConfig) (KeyStrategy, error) {
	switch cfg.Type {
	case ksClientIP, "":
		return &ClientIPStrategy{}, nil
	case ksHeader:
		if cfg.HeaderName == "" {
			return nil, fmt.Errorf("header_name is required when type is %q", cfg.Type)
		}
		return &HeaderStrategy{HeaderName: http.CanonicalHeaderKey(cfg.HeaderName)}, nil
	case ksComposite:
		if cfg.HeaderName == "" {
			return nil, fmt.Errorf("header_name is required when type is %q", cfg.Type)
		}
		return &CompositeStrategy{
			HeaderName: http.CanonicalHeaderKey(cfg.HeaderName),
			PathPrefix: cfg.PathPrefix,
		}, nil
	default:
		return nil, fmt.Errorf("unknown key strategy type %q: must be clientip, header, or composite", cfg.Type)
	}
}

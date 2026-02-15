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

// ClientIPStrategy extracts the client IP from standard proxy headers or
// RemoteAddr. When TrustedProxies is configured, proxy headers (X-Forwarded-For,
// X-Real-IP) are only honoured when RemoteAddr falls within a trusted CIDR range.
// TrustedIPDepth controls which X-Forwarded-For entry is selected (0 = leftmost,
// N > 0 = Nth from the right).
type ClientIPStrategy struct {
	trustedNets []*net.IPNet
	ipDepth     int
}

// remoteIP extracts the IP portion of RemoteAddr.
func remoteIP(remoteAddr string) string {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return ip
}

// isTrusted returns true when trustedNets is empty (legacy mode) or when the
// given IP falls within at least one configured CIDR.
func (s *ClientIPStrategy) isTrusted(ipStr string) bool {
	if len(s.trustedNets) == 0 {
		return true // no trusted proxies configured — trust everything (legacy)
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range s.trustedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Extract returns the client IP, checking X-Forwarded-For, X-Real-IP, then RemoteAddr.
// Proxy headers are only trusted when the caller's RemoteAddr is in TrustedProxies.
func (s *ClientIPStrategy) Extract(req *http.Request) (string, error) {
	callerIP := remoteIP(req.RemoteAddr)

	if s.isTrusted(callerIP) {
		if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}

			idx := 0 // default: leftmost (original client)
			if s.ipDepth > 0 && s.ipDepth <= len(parts) {
				idx = len(parts) - s.ipDepth // Nth from the right
			}

			if ip := parts[idx]; ip != "" {
				return ip, nil
			}
		}

		if xri := req.Header.Get("X-Real-IP"); xri != "" {
			return strings.TrimSpace(xri), nil
		}
	}

	return callerIP, nil
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

// parseTrustedProxies converts CIDR strings to net.IPNet entries.
// Returns an error on the first invalid CIDR.
func parseTrustedProxies(cidrs []string) ([]*net.IPNet, error) {
	if len(cidrs) == 0 {
		return nil, nil
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		// Accept bare IPs by appending /32 or /128.
		if !strings.Contains(cidr, "/") {
			if strings.Contains(cidr, ":") {
				cidr += "/128"
			} else {
				cidr += "/32"
			}
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted_proxies CIDR %q: %w", cidr, err)
		}
		nets = append(nets, ipNet)
	}
	return nets, nil
}

// NewKeyStrategy creates a KeyStrategy from the configuration.
func NewKeyStrategy(cfg config.KeyStrategyConfig) (KeyStrategy, error) {
	switch cfg.Type {
	case ksClientIP, "":
		nets, err := parseTrustedProxies(cfg.TrustedProxies)
		if err != nil {
			return nil, err
		}
		return &ClientIPStrategy{
			trustedNets: nets,
			ipDepth:     cfg.TrustedIPDepth,
		}, nil
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

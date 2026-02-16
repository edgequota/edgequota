// Package proxy contains backend URL validation to prevent SSRF attacks.
package proxy

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// BackendURLPolicy controls which dynamic backend URLs are allowed.
type BackendURLPolicy struct {
	// AllowedSchemes restricts the URL scheme. Default: ["http", "https"].
	AllowedSchemes []string
	// DenyPrivateNetworks blocks RFC 1918, loopback, link-local, and cloud
	// metadata IPs when true. Default: true.
	DenyPrivateNetworks bool
	// AllowedHosts is an optional allowlist. When non-empty, only these
	// hosts (exact match, case-insensitive) are permitted. Port is ignored.
	AllowedHosts []string
}

// privateNetworks contains CIDR ranges that are considered private/internal.
var privateNetworks = func() []*net.IPNet {
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16", // link-local + cloud metadata (169.254.169.254)
		"::1/128",
		"fc00::/7",  // unique local
		"fe80::/10", // link-local v6
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, _ := net.ParseCIDR(c)
		nets = append(nets, n)
	}
	return nets
}()

// ValidateBackendURL checks that a dynamically provided backend URL is safe
// to proxy to. Returns an error describing the rejection reason.
func ValidateBackendURL(u *url.URL, policy BackendURLPolicy) error {
	// Scheme check.
	schemes := policy.AllowedSchemes
	if len(schemes) == 0 {
		schemes = []string{"http", "https"}
	}
	schemeOK := false
	for _, s := range schemes {
		if strings.EqualFold(u.Scheme, s) {
			schemeOK = true
			break
		}
	}
	if !schemeOK {
		return fmt.Errorf("scheme %q is not allowed", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("empty host")
	}

	// Host allowlist check.
	if len(policy.AllowedHosts) > 0 {
		allowed := false
		for _, h := range policy.AllowedHosts {
			if strings.EqualFold(host, h) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("host %q is not in the allowed list", host)
		}
		// If the host is on the allowlist, skip private-network checks —
		// the operator explicitly trusts this host.
		return nil
	}

	// Private network check.
	if policy.DenyPrivateNetworks {
		if err := checkNotPrivate(host); err != nil {
			return err
		}
	}

	return nil
}

// checkNotPrivate resolves the host to IPs and rejects any that fall within
// private or reserved ranges. This prevents SSRF via DNS rebinding or direct
// IP specification.
func checkNotPrivate(host string) error {
	// Direct IP check (no DNS needed).
	if ip := net.ParseIP(host); ip != nil {
		if IsPrivateIP(ip) {
			return fmt.Errorf("IP %s is in a private/reserved range", ip)
		}
		return nil
	}

	// DNS resolution to catch hostnames pointing to private IPs.
	ips, err := net.LookupIP(host)
	if err != nil {
		// Resolution failure is treated as blocked to prevent bypass via
		// unresolvable hostnames that resolve differently at proxy time.
		return fmt.Errorf("cannot resolve host %q: %w", host, err)
	}

	for _, ip := range ips {
		if IsPrivateIP(ip) {
			return fmt.Errorf("host %q resolves to private IP %s", host, ip)
		}
	}

	return nil
}

// IsPrivateIP reports whether the IP falls within any private/reserved range.
// Exported so the safe dialer can reuse the same check at connect time to
// prevent DNS rebinding attacks (TOCTOU between validation and dial).
func IsPrivateIP(ip net.IP) bool {
	for _, n := range privateNetworks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

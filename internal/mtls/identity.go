// Package mtls provides client certificate identity extraction and header
// injection for mTLS-authenticated requests. It is designed to be called
// early in the middleware chain so that auth, rate-limiting, and the proxy
// layer all see verified client identity. Headers are non-spoofable:
// inbound values are always stripped before being set from TLS state.
package mtls

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"net/http"
)

// Canonical header names. Go's net/http canonicalises on Set/Get, but we
// use consistent Title-Case-With-Acronyms in source for greppability.
const (
	HeaderMTLS              = "X-EdgeQuota-MTLS"
	HeaderClientFingerprint = "X-EdgeQuota-Client-Fingerprint-SHA256"
	HeaderClientSerial      = "X-EdgeQuota-Client-Serial"
	HeaderClientSubject     = "X-EdgeQuota-Client-Subject"
)

// IdentityHeaders is the full set of mTLS identity headers that must be
// sanitized (deleted from inbound, set by EdgeQuota).
var IdentityHeaders = []string{
	HeaderMTLS,
	HeaderClientFingerprint,
	HeaderClientSerial,
	HeaderClientSubject,
}

// Identity holds the extracted fields from a verified client certificate.
type Identity struct {
	Fingerprint string // SHA-256 hex of the leaf DER bytes
	Serial      string // leaf.SerialNumber.String()
	Subject     string // leaf.Subject.String()
}

// ExtractIdentity extracts identity fields from a verified peer certificate.
func ExtractIdentity(leaf *x509.Certificate) Identity {
	h := sha256.Sum256(leaf.Raw)
	return Identity{
		Fingerprint: hex.EncodeToString(h[:]),
		Serial:      leaf.SerialNumber.String(),
		Subject:     leaf.Subject.String(),
	}
}

// SanitizeHeaders removes all mTLS identity headers from the request,
// preventing clients from spoofing identity. This MUST be called before
// any identity headers are set.
func SanitizeHeaders(h http.Header) {
	for _, name := range IdentityHeaders {
		h.Del(name)
	}
}

// InjectHeaders derives mTLS identity from TLS connection state and sets
// the canonical X-EdgeQuota-* headers on the request.
//
// Authentication is only considered valid when the TLS stack has completed
// chain verification (VerifiedChains populated). A request may carry
// PeerCertificates without verified chains when the listener uses
// RequestClientCert or RequireAnyClientCert — those modes do NOT verify
// the chain. In that case the headers report "false" to prevent treating
// an unverified cert as authenticated.
//
// This function always calls SanitizeHeaders first to prevent spoofing.
func InjectHeaders(r *http.Request) {
	SanitizeHeaders(r.Header)

	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		r.Header.Set(HeaderMTLS, "false")
		return
	}

	leaf := r.TLS.VerifiedChains[0][0]
	id := ExtractIdentity(leaf)

	r.Header.Set(HeaderMTLS, "true")
	r.Header.Set(HeaderClientFingerprint, id.Fingerprint)
	r.Header.Set(HeaderClientSerial, id.Serial)
	r.Header.Set(HeaderClientSubject, id.Subject)
}

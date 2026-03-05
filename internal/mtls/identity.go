// Package mtls provides client certificate identity extraction and header
// injection for mTLS-authenticated requests. It is designed to be used by
// the proxy layer to forward verified client identity to upstream services
// in a non-spoofable manner.
package mtls

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"net/http"
)

// Header names injected by EdgeQuota when forwarding mTLS identity.
// These are ALWAYS overwritten (never trusted from inbound requests)
// to prevent spoofing.
const (
	HeaderMTLS              = "X-Edgequota-Mtls"
	HeaderClientFingerprint = "X-Edgequota-Client-Fingerprint-Sha256"
	HeaderClientSerial      = "X-Edgequota-Client-Serial"
	HeaderClientSubject     = "X-Edgequota-Client-Subject"
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

// InjectHeaders sets mTLS identity headers on the request. If the request
// has verified peer certificates (from r.TLS.PeerCertificates), identity
// is extracted and injected. Otherwise, headers indicate no mTLS.
//
// This function always calls SanitizeHeaders first to prevent spoofing.
func InjectHeaders(r *http.Request) {
	SanitizeHeaders(r.Header)

	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		r.Header.Set(HeaderMTLS, "false")
		return
	}

	leaf := r.TLS.PeerCertificates[0]
	id := ExtractIdentity(leaf)

	r.Header.Set(HeaderMTLS, "true")
	r.Header.Set(HeaderClientFingerprint, id.Fingerprint)
	r.Header.Set(HeaderClientSerial, id.Serial)
	r.Header.Set(HeaderClientSubject, id.Subject)
}

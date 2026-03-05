package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func selfSignedCert(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial := big.NewInt(42)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "test-device-001",
			Organization: []string{"Acme Corp"},
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(derBytes)
	require.NoError(t, err)
	return cert
}

// verifiedTLSState builds a ConnectionState that mimics what the Go TLS
// stack produces when RequireAndVerifyClientCert succeeds: both
// PeerCertificates and VerifiedChains are populated.
func verifiedTLSState(leaf *x509.Certificate) *tls.ConnectionState {
	return &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf},
		VerifiedChains:   [][]*x509.Certificate{{leaf}},
	}
}

func TestExtractIdentity(t *testing.T) {
	cert := selfSignedCert(t)
	id := ExtractIdentity(cert)

	expectedHash := sha256.Sum256(cert.Raw)
	expectedFP := hex.EncodeToString(expectedHash[:])

	assert.Equal(t, expectedFP, id.Fingerprint, "fingerprint must be sha256(DER) hex")
	assert.Equal(t, "42", id.Serial, "serial must match")
	assert.Contains(t, id.Subject, "test-device-001", "subject must contain CN")
	assert.Contains(t, id.Subject, "Acme Corp", "subject must contain O")
}

func TestSanitizeHeaders(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderMTLS, "true")
	h.Set(HeaderClientFingerprint, "spoofed-fp")
	h.Set(HeaderClientSerial, "spoofed-serial")
	h.Set(HeaderClientSubject, "spoofed-subject")
	h.Set("X-Custom", "keep-me")

	SanitizeHeaders(h)

	assert.Empty(t, h.Get(HeaderMTLS))
	assert.Empty(t, h.Get(HeaderClientFingerprint))
	assert.Empty(t, h.Get(HeaderClientSerial))
	assert.Empty(t, h.Get(HeaderClientSubject))
	assert.Equal(t, "keep-me", h.Get("X-Custom"))
}

func TestInjectHeaders_NoTLS(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/device/bootstrap", nil)
	r.Header.Set(HeaderMTLS, "spoofed")
	r.Header.Set(HeaderClientFingerprint, "spoofed-fp")

	InjectHeaders(r)

	assert.Equal(t, "false", r.Header.Get(HeaderMTLS))
	assert.Empty(t, r.Header.Get(HeaderClientFingerprint))
	assert.Empty(t, r.Header.Get(HeaderClientSerial))
	assert.Empty(t, r.Header.Get(HeaderClientSubject))
}

func TestInjectHeaders_TLSNoPeerCerts(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/device/bootstrap", nil)
	r.TLS = &tls.ConnectionState{}
	r.Header.Set(HeaderMTLS, "spoofed")

	InjectHeaders(r)

	assert.Equal(t, "false", r.Header.Get(HeaderMTLS))
	assert.Empty(t, r.Header.Get(HeaderClientFingerprint))
}

func TestInjectHeaders_PeerCertsButNoVerifiedChains(t *testing.T) {
	cert := selfSignedCert(t)

	r := httptest.NewRequest(http.MethodGet, "/v1/device/bootstrap", nil)
	r.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{cert},
		// VerifiedChains intentionally empty — simulates RequestClientCert
		// or RequireAnyClientCert where the cert is presented but the
		// chain is not verified by the TLS stack.
	}
	r.Header.Set(HeaderMTLS, "true")
	r.Header.Set(HeaderClientFingerprint, "spoofed-fp")

	InjectHeaders(r)

	assert.Equal(t, "false", r.Header.Get(HeaderMTLS),
		"must be false when chain is not verified")
	assert.Empty(t, r.Header.Get(HeaderClientFingerprint),
		"unverified cert must not produce identity headers")
	assert.Empty(t, r.Header.Get(HeaderClientSerial))
	assert.Empty(t, r.Header.Get(HeaderClientSubject))
}

func TestInjectHeaders_WithVerifiedCert(t *testing.T) {
	cert := selfSignedCert(t)

	r := httptest.NewRequest(http.MethodGet, "/v1/device/bootstrap", nil)
	r.TLS = verifiedTLSState(cert)
	r.Header.Set(HeaderMTLS, "spoofed-should-be-overwritten")
	r.Header.Set(HeaderClientFingerprint, "spoofed-fp")

	InjectHeaders(r)

	assert.Equal(t, "true", r.Header.Get(HeaderMTLS))

	expectedHash := sha256.Sum256(cert.Raw)
	expectedFP := hex.EncodeToString(expectedHash[:])
	assert.Equal(t, expectedFP, r.Header.Get(HeaderClientFingerprint))
	assert.Equal(t, "42", r.Header.Get(HeaderClientSerial))
	assert.Contains(t, r.Header.Get(HeaderClientSubject), "test-device-001")
}

func TestInjectHeaders_AntiSpoofing(t *testing.T) {
	t.Run("spoofed headers are overwritten even without TLS", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(HeaderMTLS, "true")
		r.Header.Set(HeaderClientFingerprint, "evil-fingerprint")
		r.Header.Set(HeaderClientSerial, "evil-serial")
		r.Header.Set(HeaderClientSubject, "CN=evil")

		InjectHeaders(r)

		assert.Equal(t, "false", r.Header.Get(HeaderMTLS),
			"must be false when no TLS connection")
		assert.Empty(t, r.Header.Get(HeaderClientFingerprint),
			"spoofed fingerprint must be removed")
		assert.Empty(t, r.Header.Get(HeaderClientSerial),
			"spoofed serial must be removed")
		assert.Empty(t, r.Header.Get(HeaderClientSubject),
			"spoofed subject must be removed")
	})

	t.Run("spoofed headers are overwritten with real verified cert values", func(t *testing.T) {
		cert := selfSignedCert(t)

		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.TLS = verifiedTLSState(cert)
		r.Header.Set(HeaderMTLS, "false")
		r.Header.Set(HeaderClientFingerprint, "evil-fingerprint")
		r.Header.Set(HeaderClientSerial, "evil-serial")
		r.Header.Set(HeaderClientSubject, "CN=evil")

		InjectHeaders(r)

		assert.Equal(t, "true", r.Header.Get(HeaderMTLS))
		assert.NotEqual(t, "evil-fingerprint", r.Header.Get(HeaderClientFingerprint))
		assert.Equal(t, "42", r.Header.Get(HeaderClientSerial))
		assert.Contains(t, r.Header.Get(HeaderClientSubject), "test-device-001")
	})

	t.Run("unverified cert does not produce identity even with PeerCertificates", func(t *testing.T) {
		cert := selfSignedCert(t)

		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.TLS = &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{cert},
		}
		r.Header.Set(HeaderMTLS, "true")
		r.Header.Set(HeaderClientFingerprint, "evil-fingerprint")

		InjectHeaders(r)

		assert.Equal(t, "false", r.Header.Get(HeaderMTLS),
			"must be false when chain is not verified")
		assert.Empty(t, r.Header.Get(HeaderClientFingerprint),
			"no identity for unverified cert")
	})
}

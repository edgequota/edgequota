package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNew(t *testing.T) {
	t.Run("creates server with valid config", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.Redis.Endpoints = []string{mr.Addr()}

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		assert.NotNil(t, srv)
		assert.NotNil(t, srv.mainServer)
		assert.NotNil(t, srv.adminServer)
		assert.NotNil(t, srv.health)
		assert.NotNil(t, srv.metrics)

		// Clean up.
		srv.chain.Close()
	})

	t.Run("returns error for invalid backend URL", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "://invalid"

		_, err := New(cfg, testLogger(), "test")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "create proxy")
	})

	t.Run("creates server with rate limiting disabled", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 0

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		assert.NotNil(t, srv)
		srv.chain.Close()
	})

	t.Run("creates server with passthrough on Redis failure", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.RateLimit.Average = 100
		cfg.RateLimit.FailurePolicy = config.FailurePolicyPassThrough
		cfg.Redis.Endpoints = []string{"127.0.0.1:1"}
		cfg.Redis.DialTimeout = "100ms"

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		assert.NotNil(t, srv)
		srv.chain.Close()
	})
}

func TestServerErrorLog(t *testing.T) {
	t.Run("main and admin servers have ErrorLog set", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.Redis.Endpoints = []string{mr.Addr()}

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		defer srv.chain.Close()

		assert.NotNil(t, srv.mainServer.ErrorLog, "main server ErrorLog must be set")
		assert.NotNil(t, srv.adminServer.ErrorLog, "admin server ErrorLog must be set")
	})
}

func TestServerConfigAddresses(t *testing.T) {
	t.Run("uses configured server and admin addresses", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.Server.Address = ":7777"
		cfg.Admin.Address = ":7778"
		cfg.Redis.Endpoints = []string{mr.Addr()}

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		assert.Equal(t, ":7777", srv.mainServer.Addr)
		assert.Equal(t, ":7778", srv.adminServer.Addr)
		srv.chain.Close()
	})
}

func TestTLSMinVersion(t *testing.T) {
	t.Run("returns TLS 1.3 when configured", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Server.TLS.MinVersion = config.TLSVersion13
		assert.Equal(t, uint16(tls.VersionTLS13), tlsMinVersion(cfg))
	})

	t.Run("returns TLS 1.2 by default", func(t *testing.T) {
		cfg := config.Defaults()
		assert.Equal(t, uint16(tls.VersionTLS12), tlsMinVersion(cfg))
	})

	t.Run("returns TLS 1.2 when explicitly configured", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Server.TLS.MinVersion = config.TLSVersion12
		assert.Equal(t, uint16(tls.VersionTLS12), tlsMinVersion(cfg))
	})
}

func TestServerReload(t *testing.T) {
	t.Run("reloads chain configuration", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.Redis.Endpoints = []string{mr.Addr()}
		cfg.RateLimit.Average = 100
		cfg.RateLimit.Burst = 10

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		defer srv.chain.Close()

		// Prepare a new config with different rate limit.
		newCfg := config.Defaults()
		newCfg.Backend.URL = "http://backend:8080"
		newCfg.Redis.Endpoints = []string{mr.Addr()}
		newCfg.RateLimit.Average = 200
		newCfg.RateLimit.Burst = 20

		err = srv.Reload(newCfg)
		assert.NoError(t, err)
		assert.Equal(t, newCfg, srv.cfg)
	})

	t.Run("reloads TLS certs when TLS is enabled", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.Redis.Endpoints = []string{mr.Addr()}

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		defer srv.chain.Close()

		dir := t.TempDir()
		certFile := dir + "/tls.crt"
		keyFile := dir + "/tls.key"
		require.NoError(t, generateSelfSignedCert(certFile, keyFile))

		ch, certErr := newCertHolder(certFile, keyFile)
		require.NoError(t, certErr)
		srv.certs = ch

		// Reload with cert paths in the new config.
		newCfg := config.Defaults()
		newCfg.Backend.URL = "http://backend:8080"
		newCfg.Redis.Endpoints = []string{mr.Addr()}
		newCfg.Server.TLS.CertFile = certFile
		newCfg.Server.TLS.KeyFile = keyFile

		require.NoError(t, generateSelfSignedCert(certFile, keyFile))
		err = srv.Reload(newCfg)
		assert.NoError(t, err)
	})
}

func TestReloadCerts(t *testing.T) {
	t.Run("no-op when TLS is not enabled", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.Redis.Endpoints = []string{mr.Addr()}

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		defer srv.chain.Close()

		// Should not panic — certs is nil.
		srv.ReloadCerts("nonexistent.crt", "nonexistent.key")
	})

	t.Run("logs error for invalid cert files", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.Redis.Endpoints = []string{mr.Addr()}

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		defer srv.chain.Close()

		// Manually set a certHolder so the method doesn't short-circuit.
		dir := t.TempDir()
		certFile := dir + "/tls.crt"
		keyFile := dir + "/tls.key"
		require.NoError(t, generateSelfSignedCert(certFile, keyFile))

		ch, certErr := newCertHolder(certFile, keyFile)
		require.NoError(t, certErr)
		srv.certs = ch

		// Attempt reload with bad files — should not panic, just log.
		srv.ReloadCerts("/nonexistent.crt", "/nonexistent.key")
	})

	t.Run("successfully reloads valid cert", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.Backend.URL = "http://backend:8080"
		cfg.Redis.Endpoints = []string{mr.Addr()}

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		defer srv.chain.Close()

		dir := t.TempDir()
		certFile := dir + "/tls.crt"
		keyFile := dir + "/tls.key"
		require.NoError(t, generateSelfSignedCert(certFile, keyFile))

		ch, certErr := newCertHolder(certFile, keyFile)
		require.NoError(t, certErr)
		srv.certs = ch

		// Get initial cert.
		cert1, _ := ch.GetCertificate(nil)
		require.NotNil(t, cert1)

		// Generate a new cert and reload.
		require.NoError(t, generateSelfSignedCert(certFile, keyFile))
		srv.ReloadCerts(certFile, keyFile)

		cert2, _ := ch.GetCertificate(nil)
		require.NotNil(t, cert2)
	})
}

// generateSelfSignedCert creates a minimal self-signed cert+key for testing.
func generateSelfSignedCert(certFile, keyFile string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyFile, keyPEM, 0o644)
}

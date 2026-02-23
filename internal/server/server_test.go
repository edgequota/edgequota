package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/cache"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/edgequota/edgequota/internal/redis"
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
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
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
		cfg.RateLimit.Static.BackendURL = "://invalid"

		_, err := New(cfg, testLogger(), "test")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "create proxy")
	})

	t.Run("creates server with rate limiting disabled", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 0

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		assert.NotNil(t, srv)
		srv.chain.Close()
	})

	t.Run("creates server with passthrough on Redis failure", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 100
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
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
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
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
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
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Redis.Endpoints = []string{mr.Addr()}
		cfg.RateLimit.Static.Average = 100
		cfg.RateLimit.Static.Burst = 10

		srv, err := New(cfg, testLogger(), "test")
		require.NoError(t, err)
		defer srv.chain.Close()

		// Prepare a new config with different rate limit.
		newCfg := config.Defaults()
		newCfg.RateLimit.Static.BackendURL = "http://backend:8080"
		newCfg.Redis.Endpoints = []string{mr.Addr()}
		newCfg.RateLimit.Static.Average = 200
		newCfg.RateLimit.Static.Burst = 20

		err = srv.Reload(newCfg)
		assert.NoError(t, err)
		assert.Equal(t, newCfg, srv.cfg)
	})

	t.Run("reloads TLS certs when TLS is enabled", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
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
		newCfg.RateLimit.Static.BackendURL = "http://backend:8080"
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
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
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
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
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
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
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

func TestBackendChanged(t *testing.T) {
	base := func() *config.Config {
		c := config.Defaults()
		c.RateLimit.Static.BackendURL = "http://backend:8080"
		return c
	}

	t.Run("detects backend_protocol change", func(t *testing.T) {
		old := base()
		new_ := base()
		new_.Backend.Transport.BackendProtocol = "h2"
		assert.True(t, backendChanged(old, new_))
	})

	t.Run("no change when configs are identical", func(t *testing.T) {
		old := base()
		new_ := base()
		assert.False(t, backendChanged(old, new_))
	})

	t.Run("detects URL change", func(t *testing.T) {
		old := base()
		new_ := base()
		new_.RateLimit.Static.BackendURL = "https://other:443"
		assert.True(t, backendChanged(old, new_))
	})

	t.Run("detects body size change", func(t *testing.T) {
		old := base()
		new_ := base()
		new_.Backend.MaxRequestBodySize = 999
		assert.True(t, backendChanged(old, new_))
	})

	t.Run("detects nested transport timeout change", func(t *testing.T) {
		old := base()
		new_ := base()
		new_.Backend.Transport.DialTimeout = "5s"
		assert.True(t, backendChanged(old, new_))
	})

	t.Run("detects url_policy change", func(t *testing.T) {
		old := base()
		new_ := base()
		deny := false
		new_.Backend.URLPolicy.DenyPrivateNetworks = &deny
		assert.True(t, backendChanged(old, new_))
	})
}

func newTestCacheStore(t *testing.T) *cache.Store {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := redis.NewClient(config.RedisConfig{
		Endpoints: []string{mr.Addr()},
		Mode:      config.RedisModeSingle,
	})
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	return cache.NewStore(client)
}

func postJSON(handler http.Handler, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(body)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/cache/purge", &buf)
	r.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, r)
	return w
}

func TestCachePurgeByKey(t *testing.T) {
	store := newTestCacheStore(t)
	store.Set(context.Background(), "my-key", &cache.Entry{
		StatusCode: 200,
		Body:       []byte("data"),
	}, time.Minute)

	handler := cachePurgeHandler(func() *cache.Store { return store }, slog.Default())

	w := postJSON(handler, purgeRequest{Key: "my-key"})
	assert.Equal(t, http.StatusNoContent, w.Code)

	_, ok := store.Get(context.Background(), "my-key")
	assert.False(t, ok, "entry should be deleted after purge by key")
}

func TestCachePurgeByURL(t *testing.T) {
	store := newTestCacheStore(t)
	store.Set(context.Background(), "GET|/static/bundle.js", &cache.Entry{
		StatusCode: 200,
		Body:       []byte("js-content"),
	}, time.Minute)

	handler := cachePurgeHandler(func() *cache.Store { return store }, slog.Default())

	w := postJSON(handler, purgeRequest{URL: "/static/bundle.js", Method: "GET"})
	assert.Equal(t, http.StatusNoContent, w.Code)

	_, ok := store.Get(context.Background(), "GET|/static/bundle.js")
	assert.False(t, ok, "entry should be deleted after purge by URL")
}

func TestCachePurgeByURLDefaultMethod(t *testing.T) {
	store := newTestCacheStore(t)
	store.Set(context.Background(), "GET|/page", &cache.Entry{
		StatusCode: 200,
		Body:       []byte("html"),
	}, time.Minute)

	handler := cachePurgeHandler(func() *cache.Store { return store }, slog.Default())

	w := postJSON(handler, purgeRequest{URL: "/page"})
	assert.Equal(t, http.StatusNoContent, w.Code)

	_, ok := store.Get(context.Background(), "GET|/page")
	assert.False(t, ok, "entry should be deleted when method defaults to GET")
}

func TestCachePurgeNotFound(t *testing.T) {
	store := newTestCacheStore(t)
	handler := cachePurgeHandler(func() *cache.Store { return store }, slog.Default())

	w := postJSON(handler, purgeRequest{Key: "nonexistent"})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestCachePurgeBadRequest(t *testing.T) {
	store := newTestCacheStore(t)
	handler := cachePurgeHandler(func() *cache.Store { return store }, slog.Default())

	w := postJSON(handler, purgeRequest{})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCachePurgeMethodNotAllowed(t *testing.T) {
	store := newTestCacheStore(t)
	handler := cachePurgeHandler(func() *cache.Store { return store }, slog.Default())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/cache/purge", nil)
	handler.ServeHTTP(w, r)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestCachePurgeStoreNil(t *testing.T) {
	handler := cachePurgeHandler(func() *cache.Store { return nil }, slog.Default())

	w := postJSON(handler, purgeRequest{Key: "anything"})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestCachePurgeTagsByTag(t *testing.T) {
	store := newTestCacheStore(t)
	ctx := context.Background()

	store.Set(ctx, "page-1", &cache.Entry{
		StatusCode: 200,
		Body:       []byte("page1"),
		Tags:       []string{"pages"},
	}, time.Minute)
	store.Set(ctx, "page-2", &cache.Entry{
		StatusCode: 200,
		Body:       []byte("page2"),
		Tags:       []string{"pages"},
	}, time.Minute)
	store.Set(ctx, "unrelated", &cache.Entry{
		StatusCode: 200,
		Body:       []byte("other"),
		Tags:       []string{"other"},
	}, time.Minute)

	handler := cachePurgeTagsHandler(func() *cache.Store { return store }, slog.Default())

	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(purgeTagsRequest{Tags: []string{"pages"}})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/cache/purge/tags", &buf)
	r.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, r)

	assert.Equal(t, http.StatusNoContent, w.Code)

	_, ok := store.Get(ctx, "page-1")
	assert.False(t, ok, "page-1 should be purged")
	_, ok = store.Get(ctx, "page-2")
	assert.False(t, ok, "page-2 should be purged")

	_, ok = store.Get(ctx, "unrelated")
	assert.True(t, ok, "unrelated entry should remain")
}

func TestCachePurgeTagsEmpty(t *testing.T) {
	store := newTestCacheStore(t)
	handler := cachePurgeTagsHandler(func() *cache.Store { return store }, slog.Default())

	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(purgeTagsRequest{Tags: []string{}})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/cache/purge/tags", &buf)
	r.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, r)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCachePurgeTagsMethodNotAllowed(t *testing.T) {
	store := newTestCacheStore(t)
	handler := cachePurgeTagsHandler(func() *cache.Store { return store }, slog.Default())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/cache/purge/tags", nil)
	handler.ServeHTTP(w, r)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
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

package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validConfig returns minimal valid YAML that passes Load+Validate.
func validConfig(average int64) string {
	return fmt.Sprintf(`
backend:
  url: "http://127.0.0.1:8080"
rate_limit:
  average: %d
  burst: 10
  period: "1s"
`, average)
}

// writeFile is a helper that writes content to a file.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestWatcher_DetectsFileChange(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, validConfig(5))

	var received atomic.Int64
	var mu sync.Mutex
	var lastCfg *Config

	w := NewWatcher(cfgPath, func(newCfg *Config) {
		mu.Lock()
		lastCfg = newCfg
		mu.Unlock()
		received.Add(1)
	}, slog.Default())
	w.debounce = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = w.Start(ctx)
	}()

	// Give the watcher time to set up.
	time.Sleep(200 * time.Millisecond)

	// Modify the file.
	writeFile(t, cfgPath, validConfig(7))

	// Wait for the callback.
	assert.Eventually(t, func() bool { return received.Load() >= 1 }, 3*time.Second, 50*time.Millisecond,
		"expected at least one callback")

	mu.Lock()
	assert.NotNil(t, lastCfg)
	mu.Unlock()
}

func TestWatcher_InvalidConfigKeepsOld(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, validConfig(5))

	var received atomic.Int64
	w := NewWatcher(cfgPath, func(_ *Config) {
		received.Add(1)
	}, slog.Default())
	w.debounce = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = w.Start(ctx)
	}()

	time.Sleep(200 * time.Millisecond)

	// Write invalid YAML (no backend URL).
	writeFile(t, cfgPath, `{{{bad yaml`)

	// Wait for debounce + reload attempt.
	time.Sleep(500 * time.Millisecond)

	assert.Equal(t, int64(0), received.Load(), "callback should NOT fire for invalid config")
}

func TestWatcher_DebouncesManyWrites(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, validConfig(5))

	var received atomic.Int64
	w := NewWatcher(cfgPath, func(_ *Config) {
		received.Add(1)
	}, slog.Default())
	w.debounce = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = w.Start(ctx)
	}()

	time.Sleep(200 * time.Millisecond)

	// Rapid successive writes within the debounce window.
	for i := 0; i < 10; i++ {
		writeFile(t, cfgPath, validConfig(5))
		time.Sleep(20 * time.Millisecond)
	}

	// Wait for debounce + reload.
	time.Sleep(600 * time.Millisecond)

	got := received.Load()
	assert.LessOrEqual(t, got, int64(2),
		"debouncing should coalesce rapid writes into 1-2 callbacks, got %d", got)
}

func TestWatcher_PollingDetectsSymlinkSwap(t *testing.T) {
	// Simulate a Kubernetes-style symlink swap: the config file is a
	// symlink chain dir/config.yaml → ..data/config.yaml, and we
	// swap ..data from one timestamped directory to another.
	dir := t.TempDir()

	// Create two "timestamped" directories with different configs.
	ts1 := filepath.Join(dir, "..2026_01")
	ts2 := filepath.Join(dir, "..2026_02")
	require.NoError(t, os.Mkdir(ts1, 0o755))
	require.NoError(t, os.Mkdir(ts2, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ts1, "config.yaml"), []byte(validConfig(5)), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ts2, "config.yaml"), []byte(validConfig(99)), 0o644))

	// Create ..data symlink pointing to ts1.
	dataLink := filepath.Join(dir, "..data")
	require.NoError(t, os.Symlink(ts1, dataLink))

	// Create config.yaml symlink → ..data/config.yaml.
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.Symlink(filepath.Join("..data", "config.yaml"), cfgPath))

	var received atomic.Int64
	w := NewWatcher(cfgPath, func(_ *Config) {
		received.Add(1)
	}, slog.Default())
	w.debounce = 50 * time.Millisecond
	w.pollInterval = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = w.Start(ctx)
	}()

	time.Sleep(200 * time.Millisecond)

	// Swap the ..data symlink atomically (Kubernetes-style).
	tmpLink := filepath.Join(dir, "..data_tmp")
	require.NoError(t, os.Symlink(ts2, tmpLink))
	require.NoError(t, os.Rename(tmpLink, dataLink))

	assert.Eventually(t, func() bool { return received.Load() >= 1 }, 3*time.Second, 50*time.Millisecond,
		"expected polling to detect symlink swap")
}

func TestWatcher_StopIsIdempotent(t *testing.T) {
	w := NewWatcher("/tmp/nonexistent.yaml", func(_ *Config) {}, slog.Default())
	// Stop before Start — should not panic.
	w.Stop()
	w.Stop()
}

// ---------------------------------------------------------------------------
// CertWatcher tests
// ---------------------------------------------------------------------------

func TestCertWatcher_DetectsFileChange(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(certPath, []byte("cert-v1"), 0o644))
	require.NoError(t, os.WriteFile(keyPath, []byte("key-v1"), 0o644))

	var received atomic.Int64
	cw := NewCertWatcher(certPath, keyPath, func(_, _ string) {
		received.Add(1)
	}, slog.Default())
	cw.pollInterval = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = cw.Start(ctx)
	}()

	time.Sleep(200 * time.Millisecond)

	// Change the cert file.
	require.NoError(t, os.WriteFile(certPath, []byte("cert-v2"), 0o644))

	assert.Eventually(t, func() bool { return received.Load() >= 1 }, 3*time.Second, 50*time.Millisecond,
		"expected CertWatcher to detect cert change")
}

func TestCertWatcher_DetectsKeyChange(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(certPath, []byte("cert-v1"), 0o644))
	require.NoError(t, os.WriteFile(keyPath, []byte("key-v1"), 0o644))

	var received atomic.Int64
	cw := NewCertWatcher(certPath, keyPath, func(_, _ string) {
		received.Add(1)
	}, slog.Default())
	cw.pollInterval = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = cw.Start(ctx)
	}()

	time.Sleep(200 * time.Millisecond)

	// Change only the key file.
	require.NoError(t, os.WriteFile(keyPath, []byte("key-v2"), 0o644))

	assert.Eventually(t, func() bool { return received.Load() >= 1 }, 3*time.Second, 50*time.Millisecond,
		"expected CertWatcher to detect key change")
}

func TestCertWatcher_DetectsSymlinkSwap(t *testing.T) {
	dir := t.TempDir()

	ts1 := filepath.Join(dir, "..2026_01")
	ts2 := filepath.Join(dir, "..2026_02")
	require.NoError(t, os.Mkdir(ts1, 0o755))
	require.NoError(t, os.Mkdir(ts2, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ts1, "tls.crt"), []byte("cert-v1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ts1, "tls.key"), []byte("key-v1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ts2, "tls.crt"), []byte("cert-v2"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ts2, "tls.key"), []byte("key-v2"), 0o644))

	dataLink := filepath.Join(dir, "..data")
	require.NoError(t, os.Symlink(ts1, dataLink))

	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	require.NoError(t, os.Symlink(filepath.Join("..data", "tls.crt"), certPath))
	require.NoError(t, os.Symlink(filepath.Join("..data", "tls.key"), keyPath))

	var received atomic.Int64
	cw := NewCertWatcher(certPath, keyPath, func(_, _ string) {
		received.Add(1)
	}, slog.Default())
	cw.pollInterval = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = cw.Start(ctx)
	}()

	time.Sleep(200 * time.Millisecond)

	// Atomic symlink swap.
	tmpLink := filepath.Join(dir, "..data_tmp")
	require.NoError(t, os.Symlink(ts2, tmpLink))
	require.NoError(t, os.Rename(tmpLink, dataLink))

	assert.Eventually(t, func() bool { return received.Load() >= 1 }, 3*time.Second, 50*time.Millisecond,
		"expected CertWatcher to detect symlink swap")
}

func TestCertWatcher_NoFalsePositive(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(certPath, []byte("cert-v1"), 0o644))
	require.NoError(t, os.WriteFile(keyPath, []byte("key-v1"), 0o644))

	var received atomic.Int64
	cw := NewCertWatcher(certPath, keyPath, func(_, _ string) {
		received.Add(1)
	}, slog.Default())
	cw.pollInterval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = cw.Start(ctx)
	}()

	// Wait several poll cycles without changing anything.
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, int64(0), received.Load(), "no callback should fire when files are unchanged")
}

func TestCertWatcher_StopIsIdempotent(t *testing.T) {
	cw := NewCertWatcher("/tmp/a.crt", "/tmp/a.key", func(_, _ string) {}, slog.Default())
	cw.Stop()
	cw.Stop()
}

// ---------------------------------------------------------------------------
// hashFile / readlink helpers
// ---------------------------------------------------------------------------

func TestHashFile(t *testing.T) {
	t.Run("returns consistent hash for same content", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "test.txt")
		require.NoError(t, os.WriteFile(f, []byte("hello"), 0o644))

		h1 := hashFile(f)
		h2 := hashFile(f)
		assert.NotEmpty(t, h1)
		assert.Equal(t, h1, h2)
	})

	t.Run("returns different hash for different content", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "test.txt")

		require.NoError(t, os.WriteFile(f, []byte("hello"), 0o644))
		h1 := hashFile(f)

		require.NoError(t, os.WriteFile(f, []byte("world"), 0o644))
		h2 := hashFile(f)

		assert.NotEqual(t, h1, h2)
	})

	t.Run("returns empty string for nonexistent file", func(t *testing.T) {
		assert.Empty(t, hashFile("/tmp/does-not-exist-xyz"))
	})

	t.Run("follows symlinks", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		link := filepath.Join(dir, "link.txt")

		require.NoError(t, os.WriteFile(real, []byte("data"), 0o644))
		require.NoError(t, os.Symlink(real, link))

		assert.Equal(t, hashFile(real), hashFile(link))
	})
}

func TestReadlink(t *testing.T) {
	t.Run("returns target of symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		link := filepath.Join(dir, "link")
		require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))
		require.NoError(t, os.Symlink(target, link))

		assert.Equal(t, target, readlink(link))
	})

	t.Run("returns empty for regular file", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "regular")
		require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))

		assert.Empty(t, readlink(f))
	})

	t.Run("returns empty for nonexistent path", func(t *testing.T) {
		assert.Empty(t, readlink("/tmp/does-not-exist-xyz"))
	})
}

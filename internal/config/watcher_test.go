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

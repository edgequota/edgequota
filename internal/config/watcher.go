package config

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatcherCallback is called with the new, validated config on every
// successful reload. It runs synchronously — keep it fast.
type WatcherCallback func(newCfg *Config)

// Watcher watches a config file for changes and triggers a callback with
// the new config. It uses both fsnotify (for low-latency notification on
// real filesystems) and periodic content-hash polling (to reliably detect
// Kubernetes ConfigMap/Secret volume updates, which swap symlinks at the
// VFS layer and may not generate inotify events).
type Watcher struct {
	path         string
	dir          string // parent directory — watched for Kubernetes symlink swaps.
	callback     WatcherCallback
	logger       *slog.Logger
	debounce     time.Duration
	pollInterval time.Duration // how often to check content hash.

	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
}

// NewWatcher creates a config file watcher. The watcher does NOT start
// watching until Start is called.
func NewWatcher(path string, callback WatcherCallback, logger *slog.Logger) *Watcher {
	return &Watcher{
		path:         path,
		dir:          filepath.Dir(path),
		callback:     callback,
		logger:       logger,
		debounce:     300 * time.Millisecond,
		pollInterval: 2 * time.Second,
	}
}

// pollState tracks the content-hash and symlink-target state used by the
// polling fallback in Watcher.Start.
type pollState struct {
	dataLink   string
	lastHash   string
	lastTarget string
}

// changed reports whether the config file content has changed since the
// last snapshot, using the "..data" symlink target (fast) and then the
// file content hash (slow) as detection signals.
func (ps *pollState) changed(path string) bool {
	// Fast path: Kubernetes "..data" symlink target changed.
	if target := readlink(ps.dataLink); target != ps.lastTarget && target != "" {
		ps.lastTarget = target
		return true
	}
	// Slow path: content hash changed.
	return hashFile(path) != ps.lastHash
}

// snapshot re-captures the current file hash and symlink target.
func (ps *pollState) snapshot(path string) {
	ps.lastHash = hashFile(path)
	ps.lastTarget = readlink(ps.dataLink)
}

// Start begins watching the config file. Blocks until the context is
// canceled or Stop is called.
//
// Two detection mechanisms run concurrently:
//  1. fsnotify — gives sub-second reaction on real filesystems and editors
//     that do atomic save-and-rename.
//  2. Content-hash polling — catches Kubernetes projected-volume updates.
//     Kubelet swaps the "..data" symlink at the VFS layer, which is often
//     invisible to inotify because the mount driver does not emit events
//     for internal symlink changes. Polling the file hash every few seconds
//     is a reliable fallback that avoids missed reloads.
func (w *Watcher) Start(ctx context.Context) error {
	ctx, w.cancel = context.WithCancel(ctx)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := watcher.Add(w.dir); err != nil {
		return err
	}
	_ = watcher.Add(w.path)

	w.logger.Info("config watcher started", "path", w.path, "dir", w.dir)

	ps := &pollState{dataLink: filepath.Join(w.dir, "..data")}
	ps.snapshot(w.path)

	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	pollTicker := time.NewTicker(w.pollInterval)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("config watcher stopped")
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			debounceTimer, debounceCh = w.handleFSEvent(event, watcher, debounceTimer)

		case <-debounceCh:
			debounceCh = nil
			w.reload()
			ps.snapshot(w.path)

		case <-pollTicker.C:
			if ps.changed(w.path) {
				ps.snapshot(w.path)
				w.logger.Debug("config file change detected via polling", "path", w.path)
				w.reload()
			}

		case watchErr, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			w.logger.Error("config watcher error", "error", watchErr)
		}
	}
}

// handleFSEvent processes a single fsnotify event and returns the updated
// debounce timer and channel. Only write/create/rename events trigger a
// debounced reload.
func (w *Watcher) handleFSEvent(
	event fsnotify.Event,
	watcher *fsnotify.Watcher,
	timer *time.Timer,
) (*time.Timer, <-chan time.Time) {
	if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) && !event.Has(fsnotify.Rename) {
		var ch <-chan time.Time
		if timer != nil {
			ch = timer.C
		}
		return timer, ch
	}

	if timer != nil {
		timer.Stop()
	}
	timer = time.NewTimer(w.debounce)

	// Re-add the file path after a rename/create; some editors do atomic
	// write (rename temp → target) which removes the old inode from the watch.
	if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
		_ = watcher.Add(w.path)
	}

	return timer, timer.C
}

// hashFile returns the SHA-256 hex digest of the file at path, or an
// empty string if the file cannot be read. The hash covers the resolved
// content (following symlinks), so a Kubernetes symlink swap changes it.
func hashFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return string(h.Sum(nil))
}

// reload loads, validates, and publishes the new config. On failure the
// old config is preserved and an error is logged.
func (w *Watcher) reload() {
	newCfg, err := LoadFromPath(w.path)
	if err != nil {
		w.logger.Error("config reload failed, keeping old config", "error", err)
		return
	}

	w.logger.Info("config reloaded successfully", "path", w.path)
	w.callback(newCfg)
}

// Stop terminates the watcher goroutine.
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.stopped = true
	if w.cancel != nil {
		w.cancel()
	}
}

// ---------------------------------------------------------------------------
// CertWatcher — dedicated watcher for TLS certificate files.
// ---------------------------------------------------------------------------

// CertCallback is called when the TLS certificate files change on disk.
type CertCallback func(certFile, keyFile string)

// CertWatcher monitors TLS certificate files for changes and triggers a
// callback to reload them. It uses content-hash polling because the cert
// files typically live in a Kubernetes Secret volume (separate from the
// config ConfigMap), and inotify does not reliably detect projected-volume
// symlink swaps.
type CertWatcher struct {
	certFile     string
	keyFile      string
	callback     CertCallback
	logger       *slog.Logger
	pollInterval time.Duration

	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
}

// NewCertWatcher creates a TLS certificate file watcher. Monitoring does
// not start until Start is called.
func NewCertWatcher(certFile, keyFile string, callback CertCallback, logger *slog.Logger) *CertWatcher {
	return &CertWatcher{
		certFile:     certFile,
		keyFile:      keyFile,
		callback:     callback,
		logger:       logger,
		pollInterval: 2 * time.Second,
	}
}

// Start begins polling the certificate files. Blocks until the context is
// canceled or Stop is called.
//
// Detection uses two signals, whichever fires first:
//  1. Symlink-target change on the parent directory's "..data" link
//     (instant detection of Kubernetes volume swap).
//  2. Content-hash change on the cert/key files themselves (catches
//     any other update mechanism).
func (cw *CertWatcher) Start(ctx context.Context) error {
	ctx, cw.cancel = context.WithCancel(ctx)

	certDir := filepath.Dir(cw.certFile)
	dataLink := filepath.Join(certDir, "..data")

	cw.logger.Info("TLS cert watcher started", "cert", cw.certFile, "key", cw.keyFile, "dir", certDir)

	lastCertHash := hashFile(cw.certFile)
	lastKeyHash := hashFile(cw.keyFile)
	lastLinkTarget := readlink(dataLink)

	ticker := time.NewTicker(cw.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			cw.logger.Info("TLS cert watcher stopped")
			return nil
		case <-ticker.C:
			// Fast path: check if the Kubernetes "..data" symlink target changed.
			changed := false
			if target := readlink(dataLink); target != lastLinkTarget && target != "" {
				lastLinkTarget = target
				changed = true
			}

			// Slow path: compare content hashes.
			if !changed {
				certHash := hashFile(cw.certFile)
				keyHash := hashFile(cw.keyFile)
				if certHash != lastCertHash || keyHash != lastKeyHash {
					changed = true
				}
			}

			if changed {
				// Re-snapshot both hashes after detecting any kind of change.
				lastCertHash = hashFile(cw.certFile)
				lastKeyHash = hashFile(cw.keyFile)
				cw.logger.Info("TLS certificate change detected", "cert", cw.certFile)
				cw.callback(cw.certFile, cw.keyFile)
			}
		}
	}
}

// readlink returns the target of a symlink, or "" if the path is not a
// symlink or cannot be read.
func readlink(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return target
}

// Stop terminates the cert watcher goroutine.
func (cw *CertWatcher) Stop() {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if cw.stopped {
		return
	}
	cw.stopped = true
	if cw.cancel != nil {
		cw.cancel()
	}
}

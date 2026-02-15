package config

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatcherCallback is called with the new, validated config on every
// successful reload. It runs synchronously — keep it fast.
type WatcherCallback func(newCfg *Config)

// Watcher watches a config file for changes and triggers a callback with
// the new config. It debounces rapid file changes (e.g. editors writing
// temp files then renaming).
type Watcher struct {
	path     string
	callback WatcherCallback
	logger   *slog.Logger
	debounce time.Duration

	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
}

// NewWatcher creates a config file watcher. The watcher does NOT start
// watching until Start is called.
func NewWatcher(path string, callback WatcherCallback, logger *slog.Logger) *Watcher {
	return &Watcher{
		path:     path,
		callback: callback,
		logger:   logger,
		debounce: 300 * time.Millisecond,
	}
}

// Start begins watching the config file. Blocks until the context is
// cancelled or Stop is called.
func (w *Watcher) Start(ctx context.Context) error {
	ctx, w.cancel = context.WithCancel(ctx)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := watcher.Add(w.path); err != nil {
		return err
	}

	w.logger.Info("config watcher started", "path", w.path)

	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("config watcher stopped")
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			// React to writes, creates (editor save-and-rename), and renames.
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.NewTimer(w.debounce)
				debounceCh = debounceTimer.C

				// Re-add the path after a rename/create; some editors
				// do atomic write (rename temp → target) which removes
				// the old inode from the watch.
				if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
					_ = watcher.Add(w.path)
				}
			}

		case <-debounceCh:
			debounceCh = nil
			w.reload()

		case watchErr, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			w.logger.Error("config watcher error", "error", watchErr)
		}
	}
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

// Package proxy contains the WebSocket per-key connection limiter.
package proxy

import "sync"

// WSLimiter enforces a maximum number of concurrent WebSocket connections per
// rate-limit key (typically a client IP or tenant ID). When maxPerKey <= 0
// the limiter is a no-op and all connections are allowed.
type WSLimiter struct {
	mu        sync.Mutex
	counts    map[string]int64
	maxPerKey int64
}

// NewWSLimiter creates a per-key WebSocket limiter. A maxPerKey of 0 means
// unlimited (Acquire always succeeds).
func NewWSLimiter(maxPerKey int64) *WSLimiter {
	return &WSLimiter{
		counts:    make(map[string]int64),
		maxPerKey: maxPerKey,
	}
}

// Acquire returns true if the connection for the given key is within limits.
// The caller MUST call Release when the connection closes.
func (l *WSLimiter) Acquire(key string) bool {
	if l == nil || l.maxPerKey <= 0 {
		return true // unlimited
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.counts[key] >= l.maxPerKey {
		return false
	}
	l.counts[key]++
	return true
}

// Release decrements the connection count for the given key.
func (l *WSLimiter) Release(key string) {
	if l == nil || l.maxPerKey <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if n := l.counts[key]; n <= 1 {
		delete(l.counts, key)
	} else {
		l.counts[key] = n - 1
	}
}

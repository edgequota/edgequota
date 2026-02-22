// Package proxy contains the WebSocket per-key connection limiter.
package proxy

import (
	"hash/fnv"
	"sync"
)

const wsShardCount = 64

// WSLimiter enforces a maximum number of concurrent WebSocket connections per
// rate-limit key (typically a client IP or tenant ID). It uses 64 shards
// to reduce mutex contention under high concurrency.
type WSLimiter struct {
	shards    [wsShardCount]wsShard
	maxPerKey int64
}

type wsShard struct {
	mu     sync.Mutex
	counts map[string]int64
}

// NewWSLimiter creates a per-key WebSocket limiter. A maxPerKey of 0 means
// unlimited (Acquire always succeeds).
func NewWSLimiter(maxPerKey int64) *WSLimiter {
	l := &WSLimiter{maxPerKey: maxPerKey}
	for i := range l.shards {
		l.shards[i].counts = make(map[string]int64)
	}
	return l
}

func (l *WSLimiter) shard(key string) *wsShard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &l.shards[h.Sum32()%wsShardCount]
}

// Acquire returns true if the connection for the given key is within limits.
// The caller MUST call Release when the connection closes.
func (l *WSLimiter) Acquire(key string) bool {
	if l == nil || l.maxPerKey <= 0 {
		return true
	}
	s := l.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.counts[key] >= l.maxPerKey {
		return false
	}
	s.counts[key]++
	return true
}

// Release decrements the connection count for the given key.
func (l *WSLimiter) Release(key string) {
	if l == nil || l.maxPerKey <= 0 {
		return
	}
	s := l.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	if n := s.counts[key]; n <= 1 {
		delete(s.counts, key)
	} else {
		s.counts[key] = n - 1
	}
}

package ratelimit

import (
	"sync"
	"time"
	"unsafe"

	"github.com/dgraph-io/ristretto/v2"
)

// defaultMaxCost is the default memory budget for the fallback cache (64 MiB).
const defaultMaxCost = 64 << 20

// bucketCost is the approximate memory footprint of a single bucket entry.
// Used as the cost parameter so ristretto can manage eviction by real memory
// rather than an arbitrary key count.
var bucketCost = int64(unsafe.Sizeof(bucket{}))

// InMemoryLimiter provides per-key token bucket rate limiting using local memory.
// Used as a fallback when Redis is unavailable and the failure policy is "inMemoryFallback".
//
// IMPORTANT: This limiter is NOT globally consistent. Each EdgeQuota instance
// maintains its own independent counters. Under failover conditions the effective
// rate limit is per-instance, not per-cluster.
//
// Internally, ristretto handles concurrency, TTL-based expiry, and admission/eviction
// (TinyLFU policy) within the configured memory budget. The token bucket state is
// stored per key with a per-bucket mutex so that hot paths only contend on the
// individual key, not a global lock.
type InMemoryLimiter struct {
	disabled bool // true when rate <= 0; Allow always returns true
	cache    *ristretto.Cache[string, *bucket]
	pending  sync.Map // serializes first-insert per key to prevent duplicate buckets
	rate     float64  // tokens per second
	burst    int64
	ttl      time.Duration
}

type bucket struct {
	mu       sync.Mutex
	tokens   float64
	lastTime time.Time
}

// NewInMemoryLimiter creates an in-memory limiter backed by ristretto.
// Ristretto manages admission, eviction (TinyLFU), and TTL-based expiry
// within a fixed memory budget (64 MiB by default).
func NewInMemoryLimiter(ratePerSecond float64, burst int64, ttl time.Duration) *InMemoryLimiter {
	// Estimate the expected number of items so the frequency sketch is accurate.
	// NumCounters should be ~10x the expected max items.
	estimatedItems := defaultMaxCost / bucketCost
	numCounters := estimatedItems * 10

	cache, err := ristretto.NewCache(&ristretto.Config[string, *bucket]{
		NumCounters: numCounters,
		MaxCost:     defaultMaxCost,
		BufferItems: 64,
	})
	if err != nil {
		// Only fails with invalid config; the values above are always valid.
		panic("ristretto: " + err.Error())
	}

	return &InMemoryLimiter{
		disabled: ratePerSecond <= 0,
		cache:    cache,
		rate:     ratePerSecond,
		burst:    burst,
		ttl:      ttl,
	}
}

// Allow checks the in-memory token bucket for the given key.
// When the limiter is disabled (rate <= 0), always returns true.
func (l *InMemoryLimiter) Allow(key string) bool {
	if l.disabled {
		return true
	}

	now := time.Now()

	b, found := l.cache.Get(key)
	if !found {
		// Serialize first-insert per key so that concurrent requests for a new
		// key share one bucket instead of each creating an independent one.
		// This prevents 2-3x burst allowance during the ristretto async-set window.
		newBucket := &bucket{
			tokens:   float64(l.burst) - 1,
			lastTime: now,
		}
		actual, loaded := l.pending.LoadOrStore(key, newBucket)
		b = actual.(*bucket)
		if !loaded {
			// We won the race — insert into ristretto and clean up pending.
			l.cache.SetWithTTL(key, b, bucketCost, l.ttl)
			l.cache.Wait()
			l.pending.Delete(key)
			return true
		}
		// Another goroutine created the bucket — fall through to the refill path.
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.lastTime).Seconds()
	b.tokens += l.rate * elapsed
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.lastTime = now

	if b.tokens >= 1.0 {
		b.tokens--
		return true
	}

	return false
}

// Close releases resources held by the cache. Safe to call multiple times.
func (l *InMemoryLimiter) Close() {
	if l.cache != nil {
		l.cache.Close()
	}
}

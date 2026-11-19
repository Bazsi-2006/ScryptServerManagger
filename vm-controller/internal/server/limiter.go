package server

import (
	"sync"
	"time"
)

// failureLimiter counts failed attempts per key in a fixed window that starts
// at the first failure. Once max failures are reached the key is blocked
// until the window ends.
type failureLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	cap     int
	entries map[string]*failureEntry
}

type failureEntry struct {
	count int
	start time.Time
}

func newFailureLimiter(max int, window time.Duration, capacity int) *failureLimiter {
	return &failureLimiter{max: max, window: window, cap: capacity, entries: map[string]*failureEntry{}}
}

// blocked reports whether key is locked out and for how long.
func (l *failureLimiter) blocked(key string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return 0, false
	}
	end := e.start.Add(l.window)
	if !now.Before(end) {
		delete(l.entries, key)
		return 0, false
	}
	if e.count >= l.max {
		return end.Sub(now), true
	}
	return 0, false
}

func (l *failureLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil || !now.Before(e.start.Add(l.window)) {
		if len(l.entries) >= l.cap {
			l.pruneLocked(now)
		}
		if len(l.entries) >= l.cap {
			// Still full of live entries: evict the oldest one rather
			// than growing without bound.
			var oldestKey string
			var oldest time.Time
			for k, v := range l.entries {
				if oldestKey == "" || v.start.Before(oldest) {
					oldestKey, oldest = k, v.start
				}
			}
			delete(l.entries, oldestKey)
		}
		e = &failureEntry{start: now}
		l.entries[key] = e
	}
	e.count++
}

func (l *failureLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

func (l *failureLimiter) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
}

func (l *failureLimiter) pruneLocked(now time.Time) {
	for k, e := range l.entries {
		if !now.Before(e.start.Add(l.window)) {
			delete(l.entries, k)
		}
	}
}

// tokenBucket limits power requests per user.
type tokenBucket struct {
	mu      sync.Mutex
	burst   float64
	refill  time.Duration // time to regain one token
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newTokenBucket(burst int, refill time.Duration) *tokenBucket {
	return &tokenBucket{burst: float64(burst), refill: refill, buckets: map[string]*bucket{}}
}

// take consumes a token for key, or reports how long to wait for one.
func (t *tokenBucket) take(key string, now time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buckets[key]
	if b == nil {
		b = &bucket{tokens: t.burst, last: now}
		t.buckets[key] = b
	}
	b.tokens = min(t.burst, b.tokens+float64(now.Sub(b.last))/float64(t.refill))
	b.last = now
	if b.tokens < 1 {
		return time.Duration((1 - b.tokens) * float64(t.refill)), false
	}
	b.tokens--
	return 0, true
}

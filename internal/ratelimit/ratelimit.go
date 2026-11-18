// Package ratelimit provides an in-memory token-bucket limiter keyed by
// arbitrary strings (IP, user, user+server, ...).
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is safe for concurrent use. The limit is supplied per call so
// configuration changes take effect without rebuilding the limiter.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	lastGC  time.Time
}

func New() *Limiter {
	return &Limiter{buckets: map[string]*bucket{}, now: time.Now}
}

// SetClock overrides the time source (tests).
func (l *Limiter) SetClock(now func() time.Time) { l.now = now }

// Allow consumes one token from the bucket for key, which refills at
// limit/window and holds at most limit tokens. When the bucket is empty it
// returns false and how long to wait before the next token is available.
func (l *Limiter) Allow(key string, limit int, window time.Duration) (bool, time.Duration) {
	if limit <= 0 || window <= 0 {
		return true, 0
	}
	rate := float64(limit) / window.Seconds()
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.gc(now)
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(limit), last: now}
		l.buckets[key] = b
	} else {
		b.tokens += now.Sub(b.last).Seconds() * rate
		if b.tokens > float64(limit) {
			b.tokens = float64(limit)
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / rate * float64(time.Second))
	return false, wait
}

// Reset forgets the bucket for key (e.g. after a successful login).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.buckets, key)
	l.mu.Unlock()
}

// gc drops buckets that have been idle long enough to be full again under
// any sane configuration, bounding memory use.
func (l *Limiter) gc(now time.Time) {
	if now.Sub(l.lastGC) < time.Minute {
		return
	}
	l.lastGC = now
	for k, b := range l.buckets {
		if now.Sub(b.last) > 25*time.Hour {
			delete(l.buckets, k)
		}
	}
	// Hard cap as a defence against key-space flooding.
	if len(l.buckets) > 200_000 {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
}

// Cooldown tracks a minimum interval between events per key.
type Cooldown struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

func NewCooldown() *Cooldown { return &Cooldown{last: map[string]time.Time{}, now: time.Now} }

func (c *Cooldown) SetClock(now func() time.Time) { c.now = now }

// Remaining returns how long key must still wait given the interval.
func (c *Cooldown) Remaining(key string, interval time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.last[key]
	if !ok {
		return 0
	}
	if rem := interval - c.now().Sub(t); rem > 0 {
		return rem
	}
	return 0
}

// Mark records an event for key now.
func (c *Cooldown) Mark(key string) {
	c.mu.Lock()
	c.last[key] = c.now()
	c.mu.Unlock()
}

func (c *Cooldown) Forget(key string) {
	c.mu.Lock()
	delete(c.last, key)
	c.mu.Unlock()
}

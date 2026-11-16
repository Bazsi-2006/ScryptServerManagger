package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestLimiterBurstAndRefill(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New()
	l.SetClock(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k", 3, time.Minute); !ok {
			t.Fatalf("request %d should pass", i)
		}
	}
	ok, wait := l.Allow("k", 3, time.Minute)
	if ok {
		t.Fatal("4th request must be limited")
	}
	if wait <= 0 || wait > 21*time.Second {
		t.Fatalf("unexpected retry-after %s", wait)
	}
	if ok, _ := l.Allow("other", 3, time.Minute); !ok {
		t.Fatal("keys must be independent")
	}
	now = now.Add(20 * time.Second) // one token back
	if ok, _ := l.Allow("k", 3, time.Minute); !ok {
		t.Fatal("token should have refilled")
	}
	if ok, _ := l.Allow("k", 3, time.Minute); ok {
		t.Fatal("only one token should have refilled")
	}
	now = now.Add(time.Hour)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k", 3, time.Minute); !ok {
			t.Fatal("bucket should be full again, capped at the limit")
		}
	}
	if ok, _ := l.Allow("k", 3, time.Minute); ok {
		t.Fatal("bucket must not exceed its capacity")
	}
}

func TestLimiterReset(t *testing.T) {
	l := New()
	l.Allow("k", 1, time.Hour)
	if ok, _ := l.Allow("k", 1, time.Hour); ok {
		t.Fatal("should be limited")
	}
	l.Reset("k")
	if ok, _ := l.Allow("k", 1, time.Hour); !ok {
		t.Fatal("reset should clear the bucket")
	}
}

func TestLimiterConcurrent(t *testing.T) {
	l := New()
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow("k", 50, time.Hour); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 50 {
		t.Fatalf("allowed %d, want exactly 50", allowed)
	}
}

func TestCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	c := NewCooldown()
	c.SetClock(func() time.Time { return now })
	if c.Remaining("s", time.Minute) != 0 {
		t.Fatal("no cooldown before the first event")
	}
	c.Mark("s")
	now = now.Add(10 * time.Second)
	if rem := c.Remaining("s", time.Minute); rem != 50*time.Second {
		t.Fatalf("remaining %s", rem)
	}
	if c.Remaining("s", 5*time.Second) != 0 || c.Remaining("s", 0) != 0 {
		t.Fatal("elapsed/zero interval must not block")
	}
	c.Forget("s")
	if c.Remaining("s", time.Minute) != 0 {
		t.Fatal("forget should clear")
	}
}

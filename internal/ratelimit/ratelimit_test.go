package ratelimit

import (
	"testing"
	"time"
)

func TestKeyLimiterBurstRefillAndRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewKeyLimiter(60, 2)
	l.now = func() time.Time { return now }

	if allowed, _ := l.Allow("alice"); !allowed {
		t.Fatal("first request should be allowed")
	}
	if allowed, _ := l.Allow("alice"); !allowed {
		t.Fatal("second request should use the burst token")
	}
	allowed, retry := l.Allow("alice")
	if allowed {
		t.Fatal("third request should be rate limited")
	}
	if retry != time.Second {
		t.Fatalf("retry = %s, want 1s", retry)
	}

	now = now.Add(time.Second)
	if allowed, _ := l.Allow("alice"); !allowed {
		t.Fatal("request after refill should be allowed")
	}
}

func TestKeyLimiterIsPerKeyAndRetainDropsOldBuckets(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewKeyLimiter(1, 1)
	l.now = func() time.Time { return now }

	if allowed, _ := l.Allow("alice"); !allowed {
		t.Fatal("alice should be allowed")
	}
	if allowed, _ := l.Allow("alice"); allowed {
		t.Fatal("alice should now be limited")
	}
	if allowed, _ := l.Allow("bob"); !allowed {
		t.Fatal("bob should have an independent bucket")
	}

	l.Retain([]string{"alice"})
	l.mu.Lock()
	_, alice := l.buckets["alice"]
	_, bob := l.buckets["bob"]
	l.mu.Unlock()
	if !alice || bob {
		t.Fatalf("buckets after Retain: alice=%v bob=%v", alice, bob)
	}
}

func TestFailLimiterThresholdAndExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewFailLimiter(3, 10*time.Second, 30*time.Second)
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		l.Fail("1.2.3.4")
	}
	blocked, retry := l.Blocked("1.2.3.4")
	if !blocked {
		t.Fatal("IP should be blocked after threshold")
	}
	if retry < 29*time.Second || retry > 30*time.Second {
		t.Fatalf("retry = %s, want about 30s", retry)
	}

	now = now.Add(30 * time.Second)
	if blocked, _ := l.Blocked("1.2.3.4"); blocked {
		t.Fatal("block should have expired")
	}
	l.Fail("1.2.3.4")
	if blocked, _ := l.Blocked("1.2.3.4"); blocked {
		t.Fatal("one post-cooldown failure should not immediately re-block")
	}
}

func TestFailLimiterUsesSlidingWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewFailLimiter(3, time.Second, time.Minute)
	l.now = func() time.Time { return now }

	l.Fail("ip")
	l.Fail("ip")
	now = now.Add(2 * time.Second)
	l.Fail("ip")
	if blocked, _ := l.Blocked("ip"); blocked {
		t.Fatal("attempts outside the window should not count")
	}
}

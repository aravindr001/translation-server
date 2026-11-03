package ratelimit

import (
	"math"
	"sort"
	"sync"
	"time"
)

// KeyLimiter is a token bucket per API key. It deliberately uses only the
// standard library.
type KeyLimiter struct {
	mu       sync.Mutex
	perSec   float64
	burst    float64
	buckets  map[string]*bucket
	lastTrim time.Time
	now      func() time.Time
}

type bucket struct {
	tokens   float64
	last     time.Time
	lastSeen time.Time
}

// NewKeyLimiter creates a limiter allowing perMinute requests per minute with a
// burst capacity. A burst smaller than one is raised to one.
func NewKeyLimiter(perMinute, burst int) *KeyLimiter {
	b := float64(burst)
	if b < 1 {
		b = 1
	}
	return &KeyLimiter{
		perSec:   float64(perMinute) / 60.0,
		burst:    b,
		buckets:  make(map[string]*bucket),
		lastTrim: time.Now(),
		now:      time.Now,
	}
}

// Allow consumes one token for name. If unavailable, retryAfter is a suggestion
// for a Retry-After header (at least one second).
func (l *KeyLimiter) Allow(name string) (allowed bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.trimLocked(now)

	b := l.buckets[name]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[name] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.perSec)
		b.last = now
	}
	b.lastSeen = now

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true, 0
	}
	needed := (1.0 - b.tokens) / l.perSec
	seconds := int(math.Ceil(needed))
	if seconds < 1 {
		seconds = 1
	}
	return false, time.Duration(seconds) * time.Second
}

// Retain removes buckets for keys that are no longer configured. It prevents
// unbounded growth across many rotations.
func (l *KeyLimiter) Retain(names []string) {
	keep := make(map[string]struct{}, len(names))
	for _, n := range names {
		keep[n] = struct{}{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for name := range l.buckets {
		if _, ok := keep[name]; !ok {
			delete(l.buckets, name)
		}
	}
}

func (l *KeyLimiter) trimLocked(now time.Time) {
	if now.Sub(l.lastTrim) < time.Hour {
		return
	}
	l.lastTrim = now
	for name, b := range l.buckets {
		if now.Sub(b.lastSeen) > time.Hour {
			delete(l.buckets, name)
		}
	}
}

// FailLimiter tracks failed authentication attempts per client IP. After limit
// failures inside window, the IP is blocked for block. The response to the
// threshold-crossing request is still the normal 401; subsequent requests get
// 429 for the cooldown.
type FailLimiter struct {
	mu          sync.Mutex
	limit       int
	window      time.Duration
	block       time.Duration
	entries     map[string]*failEntry
	lastCleanup time.Time
	now         func() time.Time
}

type failEntry struct {
	times        []time.Time
	blockedUntil time.Time
	lastSeen     time.Time
}

// NewFailLimiter creates a failed-authentication limiter.
func NewFailLimiter(limit int, window, block time.Duration) *FailLimiter {
	return &FailLimiter{
		limit:   limit,
		window:  window,
		block:   block,
		entries: make(map[string]*failEntry),
		now:     time.Now,
	}
}

// Blocked reports whether ip is currently in cooldown.
func (l *FailLimiter) Blocked(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.cleanupLocked(now)
	e := l.entries[ip]
	if e == nil {
		return false, 0
	}
	if now.Before(e.blockedUntil) {
		return true, ceilDuration(e.blockedUntil.Sub(now))
	}
	return false, 0
}

// Fail records a failed attempt. Once limit failures have occurred inside the
// window, a cooldown starts.
func (l *FailLimiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.cleanupLocked(now)

	e := l.entries[ip]
	if e == nil {
		e = &failEntry{}
		l.entries[ip] = e
	}
	e.lastSeen = now

	if !e.blockedUntil.IsZero() && !now.Before(e.blockedUntil) {
		e.times = e.times[:0]
		e.blockedUntil = time.Time{}
	}

	// Drop attempts outside the sliding window.
	kept := e.times[:0]
	for _, t := range e.times {
		if now.Sub(t) <= l.window {
			kept = append(kept, t)
		}
	}
	e.times = append(kept, now)
	if len(e.times) >= l.limit {
		e.blockedUntil = now.Add(l.block)
		e.times = e.times[:0]
	}
}

func (l *FailLimiter) cleanupLocked(now time.Time) {
	if now.Sub(l.lastCleanup) < time.Minute {
		return
	}
	l.lastCleanup = now
	for ip, e := range l.entries {
		idle := now.Sub(e.lastSeen)
		if idle > l.window+l.block+time.Minute && !now.Before(e.blockedUntil) {
			delete(l.entries, ip)
		}
	}
	// Defensive cap for a public endpoint with a configurable trusted header.
	const maxEntries = 16384
	if len(l.entries) > maxEntries {
		type oldEntry struct {
			ip   string
			seen time.Time
		}
		old := make([]oldEntry, 0, len(l.entries))
		for ip, e := range l.entries {
			old = append(old, oldEntry{ip: ip, seen: e.lastSeen})
		}
		// Evict a quarter of the oldest entries. O(n log n).
		sort.Slice(old, func(i, j int) bool { return old[i].seen.Before(old[j].seen) })
		for i := 0; i < len(old)/4; i++ {
			delete(l.entries, old[i].ip)
		}
	}
}

func ceilDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	sec := int(math.Ceil(d.Seconds()))
	if sec < 1 {
		sec = 1
	}
	return time.Duration(sec) * time.Second
}

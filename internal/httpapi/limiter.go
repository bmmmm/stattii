// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import (
	"sync"
	"time"
)

// limiter is a tiny per-key token bucket: burst tokens per window.
type limiter struct {
	mu        sync.Mutex
	burst     float64
	window    time.Duration
	seen      map[string]*bucket
	lastSweep time.Time
	overflow  bucket
}

// maxTrackedKeys bounds limiter memory under key-rotation floods
// (IPv6 hands every visitor a /64 of distinct keys).
const maxTrackedKeys = 32768

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(burst int, window time.Duration) *limiter {
	return &limiter{burst: float64(burst), window: window, seen: map[string]*bucket{}, overflow: bucket{tokens: float64(burst)}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// Sweep at most once per window. An insert-triggered sweep alone never
	// shrinks the map while every incoming key is fresh — exactly the flood
	// case — and would rescan on each of those inserts.
	if now.Sub(l.lastSweep) > l.window {
		for k, v := range l.seen {
			if now.Sub(v.last) > l.window {
				delete(l.seen, k)
			}
		}
		l.lastSweep = now
	}
	b, ok := l.seen[key]
	if !ok {
		if len(l.seen) >= maxTrackedKeys {
			// Rotating keys already sidesteps per-key budgets, so tracking the
			// flood would cost memory without adding protection — but letting
			// every unseen key through unmetered undoes the limiter for
			// everyone else too. One shared bucket, refilled by the same
			// math, still bounds them instead of failing open.
			return l.take(&l.overflow, now)
		}
		b = &bucket{tokens: l.burst}
		l.seen[key] = b
	}
	return l.take(b, now)
}

// take runs the refill-then-consume math shared by every per-key bucket and
// the overflow bucket. A freshly created bucket has a zero last time and
// starts full, so it skips the refill and simply spends one token.
func (l *limiter) take(b *bucket, now time.Time) bool {
	if !b.last.IsZero() {
		b.tokens += now.Sub(b.last).Seconds() / l.window.Seconds() * l.burst
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

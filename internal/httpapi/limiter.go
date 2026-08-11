// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import (
	"sync"
	"time"
)

// limiter is a tiny per-key token bucket: burst tokens per window.
type limiter struct {
	mu     sync.Mutex
	burst  float64
	window time.Duration
	seen   map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(burst int, window time.Duration) *limiter {
	return &limiter{burst: float64(burst), window: window, seen: map[string]*bucket{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.seen[key]
	if !ok {
		// Opportunistic cleanup keeps the map bounded without a goroutine.
		if len(l.seen) > 4096 {
			for k, v := range l.seen {
				if now.Sub(v.last) > l.window {
					delete(l.seen, k)
				}
			}
		}
		b = &bucket{tokens: l.burst}
		l.seen[key] = b
	} else {
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

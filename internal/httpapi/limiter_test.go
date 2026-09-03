// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import (
	"fmt"
	"testing"
	"time"
)

// Key-rotation floods must neither grow the map without bound nor lock
// out legitimate visitors; idle buckets must actually get swept. The two
// halves need opposite windows, so each gets its own limiter: the cap half
// must not be swept at all while it fills, the sweep half must be swept
// within the test's lifetime.
func TestLimiterSweepAndCap(t *testing.T) {
	// Filling the map is this half's precondition, and a sweep firing
	// mid-fill destroys it: the sweep evicts the oldest key, the map ends
	// one short of the cap, and the unseen keys below then get private
	// buckets — a setup failure that reads exactly like a fail-open. The
	// fill is not fast enough to outrun a short window: measured 7ms plain
	// but 64-80ms under -race, against a 50ms window. A window no run can
	// reach takes the timing out of the assertion entirely.
	t.Run("cap", func(t *testing.T) {
		l := newLimiter(1, time.Hour)
		for i := range maxTrackedKeys {
			l.allow(fmt.Sprintf("k%d", i))
		}
		l.mu.Lock()
		n := len(l.seen)
		l.mu.Unlock()
		if n != maxTrackedKeys {
			t.Fatalf("precondition: map must be full before probing overflow, got %d of %d entries", n, maxTrackedKeys)
		}

		if !l.allow("overflow") {
			t.Fatal("at capacity a fresh overflow bucket must still allow one unseen key")
		}
		if l.allow("overflow2") {
			t.Fatal("at capacity the overflow bucket is shared: a second unseen key must be throttled, not fail open")
		}

		l.mu.Lock()
		n = len(l.seen)
		l.mu.Unlock()
		if n > maxTrackedKeys {
			t.Fatalf("map exceeded cap: %d", n)
		}
	})

	// Whatever the fill costs, every key it wrote is idle by the time the
	// sleep ends, so the sweep on the next call has to drop all of them.
	t.Run("sweep", func(t *testing.T) {
		l := newLimiter(1, 50*time.Millisecond)
		for i := range maxTrackedKeys {
			l.allow(fmt.Sprintf("k%d", i))
		}

		time.Sleep(60 * time.Millisecond)
		l.allow("fresh")
		l.mu.Lock()
		n := len(l.seen)
		l.mu.Unlock()
		if n > 2 {
			t.Fatalf("sweep did not shrink the map: %d entries", n)
		}
	})
}

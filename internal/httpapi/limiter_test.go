// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import (
	"fmt"
	"testing"
	"time"
)

// Key-rotation floods must neither grow the map without bound nor lock
// out legitimate visitors; idle buckets must actually get swept.
func TestLimiterSweepAndCap(t *testing.T) {
	l := newLimiter(1, 50*time.Millisecond)
	for i := range maxTrackedKeys {
		l.allow(fmt.Sprintf("k%d", i))
	}
	if !l.allow("overflow") {
		t.Fatal("at capacity the limiter must fail open for new keys")
	}
	l.mu.Lock()
	n := len(l.seen)
	l.mu.Unlock()
	if n > maxTrackedKeys {
		t.Fatalf("map exceeded cap: %d", n)
	}

	time.Sleep(60 * time.Millisecond)
	l.allow("fresh")
	l.mu.Lock()
	n = len(l.seen)
	l.mu.Unlock()
	if n > 2 {
		t.Fatalf("sweep did not shrink the map: %d entries", n)
	}
}

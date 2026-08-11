// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"context"
	"time"
)

// RunScheduler ticks until ctx is done. One immediate pass, then every
// interval — reminders, deadlines, and outbox delivery all hang off Tick.
func (s *Service) RunScheduler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	s.Tick(s.now())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(s.now())
		}
	}
}

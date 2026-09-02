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

// RunCalendarFetcher polls the source feed every CalendarFetchEvery until
// ctx is done: one fetch right away, then on the ticker. A zero interval
// keeps the fetch manual (panel button, CLI, API). The HTTP client's own
// 30s timeout is the only bound on a fetch — no second one here.
func (s *Service) RunCalendarFetcher(ctx context.Context) {
	every := s.cfg.CalendarFetchEvery
	if every <= 0 || !s.CalendarConfigured() {
		return
	}
	s.fetchOnce(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.fetchOnce(ctx)
		}
	}
}

// fetchOnce runs one automatic fetch and books its outcome. The
// round-trip happens without the lock (FetchCalendar takes it only to
// sync); the bookkeeping takes it.
func (s *Service) fetchOnce(ctx context.Context) {
	_, err := s.FetchCalendar(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noteImportResultLocked(ctx, err) {
		s.saveLocked()
	}
}

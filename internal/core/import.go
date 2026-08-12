// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bmmmm/stattii/internal/icsimport"
)

// SeriesAssignment pins a responsible person to every occurrence of an
// imported series (matched by source UID): applied to existing events
// when created and to every occurrence future imports bring in.
type SeriesAssignment struct {
	SourceUID string `json:"source_uid"`
	PersonID  string `json:"person_id"`
	Role      string `json:"role,omitempty"`
}

// ImportReport is what one calendar fetch did — shown in the panel and
// returned by the fetch API.
type ImportReport struct {
	FetchedAt time.Time `json:"fetched_at"`
	Created   int       `json:"created"`
	Moved     int       `json:"moved"`
	Updated   int       `json:"updated"`
	Unchanged int       `json:"unchanged"`
	// Vanished events disappeared from the feed but are NOT cancelled
	// automatically — a feed glitch must never send cancellation mail.
	Vanished  []string `json:"vanished,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"` // cancelled here, still/again in the feed
	Skipped   []string `json:"skipped,omitempty"`   // series the importer could not expand
	// Suspect marks a fetch whose result smells like a broken feed (zero
	// occurrences while imported events exist) — its vanished sweep was
	// skipped and the report should not be trusted as decision input.
	Suspect bool `json:"suspect,omitempty"`
}

// FetchCalendar downloads the configured source feed and syncs it.
func (s *Service) FetchCalendar(ctx context.Context) (ImportReport, error) {
	src := s.cfg.CalendarSource
	if src == "" {
		return ImportReport{}, errors.New("no calendar_source configured")
	}
	src = strings.Replace(src, "webcal://", "https://", 1)
	if !strings.HasPrefix(src, "https://") && !strings.HasPrefix(src, "http://") {
		return ImportReport{}, fmt.Errorf("calendar_source must be http(s), got %q", src)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return ImportReport{}, err
	}
	resp, err := s.calendarClient().Do(req)
	if err != nil {
		return ImportReport{}, fmt.Errorf("fetch calendar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ImportReport{}, fmt.Errorf("fetch calendar: HTTP %d", resp.StatusCode)
	}
	// Read one byte past the cap: a feed that outgrows it must fail loudly.
	// Truncating mid-VEVENT would report the cut-off tail as "vanished".
	const maxFeed = 5 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFeed+1))
	if err != nil {
		return ImportReport{}, err
	}
	if len(raw) > maxFeed {
		return ImportReport{}, fmt.Errorf("fetch calendar: feed exceeds %d bytes", maxFeed)
	}

	events, skParse := icsimport.Parse(raw)
	now := s.now()
	until := now.Add(s.cfg.CalendarWindow)
	occs, skExpand := icsimport.Expand(events, now.Add(-24*time.Hour), until)
	return s.SyncCalendar(occs, append(skParse, skExpand...), until), nil
}

// SetCalendarClient overrides the HTTP client used for feed fetches (tests).
func (s *Service) SetCalendarClient(c *http.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calendarHTTP = c
}

func (s *Service) calendarClient() *http.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calendarHTTP != nil {
		return s.calendarHTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// SyncCalendar reconciles the expanded occurrences with the store.
// New keys create events (inheriting series assignments), time changes
// run the full move transaction (owner decision 2026-08-12: the source
// calendar is the truth for times, stattii propagates the change), and
// occurrences that disappeared are only REPORTED, never auto-cancelled.
// until bounds the vanished check to the fetch window.
func (s *Service) SyncCalendar(occs []icsimport.Occurrence, skipped []icsimport.Skipped, until time.Time) ImportReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep := ImportReport{FetchedAt: s.now()}
	for _, sk := range skipped {
		label := sk.Summary
		if label == "" {
			label = sk.UID
		}
		rep.Skipped = append(rep.Skipped, label+": "+sk.Reason)
	}

	seen := map[string]bool{}
	for _, o := range occs {
		seen[o.Key] = true
		title := o.Summary
		if title == "" {
			title = "(untitled)"
		}
		e := s.eventBySourceKeyLocked(o.Key)
		if e == nil {
			ne := s.createLocked(EventInput{
				Title: title, Location: o.Location, StartsAt: o.Start, EndsAt: o.End,
				SourceUID: o.UID, SourceKey: o.Key,
			}, "import")
			ev := s.state.Event(ne.ID)
			for _, sa := range s.state.SeriesAssignments {
				if sa.SourceUID == o.UID && s.state.Person(sa.PersonID) != nil {
					s.assignLocked(ev.ID, sa.PersonID, sa.Role)
				}
			}
			rep.Created++
			continue
		}
		if e.Status == StatusCancelled {
			if !e.StartsAt.Equal(o.Start) {
				rep.Conflicts = append(rep.Conflicts,
					fmt.Sprintf("%s (%s): cancelled here but moved in the source", e.Title, o.Start.Format("02 Jan 15:04")))
			}
			continue
		}
		if !e.StartsAt.Equal(o.Start) {
			if _, err := s.moveLocked(e.ID, o.Start, o.End, "calendar update", "import"); err != nil {
				rep.Conflicts = append(rep.Conflicts, fmt.Sprintf("%s: move failed: %v", e.Title, err))
				continue
			}
			e.Title, e.Location = title, o.Location
			rep.Moved++
			continue
		}
		if !e.EndsAt.Equal(o.End) {
			// An end-only change is not a move: nobody must re-confirm, and
			// a "MOVED Old: X / New: X" mail (the body leads with the start)
			// would read like nonsense. Quiet update, like a title edit.
			e.EndsAt = o.End
			e.Title, e.Location = title, o.Location
			e.Seq++
			s.auditLocked("import.updated", map[string]any{"event_id": e.ID, "title": title, "ends_at": o.End})
			rep.Updated++
			continue
		}
		if e.Title != title || e.Location != o.Location {
			e.Title, e.Location = title, o.Location
			e.Seq++
			s.auditLocked("import.updated", map[string]any{"event_id": e.ID, "title": title})
			rep.Updated++
			continue
		}
		rep.Unchanged++
	}

	for i := range s.state.Events {
		e := &s.state.Events[i]
		if e.SourceKey == "" || seen[e.SourceKey] || e.Status == StatusCancelled {
			continue
		}
		if e.StartsAt.Before(s.now()) || e.StartsAt.After(until) {
			continue // outside this fetch's window — no statement possible
		}
		if len(occs) == 0 {
			// An empty result against a non-empty local window smells like
			// a broken feed, not a cleared calendar. The vanished list is
			// the operator's decision input — refuse to draw conclusions.
			rep.Suspect = true
			rep.Skipped = append(rep.Skipped,
				"feed returned zero occurrences while imported events exist — vanished check skipped")
			s.auditLocked("import.suspect", map[string]any{"reason": "zero occurrences"})
			break
		}
		rep.Vanished = append(rep.Vanished,
			fmt.Sprintf("%s (%s)", e.Title, e.StartsAt.Format("Mon, 02 Jan 15:04")))
		s.auditLocked("import.vanished", map[string]any{"event_id": e.ID, "title": e.Title})
	}

	s.auditLocked("import.done", map[string]any{
		"created": rep.Created, "moved": rep.Moved, "updated": rep.Updated,
		"unchanged": rep.Unchanged, "vanished": len(rep.Vanished),
		"conflicts": len(rep.Conflicts), "skipped": len(rep.Skipped),
	})
	s.state.LastImport = &rep
	s.saveLocked()
	return rep
}

// LastImport returns a copy of the most recent calendar sync report.
func (s *Service) LastImport() *ImportReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.LastImport == nil {
		return nil
	}
	cp := *s.state.LastImport
	// Deep copy — the struct copy alone would share the list backing
	// arrays with live state.
	cp.Vanished = append([]string(nil), cp.Vanished...)
	cp.Conflicts = append([]string(nil), cp.Conflicts...)
	cp.Skipped = append([]string(nil), cp.Skipped...)
	return &cp
}

// CalendarConfigured reports whether a source feed is set.
func (s *Service) CalendarConfigured() bool { return s.cfg.CalendarSource != "" }

func (s *Service) eventBySourceKeyLocked(key string) *Event {
	for i := range s.state.Events {
		if s.state.Events[i].SourceKey == key {
			return &s.state.Events[i]
		}
	}
	return nil
}

// AssignSeries stores a per-series responsible and applies it to every
// existing event of that series right away. Returns how many events
// were newly assigned.
func (s *Service) AssignSeries(sourceUID, personID, role string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sourceUID == "" {
		return 0, errors.New("missing source_uid")
	}
	if s.state.Person(personID) == nil {
		return 0, fmt.Errorf("person %s: %w", personID, ErrNotFound)
	}
	exists := false
	for i := range s.state.SeriesAssignments {
		sa := &s.state.SeriesAssignments[i]
		if sa.SourceUID == sourceUID && sa.PersonID == personID {
			sa.Role = role
			exists = true
		}
	}
	if !exists {
		s.state.SeriesAssignments = append(s.state.SeriesAssignments,
			SeriesAssignment{SourceUID: sourceUID, PersonID: personID, Role: role})
	}
	n := 0
	for i := range s.state.Events {
		e := &s.state.Events[i]
		if e.SourceUID != sourceUID || e.Status == StatusCancelled {
			continue
		}
		if s.assignLocked(e.ID, personID, role) {
			n++
		}
	}
	s.auditLocked("series.assigned", map[string]any{"source_uid": sourceUID, "person_id": personID, "role": role, "events": n})
	s.saveLocked()
	return n, nil
}

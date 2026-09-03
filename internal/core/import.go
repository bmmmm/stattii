// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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
	// Silent lists source moves whose MOVED notice reached nobody — the
	// importer pages the admin once per fetch about them, not once per
	// occurrence (a rescheduled series is thirty moves in one sync).
	Silent []string `json:"silent,omitempty"`
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
		// The value is not echoed: it is the operator's own config, and
		// a secret address must not be written into an error string.
		return ImportReport{}, errors.New("calendar_source must be an http(s) or webcal URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		// A source that passes the prefix check but does not parse (a
		// trailing newline on a pasted config value is enough) fails
		// here, and this error reaches the operator raw: the fetch API
		// renders it into its JSON body, the admin page onto its error
		// screen — neither passes the bookkeeping that redacts the rest.
		// A malformed URL is also the one shape redactURLs cannot cut
		// down, so the address is dropped whole and only the reason kept.
		var ue *url.Error
		if errors.As(err, &ue) {
			return ImportReport{}, fmt.Errorf("calendar_source is not a usable URL: %w", ue.Err)
		}
		return ImportReport{}, redactErr(err)
	}
	resp, err := s.calendarClient().Do(req)
	if err != nil {
		// A secret iCal address IS the credential, and a transport
		// failure renders the whole URL into the error. Redact here, at
		// the boundary — everything downstream (audit, admin page, mail)
		// only ever sees the host and the failure kind.
		return ImportReport{}, fmt.Errorf("fetch calendar: %w", redactErr(err))
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
	now := s.clock()
	until := now.Add(s.cfg.CalendarWindow)
	occs, skExpand := icsimport.Expand(events, now.Add(-24*time.Hour), until)
	return s.SyncCalendar(occs, append(skParse, skExpand...), until), nil
}

// noteImportResultLocked books the outcome of an automatic fetch: every
// failure is audited, the admin is paged once per failure episode (the
// healthy→failed transition) and once on recovery. Shutdown is not a
// failure. Returns whether anything was enqueued.
func (s *Service) noteImportResultLocked(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		// Second line of defence: FetchCalendar redacts its transport
		// errors, but every path into this bookkeeping ends in the audit
		// trail and in admin mail, so nothing unredacted passes here.
		err = redactErr(err)
		s.auditLocked("import.failed", map[string]any{"error": err.Error()})
		if s.importFailed {
			return false
		}
		s.importFailed = true
		s.notifyAdminLocked("Calendar fetch failing",
			fmt.Sprintf("The automatic calendar fetch failed: %v\n\n"+
				"Events keep their last imported state — nothing is cancelled by this. "+
				"Check the source URL and the network; this pages again only after a recovery.", err))
		return true
	}
	if !s.importFailed {
		return false
	}
	s.importFailed = false
	s.auditLocked("import.recovered", map[string]any{})
	s.notifyAdminLocked("Calendar fetch recovered", "The automatic calendar fetch works again.")
	return true
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
			for _, sa := range s.state.SeriesAssignments {
				if sa.SourceUID == o.UID && s.state.Person(sa.PersonID) != nil {
					s.assignLocked(ne.ID, sa.PersonID, sa.Role)
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
		if !e.VanishedAt.IsZero() {
			// Back in the feed: the glitch (or the operator's fix) is
			// over. On a suspect fetch this loop never runs, so the
			// marker survives exactly the fetches it must not trust.
			e.VanishedAt = time.Time{}
			s.auditLocked("import.reappeared", map[string]any{"event_id": e.ID, "title": e.Title})
		}
		if !e.StartsAt.Equal(o.Start) {
			// No note: the move body never carries it, and a non-empty
			// note would overwrite what the operator wrote on the event.
			if _, err := s.moveLocked(e.ID, o.Start, o.End, "", "import"); err != nil {
				rep.Conflicts = append(rep.Conflicts, fmt.Sprintf("%s: move failed: %v", e.Title, err))
				continue
			}
			e.Title, e.Location = title, o.Location
			rep.Moved++
			if e.FanOutCount == 0 {
				rep.Silent = append(rep.Silent,
					fmt.Sprintf("%s (now %s)", e.Title, e.StartsAt.Format("Mon, 02 Jan 15:04")))
			}
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

	var gone []*Event
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
		if e.VanishedAt.IsZero() {
			e.VanishedAt = rep.FetchedAt
			gone = append(gone, e)
		}
	}
	if len(gone) > 0 {
		// One page per fetch, on the transition only: a still-missing
		// event is the same fact as last time, not news.
		lines := make([]string, 0, len(gone))
		for _, e := range gone {
			line := fmt.Sprintf("- %s (%s)", e.Title, e.StartsAt.Format(timeFmt))
			if e.IfUnconfirmed == "cancel" {
				line += " — WILL auto-cancel at its deadline unless you act"
			}
			lines = append(lines, line)
		}
		// Ask first, page second: the page states how many people were
		// actually asked, and "nobody" is the line that tells the
		// operator this one is entirely theirs to chase.
		asked := s.askVanishedLocked(gone)
		who := fmt.Sprintf("%d responsible person(s) have been asked directly, once, with their own confirm/cancel links.", asked)
		if asked == 0 {
			who = "NOBODY was asked — none of these events has a responsible with a usable channel. This one is yours to chase by hand."
		}
		s.notifyAdminLocked(fmt.Sprintf("Gone from the calendar: %d event(s)", len(gone)),
			"These events are no longer in the source calendar. stattii has NOT cancelled them — a feed glitch must never send cancellation mail.\n"+
				strings.Join(lines, "\n")+
				"\n\n"+who+
				"\nIf they are really off, cancel them in the panel (that sends the notices). If the feed is wrong, fix the feed — they clear on the next fetch. "+
				"Until then their reminders go out as usual, with a note that the entry disappeared.")
	}

	if len(rep.Silent) > 0 {
		s.notifyAdminLocked(fmt.Sprintf("Moved but nobody was told: %d event(s)", len(rep.Silent)),
			"The calendar source moved these events and stattii ran the move transaction, but the MOVED notice reached nobody "+
				"(no broadcast target, no reachable responsible, no guest with an address):\n- "+
				strings.Join(rep.Silent, "\n- ")+
				"\nTell people by hand, then add a broadcast target or give the responsible people a channel.")
	}

	s.auditLocked("import.done", map[string]any{
		"created": rep.Created, "moved": rep.Moved, "updated": rep.Updated,
		"unchanged": rep.Unchanged, "vanished": len(rep.Vanished),
		"conflicts": len(rep.Conflicts), "skipped": len(rep.Skipped), "silent": len(rep.Silent),
	})
	s.state.LastImport = &rep
	s.saveLocked()
	return rep
}

// askVanishedLocked tells the responsible people that their occurrence
// left the source calendar — once per disappearance, on the marker
// transition its caller already filtered for.
//
// The admin page alone is not enough: the assignee is the one person who
// knows whether "gone from the feed" means "off" or "glitch", and the
// reminder cannot carry that question. The reminder is one-shot, so an
// occurrence that vanishes after its ask went out reaches nobody — while
// an if_unconfirmed=cancel deadline still fires. Same links as the
// reminder (linksLocked reuses the pair), so answering either message
// lands on the same decision. Returns how many people were asked — the
// admin page says so, and zero is a fact the operator must see.
func (s *Service) askVanishedLocked(gone []*Event) int {
	asked := 0
	for _, e := range gone {
		_, reachable := s.reachableLocked(e.ID)
		if len(reachable) == 0 {
			continue // unstaffed or unreachable — the admin page stands alone
		}
		header := e.Title + "\n" + e.StartsAt.Format(timeFmt)
		if e.Location != "" {
			header += "\nLocation: " + e.Location
		}
		header += "\n\nThis entry has disappeared from the source calendar. " +
			"stattii has NOT cancelled it — a feed glitch must never send cancellation mail, so this is your decision."
		if e.IfUnconfirmed == "cancel" {
			header += fmt.Sprintf("\nIf nobody answers, it WILL auto-cancel at its deadline (%s).",
				e.StartsAt.Add(-s.cfg.DeadlineLead).Format(timeFmt))
		}
		for _, p := range reachable {
			cTok, xTok := s.linksLocked(e.ID, p.ID)
			s.enqueueToPersonLocked(p, OutboxItem{
				EventID: e.ID, Purpose: "vanished",
				Subject: "Disappeared from the calendar: " + e.Title,
				Body: fmt.Sprintf(
					"%s\n\nDoes it still take place?\nYES, keep it:  %s\nNO, cancel it: %s\n\nThese links are personal — please do not forward.",
					header, s.actionURL(cTok), s.actionURL(xTok)),
				Buttons: []Button{
					{Label: "✅ Takes place", Data: cTok},
					{Label: "❌ Cancel event", Data: xTok},
				},
			})
		}
		asked += len(reachable)
		s.auditLocked("vanished.asked", map[string]any{
			"event_id": e.ID, "title": e.Title,
			"people": personNames(reachable), "count": len(reachable),
		})
	}
	return asked
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
	cp.Silent = append([]string(nil), cp.Silent...)
	return &cp
}

// CalendarConfigured reports whether a source feed is set.
func (s *Service) CalendarConfigured() bool { return s.cfg.CalendarSource != "" }

// VanishedEvents lists the imported events currently missing from the
// source and still ahead: the operator's open decisions. Cancelling one
// in the panel is the dismissal — the notices go out, the row goes away.
// A vanished occurrence whose time has passed drops off the list by
// itself (nothing left to decide); its VanishedAt stays in the record.
func (s *Service) VanishedEvents() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var out []Event
	for i := range s.state.Events {
		e := &s.state.Events[i]
		if e.VanishedAt.IsZero() || e.Status == StatusCancelled || eventExpiry(e).Before(now) {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	return out
}

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

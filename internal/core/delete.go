// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"fmt"
	"strings"
	"time"
)

// ---- deletion -------------------------------------------------------------
//
// Deleting is the operator's eraser for what should never have been
// there: a mistyped person, a leftover cancelled event. It is not a
// second way to call an event off — that goes through the cancellation
// transaction, or the people who were told about the event hear nothing
// about its end (invariant 3). Both deletions therefore refuse while
// anyone could still be waiting on the thing, and both leave the outbox
// and audit.jsonl alone: an undelivered notice still has to go out, a
// delivered one is the proof that it did.

// dropWhere removes every row the predicate picks and reports how many
// went. The state's "tables" are plain slices; this is the one filter
// for all of them.
func dropWhere[T any](rows *[]T, drop func(T) bool) int {
	kept, n := (*rows)[:0], 0
	for _, r := range *rows {
		if drop(r) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	*rows = kept
	return n
}

// eventEnd is when an event is over: EndsAt, or StartsAt when no end
// was given.
func eventEnd(e *Event) time.Time {
	if e.EndsAt.IsZero() {
		return e.StartsAt
	}
	return e.EndsAt
}

// DeleteEvent removes an event and everything that only existed for it:
// assignments, action links, responses, proposals, the invite link and
// its guests. A scheduled event that has not happened yet is refused —
// cancel it first, so the notices actually go out.
//
// An imported occurrence is refused while the import could bring it
// back: the row is derived data, and a missing one is recreated by the
// next sync as *scheduled*. For a cancelled occurrence that would undo
// the cancellation — the import itself never revives one (invariant 9),
// and a deletion must not become the loophole that does; the row IS the
// tombstone. Only a ghost the feed has already dropped (VanishedAt, set
// by the sync) and that is not cancelled can go, or anything at all once
// the import is switched off.
func (s *Service) DeleteEvent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.state.Event(id)
	if e == nil {
		return ErrNotFound
	}
	if e.Status != StatusCancelled && !eventEnd(e).Before(s.now()) {
		return fmt.Errorf("%q is still on — cancel it first so the notices go out; deleting it tells nobody", e.Title)
	}
	if e.SourceUID != "" && (e.Status == StatusCancelled || e.VanishedAt.IsZero()) {
		return fmt.Errorf("%q comes from the calendar import — deleting the row here means the next sync creates it again as scheduled (undoing the cancellation, if it is cancelled); remove it in the source calendar instead", e.Title)
	}
	if n := s.fanOutInFlightLocked(e); n > 0 {
		return fmt.Errorf("%q has %d notice(s) of its last propagation still on the way — deleting it now takes the proof that they arrived with it; wait for the outbox", e.Title, n)
	}
	gone := *e
	dropped := map[string]any{
		"event_id": id, "title": gone.Title, "status": gone.Status,
		"assignments": dropWhere(&s.state.Assignments, func(a Assignment) bool { return a.EventID == id }),
		"links":       dropWhere(&s.state.Links, func(l ActionLink) bool { return l.EventID == id }),
		"responses":   dropWhere(&s.state.Responses, func(r Response) bool { return r.EventID == id }),
		"proposals":   dropWhere(&s.state.Proposals, func(p Proposal) bool { return p.EventID == id }),
		"guests":      dropWhere(&s.state.Guests, func(g Guest) bool { return g.EventID == id }),
		"invites":     dropWhere(&s.state.Invites, func(i InviteLink) bool { return i.EventID == id }),
	}
	if gone.SourceUID != "" {
		dropped["source_uid"] = gone.SourceUID
	}
	dropWhere(&s.state.Events, func(ev Event) bool { return ev.ID == id })
	s.auditLocked("event.deleted", dropped)
	s.fireWebhooksLocked("event.deleted", gone)
	s.saveLocked()
	return nil
}

// fanOutInFlightLocked counts the rows of the event's last propagation
// transaction that are still moving — queued or retrying. Deleting the
// event would take the `propagation` view of exactly those with it, and
// leave the rows themselves rendering without a title. A row that has
// run out of attempts has stopped moving: it stays in the outbox as the
// alarm it is, and no longer blocks the deletion.
func (s *Service) fanOutInFlightLocked(e *Event) int {
	n := 0
	for _, o := range s.state.Outbox {
		if !inLastFanOut(e, o) {
			continue
		}
		if st := s.OutboxState(o); st == "queued" || st == "retrying" {
			n++
		}
	}
	return n
}

// DeletePerson removes a person and the traces that only make sense
// with them: assignments, their action links, their responses, their
// per-series responsibilities. It refuses while they are still
// responsible for something that has not happened yet — dropping that
// assignment silently would take the event's confirmation cycle with
// it, and the dead-man-switch would fire on an event nobody knew they
// still had. Unassign first; then the deletion is a bookkeeping act.
//
// Their answers are NOT removed: "someone attested that this event takes
// place" is a fact about the event, and the one this product is built to
// keep. The rows stay with the person id they were written with — it no
// longer resolves to anyone, and audit.jsonl is where the name is. No
// panel surface renders an answer without a matching assignment, so a
// kept row shows up nowhere it would confuse.
func (s *Service) DeletePerson(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.state.Person(id)
	if p == nil {
		return ErrNotFound
	}
	now := s.now()
	var open []string
	for _, a := range s.state.Assignments {
		if a.PersonID != id {
			continue
		}
		e := s.state.Event(a.EventID)
		if e == nil || eventEnd(e).Before(now) {
			continue
		}
		// Cancelled counts too: a reinstate brings the event back with
		// its assignees, and one that lost its sole responsible in the
		// meantime comes back unstaffed — nobody to ask, and the
		// dead-man-switch deciding on silence nobody could break.
		open = append(open, e.Title)
	}
	if len(open) > 0 {
		return fmt.Errorf("%s is still responsible for %s — unassign first", p.Name, strings.Join(open, ", "))
	}
	dropped := map[string]any{
		"person_id": id, "name": p.Name,
		"assignments":        dropWhere(&s.state.Assignments, func(a Assignment) bool { return a.PersonID == id }),
		"links":              dropWhere(&s.state.Links, func(l ActionLink) bool { return l.PersonID == id }),
		"proposals":          dropWhere(&s.state.Proposals, func(pr Proposal) bool { return pr.PersonID == id }),
		"series_assignments": dropWhere(&s.state.SeriesAssignments, func(sa SeriesAssignment) bool { return sa.PersonID == id }),
	}
	name := p.Name
	dropWhere(&s.state.People, func(pp Person) bool { return pp.ID == id })
	s.auditLocked("person.deleted", dropped)
	// Symmetric to event.deleted, and deliberately not the Person struct:
	// that carries the portal token and every channel address, and a
	// webhook payload is "ids and status, no third-party PII".
	s.fireWebhooksLocked("person.deleted", map[string]any{"id": id, "name": name})
	s.saveLocked()
	return nil
}

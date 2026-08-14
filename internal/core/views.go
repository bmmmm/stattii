// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// ---- read access ----------------------------------------------------------

func (s *Service) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Event(nil), s.state.Events...)
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	return out
}

func (s *Service) EventByID(id string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.state.Event(id); e != nil {
		return *e, nil
	}
	return Event{}, ErrNotFound
}

func (s *Service) People() []Person {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Deep copy: a struct copy alone would share Channels with live state,
	// letting callers mutate it outside the lock.
	out := append([]Person(nil), s.state.People...)
	for i := range out {
		out[i].Channels = append([]Address(nil), out[i].Channels...)
	}
	return out
}

func (s *Service) PersonPortalURL(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.state.Person(id)
	if p == nil {
		return "", ErrNotFound
	}
	return s.portalURL(p.PortalToken), nil
}

func (s *Service) Proposals() []Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Proposal(nil), s.state.Proposals...)
}

func (s *Service) Broadcasts() []Broadcast {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Broadcast(nil), s.state.Broadcasts...)
}

func (s *Service) Webhooks() []Webhook {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Webhook(nil), s.state.Webhooks...)
	for i := range out {
		out[i].Events = append([]string(nil), out[i].Events...)
	}
	return out
}

func (s *Service) Responses(eventID string) []Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Response
	for _, r := range s.state.Responses {
		if eventID == "" || r.EventID == eventID {
			out = append(out, r)
		}
	}
	return out
}

func (s *Service) Audit(limit int) ([]AuditEntry, error) {
	return s.store.ReadAudit(limit)
}

// ---- overview -------------------------------------------------------------

// Overview is the operator's one-glance answer: every event with its
// responsible people and their latest answer, plus outbox and proposal
// health. One server-side join so CLI and web admin render the same data.
type Overview struct {
	Events        []OverviewEvent `json:"events"`
	Outbox        OutboxSummary   `json:"outbox"`
	OpenProposals int             `json:"open_proposals"`
	People        int             `json:"people"`
}

type OverviewEvent struct {
	Event     Event              `json:"event"`
	Assignees []OverviewAssignee `json:"assignees"`
}

type OverviewAssignee struct {
	PersonID string     `json:"person_id"`
	Name     string     `json:"name"`
	Role     string     `json:"role,omitempty"`
	Trust    TrustLevel `json:"trust"`
	// Latest recorded response for this event; empty Action = pending.
	Action ActionKind `json:"action,omitempty"`
	Via    string     `json:"via,omitempty"`
	At     time.Time  `json:"at,omitzero"`
}

type OutboxSummary struct {
	Delivered int `json:"delivered"`
	Pending   int `json:"pending"`
	Failed    int `json:"failed"`
}

func (s *Service) Overview() Overview {
	s.mu.Lock()
	defer s.mu.Unlock()
	ov := Overview{People: len(s.state.People)}
	// Index once instead of rescanning per event — the nested scans were
	// quadratic in the event count. Latest-response folding keeps the
	// original tie-break: strictly-later At wins, ties keep the earlier.
	people := make(map[string]*Person, len(s.state.People))
	for i := range s.state.People {
		people[s.state.People[i].ID] = &s.state.People[i]
	}
	byEvent := make(map[string][]Assignment, len(s.state.Events))
	for _, a := range s.state.Assignments {
		byEvent[a.EventID] = append(byEvent[a.EventID], a)
	}
	type respKey struct{ event, person string }
	latest := make(map[respKey]Response, len(s.state.Responses))
	for _, r := range s.state.Responses {
		k := respKey{r.EventID, r.PersonID}
		if r.At.After(latest[k].At) {
			latest[k] = r
		}
	}
	for _, e := range s.state.Events {
		oe := OverviewEvent{Event: e}
		for _, a := range byEvent[e.ID] {
			p := people[a.PersonID]
			if p == nil {
				continue
			}
			oa := OverviewAssignee{PersonID: p.ID, Name: p.Name, Role: a.Role, Trust: p.Trust}
			if r, ok := latest[respKey{e.ID, p.ID}]; ok {
				oa.Action, oa.Via, oa.At = r.Action, r.Via, r.At
			}
			oe.Assignees = append(oe.Assignees, oa)
		}
		ov.Events = append(ov.Events, oe)
	}
	sort.Slice(ov.Events, func(i, j int) bool {
		return ov.Events[i].Event.StartsAt.Before(ov.Events[j].Event.StartsAt)
	})
	for _, o := range s.state.Outbox {
		switch s.outboxState(o) {
		case "delivered":
			ov.Outbox.Delivered++
		case "failed":
			ov.Outbox.Failed++
		default:
			ov.Outbox.Pending++
		}
	}
	for _, p := range s.state.Proposals {
		if p.DecidedAt.IsZero() {
			ov.OpenProposals++
		}
	}
	return ov
}

// OutboxItems lists outbound messages, optionally only undelivered ones.
func (s *Service) OutboxItems(pendingOnly bool) []OutboxItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []OutboxItem
	for _, o := range s.state.Outbox {
		if pendingOnly && o.Delivered() {
			continue
		}
		o.Buttons = append([]Button(nil), o.Buttons...)
		if o.Headers != nil {
			h := make(map[string]string, len(o.Headers))
			for k, v := range o.Headers {
				h[k] = v
			}
			o.Headers = h
		}
		out = append(out, o)
	}
	return out
}

// RetryOutbox re-arms a failed or stuck item; the next tick attempts it.
func (s *Service) RetryOutbox(id string) (OutboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Outbox {
		o := &s.state.Outbox[i]
		if o.ID != id {
			continue
		}
		if o.Delivered() {
			return *o, errors.New("already delivered — nothing to retry")
		}
		o.Attempts = 0
		o.NextAttempt = s.now()
		s.auditLocked("outbox.retry", map[string]any{"outbox_id": id})
		s.saveLocked()
		return *o, nil
	}
	return OutboxItem{}, ErrNotFound
}

// SendTest sends a test message to every channel of a person — through
// the outbox like any real message, so the admin gets the same
// sent/delivered proof, and with an immediate delivery attempt so the
// result is visible right away.
func (s *Service) SendTest(personID string) ([]OutboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.state.Person(personID)
	if p == nil {
		return nil, ErrNotFound
	}
	if len(p.Channels) == 0 {
		return nil, fmt.Errorf("%s has no channels to test", p.Name)
	}
	ids := map[string]bool{}
	for _, id := range s.enqueueToPersonLocked(p, OutboxItem{
		Purpose: "test",
		Subject: "stattii test message",
		Body:    "This is a test message from stattii — delivery to you works.\nNothing to do, nothing to click.",
	}) {
		ids[id] = true
	}
	s.auditLocked("test.sent", map[string]any{"person_id": p.ID, "channels": len(p.Channels)})
	s.tickOutboxLocked(s.now())
	s.saveLocked()
	var out []OutboxItem
	for _, o := range s.state.Outbox {
		if ids[o.ID] {
			out = append(out, o)
		}
	}
	return out, nil
}

// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// ---- portal ---------------------------------------------------------------

type PortalItem struct {
	Event    Event
	Response *Response
}

type PortalView struct {
	Person     Person
	CanPropose bool // propose or direct
	Direct     bool
	Items      []PortalItem
}

func (s *Service) Portal(token string) (PortalView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.personByPortalLocked(token)
	if p == nil {
		return PortalView{}, ErrNotFound
	}
	v := PortalView{
		Person:     *p,
		CanPropose: p.Trust == TrustPropose || p.Trust == TrustDirect,
		Direct:     p.Trust == TrustDirect,
	}
	now := s.now()
	for _, e := range s.state.EventsFor(p.ID) {
		if eventExpiry(e).Before(now) {
			continue
		}
		v.Items = append(v.Items, PortalItem{Event: *e, Response: s.state.ResponseFor(e.ID, p.ID)})
	}
	sort.Slice(v.Items, func(i, j int) bool { return v.Items[i].Event.StartsAt.Before(v.Items[j].Event.StartsAt) })
	return v, nil
}

func (s *Service) personByPortalLocked(token string) *Person {
	if token == "" {
		return nil
	}
	for i := range s.state.People {
		if s.state.People[i].PortalToken == token {
			return &s.state.People[i]
		}
	}
	return nil
}

func (s *Service) assignedLocked(eventID, personID string) bool {
	for _, a := range s.state.Assignments {
		if a.EventID == eventID && a.PersonID == personID {
			return true
		}
	}
	return false
}

// PortalRespond is the yes/no of an assigned person — allowed on every
// trust level.
func (s *Service) PortalRespond(token, eventID string, action ActionKind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.personByPortalLocked(token)
	if p == nil {
		return ErrNotFound
	}
	if !s.assignedLocked(eventID, p.ID) {
		return ErrNotFound
	}
	var err error
	switch action {
	case ActionConfirm:
		_, err = s.confirmLocked(eventID, p.ID, "portal")
	case ActionCancel:
		_, err = s.cancelLocked(eventID, p.ID, "", "portal")
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	if err == nil {
		s.saveLocked()
	}
	return err
}

// PortalSubmit handles cancel/move/create beyond a plain response: trust
// "direct" applies immediately, "propose" files a proposal, "respond" may not.
func (s *Service) PortalSubmit(token, kind, eventID, title, note string, start, end time.Time) (applied bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.personByPortalLocked(token)
	if p == nil {
		return false, ErrNotFound
	}
	// Both trust paths validate the same way; a proposal with no title or
	// time would just waste the admin's attention.
	switch kind {
	case "create":
		in := EventInput{Title: title, Note: note, StartsAt: start, EndsAt: end}
		if err := in.Validate(); err != nil {
			return false, err
		}
	case "move":
		if start.IsZero() {
			return false, errors.New("starts_at is required for a move")
		}
	case "cancel":
	default:
		return false, fmt.Errorf("unknown kind %q", kind)
	}
	// Cancel and move target an existing event: like PortalRespond, the
	// portal holder must be assigned to it — on every trust level. Without
	// this a single direct-trust token could cancel ANY event by id.
	if kind == "cancel" || kind == "move" {
		if !s.assignedLocked(eventID, p.ID) {
			return false, ErrNotFound
		}
	}
	switch p.Trust {
	case TrustDirect:
		if err := s.applyChangeLocked(kind, p.ID, eventID, title, note, start, end, "portal"); err != nil {
			return false, err
		}
		s.saveLocked()
		return true, nil
	case TrustPropose:
		s.fileProposalLocked(Proposal{
			PersonID: p.ID, Kind: kind, EventID: eventID,
			Title: title, Note: note, StartsAt: start, EndsAt: end,
		}, p.Name, "portal",
			fmt.Sprintf("%s proposes: %s %s %s\nDecide: stattii proposal list / decide", p.Name, kind, eventID, title))
		return false, nil
	default:
		return false, ErrForbidden
	}
}

func (s *Service) DecideProposal(id string, accept bool) (Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pr *Proposal
	for i := range s.state.Proposals {
		if s.state.Proposals[i].ID == id {
			pr = &s.state.Proposals[i]
			break
		}
	}
	if pr == nil {
		return Proposal{}, ErrNotFound
	}
	if !pr.DecidedAt.IsZero() {
		return *pr, errors.New("proposal already decided")
	}
	if accept {
		// The apply gates the decision: a proposal whose change failed
		// stays open instead of being recorded — and announced — as done.
		if err := s.applyChangeLocked(pr.Kind, pr.PersonID, pr.EventID, pr.Title, pr.Note, pr.StartsAt, pr.EndsAt, "proposal"); err != nil {
			return *pr, err
		}
	}
	pr.DecidedAt = s.now()
	pr.Accepted = accept
	s.auditLocked("proposal.decided", map[string]any{"proposal_id": id, "accepted": accept})
	s.fireWebhooksLocked("proposal.decided", *pr)
	if p := s.state.Person(pr.PersonID); p != nil {
		verdict := "declined"
		if accept {
			verdict = "accepted"
		}
		s.enqueueToPersonLocked(p, OutboxItem{
			Purpose: "proposal",
			Subject: "Proposal " + verdict,
			Body:    fmt.Sprintf("Your proposal (%s) was %s.", pr.Kind, verdict),
		})
	}
	s.saveLocked()
	return *pr, nil
}

// fileProposalLocked stamps, records, audits, webhooks, and pages the admin
// about a new proposal — the shared tail of every proposal entry point.
func (s *Service) fileProposalLocked(pr Proposal, personName, via, adminBody string) Proposal {
	pr.ID = NewID("pr")
	pr.CreatedAt = s.now()
	s.state.Proposals = append(s.state.Proposals, pr)
	s.auditLocked("proposal.created", map[string]any{"proposal_id": pr.ID, "person_id": pr.PersonID, "kind": pr.Kind, "event_id": pr.EventID, "via": via})
	s.fireWebhooksLocked("proposal.created", pr)
	s.notifyAdminLocked("Proposal from "+personName, adminBody)
	s.saveLocked()
	return pr
}

// applyChangeLocked executes a cancel/move/create on behalf of a person —
// the shared tail of direct portal submits and accepted proposals. Callers
// persist after it succeeds; on error nothing outward has happened.
func (s *Service) applyChangeLocked(kind, personID, eventID, title, note string, start, end time.Time, via string) error {
	switch kind {
	case "cancel":
		_, err := s.cancelLocked(eventID, personID, note, via)
		if errors.Is(err, ErrCancelled) {
			return nil // cancelling a cancelled event is an idempotent no-op
		}
		return err
	case "move":
		_, err := s.moveLocked(eventID, start, end, note, personID)
		return err
	case "create":
		e := s.createLocked(EventInput{Title: title, Note: note, StartsAt: start, EndsAt: end}, personID)
		s.assignLocked(e.ID, personID, "")
		return nil
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
}

// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"fmt"
	"time"
)

// ---- action links ---------------------------------------------------------

func eventExpiry(e *Event) time.Time {
	if !e.EndsAt.IsZero() {
		return e.EndsAt
	}
	return e.StartsAt.Add(6 * time.Hour)
}

// GenerateLinks creates (or reuses) the confirm/cancel link pair for one
// person and event, and returns the full URLs.
func (s *Service) GenerateLinks(eventID, personID string) (confirmURL, cancelURL string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Event(eventID) == nil || s.state.Person(personID) == nil {
		return "", "", ErrNotFound
	}
	c, x := s.linksLocked(eventID, personID)
	s.saveLocked()
	return s.actionURL(c), s.actionURL(x), nil
}

func (s *Service) linksLocked(eventID, personID string) (confirmTok, cancelTok string) {
	for _, l := range s.state.Links {
		if l.EventID == eventID && l.PersonID == personID && l.RevokedAt.IsZero() {
			switch l.Action {
			case ActionConfirm:
				confirmTok = l.Token
			case ActionCancel:
				cancelTok = l.Token
			}
		}
	}
	if confirmTok == "" {
		confirmTok = NewToken()
		s.state.Links = append(s.state.Links, ActionLink{
			Token: confirmTok, EventID: eventID, PersonID: personID, Action: ActionConfirm,
		})
	}
	if cancelTok == "" {
		cancelTok = NewToken()
		s.state.Links = append(s.state.Links, ActionLink{
			Token: cancelTok, EventID: eventID, PersonID: personID, Action: ActionCancel,
		})
	}
	return confirmTok, cancelTok
}

// RevokeLinks revokes every active action link in scope: both ids →
// that pair, event only → the whole event, person only → that person
// across all events. At least one id is required — "revoke everything"
// must not be reachable by a blank form. Regenerating afterwards mints
// fresh tokens, because linksLocked skips revoked rows.
func (s *Service) RevokeLinks(eventID, personID string) (int, error) {
	if eventID == "" && personID == "" {
		return 0, errors.New("event_id or person_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if eventID != "" && s.state.Event(eventID) == nil {
		return 0, ErrNotFound
	}
	if personID != "" && s.state.Person(personID) == nil {
		return 0, ErrNotFound
	}
	n := 0
	for i := range s.state.Links {
		l := &s.state.Links[i]
		if !l.RevokedAt.IsZero() {
			continue
		}
		if eventID != "" && l.EventID != eventID {
			continue
		}
		if personID != "" && l.PersonID != personID {
			continue
		}
		l.RevokedAt = s.now()
		n++
	}
	if n > 0 {
		s.auditLocked("links.revoked", map[string]any{"event_id": eventID, "person_id": personID, "count": n})
		s.saveLocked()
	}
	return n, nil
}

func (s *Service) actionURL(token string) string { return s.cfg.BaseURL + "/a/" + token }
func (s *Service) portalURL(token string) string { return s.cfg.BaseURL + "/p/" + token }

// ActionView is what the GET page needs to render.
type ActionView struct {
	Event   Event
	Person  Person
	Action  ActionKind
	Decided *Response // latest decision of this person, if any
}

func (s *Service) ResolveAction(token string) (ActionView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, e, p, err := s.lookupLinkLocked(token)
	if err != nil {
		return ActionView{}, err
	}
	return ActionView{Event: *e, Person: *p, Action: l.Action, Decided: s.state.ResponseFor(e.ID, p.ID)}, nil
}

func (s *Service) lookupLinkLocked(token string) (*ActionLink, *Event, *Person, error) {
	for i := range s.state.Links {
		l := &s.state.Links[i]
		if l.Token != token {
			continue
		}
		if !l.RevokedAt.IsZero() {
			return nil, nil, nil, ErrGone
		}
		e := s.state.Event(l.EventID)
		p := s.state.Person(l.PersonID)
		if e == nil || p == nil {
			return nil, nil, nil, ErrNotFound
		}
		if s.now().After(eventExpiry(e)) {
			return nil, nil, nil, ErrGone
		}
		return l, e, p, nil
	}
	return nil, nil, nil, ErrNotFound
}

// ProposeMoveViaLink files a move proposal from an action-link holder.
// Deliberately trust-independent: a proposal never applies by itself, so
// even respond-level people may counter a cancellation with a new time.
func (s *Service) ProposeMoveViaLink(token string, start, end time.Time, note string) (Proposal, error) {
	if start.IsZero() {
		return Proposal{}, errors.New("starts_at is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, e, p, err := s.lookupLinkLocked(token)
	if err != nil {
		return Proposal{}, err
	}
	pr := s.fileProposalLocked(Proposal{
		PersonID: p.ID, Kind: "move", EventID: e.ID,
		StartsAt: start, EndsAt: end, Note: note,
	}, p.Name, "link",
		fmt.Sprintf("%s proposes to move %q to %s.", p.Name, e.Title, start.Format(timeFmt)))
	return pr, nil
}

// ApplyAction performs the link's action. Only ever called on POST — a GET
// must never mutate, because mail scanners prefetch links.
func (s *Service) ApplyAction(token string) (ActionView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, e, p, err := s.lookupLinkLocked(token)
	if err != nil {
		return ActionView{}, err
	}
	switch l.Action {
	case ActionConfirm:
		_, err = s.confirmLocked(e.ID, p.ID, "link")
	case ActionCancel:
		if e.Status == StatusCancelled {
			err = nil // idempotent: their goal is reality already
		} else {
			_, err = s.cancelLocked(e.ID, p.ID, "", "link")
		}
	}
	if err != nil {
		return ActionView{Event: *e, Person: *p, Action: l.Action}, err
	}
	s.saveLocked()
	return ActionView{Event: *e, Person: *p, Action: l.Action, Decided: s.state.ResponseFor(e.ID, p.ID)}, nil
}

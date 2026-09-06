// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"fmt"
	"strconv"
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
	n := s.revokeLinksLocked(eventID, personID)
	if n > 0 {
		s.saveLocked()
	}
	return n, nil
}

// revokeLinksLocked is RevokeLinks without the lock and the id checks —
// shared with Unassign, which already holds the mutex.
func (s *Service) revokeLinksLocked(eventID, personID string) int {
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
	}
	return n
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
// must never mutate, because mail scanners prefetch links. reason is the
// optional why of a cancel link; confirms ignore it.
func (s *Service) ApplyAction(token, reason string) (ActionView, error) {
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
			_, err = s.cancelLocked(e.ID, p.ID, reason, "link")
		}
	}
	if err != nil {
		return ActionView{Event: *e, Person: *p, Action: l.Action}, err
	}
	s.saveLocked()
	return ActionView{Event: *e, Person: *p, Action: l.Action, Decided: s.state.ResponseFor(e.ID, p.ID)}, nil
}

// VerifyTelegramActor checks that a Telegram inline-button press came from
// the person the link was minted for, not from another member of a group
// chat the reminder was posted to. fromID is the callback_query's from.id,
// as a decimal string; it is compared against the person's stored telegram
// channel address (the chat id Send used), which for an ordinary one-on-one
// bot chat equals that person's own user id — so a genuine press always
// matches, while a press coming through someone else's private chat never
// does.
//
// A channel address that is not a plain positive integer cannot be
// checked this way at all: that shape is Telegram's own id for a private
// one-on-one bot chat, and only there does it equal that person's own
// user id. A negative id (group/supergroup) or a chat username (e.g.
// "@opsroom", also valid as a Bot API chat_id) never equals any individual
// member's from.id, so every press would mismatch, including the
// assignee's own — turning a working config into permanent, silent
// refusals (review findings, P1: the first pass only caught the negative
// case and still refused a username address the same way). Rather than
// break that setup, such a channel skips the identity check and is
// applied unverified: recorded as such in the audit trail, never
// silently. Only a positive numeric (private-chat) id gets the strict
// check.
//
// Never mutates; every outcome is audited. Called before ApplyAction, not
// folded into it: every other caller of ApplyAction (the HTTP link
// handler, tests) has no comparable sender identity to check.
func (s *Service) VerifyTelegramActor(token, fromID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, e, p, err := s.lookupLinkLocked(token)
	if err != nil {
		return err
	}
	unverifiable := false
	for _, ch := range p.Channels {
		if ch.Kind != "telegram" {
			continue
		}
		if ch.To == fromID {
			return nil
		}
		if !isPrivateChatID(ch.To) {
			unverifiable = true
		}
	}
	if unverifiable {
		s.auditLocked("telegram.actor_unverified", map[string]any{
			"event_id": e.ID, "person_id": p.ID, "from_id": fromID,
			"reason": "unverified: not a private chat id",
		})
		return nil
	}
	s.auditLocked("telegram.actor_mismatch", map[string]any{
		"event_id": e.ID, "person_id": p.ID, "from_id": fromID,
	})
	return ErrWrongActor
}

// isPrivateChatID reports whether a stored telegram channel address is a
// plain positive integer — the only shape a private one-on-one bot chat's
// id ever takes, and the only shape callback_query.from.id (also always a
// positive integer) could ever equal. Everything else — a negative
// group/supergroup id, a "@username" alias, or anything malformed — is
// not comparable to from.id at all, review finding P1 (round 2).
func isPrivateChatID(to string) bool {
	n, err := strconv.ParseInt(to, 10, 64)
	return err == nil && n > 0
}

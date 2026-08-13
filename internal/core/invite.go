// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- party invitations ----------------------------------------------------
//
// One shared link per event: invitees open it, register themselves with a
// name and a yes/no, and the operator gets the guest list. Guests are
// deliberately NOT People — a Person carries a portal capability, a trust
// level, and drives the reminder/deadline scheduler. A guest is an address
// and an answer, nothing more; guests never receive confirm/cancel links.

type GuestStatus string

const (
	GuestYes GuestStatus = "yes"
	GuestNo  GuestStatus = "no"
)

func (g GuestStatus) Valid() bool { return g == GuestYes || g == GuestNo }

// MaxGuestsPerEvent caps a public, unauthenticated list against floods.
// The cap gates NEW guests only — an existing guest must always be able to
// change their answer, or a full list would keep a no-show marked as coming.
const MaxGuestsPerEvent = 500

const (
	maxGuestName = 80
	maxGuestMail = 200
	maxGuestNote = 280
)

// InviteLink is the one shared capability per event — same rules as
// ActionLink: random, DB-looked-up, revocable, expiry computed live from
// the event (eventExpiry), never stored.
type InviteLink struct {
	Token     string    `json:"token"`
	EventID   string    `json:"event_id"`
	CreatedAt time.Time `json:"created_at"`
	RevokedAt time.Time `json:"revoked_at"`
}

// Guest is a self-registered party invitee. Email is optional; when set,
// the guest becomes an outward recipient of the propagation transactions.
type Guest struct {
	ID        string      `json:"id"`
	EventID   string      `json:"event_id"`
	Name      string      `json:"name"`
	Email     string      `json:"email,omitempty"`
	Status    GuestStatus `json:"status"`
	Note      string      `json:"note,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// RSVPInput is the payload from the public form. Validate normalises it in
// place; error texts are recipient-facing (they render on the public page).
type RSVPInput struct {
	Name   string
	Email  string
	Status GuestStatus
	Note   string
}

func (in *RSVPInput) Validate() error {
	in.Name = strings.Join(strings.Fields(in.Name), " ")
	if in.Name == "" {
		return errors.New("your name is required")
	}
	if utf8.RuneCountInString(in.Name) > maxGuestName {
		return fmt.Errorf("the name can be at most %d characters", maxGuestName)
	}
	if !in.Status.Valid() {
		return errors.New(`the answer must be "yes" or "no"`)
	}
	// A guest with an address is an outward recipient that gates the
	// propagation proof, so a structurally broken address must be rejected
	// here — never accepted now and failing forever in the outbox.
	in.Email = strings.TrimSpace(in.Email)
	if in.Email != "" {
		addr, err := mail.ParseAddress(in.Email)
		if err != nil || len(addr.Address) > maxGuestMail {
			return errors.New("that email address does not look right")
		}
		at := strings.LastIndex(addr.Address, "@")
		if at < 0 || !strings.Contains(addr.Address[at+1:], ".") {
			return errors.New("that email address does not look right")
		}
		in.Email = addr.Address
	}
	in.Note = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(in.Note))
	if utf8.RuneCountInString(in.Note) > maxGuestNote {
		return fmt.Errorf("the message can be at most %d characters", maxGuestNote)
	}
	return nil
}

// InviteView is what the public invite page may see: the event and the two
// counts. Deliberately aggregate-only — a page that cannot address another
// guest's name cannot leak it, whatever a future template edit does.
type InviteView struct {
	Event Event `json:"event"`
	Yes   int   `json:"yes"`
	No    int   `json:"no"`
}

// InviteStatus is the operator's view of one event's invitation.
type InviteStatus struct {
	EventID string  `json:"event_id"`
	URL     string  `json:"url,omitempty"`
	Active  bool    `json:"active"`
	Yes     int     `json:"yes"`
	No      int     `json:"no"`
	Guests  []Guest `json:"guests"`
}

func (s *State) GuestsFor(eventID string) []*Guest {
	var out []*Guest
	for i := range s.Guests {
		if s.Guests[i].EventID == eventID {
			out = append(out, &s.Guests[i])
		}
	}
	return out
}

func (s *Service) inviteURL(token string) string { return s.cfg.BaseURL + "/i/" + token }

func (s *Service) activeInviteLocked(eventID string) *InviteLink {
	for i := range s.state.Invites {
		l := &s.state.Invites[i]
		if l.EventID == eventID && l.RevokedAt.IsZero() {
			return l
		}
	}
	return nil
}

// CreateInvite mints (or reuses) the event's shared invite link and returns
// its URL. Idempotent: a second call returns the same link.
func (s *Service) CreateInvite(eventID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Event(eventID) == nil {
		return "", ErrNotFound
	}
	if l := s.activeInviteLocked(eventID); l != nil {
		return s.inviteURL(l.Token), nil
	}
	l := InviteLink{Token: NewToken(), EventID: eventID, CreatedAt: s.now()}
	s.state.Invites = append(s.state.Invites, l)
	s.auditLocked("invite.created", map[string]any{"event_id": eventID})
	s.saveLocked()
	return s.inviteURL(l.Token), nil
}

// RevokeInvite closes the guest list: the link stops resolving, new RSVPs
// stop. Existing guests survive — they still receive cancellation and move
// notices, because "close the list" must never mean "these people can no
// longer be told".
func (s *Service) RevokeInvite(eventID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.activeInviteLocked(eventID)
	if l == nil {
		return ErrNotFound
	}
	l.RevokedAt = s.now()
	s.auditLocked("invite.revoked", map[string]any{"event_id": eventID})
	s.saveLocked()
	return nil
}

// lookupInviteLocked mirrors lookupLinkLocked: revoked → ErrGone, missing
// event → ErrNotFound, past eventExpiry → ErrGone. Deliberately no Status
// check — a cancelled party must keep rendering its page, because someone
// opening the link the morning of has to see CANCELLED. That is the
// locked-door moment this product exists for.
func (s *Service) lookupInviteLocked(token string) (*InviteLink, *Event, error) {
	for i := range s.state.Invites {
		l := &s.state.Invites[i]
		if l.Token != token {
			continue
		}
		if !l.RevokedAt.IsZero() {
			return nil, nil, ErrGone
		}
		e := s.state.Event(l.EventID)
		if e == nil {
			return nil, nil, ErrNotFound
		}
		if s.now().After(eventExpiry(e)) {
			return nil, nil, ErrGone
		}
		return l, e, nil
	}
	return nil, nil, ErrNotFound
}

// ResolveInvite is the read side of the public page. GET never mutates.
func (s *Service) ResolveInvite(token string) (InviteView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, e, err := s.lookupInviteLocked(token)
	if err != nil {
		return InviteView{}, err
	}
	v := InviteView{Event: *e}
	for _, g := range s.state.GuestsFor(e.ID) {
		switch g.Status {
		case GuestYes:
			v.Yes++
		case GuestNo:
			v.No++
		}
	}
	return v, nil
}

// RSVP records an invitee's answer. Upsert by normalised name: answering
// again with the same name updates the previous answer — two real people
// sharing a name collide, which the form copy states as the trade-off of
// the one-shared-link model.
func (s *Service) RSVP(token string, in RSVPInput) (Guest, error) {
	if err := in.Validate(); err != nil {
		return Guest{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, e, err := s.lookupInviteLocked(token)
	if err != nil {
		return Guest{}, err
	}
	if e.Status == StatusCancelled {
		return Guest{}, ErrCancelled
	}
	var g *Guest
	for _, cand := range s.state.GuestsFor(e.ID) {
		if strings.EqualFold(cand.Name, in.Name) {
			g = cand
			break
		}
	}
	isNew := g == nil
	if isNew {
		if len(s.state.GuestsFor(e.ID)) >= MaxGuestsPerEvent {
			return Guest{}, errors.New("the guest list is full — please contact the host directly")
		}
		s.state.Guests = append(s.state.Guests, Guest{
			ID: NewID("gu"), EventID: e.ID, Name: in.Name, CreatedAt: s.now(),
		})
		g = &s.state.Guests[len(s.state.Guests)-1]
	}
	// A blank email never clears a stored one: anyone with the link can
	// answer under any name, and silently making a guest unreachable for a
	// cancellation is exactly the failure this product prevents. Removing
	// an address is an operator act (remove the guest).
	emailChanged := in.Email != "" && in.Email != g.Email
	if in.Email != "" {
		g.Email = in.Email
	}
	g.Status = in.Status
	g.Note = in.Note
	g.UpdatedAt = s.now()
	s.auditLocked("guest.rsvp", map[string]any{
		"event_id": e.ID, "guest_id": g.ID, "status": g.Status,
		"new": isNew, "email_changed": emailChanged,
	})
	// The one webhook an unauthenticated visitor can trigger — bounded by
	// the RSVP rate limit and the guest cap; consumers are operator-added.
	s.fireWebhooksLocked("guest.rsvp", *g)
	s.saveLocked()
	return *g, nil
}

// Invite is the operator's view: link state, counts, and the guest list.
func (s *Service) Invite(eventID string) (InviteStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Event(eventID) == nil {
		return InviteStatus{}, ErrNotFound
	}
	st := InviteStatus{EventID: eventID, Guests: []Guest{}}
	if l := s.activeInviteLocked(eventID); l != nil {
		st.Active = true
		st.URL = s.inviteURL(l.Token)
	}
	for _, g := range s.state.GuestsFor(eventID) {
		st.Guests = append(st.Guests, *g)
		switch g.Status {
		case GuestYes:
			st.Yes++
		case GuestNo:
			st.No++
		}
	}
	sort.Slice(st.Guests, func(i, j int) bool { return st.Guests[i].CreatedAt.Before(st.Guests[j].CreatedAt) })
	return st, nil
}

func (s *Service) RemoveGuest(eventID, guestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Guests {
		if s.state.Guests[i].ID == guestID && s.state.Guests[i].EventID == eventID {
			s.state.Guests = append(s.state.Guests[:i], s.state.Guests[i+1:]...)
			s.auditLocked("guest.removed", map[string]any{"event_id": eventID, "guest_id": guestID})
			s.saveLocked()
			return nil
		}
	}
	return ErrNotFound
}

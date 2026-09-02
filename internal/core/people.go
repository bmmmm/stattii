// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"fmt"
	"strings"
)

// ---- people: edit and unassign --------------------------------------------
//
// Phase 2 needs real people with real addresses, and addresses change:
// a typo'd mail, a new Telegram chat id, someone handing an event over.
// Until now the only fix was a new person and a dangling old one.

// PersonUpdate is a patch: a nil field is left alone, a set field
// replaces the value. Channels distinguishes "unchanged" (nil) from
// "clear the list" (a pointer to an empty slice) — over JSON that is the
// key being absent versus `"channels": []`.
type PersonUpdate struct {
	Name     *string     `json:"name"`
	Trust    *TrustLevel `json:"trust"`
	Channels *[]Address  `json:"channels"`
}

// UpdatePerson applies a patch. Everything is validated before anything
// is written, so a bad channel cannot leave a half-applied person. ID,
// portal token, assignments and open proposals are untouched — a
// changed address is not a new identity. Emptying the channel list of
// someone assigned to upcoming events pages the admin: that person just
// became a locked-door risk.
func (s *Service) UpdatePerson(id string, in PersonUpdate) (Person, error) {
	if in.Name != nil {
		n := strings.Join(strings.Fields(*in.Name), " ")
		if n == "" {
			return Person{}, errors.New("name is required")
		}
		in.Name = &n
	}
	if in.Trust != nil && !in.Trust.Valid() {
		return Person{}, fmt.Errorf("invalid trust %q (use respond, propose, or direct)", *in.Trust)
	}
	var channels []Address
	if in.Channels != nil {
		channels = make([]Address, 0, len(*in.Channels))
		for _, ch := range *in.Channels {
			if err := ch.Validate(); err != nil {
				return Person{}, err
			}
			channels = append(channels, ch)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.state.Person(id)
	if p == nil {
		return Person{}, ErrNotFound
	}
	audit := map[string]any{"person_id": id}
	var fields []string
	if in.Name != nil && *in.Name != p.Name {
		p.Name = *in.Name
		fields = append(fields, "name")
	}
	if in.Trust != nil && *in.Trust != p.Trust {
		audit["trust_from"], audit["trust_to"] = p.Trust, *in.Trust
		p.Trust = *in.Trust
		fields = append(fields, "trust")
	}
	wasReachable := p.Reachable()
	if in.Channels != nil && !sameChannels(p.Channels, channels) {
		// Kinds only — addresses do not belong in the audit trail.
		audit["channels_from"], audit["channels_to"] = channelKinds(p.Channels), channelKinds(channels)
		p.Channels = channels
		fields = append(fields, "channels")
	}
	out := *p
	out.Channels = append([]Address(nil), p.Channels...)
	if len(fields) == 0 {
		return out, nil // a no-op patch is not an audit event
	}
	audit["fields"] = fields
	s.auditLocked("person.updated", audit)
	if wasReachable && !p.Reachable() {
		s.noteUnreachablePersonLocked(p)
	}
	s.saveLocked()
	return out, nil
}

func sameChannels(a, b []Address) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// PatchChannels edits the one-email-one-telegram view of a channel list
// the way the panel form and the CLI present it: the first email and the
// first telegram entry are slots, everything else (a second email, a
// webhook) rides along untouched. nil keeps a slot, "" drops it, a value
// replaces it — a form that cannot show a channel must never delete it.
func PatchChannels(existing []Address, email, telegram *string) []Address {
	var emailTo, tgTo string
	var others []Address
	seenEmail, seenTelegram := false, false
	for _, ch := range existing {
		switch {
		case ch.Kind == "email" && !seenEmail:
			seenEmail, emailTo = true, ch.To
		case ch.Kind == "telegram" && !seenTelegram:
			seenTelegram, tgTo = true, ch.To
		default:
			others = append(others, ch)
		}
	}
	if email != nil {
		emailTo = strings.TrimSpace(*email)
	}
	if telegram != nil {
		tgTo = strings.TrimSpace(*telegram)
	}
	out := []Address{}
	if emailTo != "" {
		out = append(out, Address{Kind: "email", To: emailTo})
	}
	if tgTo != "" {
		out = append(out, Address{Kind: "telegram", To: tgTo})
	}
	return append(out, others...)
}

func channelKinds(chs []Address) []string {
	kinds := make([]string, 0, len(chs))
	for _, ch := range chs {
		kinds = append(kinds, ch.Kind)
	}
	return kinds
}

// noteUnreachablePersonLocked closes the loop with the scheduler's
// early warning: a person who just lost their last channel while
// assigned to upcoming events is exactly the case that disarms the
// dead-man-switch — say so now, not at the deadline.
func (s *Service) noteUnreachablePersonLocked(p *Person) {
	now := s.now()
	var upcoming []string
	for _, e := range s.state.EventsFor(p.ID) {
		if e.Status == StatusCancelled || !e.StartsAt.After(now) {
			continue
		}
		upcoming = append(upcoming, fmt.Sprintf("- %s (%s)", e.Title, e.StartsAt.Format(timeFmt)))
	}
	if len(upcoming) == 0 {
		return
	}
	s.auditLocked("person.unreachable", map[string]any{"person_id": p.ID, "events": len(upcoming)})
	s.notifyAdminLocked("Now unreachable: "+p.Name,
		fmt.Sprintf("%s has no channel left but is responsible for upcoming events:\n%s\n\n"+
			"Their confirmation asks cannot go out; deadlines will fire without an ask. "+
			"Add a channel under /admin/people or assign someone reachable.",
			p.Name, strings.Join(upcoming, "\n")))
}

// Unassign removes one person from one event. Their action links die
// with the assignment (a link is a capability of the assignment, not of
// the person); the event's status and the recorded responses stay —
// what happened, happened. No fan-out: nobody outside needs to know who
// is responsible. Series assignments are a separate table and stay.
func (s *Service) Unassign(eventID, personID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.unassignLocked(eventID, personID) {
		return ErrNotFound
	}
	s.saveLocked()
	return nil
}

func (s *Service) unassignLocked(eventID, personID string) bool {
	idx := -1
	for i, a := range s.state.Assignments {
		if a.EventID == eventID && a.PersonID == personID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	s.state.Assignments = append(s.state.Assignments[:idx], s.state.Assignments[idx+1:]...)
	revoked := s.revokeLinksLocked(eventID, personID)
	wasConfirmer := false
	if r := s.state.ResponseFor(eventID, personID); r != nil && r.Action == ActionConfirm {
		wasConfirmer = true
	}
	s.auditLocked("unassigned", map[string]any{
		"event_id": eventID, "person_id": personID,
		"links_revoked": revoked, "was_confirmer": wasConfirmer,
	})
	return true
}

// UnassignSeries removes a per-series responsible and unassigns them
// from every FUTURE, non-cancelled occurrence. Deliberately asymmetric
// to AssignSeries, which applies to all existing occurrences: the past
// is not rewritten — whoever was responsible for last week's session
// stays on record as such. Returns how many events were unassigned;
// ErrNotFound only when neither a series row nor a single assignment
// matched.
func (s *Service) UnassignSeries(sourceUID, personID string) (int, error) {
	if sourceUID == "" {
		return 0, errors.New("missing source_uid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.state.SeriesAssignments[:0]
	rows := 0
	for _, sa := range s.state.SeriesAssignments {
		if sa.SourceUID == sourceUID && sa.PersonID == personID {
			rows++
			continue
		}
		kept = append(kept, sa)
	}
	s.state.SeriesAssignments = kept
	now := s.now()
	n := 0
	for i := range s.state.Events {
		e := &s.state.Events[i]
		if e.SourceUID != sourceUID || e.Status == StatusCancelled || !e.StartsAt.After(now) {
			continue
		}
		if s.unassignLocked(e.ID, personID) {
			n++
		}
	}
	if rows == 0 && n == 0 {
		return 0, ErrNotFound
	}
	s.auditLocked("series.unassigned", map[string]any{"source_uid": sourceUID, "person_id": personID, "events": n})
	s.saveLocked()
	return n, nil
}

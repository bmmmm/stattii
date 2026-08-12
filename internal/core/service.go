// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

const timeFmt = "Mon, 02 Jan 2006 15:04 MST"

// Message is what leaves the system through a channel.
type Message struct {
	Subject string
	Body    string
	Buttons []Button
	Headers map[string]string
}

// Notifier delivers messages; implemented by the channel registry.
type Notifier interface {
	Send(kind, to string, m Message) error
}

type Config struct {
	BaseURL       string        // public base for action/portal links
	ReminderLead  time.Duration // how long before start the ask goes out
	DeadlineLead  time.Duration // no response by start-DeadlineLead => deadline.passed
	RetryDelay    time.Duration // base outbox retry delay (exponential backoff)
	MaxAttempts   int           // outbox attempts before an item counts as failed
	EscalateAfter time.Duration // undelivered for this long => notify admin
	AdminNotify   *Address      // where escalations and proposals go (optional)
}

func (c *Config) fill() {
	if c.ReminderLead == 0 {
		c.ReminderLead = 48 * time.Hour
	}
	if c.DeadlineLead == 0 {
		c.DeadlineLead = 24 * time.Hour
	}
	if c.RetryDelay == 0 {
		c.RetryDelay = time.Minute
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 5
	}
	if c.EscalateAfter == 0 {
		c.EscalateAfter = 10 * time.Minute
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
}

// Service owns the state: one mutex, mutations audit and persist themselves.
type Service struct {
	mu     sync.Mutex
	store  Store
	state  *State
	cfg    Config
	notify Notifier
	now    func() time.Time
	logf   func(format string, args ...any)
}

func NewService(store Store, cfg Config, notify Notifier) (*Service, error) {
	cfg.fill()
	st, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &Service{
		store:  store,
		state:  st,
		cfg:    cfg,
		notify: notify,
		now:    func() time.Time { return time.Now().UTC() },
		logf:   log.Printf,
	}, nil
}

// SetClock overrides the clock, for tests.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

var (
	ErrNotFound  = errors.New("not found")
	ErrGone      = errors.New("link expired or revoked")
	ErrCancelled = errors.New("event is already cancelled")
	ErrForbidden = errors.New("not allowed for this trust level")
)

func (s *Service) saveLocked() {
	if err := s.store.Save(s.state); err != nil {
		s.logf("stattii: persist failed: %v", err)
	}
}

func (s *Service) auditLocked(kind string, data any) {
	if err := s.store.Audit(kind, data); err != nil {
		s.logf("stattii: audit write failed: %v", err)
	}
}

// ---- events ---------------------------------------------------------------

func (s *Service) CreateEvent(in EventInput) (Event, error) {
	if err := in.Validate(); err != nil {
		return Event{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.createLocked(in, "api")
	s.saveLocked()
	return e, nil
}

func (s *Service) createLocked(in EventInput, actor string) Event {
	e := Event{
		ID:            NewID("ev"),
		Title:         in.Title,
		Location:      in.Location,
		Note:          in.Note,
		StartsAt:      in.StartsAt,
		EndsAt:        in.EndsAt,
		IfUnconfirmed: in.IfUnconfirmed,
		Status:        StatusScheduled,
		CreatedAt:     s.now(),
	}
	s.state.Events = append(s.state.Events, e)
	s.auditLocked("event.created", map[string]any{"event_id": e.ID, "title": in.Title, "starts_at": in.StartsAt, "actor": actor})
	s.fireWebhooksLocked("event.created", e)
	return e
}

// ReinstateEvent withdraws a cancellation — like cancelling, it is a
// propagation transaction, and it restarts the confirmation cycle.
func (s *Service) ReinstateEvent(eventID, actor string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.state.Event(eventID)
	if e == nil {
		return Event{}, ErrNotFound
	}
	if e.Status != StatusCancelled {
		return *e, errors.New("event is not cancelled")
	}
	e.Status = StatusScheduled
	e.CancelReason = ""
	e.CancelledAt = time.Time{}
	e.Seq++
	e.ReminderSentAt = time.Time{}
	e.DeadlineFiredAt = time.Time{}
	s.auditLocked("event.reinstated", map[string]any{"event_id": eventID, "actor": actor})
	subject := "REINSTATED: " + e.Title
	body := fmt.Sprintf("%s on %s takes place after all — the cancellation is withdrawn.",
		e.Title, e.StartsAt.Format(timeFmt))
	s.fanOutLocked(e, "reinstated", subject, body)
	s.fireWebhooksLocked("event.reinstated", *e)
	s.saveLocked()
	return *e, nil
}

func (s *Service) ConfirmEvent(eventID, personID, via string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.confirmLocked(eventID, personID, via)
	if err == nil {
		s.saveLocked()
	}
	return e, err
}

func (s *Service) confirmLocked(eventID, personID, via string) (Event, error) {
	e := s.state.Event(eventID)
	if e == nil {
		return Event{}, ErrNotFound
	}
	if e.Status == StatusCancelled {
		return *e, ErrCancelled
	}
	if personID != "" {
		s.state.Responses = append(s.state.Responses, Response{
			At: s.now(), EventID: eventID, PersonID: personID, Action: ActionConfirm, Via: via,
		})
		s.auditLocked("response", map[string]any{"event_id": eventID, "person_id": personID, "action": "confirm", "via": via})
	}
	if e.Status == StatusScheduled {
		e.Status = StatusConfirmed
		e.Seq++
		s.auditLocked("event.confirmed", map[string]any{"event_id": eventID, "by": personID, "via": via})
		s.fireWebhooksLocked("event.confirmed", *e)
	}
	return *e, nil
}

// CancelEvent is the propagation transaction: flip status, then fan out to
// every broadcast target and every assignee channel through the outbox, so
// "cancelled" is never silently internal-only.
func (s *Service) CancelEvent(eventID, personID, reason, via string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.cancelLocked(eventID, personID, reason, via)
	if err == nil {
		s.saveLocked()
	}
	return e, err
}

func (s *Service) cancelLocked(eventID, personID, reason, via string) (Event, error) {
	e := s.state.Event(eventID)
	if e == nil {
		return Event{}, ErrNotFound
	}
	if e.Status == StatusCancelled {
		return *e, ErrCancelled
	}
	now := s.now()
	e.Status = StatusCancelled
	e.CancelReason = reason
	e.CancelledAt = now
	e.Seq++
	actor := personID
	if actor == "" {
		actor = "admin"
	}
	if personID != "" {
		s.state.Responses = append(s.state.Responses, Response{
			At: now, EventID: eventID, PersonID: personID, Action: ActionCancel, Via: via,
		})
		s.auditLocked("response", map[string]any{"event_id": eventID, "person_id": personID, "action": "cancel", "via": via})
	}
	s.auditLocked("event.cancelled", map[string]any{"event_id": eventID, "actor": actor, "reason": reason})

	subject := "CANCELLED: " + e.Title
	body := fmt.Sprintf("%s on %s is CANCELLED.", e.Title, e.StartsAt.Format(timeFmt))
	if reason != "" {
		body += "\nReason: " + reason
	}
	s.fanOutLocked(e, "cancellation", subject, body)
	s.fireWebhooksLocked("event.cancelled", *e)
	return *e, nil
}

func (s *Service) MoveEvent(eventID string, start, end time.Time, note, actor string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.moveLocked(eventID, start, end, note, actor)
	if err == nil {
		s.saveLocked()
	}
	return e, err
}

func (s *Service) moveLocked(eventID string, start, end time.Time, note, actor string) (Event, error) {
	e := s.state.Event(eventID)
	if e == nil {
		return Event{}, ErrNotFound
	}
	if e.Status == StatusCancelled {
		return *e, ErrCancelled
	}
	old := e.StartsAt
	e.StartsAt = start
	e.EndsAt = end
	e.Seq++
	// A moved event needs a fresh confirmation cycle.
	e.Status = StatusScheduled
	e.ReminderSentAt = time.Time{}
	e.DeadlineFiredAt = time.Time{}
	if note != "" {
		e.Note = note
	}
	s.auditLocked("event.moved", map[string]any{"event_id": eventID, "from": old, "to": start, "actor": actor})

	subject := "MOVED: " + e.Title
	body := fmt.Sprintf("%s has been MOVED.\nOld: %s\nNew: %s", e.Title, old.Format(timeFmt), start.Format(timeFmt))
	s.fanOutLocked(e, "moved", subject, body)
	s.fireWebhooksLocked("event.moved", *e)
	return *e, nil
}

// fanOutLocked enqueues one outbox item per broadcast target and per
// assignee channel — the delivery proof for outward communication.
func (s *Service) fanOutLocked(e *Event, purpose, subject, body string) {
	for _, b := range s.state.Broadcasts {
		s.enqueueLocked(OutboxItem{
			EventID: e.ID, Purpose: purpose, Kind: b.Kind, To: b.To,
			Subject: subject, Body: body,
		})
	}
	for _, p := range s.state.Assignees(e.ID) {
		if len(p.Channels) == 0 {
			s.auditLocked("delivery.skipped", map[string]any{"event_id": e.ID, "person_id": p.ID, "error": "person has no channels"})
			continue
		}
		for _, ch := range p.Channels {
			s.enqueueLocked(OutboxItem{
				EventID: e.ID, PersonID: p.ID, Purpose: purpose, Kind: ch.Kind, To: ch.To,
				Subject: subject, Body: body,
			})
		}
	}
}

// ---- people, assignments, targets ----------------------------------------

func (s *Service) AddPerson(name string, trust TrustLevel, channels []Address) (Person, error) {
	if name == "" {
		return Person{}, errors.New("name is required")
	}
	if trust == "" {
		trust = TrustRespond
	}
	if !trust.Valid() {
		return Person{}, fmt.Errorf("invalid trust %q (use respond, propose, or direct)", trust)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := Person{
		ID: NewID("pe"), Name: name, Trust: trust,
		PortalToken: NewToken(), Channels: channels,
	}
	s.state.People = append(s.state.People, p)
	s.auditLocked("person.created", map[string]any{"person_id": p.ID, "name": name, "trust": trust})
	s.saveLocked()
	return p, nil
}

func (s *Service) Assign(eventID, personID, role string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Event(eventID) == nil {
		return fmt.Errorf("event %s: %w", eventID, ErrNotFound)
	}
	if s.state.Person(personID) == nil {
		return fmt.Errorf("person %s: %w", personID, ErrNotFound)
	}
	for _, a := range s.state.Assignments {
		if a.EventID == eventID && a.PersonID == personID {
			return nil // idempotent
		}
	}
	s.state.Assignments = append(s.state.Assignments, Assignment{EventID: eventID, PersonID: personID, Role: role})
	s.auditLocked("assigned", map[string]any{"event_id": eventID, "person_id": personID, "role": role})
	s.saveLocked()
	return nil
}

func (s *Service) AddBroadcast(name, kind, to string) (Broadcast, error) {
	if kind == "" || to == "" {
		return Broadcast{}, errors.New("kind and to are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := Broadcast{ID: NewID("bc"), Name: name, Kind: kind, To: to}
	s.state.Broadcasts = append(s.state.Broadcasts, b)
	s.auditLocked("broadcast.created", map[string]any{"broadcast_id": b.ID, "kind": kind})
	s.saveLocked()
	return b, nil
}

func (s *Service) AddWebhook(url string, events []string) (Webhook, error) {
	if url == "" {
		return Webhook{}, errors.New("url is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := Webhook{ID: NewID("wh"), URL: url, Secret: NewToken(), Events: events}
	s.state.Webhooks = append(s.state.Webhooks, w)
	s.auditLocked("webhook.created", map[string]any{"webhook_id": w.ID, "url": url})
	s.saveLocked()
	return w, nil
}

func (s *Service) DeleteWebhook(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, w := range s.state.Webhooks {
		if w.ID == id {
			s.state.Webhooks = append(s.state.Webhooks[:i], s.state.Webhooks[i+1:]...)
			s.auditLocked("webhook.deleted", map[string]any{"webhook_id": id})
			s.saveLocked()
			return nil
		}
	}
	return ErrNotFound
}

func (s *Service) DeleteBroadcast(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, b := range s.state.Broadcasts {
		if b.ID == id {
			s.state.Broadcasts = append(s.state.Broadcasts[:i], s.state.Broadcasts[i+1:]...)
			s.auditLocked("broadcast.deleted", map[string]any{"broadcast_id": id})
			s.saveLocked()
			return nil
		}
	}
	return ErrNotFound
}

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
	e := s.state.Event(eventID)
	if confirmTok == "" {
		confirmTok = NewToken()
		s.state.Links = append(s.state.Links, ActionLink{
			Token: confirmTok, EventID: eventID, PersonID: personID,
			Action: ActionConfirm, ExpiresAt: eventExpiry(e),
		})
	}
	if cancelTok == "" {
		cancelTok = NewToken()
		s.state.Links = append(s.state.Links, ActionLink{
			Token: cancelTok, EventID: eventID, PersonID: personID,
			Action: ActionCancel, ExpiresAt: eventExpiry(e),
		})
	}
	return confirmTok, cancelTok
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
	pr := Proposal{
		ID: NewID("pr"), PersonID: p.ID, Kind: "move", EventID: e.ID,
		StartsAt: start, EndsAt: end, Note: note, CreatedAt: s.now(),
	}
	s.state.Proposals = append(s.state.Proposals, pr)
	s.auditLocked("proposal.created", map[string]any{"proposal_id": pr.ID, "person_id": p.ID, "kind": "move", "event_id": e.ID, "via": "link"})
	s.fireWebhooksLocked("proposal.created", pr)
	s.notifyAdminLocked("Proposal from "+p.Name,
		fmt.Sprintf("%s proposes to move %q to %s.", p.Name, e.Title, start.Format(timeFmt)))
	s.saveLocked()
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

// PortalRespond is the yes/no of an assigned person — allowed on every
// trust level.
func (s *Service) PortalRespond(token, eventID string, action ActionKind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.personByPortalLocked(token)
	if p == nil {
		return ErrNotFound
	}
	assigned := false
	for _, a := range s.state.Assignments {
		if a.EventID == eventID && a.PersonID == p.ID {
			assigned = true
			break
		}
	}
	if !assigned {
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
	switch p.Trust {
	case TrustDirect:
		switch kind {
		case "cancel":
			_, err = s.cancelLocked(eventID, p.ID, note, "portal")
		case "move":
			_, err = s.moveLocked(eventID, start, end, note, p.ID)
		case "create":
			e := s.createLocked(EventInput{Title: title, Note: note, StartsAt: start, EndsAt: end}, p.ID)
			s.state.Assignments = append(s.state.Assignments, Assignment{EventID: e.ID, PersonID: p.ID})
		}
		if err == nil {
			s.saveLocked()
		}
		return true, err
	case TrustPropose:
		pr := Proposal{
			ID: NewID("pr"), PersonID: p.ID, Kind: kind, EventID: eventID,
			Title: title, Note: note, StartsAt: start, EndsAt: end, CreatedAt: s.now(),
		}
		s.state.Proposals = append(s.state.Proposals, pr)
		s.auditLocked("proposal.created", map[string]any{"proposal_id": pr.ID, "person_id": p.ID, "kind": kind, "event_id": eventID})
		s.fireWebhooksLocked("proposal.created", pr)
		s.notifyAdminLocked("Proposal from "+p.Name,
			fmt.Sprintf("%s proposes: %s %s %s\nDecide: stattii proposal list / decide", p.Name, kind, eventID, title))
		s.saveLocked()
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
	pr.DecidedAt = s.now()
	pr.Accepted = accept
	var err error
	if accept {
		switch pr.Kind {
		case "cancel":
			_, err = s.cancelLocked(pr.EventID, pr.PersonID, pr.Note, "proposal")
		case "move":
			_, err = s.moveLocked(pr.EventID, pr.StartsAt, pr.EndsAt, pr.Note, pr.PersonID)
		case "create":
			e := s.createLocked(EventInput{Title: pr.Title, Note: pr.Note, StartsAt: pr.StartsAt, EndsAt: pr.EndsAt}, pr.PersonID)
			s.state.Assignments = append(s.state.Assignments, Assignment{EventID: e.ID, PersonID: pr.PersonID})
		}
	}
	s.auditLocked("proposal.decided", map[string]any{"proposal_id": id, "accepted": accept})
	s.fireWebhooksLocked("proposal.decided", *pr)
	if p := s.state.Person(pr.PersonID); p != nil {
		verdict := "declined"
		if accept {
			verdict = "accepted"
		}
		for _, ch := range p.Channels {
			s.enqueueLocked(OutboxItem{
				PersonID: p.ID, Purpose: "proposal", Kind: ch.Kind, To: ch.To,
				Subject: "Proposal " + verdict,
				Body:    fmt.Sprintf("Your proposal (%s) was %s.", pr.Kind, verdict),
			})
		}
	}
	s.saveLocked()
	return *pr, err
}

// ---- outbox, webhooks, escalation -----------------------------------------

func (s *Service) enqueueLocked(item OutboxItem) {
	item.ID = NewID("ob")
	item.CreatedAt = s.now()
	item.NextAttempt = item.CreatedAt
	s.state.Outbox = append(s.state.Outbox, item)
}

func (s *Service) fireWebhooksLocked(event string, data any) {
	if len(s.state.Webhooks) == 0 {
		return
	}
	payload, err := json.Marshal(map[string]any{"event": event, "at": s.now(), "data": data})
	if err != nil {
		s.logf("stattii: marshal webhook payload: %v", err)
		return
	}
	for _, w := range s.state.Webhooks {
		if !webhookMatches(w, event) {
			continue
		}
		mac := hmac.New(sha256.New, []byte(w.Secret))
		mac.Write(payload)
		s.enqueueLocked(OutboxItem{
			Purpose: "webhook", Kind: "webhook", To: w.URL,
			Subject: event, Body: string(payload),
			Headers: map[string]string{
				"X-Stattii-Event":     event,
				"X-Stattii-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
			},
		})
	}
}

func webhookMatches(w Webhook, event string) bool {
	if len(w.Events) == 0 {
		return true
	}
	for _, e := range w.Events {
		if e == event {
			return true
		}
	}
	return false
}

func (s *Service) notifyAdminLocked(subject, body string) {
	if s.cfg.AdminNotify == nil {
		s.auditLocked("admin.unnotified", map[string]any{"subject": subject, "error": "no admin notify target configured (set STATTII_ADMIN_NOTIFY)"})
		return
	}
	s.enqueueLocked(OutboxItem{
		Purpose: "escalation", Kind: s.cfg.AdminNotify.Kind, To: s.cfg.AdminNotify.To,
		Subject: subject, Body: body,
	})
}

// PropagationStatus answers "is the cancellation actually out?".
type PropagationStatus struct {
	EventID   string       `json:"event_id"`
	Total     int          `json:"total"`
	Delivered int          `json:"delivered"`
	Pending   int          `json:"pending"`
	Failed    int          `json:"failed"`
	Complete  bool         `json:"complete"`
	Items     []OutboxItem `json:"items"`
}

func (s *Service) Propagation(eventID string) (PropagationStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Event(eventID) == nil {
		return PropagationStatus{}, ErrNotFound
	}
	ps := PropagationStatus{EventID: eventID}
	for _, o := range s.state.Outbox {
		if o.EventID != eventID ||
			(o.Purpose != "cancellation" && o.Purpose != "moved" && o.Purpose != "reinstated") {
			continue
		}
		ps.Total++
		ps.Items = append(ps.Items, o)
		switch {
		case o.Delivered():
			ps.Delivered++
		case o.Attempts >= s.cfg.MaxAttempts:
			ps.Failed++
		default:
			ps.Pending++
		}
	}
	ps.Complete = ps.Total > 0 && ps.Delivered == ps.Total
	return ps, nil
}

// ---- scheduler ------------------------------------------------------------

// Tick runs one scheduler pass: due reminders, missed deadlines, outbox
// delivery with backoff, and escalation of stuck items.
func (s *Service) Tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	if s.tickRemindersLocked(now) {
		changed = true
	}
	if s.tickDeadlinesLocked(now) {
		changed = true
	}
	if s.tickOutboxLocked(now) {
		changed = true
	}
	if changed {
		s.saveLocked()
	}
}

func (s *Service) tickRemindersLocked(now time.Time) bool {
	changed := false
	for i := range s.state.Events {
		e := &s.state.Events[i]
		if e.Status != StatusScheduled || !e.ReminderSentAt.IsZero() {
			continue
		}
		if now.Before(e.StartsAt.Add(-s.cfg.ReminderLead)) || now.After(e.StartsAt) {
			continue
		}
		assignees := s.state.Assignees(e.ID)
		if len(assignees) == 0 {
			// Events are created first and staffed second; a tick in
			// between must not burn the one-shot reminder on zero
			// recipients. Leave it pending until someone is assigned.
			continue
		}
		for _, p := range assignees {
			cTok, xTok := s.linksLocked(e.ID, p.ID)
			header := e.Title + "\n" + e.StartsAt.Format(timeFmt)
			if e.Location != "" {
				header += "\nLocation: " + e.Location
			}
			body := fmt.Sprintf(
				"%s\n\nWill it take place?\nYES, confirm:  %s\nNO, cancel it: %s\n\nThese links are personal — please do not forward.",
				header, s.actionURL(cTok), s.actionURL(xTok))
			if len(p.Channels) == 0 {
				s.auditLocked("delivery.skipped", map[string]any{"event_id": e.ID, "person_id": p.ID, "error": "person has no channels"})
				continue
			}
			for _, ch := range p.Channels {
				item := OutboxItem{
					EventID: e.ID, PersonID: p.ID, Purpose: "reminder", Kind: ch.Kind, To: ch.To,
					Subject: "Please confirm: " + e.Title, Body: body,
				}
				// Channels with inline buttons get one-tap callbacks; the
				// body links stay as fallback for forwarded/old clients.
				item.Buttons = []Button{
					{Label: "✅ Takes place", Data: cTok},
					{Label: "❌ Cancel event", Data: xTok},
				}
				s.enqueueLocked(item)
			}
		}
		e.ReminderSentAt = now
		s.auditLocked("reminder.sent", map[string]any{"event_id": e.ID})
		s.fireWebhooksLocked("reminder.sent", *e)
		changed = true
	}
	return changed
}

func (s *Service) tickDeadlinesLocked(now time.Time) bool {
	changed := false
	for i := range s.state.Events {
		e := &s.state.Events[i]
		if e.Status != StatusScheduled || !e.DeadlineFiredAt.IsZero() {
			continue
		}
		if e.ReminderSentAt.IsZero() && len(s.state.Assignees(e.ID)) > 0 {
			// Staffed but not asked yet — the reminder goes out first.
			// Unstaffed events skip the ask entirely, and the
			// dead-man-switch must still fire for them.
			continue
		}
		if now.Before(e.StartsAt.Add(-s.cfg.DeadlineLead)) || now.After(e.StartsAt) {
			continue
		}
		e.DeadlineFiredAt = now
		s.auditLocked("deadline.passed", map[string]any{"event_id": e.ID})
		s.fireWebhooksLocked("deadline.passed", *e)
		if e.IfUnconfirmed == "cancel" {
			// Dead-man-switch: silence means the event does not happen —
			// and the cancellation propagates like any other.
			s.cancelLocked(e.ID, "", "auto-cancelled: unconfirmed by deadline", "deadline")
			s.notifyAdminLocked("Auto-cancelled: "+e.Title,
				fmt.Sprintf("%s on %s was unconfirmed by its deadline and has been auto-cancelled (dead-man-switch). Reinstate if wrong.",
					e.Title, e.StartsAt.Format(timeFmt)))
		} else {
			s.notifyAdminLocked("No response: "+e.Title,
				fmt.Sprintf("%s on %s is still unconfirmed and the response deadline has passed.",
					e.Title, e.StartsAt.Format(timeFmt)))
		}
		changed = true
	}
	return changed
}

func (s *Service) tickOutboxLocked(now time.Time) bool {
	changed := false
	for i := 0; i < len(s.state.Outbox); i++ {
		o := &s.state.Outbox[i]
		if o.Delivered() {
			continue
		}
		// Escalate stuck items once, whatever the attempt count.
		if o.EscalatedAt.IsZero() && o.Purpose != "escalation" &&
			now.Sub(o.CreatedAt) >= s.cfg.EscalateAfter {
			o.EscalatedAt = now
			s.notifyAdminLocked("Delivery stuck: "+o.Subject,
				fmt.Sprintf("Undelivered for %s via %s to %s: %s", now.Sub(o.CreatedAt).Round(time.Minute), o.Kind, o.To, o.LastError))
			changed = true
		}
		if o.Attempts >= s.cfg.MaxAttempts || now.Before(o.NextAttempt) {
			continue
		}
		err := s.notify.Send(o.Kind, o.To, Message{Subject: o.Subject, Body: o.Body, Buttons: o.Buttons, Headers: o.Headers})
		o.Attempts++
		changed = true
		if err == nil {
			o.DeliveredAt = now
			o.LastError = ""
			s.auditLocked("delivery.ok", map[string]any{"outbox_id": o.ID, "event_id": o.EventID, "purpose": o.Purpose, "kind": o.Kind, "to": o.To, "attempts": o.Attempts})
			continue
		}
		o.LastError = err.Error()
		o.NextAttempt = now.Add(s.cfg.RetryDelay * time.Duration(1<<min(o.Attempts-1, 4)))
		s.auditLocked("delivery.fail", map[string]any{"outbox_id": o.ID, "event_id": o.EventID, "purpose": o.Purpose, "kind": o.Kind, "to": o.To, "attempts": o.Attempts, "error": err.Error()})
	}
	return changed
}

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
	return append([]Person(nil), s.state.People...)
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
	return append([]Webhook(nil), s.state.Webhooks...)
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
	for _, e := range s.state.Events {
		oe := OverviewEvent{Event: e}
		for _, a := range s.state.Assignments {
			if a.EventID != e.ID {
				continue
			}
			p := s.state.Person(a.PersonID)
			if p == nil {
				continue
			}
			oa := OverviewAssignee{PersonID: p.ID, Name: p.Name, Role: a.Role, Trust: p.Trust}
			for _, r := range s.state.Responses {
				if r.EventID == e.ID && r.PersonID == p.ID && r.At.After(oa.At) {
					oa.Action, oa.Via, oa.At = r.Action, r.Via, r.At
				}
			}
			oe.Assignees = append(oe.Assignees, oa)
		}
		ov.Events = append(ov.Events, oe)
	}
	sort.Slice(ov.Events, func(i, j int) bool {
		return ov.Events[i].Event.StartsAt.Before(ov.Events[j].Event.StartsAt)
	})
	for _, o := range s.state.Outbox {
		switch {
		case o.Delivered():
			ov.Outbox.Delivered++
		case o.Attempts >= s.cfg.MaxAttempts:
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
	for _, ch := range p.Channels {
		s.enqueueLocked(OutboxItem{
			PersonID: p.ID, Purpose: "test", Kind: ch.Kind, To: ch.To,
			Subject: "stattii test message",
			Body:    "This is a test message from stattii — delivery to you works.\nNothing to do, nothing to click.",
		})
		ids[s.state.Outbox[len(s.state.Outbox)-1].ID] = true
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

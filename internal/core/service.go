// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"fmt"
	"log"
	"net/http"
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
	// Calendar import (optional): the foreign ICS feed events come from,
	// and how far ahead occurrences are materialised.
	CalendarSource string
	CalendarWindow time.Duration
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
	if c.CalendarWindow == 0 {
		c.CalendarWindow = 60 * 24 * time.Hour
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
}

// Service owns the state: one mutex, mutations audit and persist themselves.
type Service struct {
	mu           sync.Mutex
	store        Store
	state        *State
	cfg          Config
	notify       Notifier
	now          func() time.Time
	logf         func(format string, args ...any)
	calendarHTTP *http.Client // test override for calendar fetches
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
func (s *Service) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// NoteLoginFailure records a failed admin-UI login in the audit trail —
// the admin listener has no other failed-auth signal.
func (s *Service) NoteLoginFailure(remote string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditLocked("admin.login_failed", map[string]any{"remote": remote})
}

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
		SourceUID:     in.SourceUID,
		SourceKey:     in.SourceKey,
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
	// Validate here, not only in the callers: a zero start would fan out a
	// "MOVED to 01 Jan 0001" notice to every recipient.
	if start.IsZero() {
		return Event{}, errors.New("starts_at is required for a move")
	}
	if !end.IsZero() && end.Before(start) {
		return Event{}, errors.New("ends_at is before starts_at")
	}
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
	newWhen := start.Format(timeFmt)
	if !end.IsZero() {
		newWhen += " until " + end.Format("15:04")
	}
	body := fmt.Sprintf("%s has been MOVED.\nOld: %s\nNew: %s", e.Title, old.Format(timeFmt), newWhen)
	s.fanOutLocked(e, "moved", subject, body)
	s.fireWebhooksLocked("event.moved", *e)
	return *e, nil
}

// fanOutLocked enqueues one outbox item per broadcast target, assignee
// channel, and reachable party guest — the delivery proof for outward
// communication.
func (s *Service) fanOutLocked(e *Event, purpose, subject, body string) {
	for _, b := range s.state.Broadcasts {
		s.enqueueLocked(OutboxItem{
			EventID: e.ID, Purpose: purpose, Kind: b.Kind, To: b.To,
			Subject: subject, Body: body,
		})
	}
	for _, p := range s.state.Assignees(e.ID) {
		s.enqueueToPersonLocked(p, OutboxItem{EventID: e.ID, Purpose: purpose, Subject: subject, Body: body})
	}
	// Party guests who left an address are outward recipients like any
	// other: a guest we cannot tell about a cancellation is exactly the
	// locked door this product exists to prevent. Status is deliberately
	// NOT filtered — a decliner still needs to know the party moved, and
	// a "no" who comes anyway is the classic victim. One mail per address:
	// the same address enrolled under several names is a duplicate (or a
	// flood attempt), not two recipients.
	seen := map[string]bool{}
	for _, g := range s.state.GuestsFor(e.ID) {
		if g.Email == "" || seen[strings.ToLower(g.Email)] {
			continue
		}
		seen[strings.ToLower(g.Email)] = true
		s.enqueueLocked(OutboxItem{
			EventID: e.ID, GuestID: g.ID, Purpose: purpose,
			Kind: "email", To: g.Email, Subject: subject, Body: body,
		})
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
	if s.assignLocked(eventID, personID, role) {
		s.saveLocked()
	}
	return nil
}

// assignLocked records an assignment; false means it already existed.
func (s *Service) assignLocked(eventID, personID, role string) bool {
	for _, a := range s.state.Assignments {
		if a.EventID == eventID && a.PersonID == personID {
			return false // idempotent
		}
	}
	s.state.Assignments = append(s.state.Assignments, Assignment{EventID: eventID, PersonID: personID, Role: role})
	s.auditLocked("assigned", map[string]any{"event_id": eventID, "person_id": personID, "role": role})
	return true
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

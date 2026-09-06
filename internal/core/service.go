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
	"unicode/utf8"
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
	// OutboxRetention bounds state.json: delivered items whose event is
	// this long past get pruned (proof stays in audit.jsonl). Undelivered
	// items are never pruned.
	OutboxRetention time.Duration
	AdminNotify     *Address // where escalations and proposals go (optional)
	// Calendar import (optional): the foreign ICS feed events come from,
	// how far ahead occurrences are materialised, and how often the
	// fetcher polls by itself (zero = manual only).
	CalendarSource     string
	CalendarWindow     time.Duration
	CalendarFetchEvery time.Duration
}

// minFetchEvery keeps the fetcher from hammering a foreign calendar and
// from flooding audit.jsonl with import.failed during an outage.
const minFetchEvery = time.Minute

// validate rejects combinations that would silently do nothing, or
// something silly.
func (c *Config) validate() error {
	if c.CalendarFetchEvery > 0 && c.CalendarSource == "" {
		return errors.New("calendar_fetch_every is set but calendar_source is empty — nothing to fetch")
	}
	if c.CalendarFetchEvery > 0 && c.CalendarFetchEvery < minFetchEvery {
		return fmt.Errorf("calendar_fetch_every %s is below the minimum of %s", c.CalendarFetchEvery, minFetchEvery)
	}
	return nil
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
	if c.OutboxRetention == 0 {
		c.OutboxRetention = 90 * 24 * time.Hour
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
	// Store health, one bit per file. While either is set, acknowledged
	// mutations are not durable (or the audit trail has holes) — surfaced
	// via PersistHealthy → /healthz.
	saveFailed  bool
	auditFailed bool
	// importFailed: the automatic calendar fetch is in a failure episode
	// — paged once on the way in, once on the way out. In-memory only;
	// a restart is a fresh episode.
	importFailed bool
	// channelsScanned: stored channels have been checked for format
	// problems once in this process (see noteChannelProblemsLocked).
	// Same deal — in-memory, a restart looks again.
	channelsScanned bool
}

func NewService(store Store, cfg Config, notify Notifier) (*Service, error) {
	cfg.fill()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
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

// clock reads the time under the lock — for the few code paths that run
// outside it (the calendar fetch's HTTP round-trip) and must not race
// SetClock.
func (s *Service) clock() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now()
}

// NoteLoginFailure records a failed admin-UI login in the audit trail —
// the admin listener has no other failed-auth signal.
func (s *Service) NoteLoginFailure(remote string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditLocked("admin.login_failed", map[string]any{"remote": remote})
}

var (
	ErrNotFound   = errors.New("not found")
	ErrGone       = errors.New("link expired or revoked")
	ErrCancelled  = errors.New("event is already cancelled")
	ErrForbidden  = errors.New("not allowed for this trust level")
	ErrWrongActor = errors.New("not addressed to you")
)

func (s *Service) saveLocked() {
	if err := s.store.Save(s.state); err != nil {
		s.noteStoreFailureLocked(&s.saveFailed, "state persist failed", err)
		return
	}
	s.saveFailed = false
}

func (s *Service) auditLocked(kind string, data any) {
	if err := s.store.Audit(kind, data); err != nil {
		s.noteStoreFailureLocked(&s.auditFailed, "audit write failed", err)
		return
	}
	s.auditFailed = false
}

// noteStoreFailureLocked records a store failure and pages the admin once
// per failure episode (the healthy→failed transition). Each file has its
// own bit so a healthy state save cannot mask a broken audit trail. The
// escalation item lives only in memory while the disk is broken — still
// the best available signal: the outbox sends from memory, so the admin
// learns while the process is alive instead of never.
func (s *Service) noteStoreFailureLocked(bit *bool, what string, err error) {
	s.logf("stattii: %s: %v", what, err)
	wasHealthy := !s.saveFailed && !s.auditFailed
	*bit = true
	if wasHealthy {
		s.notifyAdminLocked("STORE FAILURE: "+what,
			fmt.Sprintf("%s: %v\n\nMutations continue in memory but are NOT durable. "+
				"Fix the data dir now; /healthz reports failure until a write succeeds.", what, err))
	}
}

// PersistHealthy reports whether the last store writes succeeded — false
// means acknowledged mutations are not durable. Surfaced via /healthz.
func (s *Service) PersistHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.saveFailed && !s.auditFailed
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
	e.UnreachableNotifiedAt = time.Time{}
	s.auditLocked("event.reinstated", map[string]any{"event_id": eventID, "actor": actor})
	subject := "REINSTATED: " + e.Title
	body := fmt.Sprintf("%s on %s takes place after all — the cancellation is withdrawn.",
		e.Title, e.StartsAt.Format(timeFmt))
	n := s.fanOutLocked(e, "reinstated", subject, body)
	s.fireWebhooksLocked("event.reinstated", *e)
	s.nobodyToldLocked(e, "reinstated", n, true)
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

// maxReason bounds the free-text reason attached to a cancellation; it
// renders in every notice, on the public pages, and in the ICS feed.
const maxReason = 280

// cleanReason normalises typed free text for the notices: one line,
// trimmed, bounded. Over-long input is cut, not rejected — the
// cancellation must go through whatever was typed next to it.
func cleanReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxReason {
		s = string([]rune(s)[:maxReason])
	}
	return s
}

func (s *Service) cancelLocked(eventID, personID, reason, via string) (Event, error) {
	e := s.state.Event(eventID)
	if e == nil {
		return Event{}, ErrNotFound
	}
	if e.Status == StatusCancelled {
		return *e, ErrCancelled
	}
	// Every path — link form, portal note, accepted proposal, admin
	// form, API — ends up here, so this is where the bound lives.
	reason = cleanReason(reason)
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
	s.auditLocked("event.cancelled", map[string]any{"event_id": eventID, "actor": actor, "reason": reason, "via": via})

	// One body for every recipient — broadcast, assignee, guest. Who
	// cancelled is a name or "the organizer", never an id, a link, or the
	// machinery ("deadline" lives in the audit, not in anyone's inbox).
	by := "the organizer"
	if p := s.state.Person(personID); personID != "" && p != nil {
		by = p.Name
	}
	subject := "CANCELLED: " + e.Title
	body := fmt.Sprintf("%s on %s is CANCELLED.\nCancelled by %s.", e.Title, e.StartsAt.Format(timeFmt), by)
	if reason != "" {
		body += "\nReason: " + reason
	}
	n := s.fanOutLocked(e, "cancellation", subject, body)
	s.fireWebhooksLocked("event.cancelled", *e)
	// The dead-man-switch pages the admin itself, with the empty fan-out
	// folded into that one message — a second page here would be noise.
	s.nobodyToldLocked(e, "cancellation", n, via != "deadline")
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
	e.UnreachableNotifiedAt = time.Time{}
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
	n := s.fanOutLocked(e, "moved", subject, body)
	s.fireWebhooksLocked("event.moved", *e)
	// The importer moves whole series at once and pages once per sync
	// (ImportReport.Silent) instead of once per occurrence.
	s.nobodyToldLocked(e, "moved", n, actor != "import")
	return *e, nil
}

// fanOutLocked enqueues one outbox item per broadcast target, assignee
// channel, and reachable party guest — the delivery proof for outward
// communication. Returns the number of messages enqueued and records it
// on the event: a propagation that reached nobody is an alarm, and the
// panel must be able to raise it long after the outbox was pruned.
func (s *Service) fanOutLocked(e *Event, purpose, subject, body string) int {
	n := 0
	for _, b := range s.state.Broadcasts {
		s.enqueueLocked(OutboxItem{
			EventID: e.ID, Purpose: purpose, Kind: b.Kind, To: b.To,
			Subject: subject, Body: body,
		})
		n++
	}
	for _, p := range s.state.Assignees(e.ID) {
		n += len(s.enqueueToPersonLocked(p, OutboxItem{EventID: e.ID, Purpose: purpose, Subject: subject, Body: body}))
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
		key := strings.ToLower(g.Email)
		if g.Email == "" || seen[key] {
			continue
		}
		seen[key] = true
		s.enqueueLocked(OutboxItem{
			EventID: e.ID, GuestID: g.ID, Purpose: purpose,
			Kind: "email", To: g.Email, Subject: subject, Body: body,
		})
		n++
	}
	e.FanOutAt = s.now()
	e.FanOutCount = n
	return n
}

// nobodyToldLocked is the alarm behind every propagation transaction: a
// cancel/move/reinstate whose fan-out enqueued nothing has flipped a
// status that no person will ever hear about — the locked-door bug with
// a green status badge. Always audited; the admin page is the caller's
// call, because some callers fold it into a message they send anyway.
func (s *Service) nobodyToldLocked(e *Event, purpose string, n int, page bool) {
	if n > 0 {
		return
	}
	s.auditLocked("propagation.empty", map[string]any{"event_id": e.ID, "purpose": purpose, "title": e.Title, "status": e.Status})
	if page {
		s.notifyAdminLocked("Nobody was told: "+e.Title, s.nobodyToldBodyLocked(e, purpose))
	}
}

// nobodyToldBodyLocked names the three ways a fan-out ends up empty and
// what to do about each — the admin reads this once and must be able to
// fix it without opening the code.
func (s *Service) nobodyToldBodyLocked(e *Event, purpose string) string {
	what, known := map[string]string{"cancellation": "cancelled", "moved": "moved", "reinstated": "reinstated"}[purpose]
	if !known {
		what = purpose // a future purpose reads oddly, never as "was ,"
	}
	body := fmt.Sprintf("%s on %s was %s, but the notice reached NOBODY:\n"+
		"- no broadcast target is configured (stattii broadcast add),\n"+
		"- no responsible person has a channel (see /admin/people),\n"+
		"- no party guest left an address.\n"+
		"Tell people by hand now, then fix one of the three so the next notice goes out by itself.",
		e.Title, e.StartsAt.Format(timeFmt), what)
	if known && s.webhooksMatchingLocked("event."+what) > 0 {
		body += "\nA webhook consumer was notified — but no person."
	}
	return body
}

// reachableLocked splits an event's assignees into everyone and those
// with a usable channel. Staffed and reachable are different questions:
// the reminder waits for the latter, the deadline for neither.
func (s *Service) reachableLocked(eventID string) (all, reachable []*Person) {
	all = s.state.Assignees(eventID)
	for _, p := range all {
		if p.Reachable() {
			reachable = append(reachable, p)
		}
	}
	return all, reachable
}

func personNames(ps []*Person) string {
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
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
	// A person may have no channel yet (they get one later) — but never a
	// blank one: {"kind":"","to":""} would count as "has a channel" and
	// silently defeat every reachability check downstream.
	for _, ch := range channels {
		if err := ch.Validate(); err != nil {
			return Person{}, err
		}
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

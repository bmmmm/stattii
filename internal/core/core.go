// SPDX-License-Identifier: GPL-3.0-or-later

// Package core holds stattii's domain model, persistence, and the service
// logic around it: trust levels, action links, the cancellation propagation
// transaction, and the scheduler's due-calculations.
package core

import (
	"errors"
	"time"
)

// TrustLevel controls how much power a person has over their events.
type TrustLevel string

const (
	TrustRespond TrustLevel = "respond" // per-event yes/no links only
	TrustPropose TrustLevel = "propose" // portal: cancel/move/create become proposals
	TrustDirect  TrustLevel = "direct"  // portal actions apply immediately
)

func (t TrustLevel) Valid() bool {
	return t == TrustRespond || t == TrustPropose || t == TrustDirect
}

type EventStatus string

const (
	StatusScheduled EventStatus = "scheduled"
	StatusConfirmed EventStatus = "confirmed"
	StatusCancelled EventStatus = "cancelled"
)

type Event struct {
	ID       string      `json:"id"`
	Title    string      `json:"title"`
	Location string      `json:"location,omitempty"`
	Note     string      `json:"note,omitempty"`
	StartsAt time.Time   `json:"starts_at"`
	EndsAt   time.Time   `json:"ends_at"`
	Status   EventStatus `json:"status"`
	Seq      int         `json:"seq"` // ICS SEQUENCE, bumped on every change
	// IfUnconfirmed is the dead-man-switch: "" or "notify" pages the admin
	// on deadline.passed, "cancel" auto-cancels with full propagation.
	IfUnconfirmed   string    `json:"if_unconfirmed,omitempty"`
	CancelReason    string    `json:"cancel_reason,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	CancelledAt     time.Time `json:"cancelled_at"`
	ReminderSentAt  time.Time `json:"reminder_sent_at"`
	DeadlineFiredAt time.Time `json:"deadline_fired_at"`
	// Set on events imported from the calendar source: the series UID
	// and the stable per-occurrence key the sync matches on.
	SourceUID string `json:"source_uid,omitempty"`
	SourceKey string `json:"source_key,omitempty"`
}

// EventInput is the validated payload for creating an event.
type EventInput struct {
	Title         string    `json:"title"`
	Location      string    `json:"location"`
	Note          string    `json:"note"`
	StartsAt      time.Time `json:"starts_at"`
	EndsAt        time.Time `json:"ends_at"`
	IfUnconfirmed string    `json:"if_unconfirmed"`
}

func (in *EventInput) Validate() error {
	if in.Title == "" || in.StartsAt.IsZero() {
		return errors.New("title and starts_at are required")
	}
	switch in.IfUnconfirmed {
	case "", "notify", "cancel":
		return nil
	default:
		return errors.New(`if_unconfirmed must be "notify" (default) or "cancel"`)
	}
}

// Address is one way to reach a person or an audience.
type Address struct {
	Kind string `json:"kind"` // "email" | "telegram" | "webhook"
	To   string `json:"to"`   // mail address, chat id, or URL
}

type Person struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Trust       TrustLevel `json:"trust"`
	PortalToken string     `json:"portal_token"`
	Channels    []Address  `json:"channels"`
}

type Assignment struct {
	EventID  string `json:"event_id"`
	PersonID string `json:"person_id"`
	Role     string `json:"role,omitempty"`
}

type ActionKind string

const (
	ActionConfirm ActionKind = "confirm"
	ActionCancel  ActionKind = "cancel"
)

// ActionLink is a tokenized capability: one person, one event, one action.
type ActionLink struct {
	Token     string     `json:"token"`
	EventID   string     `json:"event_id"`
	PersonID  string     `json:"person_id"`
	Action    ActionKind `json:"action"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt time.Time  `json:"revoked_at"`
}

// Response records who decided what, when, and through which surface.
type Response struct {
	At       time.Time  `json:"at"`
	EventID  string     `json:"event_id"`
	PersonID string     `json:"person_id"`
	Action   ActionKind `json:"action"`
	Via      string     `json:"via"` // "link" | "portal" | "api"
}

// Proposal is a third-party change request awaiting an admin decision.
type Proposal struct {
	ID        string    `json:"id"`
	PersonID  string    `json:"person_id"`
	Kind      string    `json:"kind"` // "cancel" | "move" | "create"
	EventID   string    `json:"event_id,omitempty"`
	Title     string    `json:"title,omitempty"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	DecidedAt time.Time `json:"decided_at"`
	Accepted  bool      `json:"accepted"`
}

// Broadcast is an audience-facing outward target (public channel, list,
// website hook) that must be reached when an event is cancelled or moved.
type Broadcast struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind"`
	To   string `json:"to"`
}

// Webhook is an API-consumer subscription; payloads are HMAC-signed.
type Webhook struct {
	ID     string   `json:"id"`
	URL    string   `json:"url"`
	Secret string   `json:"secret"`
	Events []string `json:"events,omitempty"` // empty = all
}

// Button is an inline action a channel may render (Telegram inline
// keyboards); channels that cannot render buttons ignore them — the body
// always carries equivalent links.
type Button struct {
	Label string `json:"label"`
	Data  string `json:"data,omitempty"` // action-link token for callbacks
	URL   string `json:"url,omitempty"`
}

// OutboxItem is one pending or delivered outbound message. All sends go
// through the outbox so retries, delivery proof, and escalation are uniform.
type OutboxItem struct {
	ID          string            `json:"id"`
	EventID     string            `json:"event_id,omitempty"`
	PersonID    string            `json:"person_id,omitempty"`
	Purpose     string            `json:"purpose"` // "reminder" | "cancellation" | "moved" | "reinstated" | "proposal" | "escalation" | "webhook"
	Kind        string            `json:"kind"`    // channel kind
	To          string            `json:"to"`
	Subject     string            `json:"subject"`
	Body        string            `json:"body"`
	Buttons     []Button          `json:"buttons,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Attempts    int               `json:"attempts"`
	NextAttempt time.Time         `json:"next_attempt"`
	DeliveredAt time.Time         `json:"delivered_at"`
	EscalatedAt time.Time         `json:"escalated_at"`
	LastError   string            `json:"last_error,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

func (o *OutboxItem) Delivered() bool { return !o.DeliveredAt.IsZero() }

// State is the whole persistent world, small enough to live in memory.
type State struct {
	Events      []Event      `json:"events"`
	People      []Person     `json:"people"`
	Assignments []Assignment `json:"assignments"`
	Links       []ActionLink `json:"links"`
	Responses   []Response   `json:"responses"`
	Proposals   []Proposal   `json:"proposals"`
	Broadcasts  []Broadcast  `json:"broadcasts"`
	Webhooks    []Webhook    `json:"webhooks"`
	Outbox      []OutboxItem `json:"outbox"`
	// Calendar import: per-series responsibles and the last fetch report.
	SeriesAssignments []SeriesAssignment `json:"series_assignments,omitempty"`
	LastImport        *ImportReport      `json:"last_import,omitempty"`
}

func (s *State) Event(id string) *Event {
	for i := range s.Events {
		if s.Events[i].ID == id {
			return &s.Events[i]
		}
	}
	return nil
}

func (s *State) Person(id string) *Person {
	for i := range s.People {
		if s.People[i].ID == id {
			return &s.People[i]
		}
	}
	return nil
}

func (s *State) Assignees(eventID string) []*Person {
	var out []*Person
	for _, a := range s.Assignments {
		if a.EventID == eventID {
			if p := s.Person(a.PersonID); p != nil {
				out = append(out, p)
			}
		}
	}
	return out
}

func (s *State) EventsFor(personID string) []*Event {
	var out []*Event
	for _, a := range s.Assignments {
		if a.PersonID == personID {
			if e := s.Event(a.EventID); e != nil {
				out = append(out, e)
			}
		}
	}
	return out
}

// ResponseFor returns the latest response of a person for an event, or nil.
func (s *State) ResponseFor(eventID, personID string) *Response {
	for i := len(s.Responses) - 1; i >= 0; i-- {
		r := &s.Responses[i]
		if r.EventID == eventID && r.PersonID == personID {
			return r
		}
	}
	return nil
}

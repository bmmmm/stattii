// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// ---- outbox, webhooks, escalation -----------------------------------------

func (s *Service) enqueueLocked(item OutboxItem) string {
	item.ID = NewID("ob")
	item.CreatedAt = s.now()
	item.NextAttempt = item.CreatedAt
	s.state.Outbox = append(s.state.Outbox, item)
	return item.ID
}

// enqueueToPersonLocked fans one message out to every channel of a person,
// with the delivery.skipped audit for the unreachable — the one spelling
// of "message this person" shared by fan-out, reminders, proposal verdicts,
// and test messages. Returns the enqueued item IDs.
func (s *Service) enqueueToPersonLocked(p *Person, item OutboxItem) []string {
	if len(p.Channels) == 0 {
		s.auditLocked("delivery.skipped", map[string]any{"event_id": item.EventID, "person_id": p.ID, "error": "person has no channels"})
		return nil
	}
	ids := make([]string, 0, len(p.Channels))
	for _, ch := range p.Channels {
		it := item
		it.PersonID = p.ID
		it.Kind, it.To = ch.Kind, ch.To
		ids = append(ids, s.enqueueLocked(it))
	}
	return ids
}

// OutboxState classifies an item — the single definition shared by the
// summaries (Propagation and Overview fold retrying+queued into
// "pending") and every admin surface, so a red row and the "failed"
// counter can never disagree: "failed" means retries exhausted, a
// retrying item is in transit, not lost.
func (s *Service) OutboxState(o OutboxItem) string {
	switch {
	case o.Delivered():
		return "delivered"
	case o.Attempts >= s.cfg.MaxAttempts:
		return "failed"
	case o.Attempts > 0:
		return "retrying"
	default:
		return "queued"
	}
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
		switch s.OutboxState(o) {
		case "delivered":
			ps.Delivered++
		case "failed":
			ps.Failed++
		default:
			ps.Pending++
		}
	}
	ps.Complete = ps.Total > 0 && ps.Delivered == ps.Total
	return ps, nil
}

// ---- scheduler ------------------------------------------------------------

// confirmGrace is the minimum time between the confirmation ask and the
// dead-man-switch: recipients must get a real chance to answer before
// silence is allowed to cancel anything.
const confirmGrace = time.Hour

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
	if s.pruneOutboxLocked(now) {
		changed = true
	}
	if changed {
		s.saveLocked()
	}
}

// pruneOutboxLocked drops delivered items whose story is over: delivery
// AND the event (if it still exists) lie more than OutboxRetention in
// the past. Items for a live or recent event stay — the panel's
// per-guest delivery markers read them. Undelivered items are never
// pruned: an unproven delivery is the alarm this product exists for.
// Proof of pruned deliveries survives in audit.jsonl (delivery.ok);
// this only keeps state.json, re-marshalled on every mutation, bounded.
func (s *Service) pruneOutboxLocked(now time.Time) bool {
	cutoff := now.Add(-s.cfg.OutboxRetention)
	kept := s.state.Outbox[:0]
	pruned := 0
	for _, o := range s.state.Outbox {
		if !o.Delivered() || o.DeliveredAt.After(cutoff) {
			kept = append(kept, o)
			continue
		}
		if e := s.state.Event(o.EventID); e != nil {
			end := e.EndsAt
			if end.IsZero() {
				end = e.StartsAt
			}
			if end.After(cutoff) {
				kept = append(kept, o)
				continue
			}
		}
		pruned++
	}
	if pruned == 0 {
		return false
	}
	s.state.Outbox = kept
	s.auditLocked("outbox.pruned", map[string]any{"count": pruned, "cutoff": cutoff})
	return true
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
		reachable := 0
		for _, p := range assignees {
			if len(p.Channels) > 0 {
				reachable++
			}
		}
		if reachable == 0 {
			// Events are created first and staffed second; a tick in
			// between must not burn the one-shot reminder on zero
			// reachable recipients — assignees without channels count as
			// not staffed yet. Leave it pending.
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
			s.enqueueToPersonLocked(p, OutboxItem{
				EventID: e.ID, Purpose: "reminder",
				Subject: "Please confirm: " + e.Title, Body: body,
				// Channels with inline buttons get one-tap callbacks; the
				// body links stay as fallback for forwarded/old clients.
				Buttons: []Button{
					{Label: "✅ Takes place", Data: cTok},
					{Label: "❌ Cancel event", Data: xTok},
				},
			})
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
		if !e.ReminderSentAt.IsZero() && now.Sub(e.ReminderSentAt) < confirmGrace {
			// The ask just went out — people need a real chance to answer
			// before the dead-man-switch may fire. Without this, an event
			// created inside the deadline window is asked and
			// auto-cancelled in the same tick. If the grace reaches past
			// the start, the deadline simply never fires — the
			// conservative direction.
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
			if _, err := s.cancelLocked(e.ID, "", "auto-cancelled: unconfirmed by deadline", "deadline"); err != nil {
				s.logf("stattii: dead-man-switch cancel of %s failed: %v", e.ID, err)
			} else {
				s.notifyAdminLocked("Auto-cancelled: "+e.Title,
					fmt.Sprintf("%s on %s was unconfirmed by its deadline and has been auto-cancelled (dead-man-switch). Reinstate if wrong.",
						e.Title, e.StartsAt.Format(timeFmt)))
			}
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
	// Escalations are collected and enqueued after the loop: enqueueLocked
	// appends to the outbox, and an append mid-loop can reallocate the
	// backing array — every later write through o (Attempts, DeliveredAt)
	// would then land in the stale copy and the item would be re-sent on
	// every tick. They go out with the next pass, one interval later.
	type stuck struct{ subject, body string }
	var escalations []stuck
	for i := 0; i < len(s.state.Outbox); i++ {
		o := &s.state.Outbox[i]
		if o.Delivered() {
			continue
		}
		// Escalate stuck items once, whatever the attempt count.
		if o.EscalatedAt.IsZero() && o.Purpose != "escalation" &&
			now.Sub(o.CreatedAt) >= s.cfg.EscalateAfter {
			o.EscalatedAt = now
			escalations = append(escalations, stuck{
				subject: "Delivery stuck: " + o.Subject,
				body:    fmt.Sprintf("Undelivered for %s via %s to %s: %s", now.Sub(o.CreatedAt).Round(time.Minute), o.Kind, o.To, o.LastError),
			})
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
	for _, e := range escalations {
		s.notifyAdminLocked(e.subject, e.body)
	}
	return changed
}

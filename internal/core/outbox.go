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
	if !p.Reachable() {
		s.auditLocked("delivery.skipped", map[string]any{"event_id": item.EventID, "person_id": p.ID, "error": "person has no usable channel"})
		return nil
	}
	ids := make([]string, 0, len(p.Channels))
	for _, ch := range p.Channels {
		if !ch.Usable() {
			continue // legacy blank entry — nothing to send to
		}
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

// webhooksMatchingLocked counts the subscriptions an event name would
// reach — "a consumer heard, but no person did" is worth one sentence
// in the nobody-was-told page.
func (s *Service) webhooksMatchingLocked(event string) int {
	n := 0
	for _, w := range s.state.Webhooks {
		if webhookMatches(w, event) {
			n++
		}
	}
	return n
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

// PropagationStatus answers "is the cancellation actually out?" for ONE
// propagation transaction (one cancel, move, or reinstate) — never a
// merge of an event's whole history. Cancel, reinstate, cancel again is
// three transactions; only the latest is reported, matching FanOutTxnID.
type PropagationStatus struct {
	EventID   string `json:"event_id"`
	Total     int    `json:"total"`
	Delivered int    `json:"delivered"`
	Pending   int    `json:"pending"`
	Failed    int    `json:"failed"`
	Complete  bool   `json:"complete"`
	// Empty marks a transaction that enqueued zero items — nobody to
	// tell, not "still in progress". Derived from the event's own
	// FanOutCount (see nobodyToldLocked), never from counting Items:
	// delivered items get pruned, and an old, fully-pruned transaction
	// must not read as empty. Complete deliberately stays false here —
	// an empty fan-out is the alarm this product exists to raise, and
	// must never look like "done" (invariant 3).
	Empty bool         `json:"empty"`
	Items []OutboxItem `json:"items"`
}

func (s *Service) Propagation(eventID string) (PropagationStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.state.Event(eventID)
	if e == nil {
		return PropagationStatus{}, ErrNotFound
	}
	ps := PropagationStatus{EventID: eventID}
	if e.FanOutAt.IsZero() {
		// No propagation transaction has run for this event yet.
		return ps, nil
	}
	ps.Empty = e.FanOutCount == 0
	for _, o := range s.state.Outbox {
		if o.EventID != eventID {
			continue
		}
		if e.FanOutTxnID != "" {
			if o.TxnID != e.FanOutTxnID {
				continue
			}
		} else if o.Purpose != "cancellation" && o.Purpose != "moved" && o.Purpose != "reinstated" {
			// A pre-upgrade event fanned out before TxnID existed: fall
			// back to the old purpose-only filter for it (still subject
			// to the original merge-across-transactions limitation) —
			// its next cancel/move/reinstate stamps a real TxnID.
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
//
// The reminder pass runs before the deadline pass and the two never act
// on the same event in one tick: the deadline waits for a sent ask plus
// confirmGrace whenever someone reachable is assigned, and the
// unreachable early warning lives in [start-ReminderLead, start-
// DeadlineLead) while the deadline fires from start-DeadlineLead on —
// disjoint windows, no cross-pass flag to keep in sync.
func (s *Service) Tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	// Before anything is sent: say once what is stored but suspect. Here
	// rather than in NewService because the scan needs s.now(), and the
	// clock is set after the constructor.
	if s.noteChannelProblemsLocked() {
		changed = true
	}
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
		assignees, reachable := s.reachableLocked(e.ID)
		if len(reachable) == 0 {
			// Events are created first and staffed second; a tick in
			// between must not burn the one-shot reminder on zero
			// reachable recipients — assignees without channels count as
			// not staffed yet. Leave it pending.
			//
			// Staffed but unreachable is different: nobody is coming to
			// fix that by itself, and the deadline will not wait for an
			// ask that cannot go out. Warn the admin once, early — only
			// while the deadline window is still ahead, so this warning
			// and deadline.passed can never share a tick.
			if len(assignees) > 0 && e.UnreachableNotifiedAt.IsZero() &&
				now.Before(e.StartsAt.Add(-s.cfg.DeadlineLead)) {
				e.UnreachableNotifiedAt = now
				names := personNames(assignees)
				s.auditLocked("staffing.unreachable", map[string]any{"event_id": e.ID, "people": names, "count": len(assignees)})
				body := fmt.Sprintf("%s on %s is assigned to %s — but none of them has a channel, so the confirmation ask cannot go out.\n"+
					"Add an email or Telegram chat id under /admin/people, or assign someone reachable.",
					e.Title, e.StartsAt.Format(timeFmt), names)
				if e.IfUnconfirmed == "cancel" {
					body += fmt.Sprintf("\nThis event WILL auto-cancel at its deadline (%s) unless it is confirmed before then.",
						e.StartsAt.Add(-s.cfg.DeadlineLead).Format(timeFmt))
				}
				s.notifyAdminLocked("Nobody can be reached: "+e.Title, body)
				changed = true
			}
			continue
		}
		for _, p := range assignees {
			cTok, xTok := s.linksLocked(e.ID, p.ID)
			header := e.Title + "\n" + e.StartsAt.Format(timeFmt)
			if e.Location != "" {
				header += "\nLocation: " + e.Location
			}
			if !e.VanishedAt.IsZero() {
				// The responsible is the one person who knows whether a
				// missing feed entry means "off" or "feed glitch".
				header += "\n\nNote: this entry has disappeared from the source calendar. If it is off, click NO."
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
		// The ask went out — but if not one of these people has a channel
		// that passes its format check, it very likely went nowhere. Say
		// so now, while the deadline is still ahead, instead of waiting
		// for the delivery failure to escalate. One page per confirmation
		// cycle by construction: this branch runs once, right where
		// ReminderSentAt is set. A move or reinstate clears that and
		// earns a fresh warning, which is correct — it is a new ask.
		if !anyValidChannel(assignees) {
			names := personNames(assignees)
			s.auditLocked("staffing.channels_broken", map[string]any{
				"event_id": e.ID, "people": names, "count": len(assignees)})
			s.notifyAdminLocked("Asked over a broken channel: "+e.Title,
				fmt.Sprintf("The confirmation ask for %s on %s was sent to %s — but none of their "+
					"stored addresses passes its own format check, so it may have reached nobody.\n\n"+
					"The ask WAS sent and a delivery failure will still escalate; nothing was removed "+
					"and nobody counts as unreachable. Check the addresses under /admin/people.",
					e.Title, e.StartsAt.Format(timeFmt), names))
		}
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
		assignees, reachable := s.reachableLocked(e.ID)
		if e.ReminderSentAt.IsZero() && len(reachable) > 0 {
			// Staffed with someone reachable but not asked yet — the
			// reminder goes out first. Unstaffed events skip the ask
			// entirely, and so do events whose assignees have no channel:
			// waiting for an ask that can never go out would disarm the
			// dead-man-switch for exactly the events nobody can confirm.
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
		// Why nobody answered matters to the admin: an ask that never
		// went out is a staffing problem, not a silent responsible.
		staffing := ""
		switch {
		case len(assignees) == 0:
			staffing = " Nobody was assigned, so nobody could be asked."
		case e.ReminderSentAt.IsZero():
			staffing = fmt.Sprintf(" Responsible: %s — but none of them has a channel, so the ask never went out.", personNames(assignees))
		}
		if e.IfUnconfirmed == "cancel" {
			// Dead-man-switch: silence means the event does not happen —
			// and the cancellation propagates like any other. The reason
			// renders on the public pages and in every notice, so it is
			// written for recipients; "deadline" is audit detail.
			if _, err := s.cancelLocked(e.ID, "", "Not confirmed in time.", "deadline"); err != nil {
				s.logf("stattii: dead-man-switch cancel of %s failed: %v", e.ID, err)
			} else {
				body := fmt.Sprintf("%s on %s was unconfirmed by its deadline and has been auto-cancelled (dead-man-switch).%s Reinstate if wrong.",
					e.Title, e.StartsAt.Format(timeFmt), staffing)
				if e.FanOutCount == 0 {
					// One page, both facts: cancelLocked skips its own
					// nobody-was-told page for the deadline path.
					body += "\n\nThe cancellation itself reached NOBODY — " + s.nobodyToldBodyLocked(e, "cancellation")
				}
				s.notifyAdminLocked("Auto-cancelled: "+e.Title, body)
			}
		} else {
			s.notifyAdminLocked("No response: "+e.Title,
				fmt.Sprintf("%s on %s is still unconfirmed and the response deadline has passed.%s",
					e.Title, e.StartsAt.Format(timeFmt), staffing))
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

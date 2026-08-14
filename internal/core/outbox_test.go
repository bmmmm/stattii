// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"strings"
	"testing"
	"time"
)

// TestOutboxStateClassification pins the four states and their
// boundaries — every admin surface colours by this one classifier, so
// the retrying/failed edge (MaxAttempts, default 5) must not drift.
func TestOutboxStateClassification(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	cases := []struct {
		item OutboxItem
		want string
	}{
		{OutboxItem{}, "queued"},
		{OutboxItem{Attempts: 1}, "retrying"},
		{OutboxItem{Attempts: 4}, "retrying"},
		{OutboxItem{Attempts: 5}, "failed"},
		{OutboxItem{Attempts: 7}, "failed"},
		{OutboxItem{Attempts: 5, DeliveredAt: svc.now()}, "delivered"},
	}
	for _, c := range cases {
		if got := svc.OutboxState(c.item); got != c.want {
			t.Errorf("attempts=%d delivered=%v: got %q, want %q",
				c.item.Attempts, c.item.Delivered(), got, c.want)
		}
	}
}

// TestOutboxPruning: delivered items whose event is long past leave
// state.json on tick; anything still telling an open story stays — a
// delivered marker for an upcoming event, and undelivered items always.
func TestOutboxPruning(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	past := mustEvent(t, svc, 30*time.Hour)
	future := mustEvent(t, svc, 200*24*time.Hour)

	old := svc.now()
	svc.state.Outbox = append(svc.state.Outbox,
		// Delivered, event finished long before the cutoff — pruned.
		OutboxItem{ID: "ob_done", EventID: past.ID, Purpose: "cancellation",
			Kind: "email", To: "a@x.example", Attempts: 1, DeliveredAt: old, CreatedAt: old},
		// Delivered, no event (webhook), old — pruned by delivery age.
		OutboxItem{ID: "ob_hook", Purpose: "webhook", Kind: "webhook",
			To: "https://x.example", Attempts: 1, DeliveredAt: old, CreatedAt: old},
		// Delivered long ago, but the event is still upcoming — kept:
		// the panel's per-guest/person delivery markers read it.
		OutboxItem{ID: "ob_upcoming", EventID: future.ID, Purpose: "reminder",
			Kind: "email", To: "b@x.example", Attempts: 1, DeliveredAt: old, CreatedAt: old},
		// Failed, ancient — kept forever: the alarm is never pruned.
		OutboxItem{ID: "ob_alarm", EventID: past.ID, Purpose: "cancellation",
			Kind: "email", To: "c@x.example", Attempts: 5, LastError: "boom",
			NextAttempt: old, CreatedAt: old},
	)

	*clock = clock.Add(120 * 24 * time.Hour) // retention is 90 days
	svc.Tick(*clock)

	left := map[string]bool{}
	for _, o := range svc.state.Outbox {
		left[o.ID] = true
	}
	if left["ob_done"] || left["ob_hook"] {
		t.Fatalf("delivered items of finished stories survived pruning: %v", left)
	}
	if !left["ob_upcoming"] {
		t.Fatal("delivered marker for an upcoming event was pruned")
	}
	if !left["ob_alarm"] {
		t.Fatal("an undelivered item was pruned — the alarm must survive")
	}

	entries, err := svc.Audit(20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Kind == "outbox.pruned" && strings.Contains(string(e.Data), `"count":2`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no outbox.pruned audit entry with count 2 in %+v", entries)
	}

	// A second tick prunes nothing and must not claim otherwise.
	before := len(svc.state.Outbox)
	svc.Tick(*clock)
	if len(svc.state.Outbox) != before {
		t.Fatal("second tick pruned again")
	}
}

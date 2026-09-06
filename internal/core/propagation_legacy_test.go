// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"testing"
	"time"
)

// TestPropagationFallsBackForPreFanOutAtEvent covers a review finding
// (P1): an event cancelled by code that predates FanOutAt/FanOutCount/
// TxnID entirely (state.json seeded directly, the only way to reproduce
// that shape — v0.7.0-era production data, live since 2026-08-12) must
// still report its real, fully-delivered propagation. Bailing out early
// on FanOutAt.IsZero() reported total:0 complete:false for an event that
// was actually told in full — the admin page hid the section and the API
// said nothing was sent, on top of a correctly delivered cancellation.
func TestPropagationFallsBackForPreFanOutAtEvent(t *testing.T) {
	fake := &fakeNotifier{}
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	delivered := now.Add(-time.Hour)
	seed := &State{
		Events: []Event{{
			ID: "ev_legacy_prop", Title: "Old Cancel", StartsAt: now.Add(24 * time.Hour),
			EndsAt: now.Add(26 * time.Hour), Status: StatusCancelled, CancelledAt: now.Add(-2 * time.Hour),
			// FanOutAt/FanOutCount/FanOutTxnID intentionally left zero:
			// this event was cancelled before any of the three existed.
		}},
		Outbox: []OutboxItem{
			{ID: "ob_1", EventID: "ev_legacy_prop", Purpose: "cancellation", Kind: "email", To: "ana@test.local", DeliveredAt: delivered, CreatedAt: now.Add(-3 * time.Hour)},
			{ID: "ob_2", EventID: "ev_legacy_prop", Purpose: "cancellation", Kind: "email", To: "ben@test.local", DeliveredAt: delivered, CreatedAt: now.Add(-3 * time.Hour)},
		},
	}
	svc, _ := newTestServiceWithState(t, fake, seed)

	ps, err := svc.Propagation("ev_legacy_prop")
	if err != nil {
		t.Fatal(err)
	}
	if ps.Total != 2 || ps.Delivered != 2 || !ps.Complete {
		t.Fatalf("propagation = %+v, want 2/2 delivered complete (pre-FanOutAt event, purpose-only fallback)", ps)
	}
	if ps.Empty {
		t.Fatal("a fully-delivered legacy cancellation must not report Empty")
	}
}

// TestPropagationLegacyEmptyCountWithOlderDeliveredRows covers a second
// review finding (P2): a legacy event (no TxnID yet) whose latest
// transaction genuinely reached nobody (FanOutCount==0) but whose outbox
// still holds an OLDER transaction's delivered rows (picked up by the
// purpose-only fallback) must not report Empty:true alongside
// Complete:true and Total>0 in the same response — FanOutCount==0 alone
// is not proof of an empty fan-out once rows are actually found.
func TestPropagationLegacyEmptyCountWithOlderDeliveredRows(t *testing.T) {
	fake := &fakeNotifier{}
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	delivered := now.Add(-48 * time.Hour)
	seed := &State{
		Events: []Event{{
			ID: "ev_legacy_reinstate", Title: "Old Reinstate", StartsAt: now.Add(24 * time.Hour),
			EndsAt: now.Add(26 * time.Hour), Status: StatusScheduled,
			// The latest (reinstate) transaction reached nobody and
			// recorded FanOutCount=0 — but it predates FanOutTxnID, so
			// Propagation's purpose-only fallback still finds the OLDER
			// cancellation's delivered rows below.
			FanOutAt: now.Add(-time.Hour), FanOutCount: 0,
		}},
		Outbox: []OutboxItem{
			{ID: "ob_a", EventID: "ev_legacy_reinstate", Purpose: "cancellation", Kind: "email", To: "ana@test.local", DeliveredAt: delivered, CreatedAt: delivered},
			{ID: "ob_b", EventID: "ev_legacy_reinstate", Purpose: "cancellation", Kind: "email", To: "ben@test.local", DeliveredAt: delivered, CreatedAt: delivered},
		},
	}
	svc, _ := newTestServiceWithState(t, fake, seed)

	ps, err := svc.Propagation("ev_legacy_reinstate")
	if err != nil {
		t.Fatal(err)
	}
	if ps.Total != 2 || ps.Delivered != 2 || !ps.Complete {
		t.Fatalf("propagation = %+v, want the older delivered rows counted via the purpose-only fallback", ps)
	}
	if ps.Empty {
		t.Fatal("Empty must not be true when rows were actually found (FanOutCount==0 alone is not proof)")
	}
}

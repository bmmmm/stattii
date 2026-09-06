// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"testing"
	"time"
)

// TestPropagationReportsOnlyTheLatestTransaction covers #7: cancel,
// reinstate, cancel again on the same event used to merge all three
// propagation transactions into one Propagation() count, and "complete"
// was a statement about their union rather than about the cancellation
// that is actually in effect right now. Each fanOutLocked call must stamp
// its own transaction id, and Propagation must report only the latest one.
func TestPropagationReportsOnlyTheLatestTransaction(t *testing.T) {
	fake := &fakeNotifier{fail: map[string]bool{}}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	ana := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(e.ID, ana.ID, ""); err != nil {
		t.Fatal(err)
	}

	// Transaction 1: cancel, deliver.
	if _, err := svc.CancelEvent(e.ID, "", "rain", "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)

	// Transaction 2: reinstate, deliver.
	if _, err := svc.ReinstateEvent(e.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)

	// Transaction 3: cancel again, but fail delivery this time.
	fake.mu.Lock()
	fake.fail["email"] = true
	fake.mu.Unlock()
	if _, err := svc.CancelEvent(e.ID, "", "rain again", "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)

	ps, err := svc.Propagation(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Only transaction 3's one item must be counted — not the 3 total
	// items across all transactions merging into one inflated count.
	if ps.Total != 1 {
		t.Fatalf("Propagation.Total = %d, want 1 (only the latest transaction, not all three merged)", ps.Total)
	}
	if ps.Delivered != 0 || ps.Pending != 1 {
		t.Fatalf("propagation = %+v, want the latest (undelivered) transaction's own delivery state", ps)
	}
	if ps.Complete {
		t.Fatal("propagation must not be complete while the latest transaction's only item is undelivered")
	}
}

// TestPropagationEmptyFanOutNeverReadsComplete covers #7's second question:
// an event with zero propagation targets used to report complete:false
// forever, indistinguishable from "still delivering". Empty now says so
// explicitly, and Complete stays false — an empty fan-out is the alarm
// this product exists to raise (invariant 3), never a "done".
func TestPropagationEmptyFanOutNeverReadsComplete(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour) // nobody assigned, no broadcasts

	if _, err := svc.CancelEvent(e.ID, "", "rain", "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)

	ps, err := svc.Propagation(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Empty {
		t.Fatalf("propagation = %+v, want Empty=true for a zero-target fan-out", ps)
	}
	if ps.Complete {
		t.Fatal("an empty fan-out must never report complete:true")
	}
	if ps.Total != 0 {
		t.Fatalf("Total = %d, want 0", ps.Total)
	}
}

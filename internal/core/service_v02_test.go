// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"strings"
	"testing"
	"time"
)

func TestDeadlineAutoCancel(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	start := svc.now().Add(40 * time.Hour)
	e, err := svc.CreateEvent(EventInput{
		Title: "DMS Event", StartsAt: start, EndsAt: start.Add(time.Hour),
		IfUnconfirmed: "cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	p := mustPerson(t, svc, "ana", TrustRespond)
	svc.Assign(e.ID, p.ID, "")

	svc.Tick(*clock) // reminder out
	*clock = clock.Add(17 * time.Hour)
	svc.Tick(*clock) // deadline (24h before start) passed, nobody answered

	got, _ := svc.EventByID(e.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("dead-man-switch did not cancel, status = %s", got.Status)
	}
	if !strings.Contains(got.CancelReason, "auto-cancelled") {
		t.Fatalf("cancel reason = %q", got.CancelReason)
	}
	// The auto-cancellation must propagate like a manual one.
	ps, _ := svc.Propagation(e.ID)
	if ps.Total == 0 {
		t.Fatal("auto-cancel did not fan out")
	}
	foundAdmin := false
	for _, m := range fake.byPurposeTo("admin@test.local") {
		if strings.Contains(m.Subject, "Auto-cancelled") {
			foundAdmin = true
		}
	}
	if !foundAdmin {
		t.Fatal("admin was not told about the auto-cancel")
	}
}

func TestDeadlineNotifyOnlyByDefault(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour) // default policy
	p := mustPerson(t, svc, "ana", TrustRespond)
	svc.Assign(e.ID, p.ID, "")
	svc.Tick(*clock)
	*clock = clock.Add(17 * time.Hour)
	svc.Tick(*clock)
	if got, _ := svc.EventByID(e.ID); got.Status == StatusCancelled {
		t.Fatal("default policy must not auto-cancel")
	}
}

func TestReinstate(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	svc.Assign(e.ID, p.ID, "")
	svc.Tick(*clock) // reminder for the original cycle

	if _, err := svc.CancelEvent(e.ID, "", "mistake", "api"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.ReinstateEvent(e.ID, "api")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusScheduled || got.CancelReason != "" || !got.CancelledAt.IsZero() {
		t.Fatalf("reinstate left cancel state behind: %+v", got)
	}
	if !got.ReminderSentAt.IsZero() {
		t.Fatal("reinstate must restart the confirmation cycle")
	}
	if _, err := svc.ReinstateEvent(e.ID, "api"); err == nil {
		t.Fatal("reinstating a non-cancelled event must fail")
	}

	// The withdrawal propagates, and a fresh reminder goes out.
	svc.Tick(clock.Add(time.Minute))
	reinstated, fresh := false, 0
	for _, m := range fake.byPurposeTo("ana@test.local") {
		if strings.Contains(m.Subject, "REINSTATED") {
			reinstated = true
		}
		if strings.Contains(m.Subject, "Please confirm") {
			fresh++
		}
	}
	if !reinstated {
		t.Fatal("no REINSTATED notification delivered")
	}
	if fresh < 2 {
		t.Fatalf("want a fresh reminder after reinstate, got %d reminder(s) total", fresh)
	}
}

func TestOutboxRetryAfterFailure(t *testing.T) {
	fake := &fakeNotifier{fail: map[string]bool{"email": true}}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	svc.Assign(e.ID, p.ID, "")
	svc.CancelEvent(e.ID, "", "", "api")

	// Burn through all attempts (backoff maxes at 16m; 1h steps clear it).
	for range 6 {
		svc.Tick(*clock)
		*clock = clock.Add(time.Hour)
	}
	ps, _ := svc.Propagation(e.ID)
	if ps.Failed != 1 {
		t.Fatalf("want 1 failed item, got %+v", ps)
	}
	var failedID string
	for _, o := range svc.OutboxItems(true) {
		if o.Purpose == "cancellation" {
			failedID = o.ID
		}
	}
	if failedID == "" {
		t.Fatal("failed cancellation item not listed in pending outbox")
	}

	// Re-arm, fix the channel, deliver.
	if _, err := svc.RetryOutbox(failedID); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.fail["email"] = false
	fake.mu.Unlock()
	svc.Tick(*clock)
	ps, _ = svc.Propagation(e.ID)
	if !ps.Complete {
		t.Fatalf("retry did not deliver: %+v", ps)
	}
	if _, err := svc.RetryOutbox(failedID); err == nil {
		t.Fatal("retrying a delivered item must fail")
	}
}

func TestProposeMoveViaLink(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond) // lowest trust level
	svc.Assign(e.ID, p.ID, "")
	_, cancelURL, _ := svc.GenerateLinks(e.ID, p.ID)
	token := strings.TrimPrefix(cancelURL, "http://test.local/a/")

	newStart := svc.now().Add(60 * time.Hour)
	pr, err := svc.ProposeMoveViaLink(token, newStart, newStart.Add(time.Hour), "room clash")
	if err != nil {
		t.Fatal(err)
	}
	if pr.Kind != "move" || pr.PersonID != p.ID || pr.EventID != e.ID {
		t.Fatalf("unexpected proposal %+v", pr)
	}
	// Nothing changed yet — it is only a proposal.
	if got, _ := svc.EventByID(e.ID); !got.StartsAt.Equal(e.StartsAt) {
		t.Fatal("proposal must not move the event by itself")
	}
	// Accepting applies the move.
	if _, err := svc.DecideProposal(pr.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.EventByID(e.ID); !got.StartsAt.Equal(newStart) {
		t.Fatalf("accepted proposal did not move the event: %v", got.StartsAt)
	}
	// Missing start time is rejected.
	if _, err := svc.ProposeMoveViaLink(token, time.Time{}, time.Time{}, ""); err == nil {
		t.Fatal("empty start must be rejected")
	}
}

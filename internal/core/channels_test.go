// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"strings"
	"testing"
	"time"
)

// A channel stored before the format check existed is structurally
// reachable and formally broken. The scheduler must keep treating that
// person as reachable — anything else cancels events over a typo — while
// the operator must nevertheless be told. Scenarios, not units; the
// sibling cases for "no channel at all" live in reachability_test.go.

const legacyAddr = "ana(at)x.local"

// legacyState builds a store the way v0.6.0 left one: a person whose
// address never passed AddPerson/UpdatePerson, optionally assigned to one
// upcoming event.
func legacyState(startsAt time.Time, assign bool) *State {
	st := &State{
		People: []Person{{
			ID: "pe_legacy", Name: "ana", Trust: TrustRespond, PortalToken: "tok_legacy",
			Channels: []Address{{Kind: "email", To: legacyAddr}},
		}},
	}
	if assign {
		st.Events = []Event{{
			ID: "ev_legacy", Title: "Quiet Night", StartsAt: startsAt,
			EndsAt: startsAt.Add(time.Hour), Status: StatusScheduled, IfUnconfirmed: "cancel",
		}}
		st.Assignments = []Assignment{{EventID: "ev_legacy", PersonID: "pe_legacy"}}
	}
	return st
}

// The load-bearing one: a suspect address must not quietly demote its
// owner. Reachable stays true, the channel stays stored, and the problem
// is reported instead of acted on.
func TestLegacyChannelStaysReachable(t *testing.T) {
	fake := &fakeNotifier{}
	start := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	svc, clock := newTestServiceWithState(t, fake, legacyState(start, true))
	svc.Tick(*clock)

	p := findPerson(t, svc, "pe_legacy")
	if !p.Reachable() {
		t.Fatal("a suspect address demoted its owner to unreachable")
	}
	if len(p.Channels) != 1 || p.Channels[0].To != legacyAddr {
		t.Fatalf("channel was rewritten or removed: %+v", p.Channels)
	}
	if p.HasValidChannel() {
		t.Fatal("HasValidChannel accepted an address Validate rejects")
	}

	probs := svc.ChannelProblems()
	if len(probs) != 1 {
		t.Fatalf("want exactly 1 channel problem, got %d: %+v", len(probs), probs)
	}
	c := probs[0]
	if c.PersonID != "pe_legacy" || c.Name != "ana" || c.To != legacyAddr || c.Kind != "email" {
		t.Fatalf("problem does not identify the channel: %+v", c)
	}
	if c.Problem == "" || !strings.Contains(c.Problem, legacyAddr) {
		t.Fatalf("problem gives no reason naming the address: %q", c.Problem)
	}
	if c.Assigned != 1 {
		t.Fatalf("want 1 upcoming event riding on it, got %d", c.Assigned)
	}
}

// Once per process, and only when something is riding on it.
func TestLegacyChannelReportedOncePerProcess(t *testing.T) {
	fake := &fakeNotifier{}
	start := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	svc, clock := newTestServiceWithState(t, fake, legacyState(start, true))
	for range 3 {
		svc.Tick(*clock)
		*clock = clock.Add(time.Minute)
	}
	pages := adminMessages(fake, "Stored channels look broken")
	if len(pages) != 1 {
		t.Fatalf("want exactly 1 page over 3 ticks, got %d", len(pages))
	}
	if !strings.Contains(pages[0].Body, legacyAddr) || !strings.Contains(pages[0].Body, "ana") {
		t.Fatalf("page does not name person and address:\n%s", pages[0].Body)
	}
	if !strings.Contains(pages[0].Body, "Nothing was removed") {
		t.Fatalf("page does not say the channel was left alone:\n%s", pages[0].Body)
	}
	if n := auditCount(t, svc, "channel.invalid"); n != 1 {
		t.Fatalf("channel.invalid audited %d times, want 1", n)
	}
}

// Nobody depends on the address: audit it for the record, but do not
// wake anyone. Same threshold noteUnreachablePersonLocked uses.
func TestUnassignedBrokenChannelPagesNobody(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestServiceWithState(t, fake, legacyState(time.Time{}, false))
	svc.Tick(*clock)
	if pages := adminMessages(fake, "Stored channels look broken"); len(pages) != 0 {
		t.Fatalf("paged for an address nothing rides on: %d", len(pages))
	}
	if n := auditCount(t, svc, "channel.invalid"); n != 1 {
		t.Fatalf("channel.invalid audited %d times, want 1", n)
	}
}

// The list follows the repair — it must not be a cached snapshot, or it
// would keep naming an address that is already fixed.
func TestChannelProblemsFollowTheFix(t *testing.T) {
	fake := &fakeNotifier{}
	start := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	svc, clock := newTestServiceWithState(t, fake, legacyState(start, true))
	svc.Tick(*clock)
	if len(svc.ChannelProblems()) != 1 {
		t.Fatal("setup: expected one problem before the fix")
	}

	good := []Address{{Kind: "email", To: "ana@x.local"}}
	if _, err := svc.UpdatePerson("pe_legacy", PersonUpdate{Channels: &good}); err != nil {
		t.Fatal(err)
	}
	if probs := svc.ChannelProblems(); len(probs) != 0 {
		t.Fatalf("list still names a repaired address: %+v", probs)
	}
	*clock = clock.Add(time.Minute)
	svc.Tick(*clock)
	if pages := adminMessages(fake, "Stored channels look broken"); len(pages) != 1 {
		t.Fatalf("a second page went out after the fix: %d", len(pages))
	}
}

// The ask still goes out over the suspect channel: the delivery attempt
// is the ground truth, not the format check. And a real failure still
// escalates the way it always did.
func TestBrokenChannelStillGetsTheAsk(t *testing.T) {
	fake := &fakeNotifier{fail: map[string]bool{"email": true}}
	svc, clock := newTestServiceWithState(t, fake, nil)
	e := mustDeadmanEvent(t, svc, 30*time.Hour)
	p := mustLegacyPerson(t, svc, "ana", legacyAddr)
	if err := svc.Assign(e.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)

	got, _ := svc.EventByID(e.ID)
	if got.ReminderSentAt.IsZero() {
		t.Fatal("the ask was skipped because the address looks wrong")
	}
	var queued int
	for _, o := range svc.OutboxItems(false) {
		if o.Purpose == "reminder" && o.To == legacyAddr {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("want 1 reminder queued to the suspect address, got %d", queued)
	}

	// Delivery fails for real — the existing escalation must still run.
	// Asserted on the outbox item, not on the admin mail: this fixture
	// breaks the email channel wholesale, so the page to the admin would
	// fail to send too and prove nothing.
	for range 40 {
		*clock = clock.Add(10 * time.Minute)
		svc.Tick(*clock)
	}
	var found bool
	for _, o := range svc.OutboxItems(false) {
		if o.Purpose != "reminder" || o.To != legacyAddr {
			continue
		}
		found = true
		if o.Delivered() {
			t.Fatal("a failing channel reported success")
		}
		if o.Attempts == 0 || o.LastError == "" {
			t.Fatalf("no delivery was even attempted: %+v", o)
		}
		if o.EscalatedAt.IsZero() {
			t.Fatalf("a failing suspect channel never escalated: %+v", o)
		}
	}
	if !found {
		t.Fatal("the reminder disappeared from the outbox")
	}
	if auditCount(t, svc, "delivery.fail") == 0 {
		t.Fatal("delivery failures were not audited")
	}
}

// One warning per confirmation cycle, at the moment the ask goes out —
// and none while somebody on the event has a channel that checks out.
func TestBrokenChannelWarnsOnceWhenTheAskGoesOut(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestServiceWithState(t, fake, nil)
	e := mustDeadmanEvent(t, svc, 30*time.Hour)
	broken := mustLegacyPerson(t, svc, "ana", legacyAddr)
	if err := svc.Assign(e.ID, broken.ID, ""); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		svc.Tick(*clock)
		*clock = clock.Add(time.Minute)
	}
	warn := adminMessages(fake, "Asked over a broken channel")
	if len(warn) != 1 {
		t.Fatalf("want exactly 1 warning per cycle, got %d", len(warn))
	}
	if !strings.Contains(warn[0].Body, "ana") || !strings.Contains(warn[0].Body, "ask WAS sent") {
		t.Fatalf("warning hides that the ask went out anyway:\n%s", warn[0].Body)
	}
	if n := auditCount(t, svc, "staffing.channels_broken"); n != 1 {
		t.Fatalf("staffing.channels_broken audited %d times, want 1", n)
	}

	// A second event where somebody's address does check out: no warning,
	// the broken channel of a colleague is not the operator's emergency.
	e2 := mustDeadmanEvent(t, svc, 30*time.Hour)
	ok := mustPerson(t, svc, "bo", TrustRespond)
	if err := svc.Assign(e2.ID, broken.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Assign(e2.ID, ok.ID, ""); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	if got := adminMessages(fake, "Asked over a broken channel"); len(got) != 1 {
		t.Fatalf("warned although one assignee was properly reachable: %d", len(got))
	}
}

// The warning belongs to the ask, not to the deadline: both may land in
// one tick, but the event must survive that tick — the ask needs its
// chance to be answered first.
func TestBrokenChannelWarningDoesNotShareTickWithTheDeadline(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestServiceWithState(t, fake, nil)
	e := mustDeadmanEvent(t, svc, 30*time.Hour)
	p := mustLegacyPerson(t, svc, "ana", legacyAddr)
	if err := svc.Assign(e.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	if len(adminMessages(fake, "Asked over a broken channel")) != 1 {
		t.Fatal("setup: expected the warning on the first tick")
	}
	got, _ := svc.EventByID(e.ID)
	if got.Status != StatusScheduled {
		t.Fatalf("event was cancelled in the very tick that asked: %+v", got)
	}
	if got.ReminderSentAt.IsZero() {
		t.Fatal("no ask went out, so the warning was about nothing")
	}
}

// findPerson reads one person back out of the store.
func findPerson(t *testing.T, svc *Service, id string) Person {
	t.Helper()
	for _, p := range svc.People() {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("person %s is gone from the store", id)
	return Person{}
}

// mustLegacyPerson produces what AddPerson cannot: a person carrying an
// address that never passed Validate. Written straight into the state,
// because that is the only way such data exists — it predates the check.
func mustLegacyPerson(t *testing.T, svc *Service, name, addr string) Person {
	t.Helper()
	p := mustMutePerson(t, svc, name)
	svc.mu.Lock()
	defer svc.mu.Unlock()
	for i := range svc.state.People {
		if svc.state.People[i].ID == p.ID {
			svc.state.People[i].Channels = []Address{{Kind: "email", To: addr}}
			svc.saveLocked()
			return svc.state.People[i]
		}
	}
	t.Fatalf("person %s vanished right after creation", p.ID)
	return Person{}
}

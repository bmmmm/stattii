// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"strings"
	"testing"
	"time"
)

// Review 2026-09-02: "assigned" and "reachable" were the same thing to
// the scheduler, and a fan-out with zero recipients was a silent success.
// Both holes sit right on the product's one promise — nobody in front of
// a locked door — so every test here is a scenario, not a unit.

func adminMessages(fake *fakeNotifier, subject string) []sent {
	var out []sent
	for _, m := range fake.byPurposeTo("admin@test.local") {
		if strings.Contains(m.Subject, subject) {
			out = append(out, m)
		}
	}
	return out
}

func auditCount(t *testing.T, svc *Service, kind string) int {
	t.Helper()
	entries, err := svc.Audit(500)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func mustDeadmanEvent(t *testing.T, svc *Service, startIn time.Duration) Event {
	t.Helper()
	start := svc.now().Add(startIn)
	// Title chosen so the machine-string assertions below cannot trip on
	// the fixture itself.
	e, err := svc.CreateEvent(EventInput{
		Title: "Quiet Night", StartsAt: start, EndsAt: start.Add(time.Hour), IfUnconfirmed: "cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mustMutePerson(t *testing.T, svc *Service, name string) Person {
	t.Helper()
	p, err := svc.AddPerson(name, TrustRespond, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The probe case from the review: a dead-man-switch event whose only
// responsible has no channel. Before: no reminder (nobody reachable), no
// deadline (staffed, "the reminder goes out first"), no page, no cancel —
// the switch was disarmed exactly when nobody could confirm.
func TestDeadlineFiresForUnreachableAssignees(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustDeadmanEvent(t, svc, 30*time.Hour)
	mute := mustMutePerson(t, svc, "mute")
	if err := svc.Assign(e.ID, mute.ID, ""); err != nil {
		t.Fatal(err)
	}
	for range 32 { // T-30h … T+1h, hourly
		svc.Tick(*clock)
		*clock = clock.Add(time.Hour)
	}
	got, _ := svc.EventByID(e.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("dead-man-switch never fired for an unreachable assignee: %+v", got)
	}
	pages := adminMessages(fake, "Auto-cancelled")
	if len(pages) != 1 {
		t.Fatalf("want exactly 1 auto-cancel page, got %d", len(pages))
	}
	if !strings.Contains(pages[0].Body, "mute") || !strings.Contains(pages[0].Body, "none of them has a channel") {
		t.Fatalf("page does not explain the staffing problem:\n%s", pages[0].Body)
	}
}

// Staffed-but-unreachable gets one early warning while there is still
// time to fix it — not one per tick, and none once someone reachable
// joins and the ask actually goes out.
func TestUnreachableAssigneeWarnsAdminOnce(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustDeadmanEvent(t, svc, 40*time.Hour)
	mute := mustMutePerson(t, svc, "mute")
	if err := svc.Assign(e.ID, mute.ID, ""); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		svc.Tick(*clock)
		*clock = clock.Add(time.Minute)
	}
	warn := adminMessages(fake, "Nobody can be reached")
	if len(warn) != 1 {
		t.Fatalf("want 1 early warning over 3 ticks, got %d", len(warn))
	}
	if !strings.Contains(warn[0].Body, "mute") || !strings.Contains(warn[0].Body, "WILL auto-cancel") {
		t.Fatalf("warning body lacks names / consequence:\n%s", warn[0].Body)
	}
	if auditCount(t, svc, "staffing.unreachable") != 1 {
		t.Fatal("staffing.unreachable audited more or less than once")
	}
	if got, _ := svc.EventByID(e.ID); got.Status != StatusScheduled || !got.ReminderSentAt.IsZero() {
		t.Fatalf("warning must not touch the cycle: %+v", got)
	}

	// The remedy the warning suggests: assign someone reachable.
	ana := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(e.ID, ana.ID, ""); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	if got := fake.byPurposeTo("ana@test.local"); len(got) != 1 {
		t.Fatalf("reminder did not go out once someone reachable joined: %d", len(got))
	}
	if len(adminMessages(fake, "Nobody can be reached")) != 1 {
		t.Fatal("a second warning went out after the fix")
	}
}

// The early warning lives strictly before the deadline window: a tick
// landing on the deadline boundary fires the deadline, not the warning,
// so the admin never gets "fix this" and "too late" in the same pass.
func TestUnreachableWarningNeverSharesTickWithDeadline(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustDeadmanEvent(t, svc, 24*time.Hour) // exactly on the deadline boundary
	mute := mustMutePerson(t, svc, "mute")
	if err := svc.Assign(e.ID, mute.ID, ""); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	if len(adminMessages(fake, "Nobody can be reached")) != 0 {
		t.Fatal("early warning fired inside the deadline window")
	}
	if got, _ := svc.EventByID(e.ID); got.Status != StatusCancelled {
		t.Fatalf("deadline did not fire on the boundary: %+v", got)
	}

	// And the mirror image: inside the reminder window, before the
	// deadline window, only the warning fires.
	e2 := mustDeadmanEvent(t, svc, 25*time.Hour)
	if err := svc.Assign(e2.ID, mute.ID, ""); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	if len(adminMessages(fake, "Nobody can be reached")) != 1 {
		t.Fatal("early warning did not fire ahead of the deadline window")
	}
	if got, _ := svc.EventByID(e2.ID); got.Status != StatusScheduled {
		t.Fatalf("deadline fired ahead of its window: %+v", got)
	}
}

// {"kind":"","to":""} used to count as "has a channel" and slipped past
// every reachability check — the reminder would fire and enqueue to an
// empty address.
func TestAddPersonRejectsEmptyChannel(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	for _, bad := range [][]Address{
		{{Kind: "", To: ""}},
		{{Kind: "email", To: ""}},
		{{Kind: "email", To: "not-an-address"}},
		{{Kind: "telegram", To: " "}},
		{{Kind: "webhook", To: "ftp://x"}},
		{{Kind: "pigeon", To: "coop 3"}},
	} {
		if _, err := svc.AddPerson("bad", TrustRespond, bad); err == nil {
			t.Fatalf("AddPerson accepted %+v", bad)
		}
	}
	if len(svc.People()) != 0 {
		t.Fatal("a rejected person was stored")
	}
	if _, err := svc.AddPerson("ok", TrustRespond, []Address{
		{Kind: "email", To: "ok@x.local"}, {Kind: "telegram", To: "123"}, {Kind: "webhook", To: "https://x.local/h"},
	}); err != nil {
		t.Fatalf("valid channels rejected: %v", err)
	}
}

// A cancellation nobody will hear about is the locked-door bug with a
// green status badge: audit it, page the admin, and mark the event so
// the panel can show it.
func TestCancelWithNoRecipientsPagesAdmin(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour) // nobody assigned, no broadcasts
	if _, err := svc.CancelEvent(e.ID, "", "rain", "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	pages := adminMessages(fake, "Nobody was told")
	if len(pages) != 1 {
		t.Fatalf("want 1 nobody-was-told page, got %d", len(pages))
	}
	for _, want := range []string{"broadcast", "/admin/people", "guest"} {
		if !strings.Contains(pages[0].Body, want) {
			t.Fatalf("page does not name remedy %q:\n%s", want, pages[0].Body)
		}
	}
	if auditCount(t, svc, "propagation.empty") != 1 {
		t.Fatal("propagation.empty not audited")
	}
	got, _ := svc.EventByID(e.ID)
	if got.FanOutAt.IsZero() || got.FanOutCount != 0 {
		t.Fatalf("event does not record the empty fan-out: %+v", got)
	}

	// Control: with a recipient there is nothing to page about.
	e2 := mustEvent(t, svc, 40*time.Hour)
	ana := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(e2.ID, ana.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelEvent(e2.ID, "", "", "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	if len(adminMessages(fake, "Nobody was told")) != 1 {
		t.Fatal("paged although the cancellation had a recipient")
	}
	if got, _ := svc.EventByID(e2.ID); got.FanOutCount != 1 {
		t.Fatalf("fan-out count wrong: %+v", got)
	}
}

// The webhook-only case earns one extra sentence, not a clean bill.
func TestCancelWithOnlyWebhookStillPagesAdmin(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	if _, err := svc.AddWebhook("http://hooks.test/x", nil); err != nil {
		t.Fatal(err)
	}
	e := mustEvent(t, svc, 40*time.Hour)
	if _, err := svc.CancelEvent(e.ID, "", "", "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	pages := adminMessages(fake, "Nobody was told")
	if len(pages) != 1 || !strings.Contains(pages[0].Body, "webhook consumer was notified") {
		t.Fatalf("webhook-only cancel: %+v", pages)
	}
}

// The dead-man-switch pages the admin anyway; an empty fan-out folds
// into that one message instead of a second one.
func TestDeadmanSwitchPagesOnceWhenNobodyIsTold(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	mustDeadmanEvent(t, svc, 20*time.Hour) // inside the deadline window, unstaffed
	svc.Tick(*clock)
	*clock = clock.Add(time.Minute)
	svc.Tick(*clock)
	all := fake.byPurposeTo("admin@test.local")
	if len(all) != 1 {
		t.Fatalf("want exactly 1 admin message, got %d: %+v", len(all), all)
	}
	m := all[0]
	if !strings.Contains(m.Subject, "Auto-cancelled") ||
		!strings.Contains(m.Body, "Nobody was assigned") ||
		!strings.Contains(m.Body, "reached NOBODY") {
		t.Fatalf("page must carry the cancel, the staffing gap and the empty fan-out:\n%s\n%s", m.Subject, m.Body)
	}
}

// The notice names a person or "the organizer" — never an id, a link, or
// the machinery behind an auto-cancel.
func TestCancellationBodyNamesWhoCancelled(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	if _, err := svc.AddBroadcast("list", "email", "all@test.local"); err != nil {
		t.Fatal(err)
	}

	// Link cancel by a person, with a reason typed into the form.
	e := mustEvent(t, svc, 40*time.Hour)
	ana, err := svc.AddPerson("Ana Lopez", TrustRespond, []Address{{Kind: "email", To: "ana@test.local"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Assign(e.ID, ana.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, cancelURL, _ := svc.GenerateLinks(e.ID, ana.ID)
	if _, err := svc.ApplyAction(strings.TrimPrefix(cancelURL, "http://test.local/a/"), " room \r\nflooded "); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	got, _ := svc.EventByID(e.ID)
	if got.CancelReason != "room flooded" {
		t.Fatalf("reason not normalised: %q", got.CancelReason)
	}
	var linkBody string
	for _, m := range fake.byPurposeTo("all@test.local") {
		if strings.HasPrefix(m.Subject, "CANCELLED") {
			linkBody = m.Body
		}
	}
	if !strings.Contains(linkBody, "Cancelled by Ana Lopez.") || !strings.Contains(linkBody, "Reason: room flooded") {
		t.Fatalf("link cancel body:\n%s", linkBody)
	}
	if strings.Contains(linkBody, ana.ID) || strings.Contains(linkBody, "/a/") {
		t.Fatalf("body leaks id or link:\n%s", linkBody)
	}

	// Deadline cancel: "the organizer", a recipient-facing reason, no
	// machine strings.
	e2 := mustDeadmanEvent(t, svc, 20*time.Hour)
	svc.Tick(*clock)
	*clock = clock.Add(time.Minute)
	svc.Tick(*clock)
	var dmBody string
	for _, m := range fake.byPurposeTo("all@test.local") {
		if strings.HasPrefix(m.Subject, "CANCELLED") && strings.Contains(m.Body, "Quiet Night") {
			dmBody = m.Body
		}
	}
	if !strings.Contains(dmBody, "Cancelled by the organizer.") || !strings.Contains(dmBody, "Reason: Not confirmed in time.") {
		t.Fatalf("deadline cancel body:\n%s", dmBody)
	}
	for _, machine := range []string{"deadline", "auto-cancelled", "dead-man"} {
		if strings.Contains(strings.ToLower(dmBody), machine) {
			t.Fatalf("machine string %q in a recipient notice:\n%s", machine, dmBody)
		}
	}
	if got, _ := svc.EventByID(e2.ID); got.CancelReason != "Not confirmed in time." {
		t.Fatalf("stored reason: %q", got.CancelReason)
	}

	// Oversized reasons are cut, never rejected — the cancel must land.
	e3 := mustEvent(t, svc, 40*time.Hour)
	if err := svc.Assign(e3.ID, ana.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, cancelURL, _ = svc.GenerateLinks(e3.ID, ana.ID)
	long := strings.Repeat("ä", 300)
	if _, err := svc.ApplyAction(strings.TrimPrefix(cancelURL, "http://test.local/a/"), long); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.EventByID(e3.ID); got.Status != StatusCancelled || len([]rune(got.CancelReason)) != maxReason {
		t.Fatalf("long reason: status=%s len=%d", got.Status, len([]rune(got.CancelReason)))
	}
}

// Outbox pruning empties the propagation list of old, correctly told
// cancellations — "nobody was told" must come from the event's own
// record, or every pruned cancellation would turn red months later.
func TestPrunedOutboxDoesNotClaimNobodyWasTold(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	ana := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(e.ID, ana.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelEvent(e.ID, "", "", "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock) // delivered
	*clock = clock.Add(120 * 24 * time.Hour)
	svc.Tick(*clock) // pruned (retention 90d, event long past)
	ps, _ := svc.Propagation(e.ID)
	if ps.Total != 0 {
		t.Fatalf("expected the outbox to be pruned, got %+v", ps)
	}
	got, _ := svc.EventByID(e.ID)
	if got.FanOutAt.IsZero() || got.FanOutCount != 1 {
		t.Fatalf("fan-out record did not survive pruning: %+v", got)
	}
}

// Move and reinstate are propagation transactions too — same alarm.
func TestMoveAndReinstateWithNoRecipientsPageAdmin(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 90*time.Hour)
	if _, err := svc.MoveEvent(e.ID, svc.now().Add(100*time.Hour), time.Time{}, "", "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelEvent(e.ID, "", "", "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReinstateEvent(e.ID, "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	if n := len(adminMessages(fake, "Nobody was told")); n != 3 {
		t.Fatalf("want a page per silent transaction (move, cancel, reinstate), got %d", n)
	}
	if auditCount(t, svc, "propagation.empty") != 3 {
		t.Fatal("propagation.empty audit count wrong")
	}
}

// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"strings"
	"testing"
	"time"
)

// newNudgeService is newTestService with the nudge switched on halfway
// between the default leads (reminder 48h, deadline 24h).
func newNudgeService(t *testing.T, fake Notifier) (*Service, *time.Time) {
	t.Helper()
	store, err := NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(store, Config{
		BaseURL:       "http://test.local",
		EscalateAfter: 10 * time.Minute,
		NudgeLead:     36 * time.Hour,
		AdminNotify:   &Address{Kind: "email", To: "admin@test.local"},
	}, fake)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := &now
	svc.SetClock(func() time.Time { return *clock })
	svc.logf = t.Logf
	return svc, clock
}

func nudgesTo(fake *fakeNotifier, to string) int {
	n := 0
	for _, m := range fake.byPurposeTo(to) {
		if strings.HasPrefix(m.Subject, "Reminder — ") {
			n++
		}
	}
	return n
}

// TestNudgeAsksOnlyTheSilentOnce — the second ask goes out at
// start-NudgeLead, once, to reachable assignees without an answer since
// this cycle's ask: not to one who answered, not to one without a
// channel, and not again on the next tick.
func TestNudgeAsksOnlyTheSilentOnce(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newNudgeService(t, fake)
	e := mustEvent(t, svc, 47*time.Hour)
	ana := mustPerson(t, svc, "ana", TrustRespond)
	bo := mustPerson(t, svc, "bo", TrustRespond)
	mute, err := svc.AddPerson("mute", TrustRespond, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{ana.ID, bo.ID, mute.ID} {
		if err := svc.Assign(e.ID, id, ""); err != nil {
			t.Fatal(err)
		}
	}

	svc.Tick(*clock) // the reminder
	if got, _ := svc.EventByID(e.ID); got.ReminderSentAt.IsZero() {
		t.Fatal("reminder did not fire")
	}
	skippedAfterReminder := auditCount(t, svc, "delivery.skipped")

	// Today an answer settles the event (confirm or cancel flips its
	// status), so a scheduled event with an answer in its cycle cannot be
	// produced through the API. The row is seeded to pin the filter the
	// nudge must apply regardless.
	svc.mu.Lock()
	svc.state.Responses = append(svc.state.Responses, Response{
		At: clock.Add(time.Hour), EventID: e.ID, PersonID: bo.ID, Action: ActionConfirm, Via: "link"})
	svc.mu.Unlock()

	*clock = e.StartsAt.Add(-36*time.Hour - time.Minute)
	svc.Tick(*clock)
	if n := nudgesTo(fake, "ana@test.local"); n != 0 {
		t.Fatalf("nudged %d times before start-NudgeLead", n)
	}

	*clock = e.StartsAt.Add(-36 * time.Hour)
	svc.Tick(*clock)
	if n := nudgesTo(fake, "ana@test.local"); n != 1 {
		t.Fatalf("ana (silent) got %d nudges, want 1", n)
	}
	if n := nudgesTo(fake, "bo@test.local"); n != 0 {
		t.Fatalf("bo answered but got %d nudges", n)
	}
	if n := auditCount(t, svc, "reminder.nudged"); n != 1 {
		t.Fatalf("want one reminder.nudged audit entry, got %d", n)
	}
	if n := auditCount(t, svc, "delivery.skipped"); n != skippedAfterReminder {
		t.Fatalf("the nudge tried the unreachable assignee (%d → %d delivery.skipped)", skippedAfterReminder, n)
	}
	for _, m := range fake.byPurposeTo("ana@test.local") {
		if strings.HasPrefix(m.Subject, "Reminder — ") && !strings.Contains(m.Body, "http://test.local/a/") {
			t.Fatalf("the nudge lost the action links:\n%s", m.Body)
		}
	}

	*clock = clock.Add(time.Hour)
	svc.Tick(*clock)
	if n := nudgesTo(fake, "ana@test.local"); n != 1 {
		t.Fatalf("one nudge per cycle, got %d", n)
	}
}

// TestNudgeNeverTooLateOrTooSoon — a nudge that would land inside
// confirmGrace of the deadline gives nobody a real chance to answer, and
// an ask that itself went out inside the nudge window IS the fresh one.
func TestNudgeNeverTooLateOrTooSoon(t *testing.T) {
	t.Run("inside confirmGrace of the deadline", func(t *testing.T) {
		fake := &fakeNotifier{}
		svc, clock := newNudgeService(t, fake)
		e := mustEvent(t, svc, 47*time.Hour)
		ana := mustPerson(t, svc, "ana", TrustRespond)
		if err := svc.Assign(e.ID, ana.ID, ""); err != nil {
			t.Fatal(err)
		}
		svc.Tick(*clock)
		// The scheduler was down through the whole nudge window.
		*clock = e.StartsAt.Add(-24*time.Hour - 30*time.Minute)
		svc.Tick(*clock)
		if n := nudgesTo(fake, "ana@test.local"); n != 0 {
			t.Fatalf("nudged %d times inside confirmGrace of the deadline", n)
		}
	})
	t.Run("ask went out just before the nudge window", func(t *testing.T) {
		fake := &fakeNotifier{}
		svc, clock := newNudgeService(t, fake)
		e := mustEvent(t, svc, 36*time.Hour+2*time.Minute)
		ana := mustPerson(t, svc, "ana", TrustRespond)
		if err := svc.Assign(e.ID, ana.ID, ""); err != nil {
			t.Fatal(err)
		}
		svc.Tick(*clock)
		*clock = clock.Add(2 * time.Minute)
		svc.Tick(*clock)
		if n := nudgesTo(fake, "ana@test.local"); n != 0 {
			t.Fatalf("nudged %d times two minutes after the ask", n)
		}
	})
	t.Run("ask went out inside the nudge window", func(t *testing.T) {
		fake := &fakeNotifier{}
		svc, clock := newNudgeService(t, fake)
		e := mustEvent(t, svc, 30*time.Hour)
		ana := mustPerson(t, svc, "ana", TrustRespond)
		if err := svc.Assign(e.ID, ana.ID, ""); err != nil {
			t.Fatal(err)
		}
		svc.Tick(*clock)
		*clock = clock.Add(time.Hour)
		svc.Tick(*clock)
		if n := nudgesTo(fake, "ana@test.local"); n != 0 {
			t.Fatalf("a fresh ask was nudged %d times", n)
		}
	})
}

// TestNudgeSkipsASettledEvent — one answer settles the event for all its
// assignees: once Ana confirmed, Bo gets no nudge for it.
func TestNudgeSkipsASettledEvent(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newNudgeService(t, fake)
	e := mustEvent(t, svc, 47*time.Hour)
	ana := mustPerson(t, svc, "ana", TrustRespond)
	bo := mustPerson(t, svc, "bo", TrustRespond)
	for _, id := range []string{ana.ID, bo.ID} {
		if err := svc.Assign(e.ID, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	svc.Tick(*clock)
	if _, err := svc.ConfirmEvent(e.ID, ana.ID, "link"); err != nil {
		t.Fatal(err)
	}
	*clock = e.StartsAt.Add(-36 * time.Hour)
	svc.Tick(*clock)
	if n := nudgesTo(fake, "bo@test.local"); n != 0 {
		t.Fatalf("bo was nudged %d times for a confirmed event", n)
	}
}

// TestNudgeResetsWithTheCycle — a move or a reinstate starts a new
// confirmation cycle, and with it a new nudge; an answer from the old
// cycle does not count against the new one.
func TestNudgeResetsWithTheCycle(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newNudgeService(t, fake)
	e := mustEvent(t, svc, 47*time.Hour)
	ana := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(e.ID, ana.ID, ""); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	*clock = e.StartsAt.Add(-36 * time.Hour)
	svc.Tick(*clock)
	if got, _ := svc.EventByID(e.ID); got.NudgeSentAt.IsZero() {
		t.Fatal("nudge did not fire")
	}
	if _, err := svc.ConfirmEvent(e.ID, ana.ID, "link"); err != nil {
		t.Fatal(err)
	}

	start := clock.Add(47 * time.Hour)
	if _, err := svc.MoveEvent(e.ID, start, start.Add(2*time.Hour), "", "api"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.EventByID(e.ID); !got.NudgeSentAt.IsZero() {
		t.Fatal("a move kept the old cycle's nudge")
	}
	*clock = clock.Add(time.Minute)
	svc.Tick(*clock) // the new ask
	*clock = start.Add(-36 * time.Hour)
	svc.Tick(*clock)
	if n := nudgesTo(fake, "ana@test.local"); n != 2 {
		t.Fatalf("the moved event's nudge: want 2 in total, got %d (the old answer must not count)", n)
	}

	if _, err := svc.CancelEvent(e.ID, "", "storm", "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReinstateEvent(e.ID, "api"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.EventByID(e.ID); !got.NudgeSentAt.IsZero() {
		t.Fatal("a reinstate kept the old cycle's nudge")
	}
}

// TestNudgeLeadOutOfRangeRefused — a nudge before the ask has nothing to
// repeat, one inside confirmGrace of the deadline could never fire; both
// are config errors, not a silent no-op.
func TestNudgeLeadOutOfRangeRefused(t *testing.T) {
	for lead, ok := range map[time.Duration]bool{
		0:              true,
		36 * time.Hour: true,
		48 * time.Hour: false, // = reminder_lead
		60 * time.Hour: false,
		25 * time.Hour: false, // = deadline_lead + confirmGrace
		12 * time.Hour: false,
	} {
		store, err := NewJSONStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		_, err = NewService(store, Config{NudgeLead: lead}, &fakeNotifier{})
		if (err == nil) != ok {
			t.Fatalf("nudge_lead %s: got err=%v, want ok=%v", lead, err, ok)
		}
	}
}

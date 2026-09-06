// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/icsimport"
)

// TestDeleteEventRefusesALiveOne: deleting instead of cancelling is
// exactly the locked door — everyone who was told about the event would
// keep expecting it. Cancel first, then the row may go.
func TestDeleteEventRefusesALiveOne(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, 40*time.Hour)

	err := svc.DeleteEvent(e.ID)
	if err == nil {
		t.Fatal("a scheduled, upcoming event was deleted without a cancellation")
	}
	if !strings.Contains(err.Error(), "cancel it first") {
		t.Fatalf("the refusal must say what to do instead, got %q", err)
	}
	if _, err := svc.EventByID(e.ID); err != nil {
		t.Fatalf("the refused deletion removed it anyway: %v", err)
	}

	if _, err := svc.CancelEvent(e.ID, "", "storm", "api"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteEvent(e.ID); err != nil {
		t.Fatalf("a cancelled event must be deletable: %v", err)
	}
	if _, err := svc.EventByID(e.ID); err == nil {
		t.Fatal("the event survived its deletion")
	}
	if err := svc.DeleteEvent(e.ID); err != ErrNotFound {
		t.Fatalf("second delete: got %v, want ErrNotFound", err)
	}
}

// TestDeleteEventIsAllowedOncePast: an event that has happened needs no
// cancellation — nobody is waiting on it any more.
func TestDeleteEventIsAllowedOncePast(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, -40*time.Hour)
	if err := svc.DeleteEvent(e.ID); err != nil {
		t.Fatalf("a finished event must be deletable: %v", err)
	}
}

// TestDeleteEventClearsItsOwnRowsButKeepsTheProof: everything that only
// existed for the event goes; the outbox does not — an undelivered
// notice still has to go out, and a delivered one is the proof it did.
func TestDeleteEventClearsItsOwnRowsButKeepsTheProof(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	other := mustEvent(t, svc, 60*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(e.ID, p.ID, "host"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Assign(other.ID, p.ID, "host"); err != nil {
		t.Fatal(err)
	}
	tok := inviteToken(t, svc, e.ID)
	mustRSVP(t, svc, tok, RSVPInput{Name: "Ben", Email: "ben@party.example", Status: GuestYes})
	svc.Tick(*clock) // reminders out, action links minted
	if _, err := svc.ConfirmEvent(e.ID, p.ID, "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelEvent(e.ID, "", "storm", "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock) // deliver the fan-out
	outboxBefore := len(svc.OutboxItems(false))
	// The absence checks below are only worth anything if these rows
	// exist first — an input set that can silently go empty is no gate.
	for what, n := range map[string]int{
		"assignments": countWhere(svc.state.Assignments, func(a Assignment) bool { return a.EventID == e.ID }),
		"links":       countWhere(svc.state.Links, func(l ActionLink) bool { return l.EventID == e.ID }),
		"responses":   countWhere(svc.state.Responses, func(r Response) bool { return r.EventID == e.ID }),
		"guests":      countWhere(svc.state.Guests, func(g Guest) bool { return g.EventID == e.ID }),
		"invites":     countWhere(svc.state.Invites, func(i InviteLink) bool { return i.EventID == e.ID }),
		"outbox":      outboxBefore,
	} {
		if n == 0 {
			t.Fatalf("setup produced no %s — the deletion check would pass on an empty set", what)
		}
	}

	if err := svc.DeleteEvent(e.ID); err != nil {
		t.Fatal(err)
	}

	for _, a := range svc.state.Assignments {
		if a.EventID == e.ID {
			t.Error("an assignment survived the deletion")
		}
	}
	for _, l := range svc.state.Links {
		if l.EventID == e.ID {
			t.Error("an action link survived the deletion")
		}
	}
	for _, r := range svc.state.Responses {
		if r.EventID == e.ID {
			t.Error("a response survived the deletion")
		}
	}
	for _, g := range svc.state.Guests {
		if g.EventID == e.ID {
			t.Error("a guest survived the deletion")
		}
	}
	for _, i := range svc.state.Invites {
		if i.EventID == e.ID {
			t.Error("the invite link survived the deletion")
		}
	}
	if got := len(svc.OutboxItems(false)); got != outboxBefore {
		t.Errorf("the outbox lost %d item(s) with the event — that is the delivery proof", outboxBefore-got)
	}
	// The other event's rows are untouched.
	found := false
	for _, a := range svc.state.Assignments {
		if a.EventID == other.ID {
			found = true
		}
	}
	if !found {
		t.Error("the deletion took another event's assignment with it")
	}
}

// TestDeletePersonRefusesWhileStillResponsible: dropping the assignment
// silently would take the event's confirmation cycle with it and let the
// dead-man-switch fire on an event nobody knew they still had.
func TestDeletePersonRefusesWhileStillResponsible(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, 40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(e.ID, p.ID, "host"); err != nil {
		t.Fatal(err)
	}

	err := svc.DeletePerson(p.ID)
	if err == nil {
		t.Fatal("a person responsible for an upcoming event was deleted")
	}
	if !strings.Contains(err.Error(), "unassign first") {
		t.Fatalf("the refusal must say what to do instead, got %q", err)
	}
	if len(svc.People()) != 1 {
		t.Fatal("the refused deletion removed them anyway")
	}

	if err := svc.Unassign(e.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeletePerson(p.ID); err != nil {
		t.Fatalf("after unassigning, the deletion must go through: %v", err)
	}
	if len(svc.People()) != 0 {
		t.Fatal("the person survived their deletion")
	}
	if err := svc.DeletePerson(p.ID); err != ErrNotFound {
		t.Fatalf("second delete: got %v, want ErrNotFound", err)
	}
}

// TestDeletePersonRemovesTheirTraces: a cancelled or finished event is
// no reason to keep them, and what they leave behind goes with them.
// The journal keeps who answered what.
func TestDeletePersonRemovesTheirTraces(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	past := mustEvent(t, svc, -40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	keep := mustPerson(t, svc, "ben", TrustRespond)
	for _, id := range []string{p.ID, keep.ID} {
		if err := svc.Assign(past.ID, id, "host"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := svc.GenerateLinks(past.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmEvent(past.ID, p.ID, "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AssignSeries("uid-1", p.ID, "host"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)

	for what, n := range map[string]int{
		"assignments":        countWhere(svc.state.Assignments, func(a Assignment) bool { return a.PersonID == p.ID }),
		"links":              countWhere(svc.state.Links, func(l ActionLink) bool { return l.PersonID == p.ID }),
		"responses":          countWhere(svc.state.Responses, func(r Response) bool { return r.PersonID == p.ID }),
		"series assignments": countWhere(svc.state.SeriesAssignments, func(sa SeriesAssignment) bool { return sa.PersonID == p.ID }),
	} {
		if n == 0 {
			t.Fatalf("setup produced no %s — the deletion check would pass on an empty set", what)
		}
	}

	if err := svc.DeletePerson(p.ID); err != nil {
		t.Fatal(err)
	}
	for _, a := range svc.state.Assignments {
		if a.PersonID == p.ID {
			t.Error("an assignment survived the deletion")
		}
	}
	for _, l := range svc.state.Links {
		if l.PersonID == p.ID {
			t.Error("an action link survived the deletion")
		}
	}
	for _, r := range svc.state.Responses {
		if r.PersonID == p.ID {
			t.Error("a response survived the deletion")
		}
	}
	for _, sa := range svc.state.SeriesAssignments {
		if sa.PersonID == p.ID {
			t.Error("a series assignment survived the deletion")
		}
	}
	// The other assignee is untouched.
	found := false
	for _, a := range svc.state.Assignments {
		if a.PersonID == keep.ID {
			found = true
		}
	}
	if !found {
		t.Error("the deletion took another person's assignment with it")
	}
	// The journal still says who confirmed.
	entries, err := svc.Audit(50)
	if err != nil {
		t.Fatal(err)
	}
	said := false
	for _, en := range entries {
		if strings.Contains(string(en.Data), p.ID) {
			said = true
		}
	}
	if !said {
		t.Error("the audit journal forgot the person entirely")
	}
}

func countWhere[T any](rows []T, hit func(T) bool) int {
	n := 0
	for _, r := range rows {
		if hit(r) {
			n++
		}
	}
	return n
}

func eventBySourceUID(t *testing.T, svc *Service, uid string) Event {
	t.Helper()
	for _, e := range svc.Events() {
		if e.SourceUID == uid {
			return e
		}
	}
	t.Fatalf("no imported event for %s", uid)
	return Event{}
}

// TestDeleteRefusesWhatTheImportWouldRecreate: for an imported
// occurrence the row is derived data — and for a cancelled one it is the
// tombstone. Deleting it would let the next sync create it again as
// scheduled, which is the one thing the import itself never does
// (invariant 9).
func TestDeleteRefusesWhatTheImportWouldRecreate(t *testing.T) {
	svc, clock := newTestService(t, &fakeNotifier{})
	svc.cfg.CalendarSource = "https://feed.test/cal.ics"
	now := *clock
	until := now.Add(60 * 24 * time.Hour)
	keep := occ("other", "One-off", now.Add(30*time.Hour), time.Hour)
	mine := occ("series-1", "Weekly Thing", now.Add(40*time.Hour), 2*time.Hour)
	svc.SyncCalendar([]icsimport.Occurrence{keep, mine}, nil, until)
	e := eventBySourceUID(t, svc, "series-1")
	if _, err := svc.CancelEvent(e.ID, "", "storm", "api"); err != nil {
		t.Fatal(err)
	}

	err := svc.DeleteEvent(e.ID)
	if err == nil {
		t.Fatal("a cancelled imported occurrence was deleted — the next sync brings it back as scheduled")
	}
	if !strings.Contains(err.Error(), "calendar import") {
		t.Fatalf("the refusal must name the reason, got %q", err)
	}

	// What the refusal buys: the surviving row keeps the sync away.
	rep := svc.SyncCalendar([]icsimport.Occurrence{keep, mine}, nil, until)
	if after := eventBySourceUID(t, svc, "series-1"); after.Status != StatusCancelled {
		t.Fatalf("the sync revived the cancelled occurrence: %s (%+v)", after.Status, rep)
	}
}

// TestDeleteAllowsAnImportedGhost: an occurrence the feed itself dropped
// is not coming back — once it is also over, it may go.
func TestDeleteAllowsAnImportedGhost(t *testing.T) {
	svc, clock := newTestService(t, &fakeNotifier{})
	svc.cfg.CalendarSource = "https://feed.test/cal.ics"
	now := *clock
	until := now.Add(60 * 24 * time.Hour)
	keep := occ("other", "One-off", now.Add(30*time.Hour), time.Hour)
	mine := occ("series-1", "Weekly Thing", now.Add(40*time.Hour), 2*time.Hour)
	svc.SyncCalendar([]icsimport.Occurrence{keep, mine}, nil, until)
	svc.SyncCalendar([]icsimport.Occurrence{keep}, nil, until) // dropped from the feed
	e := eventBySourceUID(t, svc, "series-1")
	if e.VanishedAt.IsZero() {
		t.Fatal("the sync did not mark the missing occurrence as vanished")
	}
	if err := svc.DeleteEvent(e.ID); err == nil {
		t.Fatal("a vanished but still upcoming event was deleted — it is still on")
	}
	*clock = clock.Add(60 * time.Hour) // it is over now
	if err := svc.DeleteEvent(e.ID); err != nil {
		t.Fatalf("a ghost the feed dropped and time passed must be deletable: %v", err)
	}
}

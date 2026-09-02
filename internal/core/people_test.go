// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/icsimport"
)

func str(s string) *string           { return &s }
func trust(t TrustLevel) *TrustLevel { return &t }
func chans(a ...Address) *[]Address  { return &a }

func TestUpdatePersonPatchSemantics(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	p := mustPerson(t, svc, "ana", TrustPropose)
	e := mustEvent(t, svc, 40*time.Hour)
	if err := svc.Assign(e.ID, p.ID, "host"); err != nil {
		t.Fatal(err)
	}

	// nil fields stay; a name-only patch must not reset trust or wipe
	// the channels.
	got, err := svc.UpdatePerson(p.ID, PersonUpdate{Name: str("  Ana   Lopez ")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Ana Lopez" || got.Trust != TrustPropose || len(got.Channels) != 1 {
		t.Fatalf("name-only patch touched other fields: %+v", got)
	}
	if got.ID != p.ID || got.PortalToken != p.PortalToken {
		t.Fatal("identity changed on update")
	}

	// Channels replace as a whole; the returned copy must not alias state.
	in := []Address{{Kind: "telegram", To: "42"}}
	got, err = svc.UpdatePerson(p.ID, PersonUpdate{Channels: &in})
	if err != nil {
		t.Fatal(err)
	}
	in[0].To = "mutated"
	got.Channels[0].To = "mutated too"
	if stored := svc.People()[0]; len(stored.Channels) != 1 || stored.Channels[0].To != "42" {
		t.Fatalf("channels alias caller memory: %+v", stored.Channels)
	}

	// `[]` clears; assignments survive.
	got, err = svc.UpdatePerson(p.ID, PersonUpdate{Channels: chans()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Channels) != 0 || got.Reachable() {
		t.Fatalf("empty list did not clear: %+v", got)
	}
	if ov := svc.Overview(); len(ov.Events[0].Assignees) != 1 || ov.Events[0].Reachable != 0 {
		t.Fatalf("assignment lost or reachability wrong: %+v", ov.Events[0])
	}

	if _, err := svc.UpdatePerson("pe_nope", PersonUpdate{Name: str("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown person: %v", err)
	}
	if auditCount(t, svc, "person.updated") != 3 {
		t.Fatal("each applied update must audit once")
	}
}

func TestUpdatePersonRejectsBadChannel(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	p := mustPerson(t, svc, "ana", TrustRespond)
	_, err := svc.UpdatePerson(p.ID, PersonUpdate{
		Name:     str("renamed"),
		Channels: chans(Address{Kind: "email", To: "ok@x.local"}, Address{Kind: "email", To: "broken"}),
	})
	if err == nil {
		t.Fatal("bad channel accepted")
	}
	if stored := svc.People()[0]; stored.Name != "ana" || stored.Channels[0].To != "ana@test.local" {
		t.Fatalf("rejected patch was half-applied: %+v", stored)
	}
	for _, bad := range []PersonUpdate{
		{Name: str("   ")},
		{Trust: trust("boss")},
	} {
		if _, err := svc.UpdatePerson(p.ID, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

// Losing the last channel while responsible for something upcoming is
// the scheduler's unreachable case, created by hand — page now.
func TestUpdatePersonToZeroChannelsPagesAdmin(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	p := mustPerson(t, svc, "ana", TrustRespond)
	e := mustEvent(t, svc, 90*time.Hour)
	if err := svc.Assign(e.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdatePerson(p.ID, PersonUpdate{Channels: chans()}); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	pages := adminMessages(fake, "Now unreachable")
	if len(pages) != 1 || !strings.Contains(pages[0].Body, "Tuesday Session") {
		t.Fatalf("want 1 unreachable page naming the event, got %+v", pages)
	}
	if auditCount(t, svc, "person.unreachable") != 1 {
		t.Fatal("person.unreachable not audited")
	}

	// No upcoming duties: no page.
	bob := mustPerson(t, svc, "bob", TrustRespond)
	if _, err := svc.UpdatePerson(bob.ID, PersonUpdate{Channels: chans()}); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	if len(adminMessages(fake, "Now unreachable")) != 1 {
		t.Fatal("paged for a person with nothing upcoming")
	}
}

// An action link is a capability of the assignment: unassigning kills it.
func TestUnassignRevokesLinks(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	p := mustPerson(t, svc, "ana", TrustRespond)
	e := mustEvent(t, svc, 40*time.Hour)
	if err := svc.Assign(e.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmEvent(e.ID, p.ID, "api"); err != nil {
		t.Fatal(err)
	}
	confirmURL, _, _ := svc.GenerateLinks(e.ID, p.ID)
	tok := strings.TrimPrefix(confirmURL, "http://test.local/a/")

	if err := svc.Unassign(e.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyAction(tok, ""); !errors.Is(err, ErrGone) {
		t.Fatalf("link survived the unassign: %v", err)
	}
	if ov := svc.Overview(); len(ov.Events[0].Assignees) != 0 {
		t.Fatalf("assignment still there: %+v", ov.Events[0])
	}
	// Status and the recorded answer stay; nothing fanned out.
	got, _ := svc.EventByID(e.ID)
	if got.Status != StatusConfirmed || len(svc.Responses(e.ID)) != 1 {
		t.Fatalf("unassign rewrote history: %+v responses=%d", got, len(svc.Responses(e.ID)))
	}
	if len(svc.OutboxItems(false)) != 0 {
		t.Fatal("unassign fanned out")
	}
	if err := svc.Unassign(e.ID, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second unassign: %v", err)
	}
	if err := svc.Unassign("ev_nope", p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown event: %v", err)
	}
}

// Series unassign is future-only and sticks: a later sync must not bring
// the person back.
func TestUnassignSeriesSkipsPastAndCancelled(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	p := mustPerson(t, svc, "ana", TrustRespond)
	now := *clock
	until := now.Add(60 * 24 * time.Hour)
	past := occ("s", "Weekly", now.Add(-48*time.Hour), time.Hour)
	soon := occ("s", "Weekly", now.Add(48*time.Hour), time.Hour)
	later := occ("s", "Weekly", now.Add(8*24*time.Hour), time.Hour)
	svc.SyncCalendar([]icsimport.Occurrence{past, soon, later}, nil, until)
	if _, err := svc.AssignSeries("s", p.ID, "host"); err != nil {
		t.Fatal(err)
	}
	var soonID string
	for _, e := range svc.Events() {
		if e.StartsAt.Equal(soon.Start) {
			soonID = e.ID
		}
	}
	if _, err := svc.CancelEvent(soonID, "", "off", "admin"); err != nil {
		t.Fatal(err)
	}

	n, err := svc.UnassignSeries("s", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 future non-cancelled occurrence unassigned, got %d", n)
	}
	for _, oe := range svc.Overview().Events {
		e := oe.Event
		switch {
		case e.StartsAt.Equal(past.Start), e.StartsAt.Equal(soon.Start):
			if len(oe.Assignees) != 1 {
				t.Fatalf("past/cancelled occurrence rewritten: %+v", oe)
			}
		case e.StartsAt.Equal(later.Start):
			if len(oe.Assignees) != 0 {
				t.Fatalf("future occurrence still assigned: %+v", oe)
			}
		}
	}
	// The next sync brings a new occurrence — without ana.
	next := occ("s", "Weekly", now.Add(15*24*time.Hour), time.Hour)
	svc.SyncCalendar([]icsimport.Occurrence{past, soon, later, next}, nil, until)
	for _, oe := range svc.Overview().Events {
		if oe.Event.StartsAt.Equal(next.Start) && len(oe.Assignees) != 0 {
			t.Fatalf("series assignment resurrected on sync: %+v", oe)
		}
	}
	if _, err := svc.UnassignSeries("s", p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second series unassign: %v", err)
	}
}

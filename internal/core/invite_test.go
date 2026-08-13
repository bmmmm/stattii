// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func inviteToken(t *testing.T, svc *Service, eventID string) string {
	t.Helper()
	url, err := svc.CreateInvite(eventID)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(url, "http://test.local/i/")
}

func mustRSVP(t *testing.T, svc *Service, token string, in RSVPInput) Guest {
	t.Helper()
	g, err := svc.RSVP(token, in)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestInviteMintOrReuse(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, 40*time.Hour)

	u1, err := svc.CreateInvite(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := svc.CreateInvite(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u1 != u2 {
		t.Fatalf("second CreateInvite minted a new link: %s vs %s", u1, u2)
	}

	if err := svc.RevokeInvite(e.ID); err != nil {
		t.Fatal(err)
	}
	old := strings.TrimPrefix(u1, "http://test.local/i/")
	if _, err := svc.ResolveInvite(old); !errors.Is(err, ErrGone) {
		t.Fatalf("revoked invite resolves with %v, want ErrGone", err)
	}
	u3, err := svc.CreateInvite(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u3 == u1 {
		t.Fatal("post-revoke CreateInvite reused the revoked token")
	}

	if _, err := svc.CreateInvite("ev_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateInvite on unknown event: %v, want ErrNotFound", err)
	}
}

// TestGuestsGetCancellationFanOut pins invariant 3 for party guests: every
// guest who left an address is an outward recipient of the cancellation —
// including the decliner, whose "no" is not authoritative enough to withhold
// the notice (they may come anyway: the classic locked-door victim).
func TestGuestsGetCancellationFanOut(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	tok := inviteToken(t, svc, e.ID)

	mustRSVP(t, svc, tok, RSVPInput{Name: "Ana", Email: "ana@party.example", Status: GuestYes})
	mustRSVP(t, svc, tok, RSVPInput{Name: "Ben", Email: "ben@party.example", Status: GuestNo})
	mustRSVP(t, svc, tok, RSVPInput{Name: "Caro", Status: GuestYes}) // no address

	if _, err := svc.CancelEvent(e.ID, "", "venue flooded", "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)

	if got := fake.byPurposeTo("ana@party.example"); len(got) != 1 {
		t.Fatalf("ana got %d cancellation notices, want 1", len(got))
	}
	if got := fake.byPurposeTo("ben@party.example"); len(got) != 1 {
		t.Fatalf("the decliner must still be told (got %d notices) — they may come anyway", len(got))
	}

	// No broadcasts, no assignees: propagation is exactly the two reachable
	// guests, both delivered — the addressless guest cannot appear.
	ps, err := svc.Propagation(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Total != 2 || ps.Delivered != 2 || !ps.Complete {
		t.Fatalf("propagation = %+v, want 2/2 delivered", ps)
	}

	// Guest items are attributed via GuestID, never PersonID (the admin
	// timeline joins on PersonID and would mis-attribute).
	for _, o := range svc.OutboxItems(false) {
		if o.To == "ana@party.example" || o.To == "ben@party.example" {
			if o.GuestID == "" || o.PersonID != "" {
				t.Fatalf("guest outbox item badly attributed: %+v", o)
			}
		}
	}
}

func TestMovedAndReinstatedReachGuests(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	tok := inviteToken(t, svc, e.ID)
	mustRSVP(t, svc, tok, RSVPInput{Name: "Ana", Email: "ana@party.example", Status: GuestYes})

	if _, err := svc.MoveEvent(e.ID, e.StartsAt.Add(24*time.Hour), e.EndsAt.Add(24*time.Hour), "", "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelEvent(e.ID, "", "storm", "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReinstateEvent(e.ID, "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)

	got := fake.byPurposeTo("ana@party.example")
	if len(got) != 3 {
		t.Fatalf("want moved+cancelled+reinstated = 3 notices, got %d", len(got))
	}
	for i, want := range []string{"MOVED", "CANCELLED", "REINSTATED"} {
		if !strings.Contains(got[i].Subject, want) {
			t.Fatalf("notice %d subject = %q, want %s", i, got[i].Subject, want)
		}
	}
}

func TestRSVPUpsertAndCounts(t *testing.T) {
	svc, clock := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, 40*time.Hour)
	tok := inviteToken(t, svc, e.ID)

	first := mustRSVP(t, svc, tok, RSVPInput{Name: "Ana", Status: GuestYes})
	*clock = clock.Add(time.Hour)
	second := mustRSVP(t, svc, tok, RSVPInput{Name: "  ana  ", Status: GuestNo})

	if second.ID != first.ID {
		t.Fatal("re-answer with the same name created a second guest")
	}
	if !second.CreatedAt.Equal(first.CreatedAt) || !second.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("timestamps wrong: %+v vs %+v", first, second)
	}

	v, err := svc.ResolveInvite(tok)
	if err != nil {
		t.Fatal(err)
	}
	if v.Yes != 0 || v.No != 1 {
		t.Fatalf("counts = %d/%d, want 0 yes / 1 no", v.Yes, v.No)
	}
	st, err := svc.Invite(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Guests) != 1 {
		t.Fatalf("guest list has %d entries, want 1", len(st.Guests))
	}
}

// TestRSVPKeepsExistingEmailOnBlankResubmit: a blank email on a re-answer
// must never clear a stored address — otherwise anyone with the link could
// silently make a guest unreachable for a cancellation.
func TestRSVPKeepsExistingEmailOnBlankResubmit(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, 40*time.Hour)
	tok := inviteToken(t, svc, e.ID)

	mustRSVP(t, svc, tok, RSVPInput{Name: "Ana", Email: "ana@party.example", Status: GuestYes})
	g := mustRSVP(t, svc, tok, RSVPInput{Name: "Ana", Status: GuestNo})
	if g.Email != "ana@party.example" {
		t.Fatalf("blank resubmit cleared the stored email: %q", g.Email)
	}
}

// TestRSVPCannotHijackStoredEmail: the upsert key is just a name anyone
// with the link can type — a re-answer with a different address must not
// swap the stored one, or the taker inherits the cancellation notice and
// the real guest goes dark behind a green delivery mark.
func TestRSVPCannotHijackStoredEmail(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, 40*time.Hour)
	tok := inviteToken(t, svc, e.ID)

	mustRSVP(t, svc, tok, RSVPInput{Name: "Ana", Email: "ana@party.example", Status: GuestYes})
	g := mustRSVP(t, svc, tok, RSVPInput{Name: "ana", Email: "attacker@evil.example", Status: GuestNo})
	if g.Email != "ana@party.example" {
		t.Fatalf("re-answer swapped the stored email to %q", g.Email)
	}
	if g.Status != GuestNo {
		t.Fatal("the answer itself must still update")
	}
}

// TestGuestFanOutDedupesAddresses: the same address enrolled under two
// names gets one notice, not two — dedup happens at fan-out, silently,
// so rejecting duplicates cannot become an address-existence oracle.
func TestGuestFanOutDedupesAddresses(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	tok := inviteToken(t, svc, e.ID)
	mustRSVP(t, svc, tok, RSVPInput{Name: "Ana", Email: "ana@party.example", Status: GuestYes})
	mustRSVP(t, svc, tok, RSVPInput{Name: "Also Ana", Email: "Ana@Party.example", Status: GuestYes})

	if _, err := svc.CancelEvent(e.ID, "", "storm", "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	if got := fake.byPurposeTo("ana@party.example"); len(got) != 1 {
		t.Fatalf("duplicate address got %d notices, want 1", len(got))
	}
}

func TestRSVPRejectsBadInput(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, 40*time.Hour)
	tok := inviteToken(t, svc, e.ID)

	bad := []RSVPInput{
		{Name: "", Status: GuestYes},
		{Name: "   ", Status: GuestYes},
		{Name: strings.Repeat("x", maxGuestName+1), Status: GuestYes},
		{Name: "Ana", Status: "maybe"},
		{Name: "Ana", Status: GuestYes, Email: "not-an-email"},
		{Name: "Ana", Status: GuestYes, Email: "ana@nodot"},
	}
	for i, in := range bad {
		if _, err := svc.RSVP(tok, in); err == nil {
			t.Fatalf("bad input %d accepted: %+v", i, in)
		}
	}
	st, err := svc.Invite(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Guests) != 0 {
		t.Fatalf("rejected inputs still stored %d guests", len(st.Guests))
	}
}

func TestInviteExpiresWithEvent(t *testing.T) {
	svc, clock := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, 40*time.Hour) // ends at +42h
	tok := inviteToken(t, svc, e.ID)

	*clock = clock.Add(43 * time.Hour)
	if _, err := svc.ResolveInvite(tok); !errors.Is(err, ErrGone) {
		t.Fatalf("expired invite resolves with %v, want ErrGone", err)
	}
	if _, err := svc.RSVP(tok, RSVPInput{Name: "Ana", Status: GuestYes}); !errors.Is(err, ErrGone) {
		t.Fatalf("expired invite accepts RSVP: %v, want ErrGone", err)
	}
}

// TestCancelledPartyStillResolvesButRejectsRSVP: the page must keep working
// after a cancellation — someone opening the link the morning of has to see
// CANCELLED — but no new answers may be recorded for a dead party.
func TestCancelledPartyStillResolvesButRejectsRSVP(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, 40*time.Hour)
	tok := inviteToken(t, svc, e.ID)

	if _, err := svc.CancelEvent(e.ID, "", "storm", "api"); err != nil {
		t.Fatal(err)
	}
	v, err := svc.ResolveInvite(tok)
	if err != nil {
		t.Fatalf("cancelled party page must still resolve, got %v", err)
	}
	if v.Event.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", v.Event.Status)
	}
	if _, err := svc.RSVP(tok, RSVPInput{Name: "Ana", Status: GuestYes}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("RSVP on cancelled party: %v, want ErrCancelled", err)
	}
}

// TestGuestCapAllowsAnswerChange: the cap gates NEW guests only — at a full
// list an existing guest must still be able to flip their answer, or a
// no-show would stay marked as coming.
func TestGuestCapAllowsAnswerChange(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, 40*time.Hour)
	tok := inviteToken(t, svc, e.ID)

	// Seed the bulk directly — 500 RSVPs through the API would fsync the
	// store 500 times for no extra coverage; the boundary itself goes
	// through the real path below.
	for i := 0; i < MaxGuestsPerEvent-1; i++ {
		svc.state.Guests = append(svc.state.Guests, Guest{
			ID: NewID("gu"), EventID: e.ID, Name: fmt.Sprintf("guest %d", i), Status: GuestYes,
		})
	}
	mustRSVP(t, svc, tok, RSVPInput{Name: "last seat", Status: GuestYes}) // reaches the cap
	if _, err := svc.RSVP(tok, RSVPInput{Name: "one too many", Status: GuestYes}); err == nil {
		t.Fatal("guest over the cap accepted")
	}
	g := mustRSVP(t, svc, tok, RSVPInput{Name: "guest 0", Status: GuestNo})
	if g.Status != GuestNo {
		t.Fatal("existing guest could not change their answer at the cap")
	}
}

// TestInviteShowsGuestDeliveryState: the guest list answers "did they get
// the cancellation?" per guest, joined from the outbox via GuestID.
func TestInviteShowsGuestDeliveryState(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	tok := inviteToken(t, svc, e.ID)
	mustRSVP(t, svc, tok, RSVPInput{Name: "Ana", Email: "ana@party.example", Status: GuestYes})
	mustRSVP(t, svc, tok, RSVPInput{Name: "Caro", Status: GuestYes}) // unreachable

	if _, err := svc.CancelEvent(e.ID, "", "storm", "api"); err != nil {
		t.Fatal(err)
	}
	st, _ := svc.Invite(e.ID)
	if st.Guests[0].LastNotice != "cancellation" || st.Guests[0].NoticeState != "pending" {
		t.Fatalf("before tick: %+v", st.Guests[0])
	}
	if st.Guests[1].NoticeState != "" {
		t.Fatalf("addressless guest has a delivery state: %+v", st.Guests[1])
	}

	svc.Tick(*clock)
	st, _ = svc.Invite(e.ID)
	if st.Guests[0].NoticeState != "delivered" {
		t.Fatalf("after tick: %+v", st.Guests[0])
	}
}

// TestGuestsAreNotAssignees: guests must never enter the attestation cycle —
// no reminder ("will it take place?") may ever reach a guest address.
func TestGuestsAreNotAssignees(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour) // inside the 48h reminder lead
	tok := inviteToken(t, svc, e.ID)
	mustRSVP(t, svc, tok, RSVPInput{Name: "Ana", Email: "ana@party.example", Status: GuestYes})

	svc.Tick(*clock)

	if got := fake.byPurposeTo("ana@party.example"); len(got) != 0 {
		t.Fatalf("a guest received a scheduler message: %+v", got)
	}
	if len(svc.state.Assignments) != 0 {
		t.Fatal("RSVP created an assignment")
	}
}

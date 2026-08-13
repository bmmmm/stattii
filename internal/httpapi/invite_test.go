// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

func adminGet(t *testing.T, h http.Handler, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// invitePartyFixture creates a party with an active invite link and returns
// the event ID and the public page path.
func invitePartyFixture(t *testing.T, svc *core.Service) (eventID, path string) {
	t.Helper()
	start := time.Now().Add(40 * time.Hour)
	e, err := svc.CreateEvent(core.EventInput{
		Title: "Garden Party", Location: "Backyard",
		StartsAt: start, EndsAt: start.Add(3 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	inviteURL, err := svc.CreateInvite(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	return e.ID, "/i/" + strings.TrimPrefix(inviteURL, "http://x.local/i/")
}

// TestInvitePageFlow is the TestActionPageFlow shape for the invite surface:
// GET renders the form but must not mutate (scanner safety), POST records.
func TestInvitePageFlow(t *testing.T) {
	svc, pub, _ := newTestServer(t)
	eventID, path := invitePartyFixture(t, svc)

	w := do(t, pub, "GET", path, "", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "You are invited") {
		t.Fatalf("GET invite: %d\n%s", w.Code, w.Body)
	}
	if st, _ := svc.Invite(eventID); len(st.Guests) != 0 {
		t.Fatal("GET created a guest — scanner prefetch would fill the list")
	}

	form := url.Values{"name": {"Ana"}, "email": {"ana@party.example"}, "status": {"yes"}}
	w = doForm(t, pub, path, form, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Thanks, Ana — you are on the list") {
		t.Fatalf("POST rsvp: %d\n%s", w.Code, w.Body)
	}
	st, err := svc.Invite(eventID)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Guests) != 1 || st.Guests[0].Status != core.GuestYes || st.Guests[0].Email != "ana@party.example" {
		t.Fatalf("guest not recorded: %+v", st.Guests)
	}
}

// TestInvitePageHidesGuestNames pins the privacy property of the shared
// page: aggregate counts only, never another guest's name or address — and
// the POST echo must not become an email-enumeration oracle.
func TestInvitePageHidesGuestNames(t *testing.T) {
	svc, pub, _ := newTestServer(t)
	_, path := invitePartyFixture(t, svc)
	token := strings.TrimPrefix(path, "/i/")
	if _, err := svc.RSVP(token, core.RSVPInput{Name: "Ana", Email: "ana@party.example", Status: core.GuestYes}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RSVP(token, core.RSVPInput{Name: "Ben", Status: core.GuestNo}); err != nil {
		t.Fatal(err)
	}

	w := do(t, pub, "GET", path, "", "")
	body := w.Body.String()
	if strings.Contains(body, "Ana") || strings.Contains(body, "Ben") || strings.Contains(body, "ana@party.example") {
		t.Fatalf("public page leaks guest data:\n%s", body)
	}
	if !strings.Contains(body, "1 coming") || !strings.Contains(body, "1 cannot make it") {
		t.Fatalf("public page misses the counts:\n%s", body)
	}

	// Re-answering under an existing name with a blank email must not echo
	// the stored address back.
	w = doForm(t, pub, path, url.Values{"name": {"Ana"}, "status": {"no"}}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("re-answer: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "ana@party.example") {
		t.Fatal("POST echoed a stored email — enumeration oracle")
	}
}

func TestInviteRSVPRateLimited(t *testing.T) {
	svc, pub, _ := newTestServer(t)
	_, path := invitePartyFixture(t, svc)

	for i := 0; i < 10; i++ {
		form := url.Values{"name": {fmt.Sprintf("guest %d", i)}, "status": {"yes"}}
		if w := doForm(t, pub, path, form, nil); w.Code != http.StatusOK {
			t.Fatalf("rsvp %d: got %d", i, w.Code)
		}
	}
	w := doForm(t, pub, path, url.Values{"name": {"flood"}, "status": {"yes"}}, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("11th rsvp from one client: got %d, want 429", w.Code)
	}
}

func TestInviteCancelledShowsNoForm(t *testing.T) {
	svc, pub, _ := newTestServer(t)
	eventID, path := invitePartyFixture(t, svc)
	if _, err := svc.CancelEvent(eventID, "", "rain", "api"); err != nil {
		t.Fatal(err)
	}

	w := do(t, pub, "GET", path, "", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "cancelled") {
		t.Fatalf("cancelled party page: %d\n%s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), `<form method="post" action="/i/`) {
		t.Fatal("cancelled party still offers the RSVP form")
	}
}

// TestAdminInviteAndGuestList walks the owner's actual requirement end to
// end: create the link in the panel, watch an answer arrive, remove a guest.
func TestAdminInviteAndGuestList(t *testing.T) {
	svc, _, admin := newTestServer(t)
	w := doForm(t, admin, "/admin/login", url.Values{"token": {testToken}}, nil)
	cookie := adminCookieFrom(t, w)
	if cookie == nil {
		t.Fatal("login did not set the admin cookie")
	}
	start := time.Now().Add(40 * time.Hour)
	e, err := svc.CreateEvent(core.EventInput{Title: "Garden Party", StartsAt: start, EndsAt: start.Add(3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	if w := doForm(t, admin, "/admin/event/"+e.ID+"/invite", nil, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("create invite: got %d, want 303", w.Code)
	}
	w = adminGet(t, admin, "/admin/event/"+e.ID, cookie)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "http://x.local/i/") {
		t.Fatalf("event page misses the invite link: %d\n%s", w.Code, w.Body)
	}

	st, err := svc.Invite(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(st.URL, "http://x.local/i/")
	if _, err := svc.RSVP(token, core.RSVPInput{Name: "Ana", Email: "ana@party.example", Status: core.GuestYes}); err != nil {
		t.Fatal(err)
	}
	w = adminGet(t, admin, "/admin/event/"+e.ID, cookie)
	if !strings.Contains(w.Body.String(), "Ana") || !strings.Contains(w.Body.String(), "1 coming") {
		t.Fatalf("event page misses the guest:\n%s", w.Body)
	}

	st, _ = svc.Invite(e.ID)
	if w := doForm(t, admin, "/admin/event/"+e.ID+"/guests/"+st.Guests[0].ID+"/remove", nil, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("remove guest: got %d, want 303", w.Code)
	}
	if w = adminGet(t, admin, "/admin/event/"+e.ID, cookie); strings.Contains(w.Body.String(), "Ana") {
		t.Fatal("removed guest still listed")
	}
}

// TestAdminEventPageDoesNotMintInvite pins the admin-side GET-no-mutate
// rule: rendering the event page must never create the invite link.
func TestAdminEventPageDoesNotMintInvite(t *testing.T) {
	svc, _, admin := newTestServer(t)
	w := doForm(t, admin, "/admin/login", url.Values{"token": {testToken}}, nil)
	cookie := adminCookieFrom(t, w)
	start := time.Now().Add(40 * time.Hour)
	e, err := svc.CreateEvent(core.EventInput{Title: "Garden Party", StartsAt: start})
	if err != nil {
		t.Fatal(err)
	}

	adminGet(t, admin, "/admin/event/"+e.ID, cookie)
	adminGet(t, admin, "/admin/event/"+e.ID, cookie)
	if st, _ := svc.Invite(e.ID); st.Active {
		t.Fatal("rendering the event page minted an invite link")
	}
}

// TestInviteBadInputKeepsPage: a validation slip must not strand the guest
// on the generic error page — the party context and the form stay.
func TestInviteBadInputKeepsPage(t *testing.T) {
	svc, pub, _ := newTestServer(t)
	eventID, path := invitePartyFixture(t, svc)

	w := doForm(t, pub, path, url.Values{"name": {"  "}, "status": {"yes"}, "note": {"hi"}}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad input: got %d, want 400", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "your name is required") ||
		!strings.Contains(body, "Garden Party") ||
		!strings.Contains(body, `<form method="post" action="/i/`) {
		t.Fatalf("error response lost the party page:\n%s", body)
	}
	if st, _ := svc.Invite(eventID); len(st.Guests) != 0 {
		t.Fatal("rejected input still stored a guest")
	}
}

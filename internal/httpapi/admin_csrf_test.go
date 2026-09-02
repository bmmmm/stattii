// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

// The session cookie must be worthless outside the panel. Until v0.7.1 it
// WAS the admin bearer token: whoever read the cookie held the whole API.
func TestAdminCookieIsNotTheAPIToken(t *testing.T) {
	_, _, admin := newTestServer(t)
	ui := loginAdmin(t, admin)

	if ui.c.Value == testToken {
		t.Fatal("the session cookie still carries the admin token verbatim")
	}
	// The cookie value as a bearer token buys nothing.
	if w := do(t, admin, "GET", "/api/v1/events", ui.c.Value, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("cookie value worked as an API token: %d", w.Code)
	}
	// And the API token pasted into a cookie is not a session — the old
	// equivalence must not survive as a fallback.
	forged := &http.Cookie{Name: "stattii_admin", Value: testToken}
	req := httptest.NewRequest("GET", "/admin", nil)
	req.AddCookie(forged)
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin token used as a cookie logged in: %d", rec.Code)
	}
	// The real token still opens the API, though.
	if w := do(t, admin, "GET", "/api/v1/events", testToken, ""); w.Code != http.StatusOK {
		t.Fatalf("API token stopped working: %d", w.Code)
	}
}

// Logging out ends the session on the server. A cookie the browser keeps
// (or an attacker copied) must stop working immediately.
func TestAdminLogoutKillsTheSessionServerSide(t *testing.T) {
	_, _, admin := newTestServer(t)
	ui := loginAdmin(t, admin)

	if w := ui.post(t, "/admin/logout", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", w.Code)
	}
	req := httptest.NewRequest("GET", "/admin", nil)
	req.AddCookie(ui.c)
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("the cookie still works after logout: %d", rec.Code)
	}
}

// A form token belongs to one session. Replaying another session's token
// (or a guessed one) is refused.
func TestCSRFTokenIsBoundToItsSession(t *testing.T) {
	svc, _, admin := newTestServer(t)
	a, b := loginAdmin(t, admin), loginAdmin(t, admin)
	if a.csrf == b.csrf {
		t.Fatal("two sessions share one form token")
	}
	start := time.Now().Add(48 * time.Hour).UTC()
	e, err := svc.CreateEvent(core.EventInput{Title: "Session bound", StartsAt: start, EndsAt: start.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	// A's cookie with B's token: refused, nothing mutated.
	w := doForm(t, admin, "/admin/event/"+e.ID+"/cancel",
		url.Values{"reason": {"forged"}, "csrf": {b.csrf}}, a.c)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-session token accepted: %d", w.Code)
	}
	if got, _ := svc.EventByID(e.ID); got.Status == core.StatusCancelled {
		t.Fatal("a foreign form token cancelled an event")
	}
	// A's own token works.
	if w := a.post(t, "/admin/event/"+e.ID+"/cancel", url.Values{"reason": {"real"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("own token refused: %d\n%s", w.Code, w.Body)
	}
}

var postForm = regexp.MustCompile(`(?s)<form method="post" action="([^"]*)"[^>]*>(.*?)</form>`)

// Every mutating form the panel renders must carry the token — a new
// button added without it would 403 in the operator's face. The login
// form is the documented exception: there is no session to bind to yet.
func TestEveryPanelFormCarriesTheCSRFToken(t *testing.T) {
	svc, _, admin := newTestServer(t)
	ui := loginAdmin(t, admin)
	start := time.Now().Add(48 * time.Hour).UTC()
	e, err := svc.CreateEvent(core.EventInput{Title: "Forms", StartsAt: start, EndsAt: start.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.AddPerson("ana", core.TrustRespond, []core.Address{{Kind: "email", To: "ana@x.local"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Assign(e.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	// Conditional forms only render with the state behind them: an
	// invite with a guest (revoke + remove) and an open proposal
	// (accept + reject). Without this the sweep would skip them.
	if _, err := svc.CreateInvite(e.ID); err != nil {
		t.Fatal(err)
	}
	st, err := svc.Invite(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RSVP(strings.TrimPrefix(st.URL, "http://x.local/i/"),
		core.RSVPInput{Name: "Guest", Email: "guest@x.local", Status: core.GuestYes}); err != nil {
		t.Fatal(err)
	}
	_, cancelURL, err := svc.GenerateLinks(e.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProposeMoveViaLink(strings.TrimPrefix(cancelURL, "http://x.local/a/"),
		start.Add(24*time.Hour), start.Add(25*time.Hour), "later?"); err != nil {
		t.Fatal(err)
	}

	checked := 0
	for _, path := range []string{"/admin", "/admin/event/" + e.ID, "/admin/people"} {
		w := adminGet(t, admin, path, ui.c)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, w.Code)
		}
		forms := postForm.FindAllStringSubmatch(w.Body.String(), -1)
		if len(forms) == 0 {
			t.Fatalf("%s rendered no forms at all", path)
		}
		for _, f := range forms {
			action, body := f[1], f[2]
			if action == "/admin/login" {
				continue
			}
			if !strings.Contains(body, `name="csrf" value="`+ui.csrf+`"`) {
				t.Fatalf("%s: form %q has no session token:\n%s", path, action, f[0])
			}
			checked++
		}
	}
	t.Logf("panel forms carrying the session token: %d", checked)
	if checked < 16 {
		t.Fatalf("only %d forms checked — the panel markup moved, fix this test", checked)
	}
}

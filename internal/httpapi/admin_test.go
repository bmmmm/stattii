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
	"github.com/bmmmm/stattii/internal/icsimport"
)

func doForm(t *testing.T, h http.Handler, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func adminCookieFrom(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == "stattii_admin" {
			return c
		}
	}
	return nil
}

var csrfField = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

// adminUI is a logged-in browser: the session cookie plus the form token
// the panel renders. Tests post through it, so a form the panel does not
// hand a token to fails here the same way it would in a browser.
type adminUI struct {
	h    http.Handler
	c    *http.Cookie
	csrf string
}

func loginAdmin(t *testing.T, h http.Handler) adminUI {
	t.Helper()
	login := doForm(t, h, "/admin/login", url.Values{"token": {testToken}}, nil)
	c := adminCookieFrom(t, login)
	if c == nil {
		t.Fatalf("login set no session cookie: %d", login.Code)
	}
	req := httptest.NewRequest("GET", "/admin", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	m := csrfField.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("panel rendered no csrf token: %d\n%s", rec.Code, rec.Body)
	}
	return adminUI{h: h, c: c, csrf: m[1]}
}

// post submits a panel form the way the rendered page would.
func (a adminUI) post(t *testing.T, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", a.csrf)
	return doForm(t, a.h, path, form, a.c)
}

func TestAdminUILoginFlow(t *testing.T) {
	_, _, admin := newTestServer(t)

	// Unauthenticated: login form, 401, no session leaked.
	w := do(t, admin, "GET", "/admin", "", "")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "/admin/login") {
		t.Fatalf("unauthenticated /admin: %d\n%s", w.Code, w.Body)
	}

	// Wrong token: refused, no cookie set.
	w = doForm(t, admin, "/admin/login", url.Values{"token": {"wrong"}}, nil)
	if w.Code != http.StatusUnauthorized || adminCookieFrom(t, w) != nil {
		t.Fatalf("wrong token must not log in: %d, cookie=%v", w.Code, adminCookieFrom(t, w))
	}

	// Right token: redirect + HttpOnly session cookie.
	w = doForm(t, admin, "/admin/login", url.Values{"token": {testToken}}, nil)
	c := adminCookieFrom(t, w)
	if w.Code != http.StatusSeeOther || c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("login: %d, cookie=%+v", w.Code, c)
	}

	// Authenticated overview renders.
	req := httptest.NewRequest("GET", "/admin", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "New event") {
		t.Fatalf("authenticated /admin: %d\n%s", rec.Code, rec.Body)
	}
}

func TestAdminUIActions(t *testing.T) {
	svc, _, admin := newTestServer(t)
	ui := loginAdmin(t, admin)

	// Create an event through the UI form.
	start := time.Now().Add(48 * time.Hour).Format("2006-01-02T15:04")
	w := ui.post(t, "/admin/events", url.Values{
		"title": {"UI Event"}, "starts_at": {start}, "if_unconfirmed": {"notify"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create: %d\n%s", w.Code, w.Body)
	}
	events := svc.Events()
	if len(events) != 1 || events[0].Title != "UI Event" {
		t.Fatalf("event not created: %+v", events)
	}
	id := events[0].ID

	// Cancel it with a reason — the propagation transaction must run.
	w = ui.post(t, "/admin/event/"+id+"/cancel", url.Values{"reason": {"ui test"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("cancel: %d\n%s", w.Code, w.Body)
	}
	if e, _ := svc.EventByID(id); e.Status != core.StatusCancelled || e.CancelReason != "ui test" {
		t.Fatalf("cancel did not apply: %+v", e)
	}

	// Without the cookie the same action is refused.
	w = doForm(t, admin, "/admin/event/"+id+"/reinstate", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated action: got %d, want 401", w.Code)
	}
	if e, _ := svc.EventByID(id); e.Status != core.StatusCancelled {
		t.Fatal("unauthenticated action mutated state")
	}

	// With the cookie but without the form token it is refused too — a
	// cross-site POST carries the cookie, never the rendered field.
	w = doForm(t, admin, "/admin/event/"+id+"/reinstate", nil, ui.c)
	if w.Code != http.StatusForbidden {
		t.Fatalf("CSRF-less action: got %d, want 403", w.Code)
	}
	if e, _ := svc.EventByID(id); e.Status != core.StatusCancelled {
		t.Fatal("a POST without the form token mutated state")
	}
}

func TestAdminLoginThrottledAndAudited(t *testing.T) {
	svc, _, admin := newTestServer(t)
	throttled := false
	for range 8 {
		w := doForm(t, admin, "/admin/login", url.Values{"token": {"wrong"}}, nil)
		if w.Code == http.StatusTooManyRequests {
			throttled = true
			break
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong token: got %d", w.Code)
		}
	}
	if !throttled {
		t.Fatal("8 wrong logins never throttled")
	}
	entries, err := svc.Audit(50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Kind == "admin.login_failed" {
			return
		}
	}
	t.Fatal("no admin.login_failed audit entry")
}

// The people page edit form round-trips, keeps channels it cannot show,
// and the event page's Unassign button removes the track.
func TestAdminPeopleEditAndUnassign(t *testing.T) {
	svc, _, admin := newTestServer(t)
	ui := loginAdmin(t, admin)
	c := ui.c
	p, err := svc.AddPerson("ana", core.TrustRespond, []core.Address{
		{Kind: "email", To: "ana@x.local"}, {Kind: "webhook", To: "https://hooks.x.local/ana"},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/admin/people", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `value="ana@x.local"`) || !strings.Contains(rec.Body.String(), "/admin/people/"+p.ID+"/edit") {
		t.Fatalf("people page lacks the prefilled edit form:\n%s", rec.Body)
	}

	w := ui.post(t, "/admin/people/"+p.ID+"/edit", url.Values{
		"name": {"Ana L."}, "trust": {"propose"}, "email": {"ana@new.local"}, "telegram": {"99"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("edit: %d\n%s", w.Code, w.Body)
	}
	got := svc.People()[0]
	if got.Name != "Ana L." || got.Trust != core.TrustPropose || len(got.Channels) != 3 {
		t.Fatalf("edit did not apply as a patch: %+v", got)
	}
	kinds := map[string]string{}
	for _, ch := range got.Channels {
		kinds[ch.Kind] = ch.To
	}
	if kinds["email"] != "ana@new.local" || kinds["telegram"] != "99" || kinds["webhook"] != "https://hooks.x.local/ana" {
		t.Fatalf("two-field form lost a channel: %+v", got.Channels)
	}
	if got.ID != p.ID || got.PortalToken != p.PortalToken {
		t.Fatal("edit changed identity")
	}

	// Unassign from the event page.
	start := time.Now().Add(72 * time.Hour).UTC()
	e, _ := svc.CreateEvent(core.EventInput{Title: "Track", StartsAt: start, EndsAt: start.Add(time.Hour)})
	svc.Assign(e.ID, p.ID, "")
	req = httptest.NewRequest("GET", "/admin/event/"+e.ID, nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	admin.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "/admin/event/"+e.ID+"/unassign") {
		t.Fatalf("event page lacks the unassign button:\n%s", rec.Body)
	}
	w = ui.post(t, "/admin/event/"+e.ID+"/unassign", url.Values{"person_id": {p.ID}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("unassign: %d\n%s", w.Code, w.Body)
	}
	if ov := svc.Overview(); len(ov.Events[0].Assignees) != 0 {
		t.Fatalf("still assigned: %+v", ov.Events[0])
	}
}

// A vanished occurrence stays on the overview across fetches — it is an
// open decision, not a line in the last report.
func TestAdminOverviewKeepsVanishedAcrossFetches(t *testing.T) {
	svc, _, admin := newTestServer(t)
	ui := loginAdmin(t, admin)
	c := ui.c
	now := time.Now()
	until := now.Add(60 * 24 * time.Hour)
	a := icsimport.Occurrence{Key: "a/1", UID: "a", Summary: "Stays", Start: now.Add(48 * time.Hour), End: now.Add(49 * time.Hour)}
	b := icsimport.Occurrence{Key: "b/1", UID: "b", Summary: "Disappears", Start: now.Add(72 * time.Hour), End: now.Add(73 * time.Hour)}
	svc.SyncCalendar([]icsimport.Occurrence{a, b}, nil, until)
	svc.SyncCalendar([]icsimport.Occurrence{a}, nil, until) // b goes missing
	// The next fetch is suspect (empty feed): its report lists nothing
	// vanished — a panel reading the last report would show b no more.
	svc.SyncCalendar(nil, nil, until)
	req := httptest.NewRequest("GET", "/admin", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Gone from the calendar") || !strings.Contains(body, "Disappears") {
		t.Fatalf("vanished event missing from the overview: %d\n%s", rec.Code, body)
	}
}

// A cancellation that reached nobody must be visible on the event page —
// from the event's own fan-out record, so it stays correct after the
// outbox was pruned.
func TestAdminEventPageShowsNobodyWasTold(t *testing.T) {
	svc, _, admin := newTestServer(t)
	ui := loginAdmin(t, admin)
	c := ui.c
	start := time.Now().Add(72 * time.Hour).UTC()
	e, _ := svc.CreateEvent(core.EventInput{Title: "Silent", StartsAt: start, EndsAt: start.Add(time.Hour)})
	if _, err := svc.CancelEvent(e.ID, "", "", "admin"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/admin/event/"+e.ID, nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Nobody was told") {
		t.Fatalf("event page after an empty cancel: %d\n%s", rec.Code, rec.Body)
	}

	// Control: a told cancellation renders the propagation card instead.
	e2, _ := svc.CreateEvent(core.EventInput{Title: "Told", StartsAt: start, EndsAt: start.Add(time.Hour)})
	p, _ := svc.AddPerson("ana", core.TrustRespond, []core.Address{{Kind: "email", To: "ana@x.local"}})
	svc.Assign(e2.ID, p.ID, "")
	if _, err := svc.CancelEvent(e2.ID, "", "", "admin"); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/admin/event/"+e2.ID, nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	admin.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "Nobody was told") || !strings.Contains(rec.Body.String(), "Propagation") {
		t.Fatalf("told cancellation mis-rendered:\n%s", rec.Body)
	}

	// The guard rail: 120 days later the outbox has been pruned and the
	// propagation card is gone — the told cancellation must NOT turn red.
	// A card keyed on Propagation.Total would.
	svc.Tick(time.Now()) // deliver
	later := time.Now().Add(120 * 24 * time.Hour)
	svc.SetClock(func() time.Time { return later })
	svc.Tick(later) // prune
	if ps, _ := svc.Propagation(e2.ID); ps.Total != 0 {
		t.Fatalf("setup: expected the outbox pruned, got %+v", ps)
	}
	req = httptest.NewRequest("GET", "/admin/event/"+e2.ID, nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	admin.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "Nobody was told") {
		t.Fatalf("pruned outbox turned a told cancellation red:\n%s", rec.Body)
	}
}

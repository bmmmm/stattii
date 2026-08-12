// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/core"
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
	login := doForm(t, admin, "/admin/login", url.Values{"token": {testToken}}, nil)
	c := adminCookieFrom(t, login)

	// Create an event through the UI form.
	start := time.Now().Add(48 * time.Hour).Format("2006-01-02T15:04")
	w := doForm(t, admin, "/admin/events", url.Values{
		"title": {"UI Event"}, "starts_at": {start}, "if_unconfirmed": {"notify"},
	}, c)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create: %d\n%s", w.Code, w.Body)
	}
	events := svc.Events()
	if len(events) != 1 || events[0].Title != "UI Event" {
		t.Fatalf("event not created: %+v", events)
	}
	id := events[0].ID

	// Cancel it with a reason — the propagation transaction must run.
	w = doForm(t, admin, "/admin/event/"+id+"/cancel", url.Values{"reason": {"ui test"}}, c)
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

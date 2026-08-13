// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/core"
	"github.com/bmmmm/stattii/internal/httpapi"
)

type nullNotifier struct{}

func (nullNotifier) Send(kind, to string, m core.Message) error { return nil }

const testToken = "test-admin-token"

// newTestServer returns the service plus BOTH handlers — public and
// admin are separate muxes on purpose (separate listeners in serve).
func newTestServer(t *testing.T) (*core.Service, http.Handler, http.Handler) {
	t.Helper()
	store, err := core.NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := core.NewService(store, core.Config{BaseURL: "http://x.local"}, nullNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	s := httpapi.New(svc, testToken, "test", nil)
	return svc, s.PublicHandler(), s.AdminHandler()
}

func do(t *testing.T, h http.Handler, method, path, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAdminAuthRequired(t *testing.T) {
	_, _, h := newTestServer(t)
	if w := do(t, h, "GET", "/api/v1/events", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", w.Code)
	}
	if w := do(t, h, "GET", "/api/v1/events", "wrong", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", w.Code)
	}
	if w := do(t, h, "GET", "/api/v1/events", testToken, ""); w.Code != http.StatusOK {
		t.Fatalf("right token: got %d, want 200", w.Code)
	}
}

func TestCreateEventViaAPI(t *testing.T) {
	_, _, h := newTestServer(t)
	start := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	w := do(t, h, "POST", "/api/v1/events", testToken,
		`{"title":"API Event","starts_at":"`+start+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
	var e core.Event
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.ID == "" || e.Status != core.StatusScheduled {
		t.Fatalf("unexpected event %+v", e)
	}
}

func TestActionPageFlow(t *testing.T) {
	svc, h, _ := newTestServer(t)
	start := time.Now().Add(72 * time.Hour).UTC()
	e, _ := svc.CreateEvent(core.EventInput{Title: "Click Test", StartsAt: start, EndsAt: start.Add(time.Hour)})
	p, _ := svc.AddPerson("ana", core.TrustRespond, nil)
	svc.Assign(e.ID, p.ID, "")
	confirmURL, _, err := svc.GenerateLinks(e.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(confirmURL)

	// GET renders the button but must not mutate (scanner safety).
	w := do(t, h, "GET", u.Path, "", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Yes, this event takes place") {
		t.Fatalf("GET action page: %d\n%s", w.Code, w.Body)
	}
	if got, _ := svc.EventByID(e.ID); got.Status != core.StatusScheduled {
		t.Fatal("GET mutated the event — mail scanners would confirm events")
	}

	// POST applies.
	if w := do(t, h, "POST", u.Path, "", ""); w.Code != http.StatusOK {
		t.Fatalf("POST action: %d", w.Code)
	}
	if got, _ := svc.EventByID(e.ID); got.Status != core.StatusConfirmed {
		t.Fatal("POST did not confirm")
	}
}

func TestActionProposeFlow(t *testing.T) {
	svc, h, _ := newTestServer(t)
	start := time.Now().Add(72 * time.Hour).UTC()
	e, _ := svc.CreateEvent(core.EventInput{Title: "Move Me", StartsAt: start, EndsAt: start.Add(time.Hour)})
	p, _ := svc.AddPerson("ana", core.TrustRespond, nil)
	svc.Assign(e.ID, p.ID, "")
	_, cancelURL, err := svc.GenerateLinks(e.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(cancelURL)

	form := url.Values{"starts_at": {"2026-09-01T19:00"}, "note": {"room clash"}}
	req := httptest.NewRequest("POST", u.Path+"/propose", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "proposal was sent") {
		t.Fatalf("propose: %d\n%s", w.Code, w.Body)
	}
	props := svc.Proposals()
	if len(props) != 1 || props[0].Kind != "move" || props[0].PersonID != p.ID {
		t.Fatalf("proposal not filed: %+v", props)
	}
	// The event itself is untouched.
	if got, _ := svc.EventByID(e.ID); !got.StartsAt.Equal(start) {
		t.Fatal("propose must not move the event")
	}
}

func TestUnknownTokenIs404(t *testing.T) {
	_, h, _ := newTestServer(t)
	if w := do(t, h, "GET", "/a/deadbeefdeadbeefdeadbeefdeadbeef", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestFeedServed(t *testing.T) {
	svc, h, _ := newTestServer(t)
	start := time.Now().Add(24 * time.Hour).UTC()
	svc.CreateEvent(core.EventInput{Title: "Feed Event", StartsAt: start, EndsAt: start.Add(time.Hour)})
	w := do(t, h, "GET", "/feed.ics", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/calendar") {
		t.Fatalf("content-type %q", ct)
	}
	if !strings.Contains(w.Body.String(), "SUMMARY:Feed Event") {
		t.Fatal("event missing from feed")
	}
}

func TestPublicRateLimit(t *testing.T) {
	_, h, _ := newTestServer(t)
	limited := false
	for range 40 {
		if w := do(t, h, "GET", "/a/nosuchtoken", "", ""); w.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("40 rapid requests never hit the rate limit")
	}
}

// The admin API must not exist on the public mux — not even with a valid
// token. This is the listener-split guarantee deployers rely on: the
// public listener cannot leak management routes, whatever the proxy does.
func TestAdminAPIAbsentFromPublic(t *testing.T) {
	_, pub, _ := newTestServer(t)
	for _, probe := range []struct{ method, path string }{
		{"GET", "/api/v1/events"},
		{"POST", "/api/v1/events"},
		{"GET", "/api/v1/overview"},
		{"GET", "/api/v1/audit"},
		{"POST", "/api/v1/tick"},
		// The guest list is exactly the payload that must never be
		// reachable on the public mux — and route registration is
		// per-pattern, so every mutating member gets its own probe.
		{"GET", "/api/v1/events/ev_x/guests"},
		{"POST", "/api/v1/events/ev_x/invite"},
		{"DELETE", "/api/v1/events/ev_x/invite"},
		{"DELETE", "/api/v1/events/ev_x/guests/gu_x"},
		{"POST", "/api/v1/people/pe_x/rotate-portal"},
		{"DELETE", "/api/v1/people/pe_x/links"},
		{"DELETE", "/api/v1/events/ev_x/links"},
	} {
		if w := do(t, pub, probe.method, probe.path, testToken, ""); w.Code != http.StatusNotFound {
			t.Fatalf("%s %s on public mux: got %d, want 404", probe.method, probe.path, w.Code)
		}
	}
}

// The feed walks the whole event store per request — it must sit behind
// the same limiter as every other public route.
func TestFeedRateLimited(t *testing.T) {
	_, h, _ := newTestServer(t)
	limited := false
	for range 40 {
		if w := do(t, h, "GET", "/feed.ics", "", ""); w.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("40 rapid feed requests never hit the rate limit")
	}
}

func TestPublicSecurityHeaders(t *testing.T) {
	_, h, _ := newTestServer(t)
	w := do(t, h, "GET", "/a/nosuchtoken", "", "")
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (tokens ride in the URL)", got)
	}
	if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
}

// A portal token — even at direct trust — must only act on events its
// holder is assigned to. Anything else is a capability escalation.
func TestPortalSubmitForeignEventRejected(t *testing.T) {
	svc, h, _ := newTestServer(t)
	start := time.Now().Add(72 * time.Hour).UTC()
	e, _ := svc.CreateEvent(core.EventInput{Title: "Not Yours", StartsAt: start, EndsAt: start.Add(time.Hour)})
	ana, _ := svc.AddPerson("ana", core.TrustRespond, nil)
	svc.Assign(e.ID, ana.ID, "")
	mallory, _ := svc.AddPerson("mallory", core.TrustDirect, nil)
	portal, err := svc.PersonPortalURL(mallory.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(portal)

	form := url.Values{"kind": {"cancel"}, "event_id": {e.ID}, "note": {"gone"}}
	req := httptest.NewRequest("POST", u.Path+"/submit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign event_id: got %d, want 404", w.Code)
	}
	if got, _ := svc.EventByID(e.ID); got.Status != core.StatusScheduled {
		t.Fatalf("foreign portal submit cancelled the event: %+v", got)
	}

	// Control: once assigned, the same submit applies.
	svc.Assign(e.ID, mallory.ID, "")
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", u.Path+"/submit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("assigned submit: got %d, want 303", w.Code)
	}
	if got, _ := svc.EventByID(e.ID); got.Status != core.StatusCancelled {
		t.Fatalf("assigned submit did not apply: %+v", got)
	}
}

// The HMAC secret is shown once, on registration — the list must not
// keep echoing it.
func TestWebhookSecretRedacted(t *testing.T) {
	svc, _, h := newTestServer(t)
	wh, err := svc.AddWebhook("https://example.org/hook", nil)
	if err != nil {
		t.Fatal(err)
	}
	if wh.Secret == "" {
		t.Fatal("registration must return the secret")
	}
	w := do(t, h, "GET", "/api/v1/webhooks", testToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list webhooks: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), wh.Secret) {
		t.Fatal("webhook list echoes the HMAC secret")
	}
}

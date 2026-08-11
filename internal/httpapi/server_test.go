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

func newTestServer(t *testing.T) (*core.Service, http.Handler) {
	t.Helper()
	store, err := core.NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := core.NewService(store, core.Config{BaseURL: "http://x.local"}, nullNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	return svc, httpapi.New(svc, testToken, "test").Handler()
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
	_, h := newTestServer(t)
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
	_, h := newTestServer(t)
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
	svc, h := newTestServer(t)
	start := time.Now().Add(72 * time.Hour).UTC()
	e, _ := svc.CreateEvent("Click Test", "", "", start, start.Add(time.Hour))
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

func TestUnknownTokenIs404(t *testing.T) {
	_, h := newTestServer(t)
	if w := do(t, h, "GET", "/a/deadbeefdeadbeefdeadbeefdeadbeef", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestFeedServed(t *testing.T) {
	svc, h := newTestServer(t)
	start := time.Now().Add(24 * time.Hour).UTC()
	svc.CreateEvent("Feed Event", "", "", start, start.Add(time.Hour))
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
	_, h := newTestServer(t)
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

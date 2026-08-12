// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

func decode(t *testing.T, body string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}

// TestAdminAPIContract walks EVERY /api/v1 route once over real HTTP:
// method+pattern routing, request field names, status codes and the
// error mapping. The business logic behind each handler is covered in
// internal/core — this pins the wiring.
func TestAdminAPIContract(t *testing.T) {
	svc, _, h := newTestServer(t)
	step := func(want int, method, path, body string) *json.Decoder {
		t.Helper()
		w := do(t, h, method, path, testToken, body)
		if w.Code != want {
			t.Fatalf("%s %s: got %d want %d\n%s", method, path, w.Code, want, w.Body)
		}
		return json.NewDecoder(strings.NewReader(w.Body.String()))
	}

	// people
	var created struct {
		Person    core.Person `json:"person"`
		PortalURL string      `json:"portal_url"`
	}
	w := do(t, h, "POST", "/api/v1/people", testToken,
		`{"name":"ana","trust":"respond","channels":[{"kind":"email","to":"ana@x.local"}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create person: %d %s", w.Code, w.Body)
	}
	decode(t, w.Body.String(), &created)
	if created.Person.ID == "" || !strings.Contains(created.PortalURL, "/p/") {
		t.Fatalf("person payload: %+v", created)
	}
	step(200, "GET", "/api/v1/people", "")

	// events: create → list → get → 404 for unknown
	start := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	var ev core.Event
	w = do(t, h, "POST", "/api/v1/events", testToken, `{"title":"Contract","starts_at":"`+start+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create event: %d %s", w.Code, w.Body)
	}
	decode(t, w.Body.String(), &ev)
	step(200, "GET", "/api/v1/events", "")
	step(200, "GET", "/api/v1/events/"+ev.ID, "")
	step(404, "GET", "/api/v1/events/ev_nope", "")
	step(400, "POST", "/api/v1/events", `{"title":`)

	// assignment + links + responses
	step(200, "POST", "/api/v1/assignments", `{"event_id":"`+ev.ID+`","person_id":"`+created.Person.ID+`","role":"host"}`)
	var links map[string]string
	w = do(t, h, "POST", "/api/v1/events/"+ev.ID+"/links", testToken, `{"person_id":"`+created.Person.ID+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("links: %d %s", w.Code, w.Body)
	}
	decode(t, w.Body.String(), &links)
	if !strings.Contains(links["confirm_url"], "/a/") || !strings.Contains(links["cancel_url"], "/a/") {
		t.Fatalf("links payload: %v", links)
	}
	step(200, "GET", "/api/v1/events/"+ev.ID+"/responses", "")

	// lifecycle over HTTP: confirm → move (resets cycle) → cancel →
	// confirm on cancelled must map to 409 → propagation → reinstate
	step(200, "POST", "/api/v1/events/"+ev.ID+"/confirm", "")
	newStart := time.Now().Add(96 * time.Hour).UTC().Format(time.RFC3339)
	step(200, "POST", "/api/v1/events/"+ev.ID+"/move", `{"starts_at":"`+newStart+`"}`)
	step(200, "POST", "/api/v1/events/"+ev.ID+"/cancel", `{"reason":"contract"}`)
	step(409, "POST", "/api/v1/events/"+ev.ID+"/confirm", "")
	var prop core.PropagationStatus
	w = do(t, h, "GET", "/api/v1/events/"+ev.ID+"/propagation", testToken, "")
	decode(t, w.Body.String(), &prop)
	if prop.Total == 0 {
		t.Fatalf("cancel produced no propagation items: %+v", prop)
	}
	step(200, "POST", "/api/v1/events/"+ev.ID+"/reinstate", "")

	// outbox: pending items exist (no tick ran), retry one, then tick
	var items []core.OutboxItem
	w = do(t, h, "GET", "/api/v1/outbox?pending=1", testToken, "")
	decode(t, w.Body.String(), &items)
	if len(items) == 0 {
		t.Fatal("expected pending outbox items after cancel/reinstate")
	}
	step(200, "POST", "/api/v1/outbox/"+items[0].ID+"/retry", "")
	step(404, "POST", "/api/v1/outbox/ob_nope/retry", "")
	step(200, "GET", "/api/v1/outbox", "")
	step(200, "POST", "/api/v1/tick", "")

	// broadcasts: create → list → delete → delete again is 404
	var bc core.Broadcast
	w = do(t, h, "POST", "/api/v1/broadcasts", testToken, `{"name":"list","kind":"email","to":"all@x.local"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create broadcast: %d %s", w.Code, w.Body)
	}
	decode(t, w.Body.String(), &bc)
	step(200, "GET", "/api/v1/broadcasts", "")
	step(200, "DELETE", "/api/v1/broadcasts/"+bc.ID, "")
	step(404, "DELETE", "/api/v1/broadcasts/"+bc.ID, "")

	// webhooks: create (secret returned once) → list → delete
	var wh core.Webhook
	w = do(t, h, "POST", "/api/v1/webhooks", testToken, `{"url":"http://hooks.local/x","events":["event.cancelled"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create webhook: %d %s", w.Code, w.Body)
	}
	decode(t, w.Body.String(), &wh)
	if wh.Secret == "" {
		t.Fatalf("webhook secret not returned on registration: %+v", wh)
	}
	step(200, "GET", "/api/v1/webhooks", "")
	step(200, "DELETE", "/api/v1/webhooks/"+wh.ID, "")

	// proposals: file one via an action link, list it, reject it
	_, cancelURL, err := svc.GenerateLinks(ev.ID, created.Person.ID)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(cancelURL, "http://x.local/a/")
	if _, err := svc.ProposeMoveViaLink(token, time.Now().Add(120*time.Hour), time.Time{}, "later?"); err != nil {
		t.Fatal(err)
	}
	var props []core.Proposal
	w = do(t, h, "GET", "/api/v1/proposals", testToken, "")
	decode(t, w.Body.String(), &props)
	if len(props) != 1 {
		t.Fatalf("want 1 proposal, got %+v", props)
	}
	step(200, "POST", "/api/v1/proposals/"+props[0].ID+"/decide", `{"accept":false}`)
	step(404, "POST", "/api/v1/proposals/pr_nope/decide", `{"accept":true}`)

	// audit + overview
	step(200, "GET", "/api/v1/audit?limit=5", "")
	var ov core.Overview
	w = do(t, h, "GET", "/api/v1/overview", testToken, "")
	decode(t, w.Body.String(), &ov)
	if ov.People != 1 || len(ov.Events) != 1 {
		t.Fatalf("overview counts: %+v", ov)
	}
}

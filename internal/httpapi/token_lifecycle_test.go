// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi_test

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

// TestAdminRotatePortalButton drives the panel flow of issue #4: rotate a
// person's portal link, old capability dies, page shows the new one.
func TestAdminRotatePortalButton(t *testing.T) {
	svc, _, admin := newTestServer(t)
	w := doForm(t, admin, "/admin/login", url.Values{"token": {testToken}}, nil)
	cookie := adminCookieFrom(t, w)
	if cookie == nil {
		t.Fatal("login did not set the admin cookie")
	}
	p, err := svc.AddPerson("ana", core.TrustRespond, []core.Address{{Kind: "email", To: "ana@x.local"}})
	if err != nil {
		t.Fatal(err)
	}
	oldURL, err := svc.PersonPortalURL(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldTok := strings.TrimPrefix(oldURL, "http://x.local/p/")

	if w := doForm(t, admin, "/admin/people/"+p.ID+"/rotate-portal", nil, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("rotate: got %d, want 303", w.Code)
	}
	if _, err := svc.Portal(oldTok); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("old portal token still resolves: %v", err)
	}
	newURL, _ := svc.PersonPortalURL(p.ID)
	w = adminGet(t, admin, "/admin/people", cookie)
	if !strings.Contains(w.Body.String(), newURL) || strings.Contains(w.Body.String(), oldURL) {
		t.Fatalf("people page does not show the rotated URL")
	}
}

// TestAdminRevokeLinksButton: the per-assignee revoke on the event page
// kills that person's action links.
func TestAdminRevokeLinksButton(t *testing.T) {
	svc, _, admin := newTestServer(t)
	w := doForm(t, admin, "/admin/login", url.Values{"token": {testToken}}, nil)
	cookie := adminCookieFrom(t, w)
	start := time.Now().Add(40 * time.Hour)
	e, err := svc.CreateEvent(core.EventInput{Title: "Session", StartsAt: start, EndsAt: start.Add(2 * time.Hour)})
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
	confirmURL, _, err := svc.GenerateLinks(e.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	form := url.Values{"person_id": {p.ID}}
	if w := doForm(t, admin, "/admin/event/"+e.ID+"/links/revoke", form, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("revoke: got %d, want 303", w.Code)
	}
	tok := strings.TrimPrefix(confirmURL, "http://x.local/a/")
	if _, err := svc.ResolveAction(tok); !errors.Is(err, core.ErrGone) {
		t.Fatalf("revoked link still resolves: %v", err)
	}
}

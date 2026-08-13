// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Issue #4: invariant 5 promises revocable tokens — these tests make the
// promise executable for action links and portal tokens.

func actionToken(u string) string { return strings.TrimPrefix(u, "http://test.local/a/") }

func TestRevokeLinksPairAndRegenerate(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, 40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(e.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	confirmURL, cancelURL, err := svc.GenerateLinks(e.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	n, err := svc.RevokeLinks(e.ID, p.ID)
	if err != nil || n != 2 {
		t.Fatalf("revoked %d links (err %v), want 2", n, err)
	}
	for _, u := range []string{confirmURL, cancelURL} {
		if _, err := svc.ApplyAction(actionToken(u)); !errors.Is(err, ErrGone) {
			t.Fatalf("revoked link still applies: %v", err)
		}
	}

	// Regenerating mints fresh tokens; the new ones work.
	newConfirm, newCancel, err := svc.GenerateLinks(e.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newConfirm == confirmURL || newCancel == cancelURL {
		t.Fatal("regeneration reused a revoked token")
	}
	v, err := svc.ApplyAction(actionToken(newConfirm))
	if err != nil || v.Event.Status != StatusConfirmed {
		t.Fatalf("fresh link does not work: %v %+v", err, v)
	}
}

func TestRevokeLinksScopes(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	e1 := mustEvent(t, svc, 40*time.Hour)
	e2 := mustEvent(t, svc, 44*time.Hour)
	pa := mustPerson(t, svc, "ana", TrustRespond)
	pb := mustPerson(t, svc, "ben", TrustRespond)
	for _, pair := range []struct{ e, p string }{{e1.ID, pa.ID}, {e2.ID, pa.ID}, {e1.ID, pb.ID}} {
		if _, _, err := svc.GenerateLinks(pair.e, pair.p); err != nil {
			t.Fatal(err)
		}
	}

	// Person scope: ana loses her links across BOTH events, ben keeps his.
	n, err := svc.RevokeLinks("", pa.ID)
	if err != nil || n != 4 {
		t.Fatalf("person-scope revoked %d (err %v), want 4", n, err)
	}
	benConfirm, _, err := svc.GenerateLinks(e1.ID, pb.ID) // mint-or-reuse: still the live pair
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveAction(actionToken(benConfirm)); err != nil {
		t.Fatalf("person-scope revoke hit another person's link: %v", err)
	}

	// Event scope: now ben's e1 links die too.
	if n, err = svc.RevokeLinks(e1.ID, ""); err != nil || n != 2 {
		t.Fatalf("event-scope revoked %d (err %v), want 2", n, err)
	}
	if _, err := svc.ResolveAction(actionToken(benConfirm)); !errors.Is(err, ErrGone) {
		t.Fatalf("event-scope revoke missed a link: %v", err)
	}

	// Guard rails: a blank form must not revoke the world.
	if _, err := svc.RevokeLinks("", ""); err == nil {
		t.Fatal("revoke-everything must be rejected")
	}
	if _, err := svc.RevokeLinks("ev_missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown event: %v, want ErrNotFound", err)
	}
}

func TestRotatePortal(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	p := mustPerson(t, svc, "ana", TrustRespond)
	oldURL, err := svc.PersonPortalURL(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldTok := strings.TrimPrefix(oldURL, "http://test.local/p/")
	if _, err := svc.Portal(oldTok); err != nil {
		t.Fatal(err)
	}

	newURL, err := svc.RotatePortal(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newURL == oldURL {
		t.Fatal("rotation returned the same URL")
	}
	if _, err := svc.Portal(oldTok); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old portal token still resolves: %v", err)
	}
	if _, err := svc.Portal(strings.TrimPrefix(newURL, "http://test.local/p/")); err != nil {
		t.Fatalf("new portal token does not resolve: %v", err)
	}
	if _, err := svc.RotatePortal("pe_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown person: %v, want ErrNotFound", err)
	}
}

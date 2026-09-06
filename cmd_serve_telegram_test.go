// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

type noopNotifier struct{}

func (noopNotifier) Send(kind, to string, m core.Message) error { return nil }

// TestTelegramApplyVerifiesActorBeforeApplying covers a review finding
// (P2): the join between VerifyTelegramActor and ApplyAction lives in a
// closure inlined in cmdServe, which cannot be exercised without a live
// server — a prior version of that closure could have its
// VerifyTelegramActor call deleted without any test turning red.
// telegramApply is that same closure pulled out as its own function so
// this test can call it directly.
func TestTelegramApplyVerifiesActorBeforeApplying(t *testing.T) {
	store, err := core.NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := core.NewService(store, core.Config{BaseURL: "http://test.local"}, noopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now })

	e, err := svc.CreateEvent(core.EventInput{Title: "Standup", StartsAt: now.Add(40 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.AddPerson("ana", core.TrustRespond, []core.Address{{Kind: "telegram", To: "111"}})
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
	token := strings.TrimPrefix(confirmURL, "http://test.local/a/")

	apply := telegramApply(svc)

	if _, err := apply(token, "222"); err == nil {
		t.Fatal("a press from a chat id that is not the assignee's own must be refused, not applied")
	}
	got, err := svc.EventByID(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != core.StatusScheduled {
		t.Fatalf("event status = %s, want still scheduled (unverified press must not confirm)", got.Status)
	}

	text, err := apply(token, "111")
	if err != nil {
		t.Fatalf("the assignee's own chat id must be accepted: %v", err)
	}
	if !strings.Contains(text, "takes place") {
		t.Fatalf("unexpected confirm text: %q", text)
	}
	got, err = svc.EventByID(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != core.StatusConfirmed {
		t.Fatalf("event status = %s, want confirmed", got.Status)
	}
}

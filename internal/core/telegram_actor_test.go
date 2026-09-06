// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestVerifyTelegramActorRejectsMismatch covers #6: a Telegram inline
// button carries an action-link token, but the poller used to apply it for
// whoever pressed the button — if the reminder ever landed in a group
// chat, any member could answer as the assignee. VerifyTelegramActor must
// refuse a from.id that isn't the assignee's own telegram chat id, audit
// the attempt, and leave the event untouched; it must accept the real
// assignee's id.
func TestVerifyTelegramActorRejectsMismatch(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p, err := svc.AddPerson("ana", TrustRespond, []Address{{Kind: "telegram", To: "111"}})
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

	// A different chat pressed the button: reject, no mutation.
	if err := svc.VerifyTelegramActor(token, "222"); !errors.Is(err, ErrWrongActor) {
		t.Fatalf("VerifyTelegramActor(wrong id) = %v, want ErrWrongActor", err)
	}
	if auditCount(t, svc, "telegram.actor_mismatch") != 1 {
		t.Fatal("telegram.actor_mismatch not audited")
	}
	if v, err := svc.ResolveAction(token); err != nil || v.Decided != nil {
		t.Fatalf("mismatch must not decide anything: view=%+v err=%v", v, err)
	}

	// The real assignee's chat id is accepted.
	if err := svc.VerifyTelegramActor(token, "111"); err != nil {
		t.Fatalf("VerifyTelegramActor(right id) = %v, want nil", err)
	}
	// Accepting a check still must not itself apply the action.
	if v, err := svc.ResolveAction(token); err != nil || v.Decided != nil {
		t.Fatalf("verify alone must not decide anything: view=%+v err=%v", v, err)
	}
	// A second mismatch must not have been double-counted by the accept path.
	if auditCount(t, svc, "telegram.actor_mismatch") != 1 {
		t.Fatal("telegram.actor_mismatch audited on a matching actor")
	}
}

// TestVerifyTelegramActorAcceptsGroupChatUnverified covers a review
// finding (P1): a person's telegram channel can be configured as a
// group/supergroup chat (Telegram gives those a negative id) — the exact
// configuration #6 itself names as the threat. But the group's own chat id
// can never equal any individual member's user id, so the strict check
// would refuse every press there, including the real assignee's own,
// turning a working (if less strict) config into permanent silent
// refusals and, on an if_unconfirmed=cancel event, an auto-cancel nobody
// caused. A negative channel must therefore be applied unverified rather
// than rejected — but the audit trail must say so plainly.
func TestVerifyTelegramActorAcceptsGroupChatUnverified(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p, err := svc.AddPerson("ops-team", TrustRespond, []Address{{Kind: "telegram", To: "-1009876543210"}})
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

	// A group member's own user id never equals the group's chat id, yet
	// the press must be accepted, not refused.
	if err := svc.VerifyTelegramActor(token, "555111222"); err != nil {
		t.Fatalf("VerifyTelegramActor(group chat member) = %v, want nil (unverified, not refused)", err)
	}
	if auditCount(t, svc, "telegram.actor_mismatch") != 0 {
		t.Fatal("a group-chat press must never be audited as a mismatch")
	}
	if auditCount(t, svc, "telegram.actor_unverified") != 1 {
		t.Fatal("a group-chat press must be audited as unverified")
	}
	entries, err := svc.Audit(500)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, en := range entries {
		if en.Kind != "telegram.actor_unverified" {
			continue
		}
		found = true
		if !strings.Contains(string(en.Data), `"reason":"unverified: not a private chat id"`) {
			t.Fatalf("audit entry missing the reason: %s", en.Data)
		}
	}
	if !found {
		t.Fatal("telegram.actor_unverified entry not found in the audit trail")
	}
}

// TestVerifyTelegramActorAcceptsUsernameChatUnverified covers a review
// finding (P1, round 2): the first fix only caught a negative (group) chat
// id — a channel address given as a Telegram chat username (e.g.
// "@opsroom", also a valid Bot API chat_id, and not numeric at all) still
// took the strict path and was refused, including for the real assignee.
// Any address that is not a plain positive integer must be unverifiable,
// not just a negative one.
func TestVerifyTelegramActorAcceptsUsernameChatUnverified(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p, err := svc.AddPerson("ops-team", TrustRespond, []Address{{Kind: "telegram", To: "@opsroom"}})
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

	if err := svc.VerifyTelegramActor(token, "555111222"); err != nil {
		t.Fatalf("VerifyTelegramActor(@opsroom member) = %v, want nil (unverified, not refused)", err)
	}
	if auditCount(t, svc, "telegram.actor_mismatch") != 0 {
		t.Fatal("a @opsroom press must never be audited as a mismatch")
	}
	if auditCount(t, svc, "telegram.actor_unverified") != 1 {
		t.Fatal("a @opsroom press must be audited as unverified")
	}
}

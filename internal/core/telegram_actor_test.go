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

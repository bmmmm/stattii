// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Telegram onboarding (#13): a one-shot deep link binds the private chat
// that sends "/start <token>" as the person's telegram channel.

func onboardToken(t *testing.T, v TelegramOnboardView) string {
	t.Helper()
	tok, ok := strings.CutPrefix(v.Start, "/start ")
	if !ok || tok == "" {
		t.Fatalf("no /start payload in %+v", v)
	}
	return tok
}

func telegramChannels(t *testing.T, svc *Service, id string) []string {
	t.Helper()
	var out []string
	for _, ch := range findPerson(t, svc, id).Channels {
		if ch.Kind == "telegram" {
			out = append(out, ch.To)
		}
	}
	return out
}

func TestTelegramOnboardingBindsThePrivateChatOnce(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	p := mustPerson(t, svc, "ana", TrustRespond)
	svc.SetTelegramBot("stattii_bot")
	v, err := svc.CreateTelegramOnboarding(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	tok := onboardToken(t, v)
	if v.URL != "https://t.me/stattii_bot?start="+tok {
		t.Fatalf("deep link is not built on the bot's name: %q", v.URL)
	}

	if err := svc.BindTelegram(tok, "4242", "4242"); err != nil {
		t.Fatalf("a genuine /start was refused: %v", err)
	}
	if got := telegramChannels(t, svc, p.ID); len(got) != 1 || got[0] != "4242" {
		t.Fatalf("want the chat bound as the telegram channel, got %v", got)
	}
	log := auditDump(t, svc)
	if !strings.Contains(log, `person.updated {"channels_from":["email"],"channels_to":["email","telegram"]`) {
		t.Fatalf("no person.updated audit with the channel kinds:\n%s", log)
	}
	mustNotContain(t, "the audit trail", log, "4242")
	mustNotContain(t, "the audit trail", log, tok)
	var confirm []OutboxItem
	for _, o := range svc.OutboxItems(false) {
		if o.Purpose == "onboarding" {
			confirm = append(confirm, o)
		}
	}
	if len(confirm) != 1 || confirm[0].Kind != "telegram" || confirm[0].To != "4242" {
		t.Fatalf("want one confirmation queued to the new chat, got %+v", confirm)
	}

	// Single use: the second /start — even from the same chat — is ignored.
	if err := svc.BindTelegram(tok, "5151", "5151"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a used token bound again: %v", err)
	}
	if got := telegramChannels(t, svc, p.ID); len(got) != 1 {
		t.Fatalf("a used token changed the channels: %v", got)
	}
	if len(svc.TelegramOnboardings()) != 0 {
		t.Fatal("a used link is still listed for handing over")
	}
}

func TestTelegramOnboardingExpiresAfterSevenDays(t *testing.T) {
	svc, clock := newTestService(t, &fakeNotifier{})
	p := mustPerson(t, svc, "ana", TrustRespond)
	v, err := svc.CreateTelegramOnboarding(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !v.ExpiresAt.Equal(clock.Add(7 * 24 * time.Hour)) {
		t.Fatalf("want a 7-day expiry, got %v", v.ExpiresAt.Sub(*clock))
	}
	*clock = clock.Add(7 * 24 * time.Hour)
	if err := svc.BindTelegram(onboardToken(t, v), "4242", "4242"); err == nil {
		t.Fatal("an expired link bound a chat")
	}
	if got := telegramChannels(t, svc, p.ID); len(got) != 0 {
		t.Fatalf("an expired link changed the channels: %v", got)
	}
}

// A group chat's id never equals a person's own user id, so it could not
// be checked later (VerifyTelegramActor); /start there binds nothing and
// leaves the token for the person's own private chat.
func TestTelegramOnboardingRefusesAGroupChat(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	p := mustPerson(t, svc, "ana", TrustRespond)
	v, err := svc.CreateTelegramOnboarding(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	tok := onboardToken(t, v)
	if err := svc.BindTelegram(tok, "-1001234", "4242"); err == nil {
		t.Fatal("a group chat was bound")
	}
	if got := telegramChannels(t, svc, p.ID); len(got) != 0 {
		t.Fatalf("a group chat changed the channels: %v", got)
	}
	if err := svc.BindTelegram(tok, "4242", "4242"); err != nil {
		t.Fatalf("the group attempt spent the token: %v", err)
	}
}

// Handing out a new link revokes the old one — the answer to a leaked one.
func TestTelegramOnboardingNewLinkRevokesTheOld(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	p := mustPerson(t, svc, "ana", TrustRespond)
	old, err := svc.CreateTelegramOnboarding(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateTelegramOnboarding(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.BindTelegram(onboardToken(t, old), "4242", "4242"); err == nil {
		t.Fatal("the replaced link still binds")
	}
	if n := len(svc.TelegramOnboardings()); n != 1 {
		t.Fatalf("want exactly the new link listed, got %d", n)
	}
}

func TestDeletePersonDropsTheOnboardingLink(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	p := mustPerson(t, svc, "ana", TrustRespond)
	if _, err := svc.CreateTelegramOnboarding(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeletePerson(p.ID); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	n := len(svc.state.TelegramOnboardings)
	svc.mu.Unlock()
	if n != 0 {
		t.Fatalf("a deleted person's onboarding link is still stored (%d)", n)
	}
}

// The bound chat takes the telegram slot the panel and `person set` edit:
// a wrong hand-typed id is replaced, not kept failing beside the right
// one, and nothing is hidden in a second telegram channel.
func TestTelegramOnboardingReplacesTheTelegramSlot(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	p, err := svc.AddPerson("ana", TrustRespond, []Address{
		{Kind: "email", To: "ana@test.local"}, {Kind: "telegram", To: "999"},
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := svc.CreateTelegramOnboarding(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.BindTelegram(onboardToken(t, v), "4242", "4242"); err != nil {
		t.Fatal(err)
	}
	want := []Address{{Kind: "email", To: "ana@test.local"}, {Kind: "telegram", To: "4242"}}
	if got := findPerson(t, svc, p.ID).Channels; !sameChannels(got, want) {
		t.Fatalf("want the telegram slot replaced:\n got %+v\nwant %+v", got, want)
	}
	if !strings.Contains(auditDump(t, svc), `"channels_from":["email","telegram"],"channels_to":["email","telegram"]`) {
		t.Fatalf("the replacement is not audited as person.updated:\n%s", auditDump(t, svc))
	}
}

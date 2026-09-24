// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// #16, owner decision 2026-09-24 (Option A): a webhook target URL is a
// credential on every surface, the admin API and panel included — they
// show scheme+host, the way Webhook.Secret is blanked, and only the
// stored record keeps the whole value.

const (
	shownSecret = "SENTINELshownSECRETpath"
	shownHook   = "https://hooks.x.local/services/" + shownSecret
	shownHost   = "https://hooks.x.local/" + redactedMark
)

// mustBeShown fails when v, marshalled the way the API writes it,
// carries the secret — or no longer names the host.
func mustBeShown(t *testing.T, what string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), shownSecret) {
		t.Errorf("%s hands out the webhook target:\n%s", what, b)
	}
	if !strings.Contains(string(b), shownHost) {
		t.Errorf("%s lost the host the operator needs to tell targets apart:\n%s", what, b)
	}
}

// TestWebhookTargetIsShownAsHostOnEverySurface — every Service method
// that hands addresses out: each surface is its own subtest, so one leak
// cannot hide another.
func TestWebhookTargetIsShownAsHostOnEverySurface(t *testing.T) {
	// Webhook sends fail, so the items stay undelivered and retryable.
	fake := &fakeNotifier{fail: map[string]bool{"webhook": true}}
	svc, clock := newTestService(t, fake)

	p, err := svc.AddPerson("ana", TrustRespond, []Address{{Kind: "webhook", To: shownHook}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.AddBroadcast("board", "webhook", shownHook)
	if err != nil {
		t.Fatal(err)
	}
	wh, err := svc.AddWebhook(shownHook, nil)
	if err != nil {
		t.Fatal(err)
	}
	if wh.Secret == "" {
		t.Fatal("the HMAC secret must still be shown once, on registration")
	}
	name := "Ana"
	up, err := svc.UpdatePerson(p.ID, PersonUpdate{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	start := clock.Add(72 * time.Hour)
	e, err := svc.CreateEvent(EventInput{Title: "Night", StartsAt: start, EndsAt: start.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Assign(e.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelEvent(e.ID, "", "flood", "api"); err != nil {
		t.Fatal(err)
	}
	tested, err := svc.SendTest(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tested) == 0 {
		t.Fatal("setup: SendTest queued nothing")
	}
	retried, err := svc.RetryOutbox(tested[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	prop, err := svc.Propagation(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prop.Total == 0 {
		t.Fatal("setup: the cancellation fanned out to nobody")
	}

	for _, c := range []struct {
		what string
		v    any
	}{
		{"AddPerson's result", p},
		{"UpdatePerson's result", up},
		{"People", svc.People()},
		{"AddBroadcast's result", b},
		{"Broadcasts", svc.Broadcasts()},
		{"AddWebhook's result", wh},
		{"Webhooks", svc.Webhooks()},
		{"OutboxItems", svc.OutboxItems(false)},
		{"Propagation", prop},
		{"SendTest's result", tested},
		{"RetryOutbox's result", retried},
	} {
		t.Run(c.what, func(t *testing.T) { mustBeShown(t, c.what, c.v) })
	}

	// The record itself stays whole — the sends went to the real target.
	t.Run("stored record", func(t *testing.T) {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		if got := svc.state.People[0].Channels[0].To; got != shownHook {
			t.Errorf("the stored person channel was rewritten: %q", got)
		}
		if got := svc.state.Broadcasts[0].To; got != shownHook {
			t.Errorf("the stored broadcast was rewritten: %q", got)
		}
		if got := svc.state.Webhooks[0].URL; got != shownHook {
			t.Errorf("the stored subscription was rewritten: %q", got)
		}
	})
}

// TestInvalidStoredWebhookStaysOutOfTheReport — #16 item 3, the known
// gap invariant 12 named: a stored person webhook that fails Validate
// was echoed whole by Address.Problem into channel.invalid, the panel's
// broken-channel list and the admin mail.
func TestInvalidStoredWebhookStaysOutOfTheReport(t *testing.T) {
	for name, target := range map[string]string{
		"no scheme":    "hooks.x.local/services/" + shownSecret,
		"wrong scheme": "ftp://hooks.x.local/services/" + shownSecret,
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeNotifier{}
			start := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
			st := legacyState(start, true)
			st.People[0].Channels = []Address{{Kind: "webhook", To: target}}
			svc, clock := newTestServiceWithState(t, fake, st)
			svc.Tick(*clock)

			probs := svc.ChannelProblems()
			if len(probs) != 1 || probs[0].Kind != "webhook" || probs[0].Problem == "" {
				t.Fatalf("setup: want the one broken webhook reported, got %+v", probs)
			}
			mustNotContain(t, "the panel's broken-channel list", probs[0].To+" "+probs[0].Problem, shownSecret)
			log := auditDump(t, svc)
			if !strings.Contains(log, "channel.invalid") {
				t.Fatalf("setup: no channel.invalid entry:\n%s", log)
			}
			mustNotContain(t, "the audit trail", log, shownSecret)
			pages := adminMessages(fake, "Stored channels look broken")
			if len(pages) != 1 {
				t.Fatalf("setup: want one admin page, got %d", len(pages))
			}
			mustNotContain(t, "the admin mail", pages[0].Body, shownSecret)
			if !strings.Contains(pages[0].Body, "ana") || !strings.Contains(pages[0].Body, "webhook") {
				t.Fatalf("the page no longer says whose channel is broken:\n%s", pages[0].Body)
			}
		})
	}
}

// TestShownWebhookRoundTripKeepsTheStoredTarget — the panel form and
// `person set` read a person through the view and send the whole
// channel list back. The display must restore to the stored target, not
// overwrite it; a display that matches nothing stored is refused.
func TestShownWebhookRoundTripKeepsTheStoredTarget(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	p, err := svc.AddPerson("ana", TrustRespond, []Address{
		{Kind: "email", To: "ana@x.local"}, {Kind: "webhook", To: shownHook},
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := findPerson(t, svc, p.ID)
	newMail := "ana@new.local"
	patched := PatchChannels(seen.Channels, &newMail, nil)
	if _, err := svc.UpdatePerson(p.ID, PersonUpdate{Channels: &patched}); err != nil {
		t.Fatalf("the panel's round trip was refused: %v", err)
	}
	svc.mu.Lock()
	stored := append([]Address(nil), svc.state.Person(p.ID).Channels...)
	svc.mu.Unlock()
	want := []Address{{Kind: "email", To: newMail}, {Kind: "webhook", To: shownHook}}
	if !sameChannels(stored, want) {
		t.Fatalf("the round trip rewrote the stored target:\n got %+v\nwant %+v", stored, want)
	}

	foreign := []Address{{Kind: "webhook", To: "https://other.x.local/" + redactedMark}}
	if _, err := svc.UpdatePerson(p.ID, PersonUpdate{Channels: &foreign}); err == nil {
		t.Error("UpdatePerson stored a display that matches no stored target")
	}
	if _, err := svc.AddPerson("bo", TrustRespond, foreign); err == nil {
		t.Error("AddPerson stored a display as a target")
	}
}

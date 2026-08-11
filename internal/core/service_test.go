// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type sent struct {
	Kind, To, Subject, Body string
	Headers                 map[string]string
}

// fakeNotifier records sends and can be told to fail per channel kind.
type fakeNotifier struct {
	mu   sync.Mutex
	msgs []sent
	fail map[string]bool
}

func (f *fakeNotifier) Send(kind, to string, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[kind] {
		return &simError{kind}
	}
	f.msgs = append(f.msgs, sent{kind, to, m.Subject, m.Body, m.Headers})
	return nil
}

type simError struct{ kind string }

func (e *simError) Error() string { return "simulated failure on " + e.kind }

func (f *fakeNotifier) byPurposeTo(to string) []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sent
	for _, m := range f.msgs {
		if m.To == to {
			out = append(out, m)
		}
	}
	return out
}

func newTestService(t *testing.T, fake *fakeNotifier) (*Service, *time.Time) {
	t.Helper()
	store, err := NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(store, Config{
		BaseURL:       "http://test.local",
		EscalateAfter: 10 * time.Minute,
		AdminNotify:   &Address{Kind: "email", To: "admin@test.local"},
	}, fake)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := &now
	svc.SetClock(func() time.Time { return *clock })
	svc.logf = t.Logf
	return svc, clock
}

func mustEvent(t *testing.T, svc *Service, startIn time.Duration) Event {
	t.Helper()
	start := svc.now().Add(startIn)
	e, err := svc.CreateEvent("Tuesday Session", "Hall 3", "", start, start.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mustPerson(t *testing.T, svc *Service, name string, trust TrustLevel) Person {
	t.Helper()
	p, err := svc.AddPerson(name, trust, []Address{{Kind: "email", To: name + "@test.local"}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReminderCreatesLinksAndDelivers(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour) // inside the 48h lead
	p := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(e.ID, p.ID, "host"); err != nil {
		t.Fatal(err)
	}

	svc.Tick(*clock)

	got := fake.byPurposeTo("ana@test.local")
	if len(got) != 1 {
		t.Fatalf("want 1 reminder, got %d", len(got))
	}
	if !strings.Contains(got[0].Body, "http://test.local/a/") {
		t.Fatalf("reminder body has no action links:\n%s", got[0].Body)
	}
	// Second tick must not send again.
	svc.Tick(clock.Add(time.Minute))
	if len(fake.byPurposeTo("ana@test.local")) != 1 {
		t.Fatal("reminder sent twice")
	}
}

func TestConfirmViaLink(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	svc.Assign(e.ID, p.ID, "")
	confirmURL, _, err := svc.GenerateLinks(e.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(confirmURL, "http://test.local/a/")

	v, err := svc.ApplyAction(token)
	if err != nil {
		t.Fatal(err)
	}
	if v.Event.Status != StatusConfirmed {
		t.Fatalf("status = %s, want confirmed", v.Event.Status)
	}
	if v.Decided == nil || v.Decided.Action != ActionConfirm {
		t.Fatal("response not recorded")
	}
}

func TestCancelPropagatesAndEscalates(t *testing.T) {
	fake := &fakeNotifier{fail: map[string]bool{"telegram": true}}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	svc.Assign(e.ID, p.ID, "")
	if _, err := svc.AddBroadcast("channel", "telegram", "-100123"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.CancelEvent(e.ID, "", "storm damage", "api"); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)

	// Email to assignee delivered, telegram broadcast failing.
	ps, err := svc.Propagation(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Total != 2 || ps.Delivered != 1 || ps.Pending != 1 {
		t.Fatalf("propagation = %+v", ps)
	}
	if ps.Complete {
		t.Fatal("propagation must not be complete while telegram fails")
	}

	// After EscalateAfter the admin gets paged exactly once.
	*clock = clock.Add(11 * time.Minute)
	svc.Tick(*clock)
	*clock = clock.Add(time.Minute)
	svc.Tick(*clock)
	admin := fake.byPurposeTo("admin@test.local")
	if len(admin) != 1 {
		t.Fatalf("want exactly 1 escalation, got %d", len(admin))
	}
	if !strings.Contains(admin[0].Subject, "Delivery stuck") {
		t.Fatalf("unexpected escalation subject %q", admin[0].Subject)
	}

	// Telegram recovers: propagation completes.
	fake.mu.Lock()
	fake.fail["telegram"] = false
	fake.mu.Unlock()
	*clock = clock.Add(30 * time.Minute)
	svc.Tick(*clock)
	ps, _ = svc.Propagation(e.ID)
	if !ps.Complete {
		t.Fatalf("propagation should be complete, got %+v", ps)
	}
}

func TestCancelledEventRejectsConfirm(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	svc.Assign(e.ID, p.ID, "")
	confirmURL, cancelURL, _ := svc.GenerateLinks(e.ID, p.ID)
	cTok := strings.TrimPrefix(confirmURL, "http://test.local/a/")
	xTok := strings.TrimPrefix(cancelURL, "http://test.local/a/")

	if _, err := svc.ApplyAction(xTok); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyAction(cTok); err == nil {
		t.Fatal("confirm on a cancelled event must fail")
	}
	// Cancel again is idempotent, not an error.
	if _, err := svc.ApplyAction(xTok); err != nil {
		t.Fatalf("re-cancel should be idempotent, got %v", err)
	}
}

func TestDeadlinePassedNotifiesAdmin(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	svc.Assign(e.ID, p.ID, "")

	svc.Tick(*clock) // reminder out
	*clock = clock.Add(17 * time.Hour)
	svc.Tick(*clock) // now inside deadline-lead (24h before start), nobody answered

	found := false
	for _, m := range fake.byPurposeTo("admin@test.local") {
		if strings.Contains(m.Subject, "No response") {
			found = true
		}
	}
	if !found {
		t.Fatal("deadline.passed did not page the admin")
	}
	got, _ := svc.EventByID(e.ID)
	if got.DeadlineFiredAt.IsZero() {
		t.Fatal("DeadlineFiredAt not set")
	}
}

func TestTrustLevels(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)

	responder := mustPerson(t, svc, "resp", TrustRespond)
	proposer := mustPerson(t, svc, "prop", TrustPropose)
	director := mustPerson(t, svc, "dir", TrustDirect)
	for _, p := range []Person{responder, proposer, director} {
		svc.Assign(e.ID, p.ID, "")
	}

	// respond: no portal submits.
	if _, err := svc.PortalSubmit(responder.PortalToken, "cancel", e.ID, "", "", time.Time{}, time.Time{}); err != ErrForbidden {
		t.Fatalf("respond-level submit: want ErrForbidden, got %v", err)
	}

	// propose: files a proposal, nothing applied yet.
	applied, err := svc.PortalSubmit(proposer.PortalToken, "cancel", e.ID, "", "rain", time.Time{}, time.Time{})
	if err != nil || applied {
		t.Fatalf("propose-level submit: applied=%v err=%v", applied, err)
	}
	if got, _ := svc.EventByID(e.ID); got.Status == StatusCancelled {
		t.Fatal("proposal must not cancel immediately")
	}
	props := svc.Proposals()
	if len(props) != 1 {
		t.Fatalf("want 1 proposal, got %d", len(props))
	}

	// Accepting the proposal applies it.
	if _, err := svc.DecideProposal(props[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.EventByID(e.ID); got.Status != StatusCancelled {
		t.Fatal("accepted cancel proposal did not cancel")
	}

	// direct: creates immediately and is auto-assigned.
	start := svc.now().Add(100 * time.Hour)
	applied, err = svc.PortalSubmit(director.PortalToken, "create", "", "New Thing", "", start, start.Add(time.Hour))
	if err != nil || !applied {
		t.Fatalf("direct-level create: applied=%v err=%v", applied, err)
	}
	pv, err := svc.Portal(director.PortalToken)
	if err != nil {
		t.Fatal(err)
	}
	foundNew := false
	for _, it := range pv.Items {
		if it.Event.Title == "New Thing" {
			foundNew = true
		}
	}
	if !foundNew {
		t.Fatal("direct-created event not assigned to creator")
	}
}

func TestLinkExpiresWithEvent(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 2*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	svc.Assign(e.ID, p.ID, "")
	confirmURL, _, _ := svc.GenerateLinks(e.ID, p.ID)
	token := strings.TrimPrefix(confirmURL, "http://test.local/a/")

	*clock = clock.Add(5 * time.Hour) // event (2h + 2h duration) is over
	if _, err := svc.ApplyAction(token); err != ErrGone {
		t.Fatalf("want ErrGone after event end, got %v", err)
	}
}

func TestMoveResetsConfirmationCycle(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	svc.Assign(e.ID, p.ID, "")
	svc.Tick(*clock) // reminder for original date
	if _, err := svc.ConfirmEvent(e.ID, p.ID, "api"); err != nil {
		t.Fatal(err)
	}

	newStart := svc.now().Add(30 * time.Hour)
	moved, err := svc.MoveEvent(e.ID, newStart, newStart.Add(time.Hour), "", "api")
	if err != nil {
		t.Fatal(err)
	}
	if moved.Status != StatusScheduled || !moved.ReminderSentAt.IsZero() {
		t.Fatalf("move must reset the confirmation cycle, got %+v", moved)
	}
	if moved.Seq < 2 {
		t.Fatalf("sequence must bump on move, got %d", moved.Seq)
	}

	// Next tick sends a fresh reminder for the new date.
	before := len(fake.byPurposeTo("ana@test.local"))
	svc.Tick(clock.Add(time.Minute))
	after := len(fake.byPurposeTo("ana@test.local"))
	if after <= before {
		t.Fatal("no fresh reminder after move")
	}
}

func TestWebhookSignedDispatch(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	if _, err := svc.AddWebhook("http://hooks.test/x", []string{"event.cancelled"}); err != nil {
		t.Fatal(err)
	}
	e := mustEvent(t, svc, 40*time.Hour)
	svc.CancelEvent(e.ID, "", "", "api")
	svc.Tick(*clock)

	hooks := fake.byPurposeTo("http://hooks.test/x")
	if len(hooks) != 1 {
		t.Fatalf("want 1 webhook delivery, got %d", len(hooks))
	}
	h := hooks[0]
	if h.Headers["X-Stattii-Event"] != "event.cancelled" {
		t.Fatalf("missing event header: %+v", h.Headers)
	}
	if !strings.HasPrefix(h.Headers["X-Stattii-Signature"], "sha256=") {
		t.Fatal("missing HMAC signature")
	}
	if !strings.Contains(h.Body, e.ID) {
		t.Fatal("payload does not contain the event")
	}
	// The filter must have suppressed event.created.
	for _, m := range hooks {
		if strings.Contains(m.Body, `"event":"event.created"`) {
			t.Fatal("filtered webhook received event.created")
		}
	}
}

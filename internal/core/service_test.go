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
	e, err := svc.CreateEvent(EventInput{
		Title: "Tuesday Session", Location: "Hall 3",
		StartsAt: start, EndsAt: start.Add(2 * time.Hour),
	})
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

// Events are usually created first and staffed second. When creation already
// falls inside the reminder window, a tick between the two must not burn the
// one-shot reminder on zero recipients (found live 2026-08-12: the scheduler
// ticked 6s after event.created and before the assignment — reminder.sent
// with an empty outbox, and it never fires again).
func TestReminderWaitsForAssignment(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour) // inside the 48h lead, nobody assigned

	svc.Tick(*clock) // the racing tick

	p := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(e.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	svc.Tick(clock.Add(time.Minute))

	if got := fake.byPurposeTo("ana@test.local"); len(got) != 1 {
		t.Fatalf("want the reminder to fire once staffed, got %d sends", len(got))
	}
}

// The reminder now waits for assignees — but the dead-man-switch must not:
// an event nobody was ever assigned to cannot be confirmed, so
// if_unconfirmed=cancel has to auto-cancel it at the deadline regardless.
func TestDeadmanSwitchFiresWithoutAssignees(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	start := svc.now().Add(20 * time.Hour) // inside the 24h deadline lead
	e, err := svc.CreateEvent(EventInput{
		Title: "Unstaffed", StartsAt: start, EndsAt: start.Add(time.Hour),
		IfUnconfirmed: "cancel",
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.Tick(*clock)

	got, err := svc.EventByID(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCancelled {
		t.Fatalf("dead-man-switch did not fire for unstaffed event: status=%s", got.Status)
	}
}

func TestSendTestGoesThroughOutbox(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	p := mustPerson(t, svc, "ana", TrustRespond)

	items, err := svc.SendTest(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Purpose != "test" || !items[0].Delivered() {
		t.Fatalf("test message not delivered through the outbox: %+v", items)
	}
	if got := fake.byPurposeTo("ana@test.local"); len(got) != 1 || !strings.Contains(got[0].Subject, "test") {
		t.Fatalf("notifier saw %+v", got)
	}

	if _, err := svc.SendTest("pe_nope"); err == nil {
		t.Fatal("unknown person must error")
	}
	noChan, _ := svc.AddPerson("bob", TrustRespond, nil)
	if _, err := svc.SendTest(noChan.ID); err == nil {
		t.Fatal("person without channels must error")
	}
}

func TestOverviewJoinsAssignmentsAndResponses(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	ana := mustPerson(t, svc, "ana", TrustRespond)
	bob := mustPerson(t, svc, "bob", TrustRespond)
	if err := svc.Assign(e.ID, ana.ID, "host"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Assign(e.ID, bob.ID, ""); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock) // reminder to both, delivered via fake notifier

	confirmURL, _, err := svc.GenerateLinks(e.ID, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyAction(strings.TrimPrefix(confirmURL, "http://test.local/a/")); err != nil {
		t.Fatal(err)
	}

	ov := svc.Overview()
	if len(ov.Events) != 1 || len(ov.Events[0].Assignees) != 2 {
		t.Fatalf("unexpected overview shape: %+v", ov)
	}
	byName := map[string]OverviewAssignee{}
	for _, a := range ov.Events[0].Assignees {
		byName[a.Name] = a
	}
	if a := byName["ana"]; a.Action != ActionConfirm || a.Via != "link" || a.Role != "host" {
		t.Fatalf("ana not joined correctly: %+v", a)
	}
	if b := byName["bob"]; b.Action != "" {
		t.Fatalf("bob should be pending, got %+v", b)
	}
	if ov.Outbox.Delivered != 2 || ov.Outbox.Pending != 0 || ov.Outbox.Failed != 0 {
		t.Fatalf("outbox summary wrong: %+v", ov.Outbox)
	}
	if ov.People != 2 || ov.OpenProposals != 0 {
		t.Fatalf("counts wrong: people=%d proposals=%d", ov.People, ov.OpenProposals)
	}
}

// A tick whose escalations enqueue admin mail must not lose the delivery
// marks of items later in the same pass — a pointer into a reallocated
// outbox backing array meant delivered items were re-sent every tick.
func TestTickEscalationKeepsDeliveryMarks(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(e.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelEvent(e.ID, "", "storm", "api"); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(11 * time.Minute) // past EscalateAfter, still deliverable
	svc.Tick(*clock)
	for _, o := range svc.OutboxItems(false) {
		if o.Purpose == "cancellation" && (!o.Delivered() || o.Attempts != 1) {
			t.Fatalf("delivery marks lost on escalating tick: %+v", o)
		}
	}
	// The escalation itself goes out one tick later.
	*clock = clock.Add(time.Minute)
	svc.Tick(*clock)
	if got := fake.byPurposeTo("admin@test.local"); len(got) == 0 {
		t.Fatal("escalation never reached the admin")
	}
}

// An event created inside the deadline window gets its ask plus a real
// grace period — never asked and auto-cancelled in the same tick.
func TestReminderAndDeadlineNeverSameTick(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	start := svc.now().Add(20 * time.Hour) // inside the 24h deadline lead
	e, err := svc.CreateEvent(EventInput{
		Title: "Late Setup", StartsAt: start, EndsAt: start.Add(time.Hour),
		IfUnconfirmed: "cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	p := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(e.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}

	svc.Tick(*clock)
	got, _ := svc.EventByID(e.ID)
	if got.ReminderSentAt.IsZero() {
		t.Fatal("reminder did not fire")
	}
	if got.Status == StatusCancelled {
		t.Fatal("asked and auto-cancelled in the same tick")
	}

	*clock = clock.Add(90 * time.Minute) // grace over
	svc.Tick(*clock)
	if got, _ = svc.EventByID(e.ID); got.Status != StatusCancelled {
		t.Fatal("dead-man-switch never fired after the grace")
	}
}

// A proposal whose apply fails stays open — deciding it, webhooking it,
// and telling the proposer "accepted" about a change that never happened
// is exactly the wrong-communication failure stattii exists to prevent.
func TestDecideProposalFailedApplyStaysOpen(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	e := mustEvent(t, svc, 90*time.Hour)
	p := mustPerson(t, svc, "bo", TrustPropose)
	if err := svc.Assign(e.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PortalSubmit(p.PortalToken, "move", e.ID, "", "clash",
		svc.now().Add(100*time.Hour), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelEvent(e.ID, "", "storm", "api"); err != nil {
		t.Fatal(err)
	}
	pr := svc.Proposals()[0]
	if _, err := svc.DecideProposal(pr.ID, true); err == nil {
		t.Fatal("accepting a move on a cancelled event must fail")
	}
	if got := svc.Proposals()[0]; !got.DecidedAt.IsZero() || got.Accepted {
		t.Fatalf("failed apply recorded a decision: %+v", got)
	}
	for _, o := range svc.OutboxItems(false) {
		if o.Purpose == "proposal" && strings.Contains(o.Subject, "accepted") {
			t.Fatalf("verdict mail enqueued despite failure: %+v", o)
		}
	}
}

func TestMoveEventRejectsZeroStart(t *testing.T) {
	fake := &fakeNotifier{}
	svc, _ := newTestService(t, fake)
	e := mustEvent(t, svc, 90*time.Hour)
	before := len(svc.OutboxItems(false))
	if _, err := svc.MoveEvent(e.ID, time.Time{}, time.Time{}, "", "api"); err == nil {
		t.Fatal("a move to the zero time was accepted")
	}
	if got := len(svc.OutboxItems(false)); got != before {
		t.Fatal("a rejected move still fanned out")
	}
}

// Assignees without channels count as not staffed: the one-shot reminder
// must stay pending instead of burning on zero deliverable recipients.
func TestReminderWaitsForReachableAssignee(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	e := mustEvent(t, svc, 40*time.Hour)
	p, err := svc.AddPerson("mute", TrustRespond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Assign(e.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	svc.Tick(*clock)
	if got, _ := svc.EventByID(e.ID); !got.ReminderSentAt.IsZero() {
		t.Fatal("one-shot reminder burned on an unreachable assignee")
	}
}

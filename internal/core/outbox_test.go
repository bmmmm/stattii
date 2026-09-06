// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestOutboxStateClassification pins the four states and their
// boundaries — every admin surface colours by this one classifier, so
// the retrying/failed edge (MaxAttempts, default 5) must not drift.
func TestOutboxStateClassification(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	cases := []struct {
		item OutboxItem
		want string
	}{
		{OutboxItem{}, "queued"},
		{OutboxItem{Attempts: 1}, "retrying"},
		{OutboxItem{Attempts: 4}, "retrying"},
		{OutboxItem{Attempts: 5}, "failed"},
		{OutboxItem{Attempts: 7}, "failed"},
		{OutboxItem{Attempts: 5, DeliveredAt: svc.now()}, "delivered"},
	}
	for _, c := range cases {
		if got := svc.OutboxState(c.item); got != c.want {
			t.Errorf("attempts=%d delivered=%v: got %q, want %q",
				c.item.Attempts, c.item.Delivered(), got, c.want)
		}
	}
}

// TestOutboxPruning: delivered items whose event is long past leave
// state.json on tick; anything still telling an open story stays — a
// delivered marker for an upcoming event, and undelivered items always.
func TestOutboxPruning(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	past := mustEvent(t, svc, 30*time.Hour)
	future := mustEvent(t, svc, 200*24*time.Hour)

	old := svc.now()
	svc.state.Outbox = append(svc.state.Outbox,
		// Delivered, event finished long before the cutoff — pruned.
		OutboxItem{ID: "ob_done", EventID: past.ID, Purpose: "cancellation",
			Kind: "email", To: "a@x.example", Attempts: 1, DeliveredAt: old, CreatedAt: old},
		// Delivered, no event (webhook), old — pruned by delivery age.
		OutboxItem{ID: "ob_hook", Purpose: "webhook", Kind: "webhook",
			To: "https://x.example", Attempts: 1, DeliveredAt: old, CreatedAt: old},
		// Delivered long ago, but the event is still upcoming — kept:
		// the panel's per-guest/person delivery markers read it.
		OutboxItem{ID: "ob_upcoming", EventID: future.ID, Purpose: "reminder",
			Kind: "email", To: "b@x.example", Attempts: 1, DeliveredAt: old, CreatedAt: old},
		// Failed, ancient — kept forever: the alarm is never pruned.
		OutboxItem{ID: "ob_alarm", EventID: past.ID, Purpose: "cancellation",
			Kind: "email", To: "c@x.example", Attempts: 5, LastError: "boom",
			NextAttempt: old, CreatedAt: old},
	)

	*clock = clock.Add(120 * 24 * time.Hour) // retention is 90 days
	svc.Tick(*clock)

	left := map[string]bool{}
	for _, o := range svc.state.Outbox {
		left[o.ID] = true
	}
	if left["ob_done"] || left["ob_hook"] {
		t.Fatalf("delivered items of finished stories survived pruning: %v", left)
	}
	if !left["ob_upcoming"] {
		t.Fatal("delivered marker for an upcoming event was pruned")
	}
	if !left["ob_alarm"] {
		t.Fatal("an undelivered item was pruned — the alarm must survive")
	}

	entries, err := svc.Audit(20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Kind == "outbox.pruned" && strings.Contains(string(e.Data), `"count":2`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no outbox.pruned audit entry with count 2 in %+v", entries)
	}

	// A second tick prunes nothing and must not claim otherwise.
	before := len(svc.state.Outbox)
	svc.Tick(*clock)
	if len(svc.state.Outbox) != before {
		t.Fatal("second tick pruned again")
	}
}

// blockingNotifier parks every send until unblock is called — the slow
// SMTP/Telegram peer the unlocked delivery pass exists for. entered
// closes on the first send, so a test can wait for "a send is in
// flight" without sleeping.
type blockingNotifier struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
	err         error
}

func newBlockingNotifier(err error) *blockingNotifier {
	return &blockingNotifier{entered: make(chan struct{}), release: make(chan struct{}), err: err}
}

func (b *blockingNotifier) Send(kind, to string, m Message) error {
	b.enteredOnce.Do(func() { close(b.entered) })
	<-b.release
	return b.err
}

func (b *blockingNotifier) unblock() { b.releaseOnce.Do(func() { close(b.release) }) }

func (b *blockingNotifier) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no send was attempted")
	}
}

// runParked starts fn and returns once its first send is parked. The
// returned finish releases the send and joins the pass; it also runs as
// cleanup, so a failed test never leaks a pass that would write into
// the already-removed temp dir.
func runParked(t *testing.T, block *blockingNotifier, fn func()) (finish func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	var once sync.Once
	finish = func() {
		once.Do(func() {
			block.unblock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("delivery pass did not finish")
			}
		})
	}
	t.Cleanup(finish)
	block.waitEntered(t)
	return finish
}

// TestDeliveryRunsOutsideTheLock is the whole point of the split pass:
// one peer that hangs for its full timeout must not serialize the rest
// of the service behind s.mu. Both delivery paths are checked — the
// scheduler pass and the admin's immediate test send.
func TestDeliveryRunsOutsideTheLock(t *testing.T) {
	for _, c := range []struct {
		name    string
		deliver func(svc *Service, clock *time.Time, personID string)
	}{
		{"tick", func(svc *Service, clock *time.Time, _ string) { svc.Tick(*clock) }},
		{"sendtest", func(svc *Service, _ *time.Time, personID string) { svc.SendTest(personID) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			block := newBlockingNotifier(nil)
			svc, clock := newTestService(t, block)
			e := mustEvent(t, svc, 40*time.Hour) // inside the 48h reminder lead
			p := mustPerson(t, svc, "ana", TrustRespond)
			if err := svc.Assign(e.ID, p.ID, "host"); err != nil {
				t.Fatal(err)
			}
			runParked(t, block, func() { c.deliver(svc, clock, p.ID) })

			// A send is parked. Everything else must still get through:
			// a mutation from the admin listener, not just a read.
			free := make(chan error, 1)
			go func() {
				_, err := svc.ConfirmEvent(e.ID, p.ID, "api")
				free <- err
			}()
			select {
			case err := <-free:
				if err != nil {
					t.Fatalf("confirm during an in-flight send: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("a mutation blocked behind an in-flight send — delivery still runs under s.mu")
			}
		})
	}
}

// TestRetryDuringAnInFlightSendSurvives: the operator re-arms an item
// while its send is parked. Booking the failed attempt afterwards must
// not undo that — otherwise "Retry" in the panel silently does nothing
// whenever the send it is racing is still out. Both attempt counts are
// checked: the panel offers Retry on a queued item too, and there the
// count stays 0, so it cannot serve as the "was touched" marker.
func TestRetryDuringAnInFlightSendSurvives(t *testing.T) {
	for _, attempts := range []int{0, 2} {
		t.Run(fmt.Sprintf("attempts=%d", attempts), func(t *testing.T) {
			block := newBlockingNotifier(&simError{"email"})
			svc, clock := newTestService(t, block)
			old := clock.Add(-time.Hour)
			svc.state.Outbox = append(svc.state.Outbox, OutboxItem{
				ID: "ob_stuck", Purpose: "cancellation", Kind: "email", To: "a@x.example",
				Subject: "s", Body: "b", Attempts: attempts,
				CreatedAt: old, NextAttempt: old,
			})
			finish := runParked(t, block, func() { svc.Tick(*clock) })

			retried := make(chan error, 1)
			go func() {
				_, err := svc.RetryOutbox("ob_stuck")
				retried <- err
			}()
			select {
			case err := <-retried:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("RetryOutbox blocked behind an in-flight send")
			}

			finish() // let the send fail and the pass book its result

			var got OutboxItem
			for _, o := range svc.OutboxItems(false) {
				if o.ID == "ob_stuck" {
					got = o
				}
			}
			if got.ID == "" {
				t.Fatal("the item vanished")
			}
			if got.Attempts != 0 {
				t.Fatalf("the re-arm was overwritten by the in-flight attempt: attempts=%d", got.Attempts)
			}
			if got.NextAttempt.After(*clock) {
				t.Fatalf("the re-armed item was pushed into backoff: next attempt %s > now %s", got.NextAttempt, *clock)
			}
		})
	}
}

// panicNotifier stands in for a channel with a bug in it.
type panicNotifier struct{}

func (panicNotifier) Send(kind, to string, m Message) error { panic("boom") }

// TestAPanickingChannelIsAFailedAttempt: net/http recovers a handler
// panic, and both SendTest and the tick endpoint deliver from a handler
// — a panicking sender would otherwise leave its item marked in flight
// and never attempt it again.
func TestAPanickingChannelIsAFailedAttempt(t *testing.T) {
	svc, clock := newTestService(t, panicNotifier{})
	svc.state.Outbox = append(svc.state.Outbox, OutboxItem{
		ID: "ob_boom", Purpose: "cancellation", Kind: "email", To: "a@x.example",
		Subject: "s", Body: "b", CreatedAt: *clock, NextAttempt: *clock,
	})

	svc.Tick(*clock)

	var got OutboxItem
	for _, o := range svc.OutboxItems(false) {
		if o.ID == "ob_boom" {
			got = o
		}
	}
	if got.Attempts != 1 {
		t.Fatalf("the panicking send was not booked as an attempt: %+v", got)
	}
	if !strings.Contains(got.LastError, "panicked") {
		t.Fatalf("the failure does not say what happened: %q", got.LastError)
	}
	if _, out := svc.sending[got.ID]; out {
		t.Fatal("the item is still marked in flight — it would never be tried again")
	}
}

// TestNotifySendHasOneCallerOnly is the gate under invariant 11: the
// delivery must not move back under s.mu. A second call site — most
// plausibly inside some …Locked helper, where it is invisible in a
// diff — fails here whatever it is called.
func TestNotifySendHasOneCallerOnly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var callers []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Send" {
					return true
				}
				field, ok := sel.X.(*ast.SelectorExpr)
				if !ok || field.Sel.Name != "notify" {
					return true
				}
				callers = append(callers, fn.Name.Name)
				return true
			})
		}
	}
	if len(callers) != 1 || callers[0] != "send" {
		t.Fatalf("notify.Send is called from %v — it belongs in send() alone, which runs with s.mu free", callers)
	}
}

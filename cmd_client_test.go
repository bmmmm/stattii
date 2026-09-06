// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// recorded is what a command WOULD have sent. The dispatch tests swap
// the two API seams for recorders, so the whole table can be walked
// without a server behind it.
type recorded struct {
	method, path string
	body         any
}

// sameBody compares two request bodies; two absent bodies are equal,
// which reflect.DeepEqual does not say about a pair of nil interfaces.
func sameBody(got, want any) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return reflect.DeepEqual(got, want)
}

func record(t *testing.T) *recorded {
	t.Helper()
	got := &recorded{}
	oldSend, oldFetch := apiSend, apiFetch
	apiSend = func(method, path string, body any) error {
		got.method, got.path, got.body = method, path, body
		return nil
	}
	apiFetch = func(path string, out any) error {
		got.method, got.path = "GET", path
		return nil
	}
	t.Cleanup(func() { apiSend, apiFetch = oldSend, oldFetch })
	return got
}

// TestClientDispatchTable pins the request every command produces —
// the proof that turning five near-identical switch dispatchers into
// one table kept every route exactly where it was.
func TestClientDispatchTable(t *testing.T) {
	when := func(s string) time.Time { v, _ := parseWhen(s); return v }
	cases := []struct {
		args         []string
		method, path string
		body         any
	}{
		{[]string{"event", "list"}, "GET", "/api/v1/events", nil},
		{[]string{"event", "create", "--title", "x", "--at", "2026-08-18T19:00"}, "POST", "/api/v1/events",
			map[string]any{"title": "x", "location": "", "note": "", "starts_at": when("2026-08-18T19:00"), "ends_at": time.Time{}, "if_unconfirmed": "notify"}},
		{[]string{"event", "show", "ev_1"}, "GET", "/api/v1/events/ev_1", nil},
		{[]string{"event", "confirm", "ev_1"}, "POST", "/api/v1/events/ev_1/confirm", map[string]any{}},
		{[]string{"event", "cancel", "ev_1", "--reason", "storm"}, "POST", "/api/v1/events/ev_1/cancel", map[string]string{"reason": "storm"}},
		{[]string{"event", "reinstate", "ev_1"}, "POST", "/api/v1/events/ev_1/reinstate", map[string]any{}},
		{[]string{"event", "move", "ev_1", "--at", "2026-08-19T19:00"}, "POST", "/api/v1/events/ev_1/move",
			map[string]any{"starts_at": when("2026-08-19T19:00"), "ends_at": time.Time{}, "note": ""}},
		{[]string{"event", "links", "ev_1", "pe_1"}, "POST", "/api/v1/events/ev_1/links", map[string]string{"person_id": "pe_1"}},
		{[]string{"event", "revoke-links", "ev_1"}, "DELETE", "/api/v1/events/ev_1/links", nil},
		{[]string{"event", "revoke-links", "ev_1", "pe_1"}, "DELETE", "/api/v1/events/ev_1/links?person_id=pe_1", nil},
		{[]string{"event", "responses", "ev_1"}, "GET", "/api/v1/events/ev_1/responses", nil},
		{[]string{"event", "propagation", "ev_1"}, "GET", "/api/v1/events/ev_1/propagation", nil},
		{[]string{"event", "invite", "ev_1"}, "POST", "/api/v1/events/ev_1/invite", map[string]any{}},
		{[]string{"event", "invite", "ev_1", "--revoke"}, "DELETE", "/api/v1/events/ev_1/invite", nil},
		{[]string{"event", "guests", "ev_1"}, "GET", "/api/v1/events/ev_1/guests", nil},
		{[]string{"event", "guests", "ev_1", "--remove", "gu_1"}, "DELETE", "/api/v1/events/ev_1/guests/gu_1", nil},
		{[]string{"event", "rm", "ev_1"}, "DELETE", "/api/v1/events/ev_1", nil},
		{[]string{"person", "list"}, "GET", "/api/v1/people", nil},
		{[]string{"person", "add", "--name", "Ana"}, "POST", "/api/v1/people",
			map[string]any{"name": "Ana", "trust": "respond", "channels": []map[string]string(nil)}},
		{[]string{"person", "set", "pe_1", "--name", "Ana"}, "PATCH", "/api/v1/people/pe_1", map[string]any{"name": "Ana"}},
		{[]string{"person", "test", "pe_1"}, "POST", "/api/v1/people/pe_1/test-message", nil},
		{[]string{"person", "rotate-portal", "pe_1"}, "POST", "/api/v1/people/pe_1/rotate-portal", nil},
		{[]string{"person", "rm", "pe_1"}, "DELETE", "/api/v1/people/pe_1", nil},
		{[]string{"broadcast", "list"}, "GET", "/api/v1/broadcasts", nil},
		{[]string{"broadcast", "add", "--kind", "email", "--to", "a@x"}, "POST", "/api/v1/broadcasts",
			map[string]string{"name": "", "kind": "email", "to": "a@x"}},
		{[]string{"broadcast", "rm", "bc_1"}, "DELETE", "/api/v1/broadcasts/bc_1", nil},
		{[]string{"webhook", "list"}, "GET", "/api/v1/webhooks", nil},
		{[]string{"webhook", "add", "--url", "https://x.example"}, "POST", "/api/v1/webhooks",
			map[string]any{"url": "https://x.example", "events": []string(nil)}},
		{[]string{"webhook", "rm", "wh_1"}, "DELETE", "/api/v1/webhooks/wh_1", nil},
		{[]string{"proposal", "list"}, "GET", "/api/v1/proposals", nil},
		{[]string{"proposal", "accept", "pr_1"}, "POST", "/api/v1/proposals/pr_1/decide", map[string]bool{"accept": true}},
		{[]string{"proposal", "reject", "pr_1"}, "POST", "/api/v1/proposals/pr_1/decide", map[string]bool{"accept": false}},
		{[]string{"outbox", "list"}, "GET", "/api/v1/outbox", nil},
		{[]string{"outbox", "list", "--pending"}, "GET", "/api/v1/outbox?pending=1", nil},
		{[]string{"outbox", "retry", "ob_1"}, "POST", "/api/v1/outbox/ob_1/retry", nil},
		{[]string{"calendar", "fetch"}, "POST", "/api/v1/calendar/fetch", nil},
		{[]string{"assign", "ev_1", "pe_1"}, "POST", "/api/v1/assignments",
			map[string]string{"event_id": "ev_1", "person_id": "pe_1", "role": ""}},
		{[]string{"assign", "ev_1", "pe_1", "host"}, "POST", "/api/v1/assignments",
			map[string]string{"event_id": "ev_1", "person_id": "pe_1", "role": "host"}},
		{[]string{"unassign", "ev_1", "pe_1"}, "DELETE", "/api/v1/events/ev_1/assignees/pe_1", nil},
		{[]string{"series-assign", "uid-1", "pe_1"}, "POST", "/api/v1/series-assignments",
			map[string]string{"source_uid": "uid-1", "person_id": "pe_1", "role": ""}},
		{[]string{"series-unassign", "uid-1", "pe_1"}, "DELETE", "/api/v1/series-assignments?person_id=pe_1&source_uid=uid-1", nil},
		{[]string{"audit"}, "GET", "/api/v1/audit?limit=200", nil},
		{[]string{"audit", "--limit", "5"}, "GET", "/api/v1/audit?limit=5", nil},
		{[]string{"tick"}, "POST", "/api/v1/tick", nil},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			got := record(t)
			if err := cmdClient(c.args); err != nil {
				t.Fatalf("%v: %v", c.args, err)
			}
			if got.method != c.method || got.path != c.path {
				t.Fatalf("got %s %s, want %s %s", got.method, got.path, c.method, c.path)
			}
			if !sameBody(got.body, c.body) {
				t.Fatalf("body:\n got %#v\nwant %#v", got.body, c.body)
			}
		})
	}
}

// TestClientUsageStrings keeps the CLI's own output identical across
// the dispatch rewrite: every one of these strings came out of the
// hand-written switch statements before it. The two group lines that
// changed did so because this issue adds a command to them.
func TestClientUsageStrings(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"event"}, "usage: stattii event list|create|show|confirm|cancel|reinstate|move|links|revoke-links|responses|propagation|invite|guests|rm"},
		{[]string{"event", "show"}, "usage: stattii event show <event-id>"},
		{[]string{"event", "cancel"}, "usage: stattii event cancel <event-id> [--reason ...]"},
		{[]string{"event", "move"}, "usage: stattii event move <event-id> --at ... [--end ...] [--note ...]"},
		{[]string{"event", "links", "ev_1"}, "usage: stattii event links <event-id> <person-id>"},
		{[]string{"event", "revoke-links"}, "usage: stattii event revoke-links <event-id> [person-id]"},
		{[]string{"event", "invite"}, "usage: stattii event invite <event-id> [--revoke]"},
		{[]string{"event", "guests"}, "usage: stattii event guests <event-id> [--remove <guest-id>]"},
		{[]string{"event", "nope"}, `unknown event subcommand "nope"`},
		{[]string{"person"}, "usage: stattii person list|add|set|test|rotate-portal|rm"},
		{[]string{"person", "set"}, "usage: stattii person set <person-id> [--name ...] [--trust ...] [--email ...] [--telegram ...]"},
		{[]string{"person", "test"}, "usage: stattii person test <person-id>"},
		{[]string{"person", "rotate-portal"}, "usage: stattii person rotate-portal <person-id>"},
		{[]string{"person", "nope"}, `unknown person subcommand "nope"`},
		{[]string{"broadcast"}, "usage: stattii broadcast list|add|rm"},
		{[]string{"broadcast", "rm"}, "usage: stattii broadcast rm <id>"},
		{[]string{"webhook"}, "usage: stattii webhook list|add|rm"},
		{[]string{"webhook", "rm"}, "usage: stattii webhook rm <id>"},
		{[]string{"proposal"}, "usage: stattii proposal list|accept|reject"},
		{[]string{"proposal", "accept"}, "usage: stattii proposal accept <id>"},
		{[]string{"outbox"}, "usage: stattii outbox list [--pending] | retry <id>"},
		// calendar is the one group whose wrong-subcommand wording moved:
		// it used to answer "usage: stattii calendar fetch" for anything,
		// and now answers like every other group. Pinned so the next
		// change to it is a decision, not a surprise.
		{[]string{"calendar"}, "usage: stattii calendar fetch"},
		{[]string{"calendar", "fetchh"}, `unknown calendar subcommand "fetchh"`},
		{[]string{"outbox", "retry"}, "usage: stattii outbox retry <id>"},
		{[]string{"assign", "ev_1"}, "usage: stattii assign <event-id> <person-id> [role]"},
		{[]string{"unassign", "ev_1"}, "usage: stattii unassign <event-id> <person-id>"},
		{[]string{"series-assign", "uid-1"}, "usage: stattii series-assign <source-uid> <person-id> [role]"},
		{[]string{"series-unassign", "uid-1"}, "usage: stattii series-unassign <source-uid> <person-id>"},
	}
	for _, c := range cases {
		record(t)
		err := cmdClient(c.args)
		if err == nil {
			t.Errorf("%v: no error, want %q", c.args, c.want)
			continue
		}
		if err.Error() != c.want {
			t.Errorf("%v:\n got %q\nwant %q", c.args, err.Error(), c.want)
		}
	}
}

// TestGroupHelpAnswers: "stattii event --help" used to come back as an
// error about an unknown subcommand.
func TestGroupHelpAnswers(t *testing.T) {
	record(t)
	for _, g := range clientGroups {
		for _, flagSpelling := range []string{"--help", "-h", "help"} {
			if err := cmdClient([]string{g.name, flagSpelling}); err != nil {
				t.Errorf("stattii %s %s: %v", g.name, flagSpelling, err)
			}
		}
		var b strings.Builder
		g.printHelp(&b)
		out := b.String()
		for _, l := range g.leaves {
			if !strings.Contains(out, "stattii "+g.name+" "+l.name) {
				t.Errorf("%s --help does not mention %q:\n%s", g.name, l.name, out)
			}
			if l.help == "" {
				t.Errorf("%s %s has no help text", g.name, l.name)
			}
		}
	}
}

// TestExtraArgumentsAreAnError: they used to be dropped on the floor,
// which turns a typo into a command that quietly did something else.
func TestExtraArgumentsAreAnError(t *testing.T) {
	got := record(t)
	for _, args := range [][]string{
		{"event", "show", "ev_1", "oops"},
		{"event", "cancel", "ev_1", "--reason", "storm", "oops"},
		{"event", "rm", "ev_1", "oops"},
		{"outbox", "list", "oops"},
		{"assign", "ev_1", "pe_1", "host", "oops"},
		{"tick", "oops"},
		// Flag-shaped extras on a command that has no flags: nothing
		// downstream would ever look at them, so "rm --force" must not
		// quietly delete. And a positional that starts with a dash must
		// not disappear into the flag tail either — that one silently
		// WIDENED the operation (revoke-links for everybody).
		{"event", "rm", "ev_1", "--force"},
		{"person", "rm", "pe_1", "--yes-really"},
		{"event", "revoke-links", "ev_1", "-pe_1"},
		{"assign", "ev_1", "pe_1", "-host"},
		{"unassign", "ev_1", "pe_1", "--dry-run"},
		{"proposal", "accept", "pr_1", "--reason", "x"},
	} {
		*got = recorded{}
		if err := cmdClient(args); err == nil {
			t.Errorf("%v: the extra argument was accepted", args)
		}
		if got.method != "" {
			t.Errorf("%v: it still sent %s %s", args, got.method, got.path)
		}
	}
}

// TestDashDashEscapesOneArgument: an imported series uid is foreign data
// (AGENTS.md invariant 10) and may start with a dash. Refusing those in
// the flag tail is right, but there has to be a way in — and it escapes
// exactly one argument, so the flags behind it still work.
func TestDashDashEscapesOneArgument(t *testing.T) {
	got := record(t)
	if err := cmdClient([]string{"series-assign", "--", "-odd-uid", "pe_1"}); err != nil {
		t.Fatal(err)
	}
	if got.method != "POST" || got.path != "/api/v1/series-assignments" {
		t.Fatalf("got %s %s", got.method, got.path)
	}
	body, ok := got.body.(map[string]string)
	if !ok || body["source_uid"] != "-odd-uid" || body["person_id"] != "pe_1" {
		t.Fatalf("body: %#v", got.body)
	}

	*got = recorded{}
	if err := cmdClient([]string{"event", "guests", "--", "-ev1", "--remove", "gu_1"}); err != nil {
		t.Fatal(err)
	}
	if got.method != "DELETE" || got.path != "/api/v1/events/-ev1/guests/gu_1" {
		t.Fatalf("a flag after the escaped argument was lost: got %s %s", got.method, got.path)
	}
}

// TestLeafHelpAnswers: every leaf explains itself, including the ones
// with no flags — those have no FlagSet to answer --help for them.
func TestLeafHelpAnswers(t *testing.T) {
	got := record(t)
	for _, args := range [][]string{
		{"event", "rm", "--help"},
		{"person", "rm", "-h"},
		{"tick", "--help"},
		{"event", "cancel", "--help"},
		{"unassign", "help"},
	} {
		*got = recorded{}
		if err := cmdClient(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
		if got.method != "" {
			t.Errorf("%v: --help sent %s %s", args, got.method, got.path)
		}
	}
}

// TestUsageNamesTheEscape: a value that starts with a dash lands in the
// flag tail, and the error has to say how to get it through — otherwise
// "series-unassign -weird pe_1" is a dead end.
func TestUsageNamesTheEscape(t *testing.T) {
	record(t)
	for _, args := range [][]string{
		{"series-unassign", "-weird", "pe_1"},
		{"event", "revoke-links", "ev_1", "-pe_1"},
	} {
		err := cmdClient(args)
		if err == nil {
			t.Fatalf("%v: accepted", args)
		}
		if !strings.Contains(err.Error(), "-- -") {
			t.Errorf("%v: the error does not name the escape: %q", args, err)
		}
	}
}

// TestArityFromSpec: the usage line and the argument check are the same
// string, so they cannot drift apart.
func TestArityFromSpec(t *testing.T) {
	cases := []struct {
		spec     string
		min, max int
	}{
		{"", 0, 0},
		{"<event-id>", 1, 1},
		{"<event-id> <person-id>", 2, 2},
		{"<event-id> [person-id]", 1, 2},
		{"<event-id> [--reason ...]", 1, 1},
		{"<event-id> --at ... [--end ...] [--note ...]", 1, 1},
		{"<event-id> [--remove <guest-id>]", 1, 1},
		{"<source-uid> <person-id> [role]", 2, 3},
		{"[--pending]", 0, 0},
	}
	for _, c := range cases {
		min, max, _ := arity(c.spec)
		if min != c.min || max != c.max {
			t.Errorf("arity(%q) = %d,%d want %d,%d", c.spec, min, max, c.min, c.max)
		}
	}
	// The same string also says whether the command takes flags at all —
	// the only thing that can refuse a flag to a command that has none.
	for _, c := range []struct {
		spec string
		want bool
	}{
		{"<event-id>", false},
		{"<event-id> [person-id]", false},
		{"<event-id> [--reason ...]", true},
		{"--title ... --at ...", true},
		{"[--pending]", true},
		{"", false},
	} {
		if _, _, got := arity(c.spec); got != c.want {
			t.Errorf("arity(%q) takesFlags = %v, want %v", c.spec, got, c.want)
		}
	}
}

// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

// apiRequest performs one authenticated request against the stattii
// server. The CLI talks to the ADMIN listener (loopback by default) —
// the public listener does not carry /api/v1 at all.
func apiRequest(method, path string, body any) (*http.Response, error) {
	base := os.Getenv("STATTII_URL")
	if base == "" {
		base = "http://127.0.0.1:8789"
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, strings.TrimRight(base, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	if tok := os.Getenv("STATTII_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%v (is the server running? STATTII_URL must point at the admin listener, not %s)", err, base)
	}
	return resp, nil
}

// api pretty-prints the JSON response — the CLI is a thin skin over the API.
func api(method, path string, body any) error {
	resp, err := apiRequest(method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") == nil {
		raw = pretty.Bytes()
	}
	if resp.StatusCode >= 400 {
		// The error payload must not land in a redirected stdout as if it
		// were the requested data.
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

// apiJSON fetches path and decodes the response for rendered views.
func apiJSON(path string, out any) error {
	resp, err := apiRequest("GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
}

// The two request paths, indirected: the dispatch test records what
// each command WOULD send, so the whole table is walked without a
// server. Every leaf goes through these, never through api/apiJSON.
var (
	apiSend  = api
	apiFetch = apiJSON
)

// parseWhen accepts RFC3339 or the shorter "2006-01-02T15:04" (local time).
func parseWhen(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q (use RFC3339 or 2006-01-02T15:04)", s)
}

// ---- dispatch -------------------------------------------------------------

// call is what a leaf command receives: the positional arguments it was
// given, the flag tail that followed them, and the usage line to quote
// when something is wrong.
type call struct {
	pos   []string
	flags []string
	usage string
}

// parse reads the flag tail and refuses what is left over: "stattii
// event cancel ev_1 --reason storm oops" used to drop "oops" without a
// word, and a mistyped id is exactly what hides there.
func (c call) parse(fs *flag.FlagSet) error {
	fs.Parse(c.flags)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q — usage: %s", fs.Arg(0), c.usage)
	}
	return nil
}

// leaf is one command. spec is both the usage line and the arity
// contract: `<x>` is a required positional, `[x]` an optional one, and
// the first token starting with a dash begins the flag syntax the leaf
// parses itself. One string, so usage and check cannot drift apart.
type leaf struct {
	name string
	spec string
	help string
	run  func(call) error
}

// group is a set of leaves under one word (event, person, …): dispatch,
// usage, arity and --help for all of them in one place, instead of the
// near-identical switch statements this replaces.
type group struct {
	name   string
	help   string
	usage  string // overrides the "a|b|c" list where a group needs prose
	leaves []leaf
}

// arity derives the positional-argument window from the spec, and
// whether the command takes flags at all.
func arity(spec string) (min, max int, flags bool) {
	for _, tok := range strings.Fields(spec) {
		if isFlagToken(tok) {
			return min, max, true // flag syntax from here on
		}
		switch {
		case strings.HasPrefix(tok, "<"):
			min++
			max++
		case strings.HasPrefix(tok, "["):
			max++
		}
	}
	return min, max, false
}

func isFlagToken(tok string) bool {
	return strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "[-")
}

// splitArgs cuts the argument list where the flags begin — positionals
// come first everywhere in this CLI ("event move <id> --at ..."). A bare
// "--" escapes exactly the argument behind it, which is how a value that
// starts with a dash gets through (an imported series uid is foreign
// data); escaping one argument rather than the whole tail keeps the
// flags after it working.
func splitArgs(args []string) (pos, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 >= len(args) {
				return pos, args[i:] // escapes nothing — an argument too many
			}
			pos = append(pos, args[i+1])
			i++
			continue
		}
		if len(a) > 1 && strings.HasPrefix(a, "-") {
			return pos, args[i:]
		}
		pos = append(pos, a)
	}
	return pos, nil
}

// dashHint names the escape when the argument in the way looks like a
// flag: the caller may have meant it as a value.
func dashHint(flags []string) string {
	if len(flags) == 0 || !strings.HasPrefix(flags[0], "-") {
		return ""
	}
	return fmt.Sprintf(" (to pass %q as a value: -- %s)", flags[0], flags[0])
}

func (l leaf) dispatch(prefix string, args []string) error {
	usage := strings.TrimSpace(prefix + " " + l.name + " " + l.spec)
	if len(args) > 0 && isHelpArg(args[0]) {
		fmt.Printf("usage: %s\n      %s\n", usage, l.help)
		return nil
	}
	pos, flags := splitArgs(args)
	min, max, takesFlags := arity(l.spec)
	if len(pos) < min || len(pos) > max {
		return fmt.Errorf("usage: %s%s", usage, dashHint(flags))
	}
	if len(flags) > 0 && !takesFlags {
		// Nothing downstream would look at these: a command without
		// flags has no FlagSet to refuse them, so this is the only place
		// that can. "event rm ev_1 --force" must not delete anything.
		return fmt.Errorf("unexpected argument %q%s — usage: %s", flags[0], dashHint(flags), usage)
	}
	return l.run(call{pos: pos, flags: flags, usage: usage})
}

func (g group) dispatch(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stattii %s %s", g.name, g.usageLine())
	}
	if isHelpArg(args[0]) {
		g.printHelp(os.Stdout)
		return nil
	}
	for _, l := range g.leaves {
		if l.name == args[0] {
			return l.dispatch("stattii "+g.name, args[1:])
		}
	}
	return fmt.Errorf("unknown %s subcommand %q", g.name, args[0])
}

func (g group) usageLine() string {
	if g.usage != "" {
		return g.usage
	}
	names := make([]string, len(g.leaves))
	for i, l := range g.leaves {
		names[i] = l.name
	}
	return strings.Join(names, "|")
}

func isHelpArg(a string) bool { return a == "--help" || a == "-h" || a == "help" }

// printHelp answers "stattii event --help" — the question that used to
// come back as an error about an unknown subcommand.
func (g group) printHelp(w io.Writer) {
	fmt.Fprintf(w, "stattii %s — %s\n\n", g.name, g.help)
	for _, l := range g.leaves {
		fmt.Fprintf(w, "  stattii %s\n      %s\n",
			strings.TrimSpace(g.name+" "+l.name+" "+l.spec), l.help)
	}
	fmt.Fprint(w, "\nArguments a command does not know are an error, never ignored.\n"+
		"An argument that starts with a dash needs \"--\" in front of it\n"+
		"(e.g. stattii series-assign -- -odd-uid pe_1).\n")
}

// ---- the table ------------------------------------------------------------

// clientGroups and clientLeaves ARE the CLI: one row per command,
// carrying its own usage line, its own help text, and its own request.
var clientGroups = []group{
	{
		name: "event", help: "events and everything that hangs off one",
		leaves: []leaf{
			{"list", "", "list all events", func(c call) error {
				return apiSend("GET", "/api/v1/events", nil)
			}},
			{"create", "--title ... --at ... [--end ...] [--location ...] [--note ...] [--if-unconfirmed notify|cancel]",
				"create an event", func(c call) error {
					fs := flag.NewFlagSet("event create", flag.ExitOnError)
					title := fs.String("title", "", "event title (required)")
					at := fs.String("at", "", "start time (required)")
					end := fs.String("end", "", "end time")
					location := fs.String("location", "", "location")
					note := fs.String("note", "", "note")
					ifUnconfirmed := fs.String("if-unconfirmed", "notify", "notify | cancel (dead-man-switch)")
					if err := c.parse(fs); err != nil {
						return err
					}
					start, err := parseWhen(*at)
					if err != nil {
						return err
					}
					endT, err := parseWhen(*end)
					if err != nil {
						return err
					}
					return apiSend("POST", "/api/v1/events", map[string]any{
						"title": *title, "location": *location, "note": *note,
						"starts_at": start, "ends_at": endT, "if_unconfirmed": *ifUnconfirmed,
					})
				}},
			{"show", "<event-id>", "print one event as JSON", func(c call) error {
				return apiSend("GET", "/api/v1/events/"+c.pos[0], nil)
			}},
			{"confirm", "<event-id>", "confirm it on the operator's behalf", func(c call) error {
				return apiSend("POST", "/api/v1/events/"+c.pos[0]+"/confirm", map[string]any{})
			}},
			{"cancel", "<event-id> [--reason ...]", "cancel it and propagate outward", func(c call) error {
				fs := flag.NewFlagSet("event cancel", flag.ExitOnError)
				reason := fs.String("reason", "", "why the event is cancelled")
				if err := c.parse(fs); err != nil {
					return err
				}
				return apiSend("POST", "/api/v1/events/"+c.pos[0]+"/cancel", map[string]string{"reason": *reason})
			}},
			{"reinstate", "<event-id>", "withdraw a cancellation", func(c call) error {
				return apiSend("POST", "/api/v1/events/"+c.pos[0]+"/reinstate", map[string]any{})
			}},
			{"move", "<event-id> --at ... [--end ...] [--note ...]", "move it and tell everyone", func(c call) error {
				fs := flag.NewFlagSet("event move", flag.ExitOnError)
				at := fs.String("at", "", "new start time (required)")
				end := fs.String("end", "", "new end time")
				note := fs.String("note", "", "note")
				if err := c.parse(fs); err != nil {
					return err
				}
				if *at == "" {
					return fmt.Errorf("--at is required: a move without a new start would notify everyone of a move to nowhere")
				}
				start, err := parseWhen(*at)
				if err != nil {
					return err
				}
				endT, err := parseWhen(*end)
				if err != nil {
					return err
				}
				return apiSend("POST", "/api/v1/events/"+c.pos[0]+"/move", map[string]any{
					"starts_at": start, "ends_at": endT, "note": *note,
				})
			}},
			{"links", "<event-id> <person-id>", "mint the personal yes/no links", func(c call) error {
				return apiSend("POST", "/api/v1/events/"+c.pos[0]+"/links", map[string]string{"person_id": c.pos[1]})
			}},
			{"revoke-links", "<event-id> [person-id]", "revoke them again", func(c call) error {
				path := "/api/v1/events/" + c.pos[0] + "/links"
				if len(c.pos) > 1 {
					path += "?person_id=" + c.pos[1]
				}
				return apiSend("DELETE", path, nil)
			}},
			{"responses", "<event-id>", "who answered what", func(c call) error {
				return apiSend("GET", "/api/v1/events/"+c.pos[0]+"/responses", nil)
			}},
			{"propagation", "<event-id>", "is the cancellation actually out", func(c call) error {
				return apiSend("GET", "/api/v1/events/"+c.pos[0]+"/propagation", nil)
			}},
			{"invite", "<event-id> [--revoke]", "mint or revoke the shared guest link", func(c call) error {
				fs := flag.NewFlagSet("event invite", flag.ExitOnError)
				revoke := fs.Bool("revoke", false, "revoke the current invite link (guests keep getting notices)")
				if err := c.parse(fs); err != nil {
					return err
				}
				if *revoke {
					return apiSend("DELETE", "/api/v1/events/"+c.pos[0]+"/invite", nil)
				}
				return apiSend("POST", "/api/v1/events/"+c.pos[0]+"/invite", map[string]any{})
			}},
			{"guests", "<event-id> [--remove <guest-id>]", "list the guests, or remove one", func(c call) error {
				fs := flag.NewFlagSet("event guests", flag.ExitOnError)
				remove := fs.String("remove", "", "remove one guest by id")
				if err := c.parse(fs); err != nil {
					return err
				}
				if *remove != "" {
					return apiSend("DELETE", "/api/v1/events/"+c.pos[0]+"/guests/"+*remove, nil)
				}
				return apiSend("GET", "/api/v1/events/"+c.pos[0]+"/guests", nil)
			}},
			{"rm", "<event-id>", "delete a cancelled or finished event for good", func(c call) error {
				return apiSend("DELETE", "/api/v1/events/"+c.pos[0], nil)
			}},
		},
	},
	{
		name: "person", help: "the people who are responsible",
		leaves: []leaf{
			{"list", "", "list everyone", func(c call) error {
				return apiSend("GET", "/api/v1/people", nil)
			}},
			{"add", "--name ... [--trust respond|propose|direct] [--email ...] [--telegram ...]",
				"add a person", func(c call) error {
					fs := flag.NewFlagSet("person add", flag.ExitOnError)
					name := fs.String("name", "", "name (required)")
					trust := fs.String("trust", "respond", "respond | propose | direct")
					email := fs.String("email", "", "email address")
					telegram := fs.String("telegram", "", "telegram chat id")
					if err := c.parse(fs); err != nil {
						return err
					}
					var channels []map[string]string
					if *email != "" {
						channels = append(channels, map[string]string{"kind": "email", "to": *email})
					}
					if *telegram != "" {
						channels = append(channels, map[string]string{"kind": "telegram", "to": *telegram})
					}
					return apiSend("POST", "/api/v1/people", map[string]any{"name": *name, "trust": *trust, "channels": channels})
				}},
			{"set", "<person-id> [--name ...] [--trust ...] [--email ...] [--telegram ...]",
				"patch one person — flags you leave out stay untouched", cmdPersonSet},
			{"test", "<person-id>", "send a test message to every channel", func(c call) error {
				return apiSend("POST", "/api/v1/people/"+c.pos[0]+"/test-message", nil)
			}},
			{"rotate-portal", "<person-id>", "mint a fresh portal token", func(c call) error {
				return apiSend("POST", "/api/v1/people/"+c.pos[0]+"/rotate-portal", nil)
			}},
			{"rm", "<person-id>", "delete a person nobody is waiting on", func(c call) error {
				return apiSend("DELETE", "/api/v1/people/"+c.pos[0], nil)
			}},
		},
	},
	{
		name: "broadcast", help: "audience-facing targets that get propagation notices",
		leaves: []leaf{
			{"list", "", "list the targets", func(c call) error {
				return apiSend("GET", "/api/v1/broadcasts", nil)
			}},
			{"add", "--kind email|telegram|webhook --to ... [--name ...]", "add one", func(c call) error {
				fs := flag.NewFlagSet("broadcast add", flag.ExitOnError)
				name := fs.String("name", "", "label")
				kind := fs.String("kind", "", "email | telegram | webhook (required)")
				to := fs.String("to", "", "address / chat id / URL (required)")
				if err := c.parse(fs); err != nil {
					return err
				}
				return apiSend("POST", "/api/v1/broadcasts", map[string]string{"name": *name, "kind": *kind, "to": *to})
			}},
			{"rm", "<id>", "remove one", func(c call) error {
				return apiSend("DELETE", "/api/v1/broadcasts/"+c.pos[0], nil)
			}},
		},
	},
	{
		name: "webhook", help: "signed JSON subscriptions",
		leaves: []leaf{
			{"list", "", "list the subscriptions (secrets redacted)", func(c call) error {
				return apiSend("GET", "/api/v1/webhooks", nil)
			}},
			{"add", "--url ... [--events a,b]", "subscribe (the secret is printed once)", func(c call) error {
				fs := flag.NewFlagSet("webhook add", flag.ExitOnError)
				target := fs.String("url", "", "target URL (required)")
				events := fs.String("events", "", "comma-separated filter (empty = all)")
				if err := c.parse(fs); err != nil {
					return err
				}
				var evs []string
				if *events != "" {
					evs = strings.Split(*events, ",")
				}
				return apiSend("POST", "/api/v1/webhooks", map[string]any{"url": *target, "events": evs})
			}},
			{"rm", "<id>", "unsubscribe", func(c call) error {
				return apiSend("DELETE", "/api/v1/webhooks/"+c.pos[0], nil)
			}},
		},
	},
	{
		name: "proposal", help: "change requests waiting for a decision",
		leaves: []leaf{
			{"list", "", "list them", func(c call) error {
				return apiSend("GET", "/api/v1/proposals", nil)
			}},
			{"accept", "<id>", "accept and apply it", func(c call) error {
				return apiSend("POST", "/api/v1/proposals/"+c.pos[0]+"/decide", map[string]bool{"accept": true})
			}},
			{"reject", "<id>", "turn it down", func(c call) error {
				return apiSend("POST", "/api/v1/proposals/"+c.pos[0]+"/decide", map[string]bool{"accept": false})
			}},
		},
	},
	{
		name: "outbox", help: "what went out, and what is stuck",
		usage: "list [--pending] | retry <id>",
		leaves: []leaf{
			{"list", "[--pending]", "list the outbox", func(c call) error {
				fs := flag.NewFlagSet("outbox list", flag.ExitOnError)
				pending := fs.Bool("pending", false, "only undelivered items")
				if err := c.parse(fs); err != nil {
					return err
				}
				path := "/api/v1/outbox"
				if *pending {
					path += "?pending=1"
				}
				return apiSend("GET", path, nil)
			}},
			{"retry", "<id>", "re-arm one item for the next tick", func(c call) error {
				return apiSend("POST", "/api/v1/outbox/"+c.pos[0]+"/retry", nil)
			}},
		},
	},
	{
		name: "calendar", help: "the imported source feed",
		leaves: []leaf{
			{"fetch", "", "fetch and sync the source feed now", func(c call) error {
				return apiSend("POST", "/api/v1/calendar/fetch", nil)
			}},
		},
	},
}

var clientLeaves = []leaf{
	{"overview", "[--all]", "the operator's one-glance view", cmdOverview},
	{"assign", "<event-id> <person-id> [role]", "make someone responsible", func(c call) error {
		return apiSend("POST", "/api/v1/assignments", map[string]string{
			"event_id": c.pos[0], "person_id": c.pos[1], "role": posOr(c.pos, 2),
		})
	}},
	{"unassign", "<event-id> <person-id>", "take that responsibility away", func(c call) error {
		return apiSend("DELETE", "/api/v1/events/"+c.pos[0]+"/assignees/"+c.pos[1], nil)
	}},
	{"series-assign", "<source-uid> <person-id> [role]", "make someone responsible for a whole series", func(c call) error {
		return apiSend("POST", "/api/v1/series-assignments", map[string]string{
			"source_uid": c.pos[0], "person_id": c.pos[1], "role": posOr(c.pos, 2),
		})
	}},
	{"series-unassign", "<source-uid> <person-id>", "and take it away again", func(c call) error {
		q := url.Values{"source_uid": {c.pos[0]}, "person_id": {c.pos[1]}}
		return apiSend("DELETE", "/api/v1/series-assignments?"+q.Encode(), nil)
	}},
	{"audit", "[--limit N]", "the append-only journal", func(c call) error {
		fs := flag.NewFlagSet("audit", flag.ExitOnError)
		limit := fs.Int("limit", 200, "max entries")
		if err := c.parse(fs); err != nil {
			return err
		}
		return apiSend("GET", fmt.Sprintf("/api/v1/audit?limit=%d", *limit), nil)
	}},
	{"tick", "", "run one scheduler pass now", func(c call) error {
		return apiSend("POST", "/api/v1/tick", nil)
	}},
}

// posOr reads an optional positional argument.
func posOr(pos []string, i int) string {
	if i < len(pos) {
		return pos[i]
	}
	return ""
}

func cmdClient(args []string) error {
	cmd, rest := args[0], args[1:]
	for _, g := range clientGroups {
		if g.name == cmd {
			return g.dispatch(rest)
		}
	}
	for _, l := range clientLeaves {
		if l.name == cmd {
			return l.dispatch("stattii", rest)
		}
	}
	usage()
	return fmt.Errorf("unknown command %q", cmd)
}

// cmdPersonSet is a patch: only flags actually given are sent, so
// `--name` alone does not reset trust or wipe the channels. A channel
// flag edits its one slot (`--email ""` drops the email, nothing else)
// — the API replaces the whole list, so the CLI reads the current one
// and rebuilds it the way the panel form does.
func cmdPersonSet(c call) error {
	fs := flag.NewFlagSet("person set", flag.ExitOnError)
	name := fs.String("name", "", "new name")
	trust := fs.String("trust", "", "respond | propose | direct")
	email := fs.String("email", "", "email address (empty drops it)")
	telegram := fs.String("telegram", "", "telegram chat id (empty drops it)")
	if err := c.parse(fs); err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	patch := map[string]any{}
	if set["name"] {
		patch["name"] = *name
	}
	if set["trust"] {
		patch["trust"] = *trust
	}
	if set["email"] || set["telegram"] {
		var people []core.Person
		if err := apiFetch("/api/v1/people", &people); err != nil {
			return err
		}
		var current *core.Person
		for i := range people {
			if people[i].ID == c.pos[0] {
				current = &people[i]
			}
		}
		if current == nil {
			return fmt.Errorf("person %s not found", c.pos[0])
		}
		var e, tg *string
		if set["email"] {
			e = email
		}
		if set["telegram"] {
			tg = telegram
		}
		patch["channels"] = core.PatchChannels(current.Channels, e, tg)
	}
	if len(patch) == 0 {
		return fmt.Errorf("nothing to change — give at least one of --name, --trust, --email, --telegram")
	}
	return apiSend("PATCH", "/api/v1/people/"+c.pos[0], patch)
}

// cmdOverview renders the operator's one-glance view: upcoming events,
// who is responsible, who answered what, and outbox/proposal health.
func cmdOverview(c call) error {
	fs := flag.NewFlagSet("overview", flag.ExitOnError)
	all := fs.Bool("all", false, "include past events")
	if err := c.parse(fs); err != nil {
		return err
	}

	var ov core.Overview
	if err := apiFetch("/api/v1/overview", &ov); err != nil {
		return err
	}

	now := time.Now()
	shown := 0
	for _, oe := range ov.Events {
		e := oe.Event
		if !*all && e.EndsAt.Before(now) && e.StartsAt.Before(now) {
			continue
		}
		shown++
		extra := ""
		if !e.ReminderSentAt.IsZero() {
			extra = " · reminder sent"
		}
		fmt.Printf("%s  %s  [%s]%s\n",
			e.StartsAt.Local().Format("Mon 02 Jan 15:04"), e.Title, e.Status, extra)
		if oe.Reachable == 0 {
			fmt.Printf("    (nobody reachable — the reminder waits, the deadline does not)\n")
		}
		for _, a := range oe.Assignees {
			mark, detail := "–", "no response yet"
			switch a.Action {
			case core.ActionConfirm:
				mark, detail = "✓", fmt.Sprintf("confirmed via %s, %s", a.Via, a.At.Local().Format("02 Jan 15:04"))
			case core.ActionCancel:
				mark, detail = "✗", fmt.Sprintf("cancelled via %s, %s", a.Via, a.At.Local().Format("02 Jan 15:04"))
			}
			role := ""
			if a.Role != "" {
				role = " (" + a.Role + ")"
			}
			if !a.Reachable {
				role += " [no channel]"
			} else if a.ChannelProblem {
				role += " [channel looks broken]"
			}
			fmt.Printf("    %s %s%s — %s\n", mark, a.Name, role, detail)
		}
	}
	if shown == 0 {
		fmt.Println("(no upcoming events — use --all for past ones)")
	}
	fmt.Printf("\noutbox: %d delivered · %d pending · %d failed",
		ov.Outbox.Delivered, ov.Outbox.Pending, ov.Outbox.Failed)
	if ov.OpenProposals > 0 {
		fmt.Printf(" · %d open proposal(s)!", ov.OpenProposals)
	}
	fmt.Printf("\npeople: %d\n", ov.People)
	return nil
}

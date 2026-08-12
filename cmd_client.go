// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
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
	fmt.Println(strings.TrimSpace(string(raw)))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
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

func cmdClient(args []string) error {
	cmd := args[0]
	rest := args[1:]
	switch cmd {
	case "overview":
		return cmdOverview(rest)
	case "calendar":
		if len(rest) < 1 || rest[0] != "fetch" {
			return fmt.Errorf("usage: stattii calendar fetch")
		}
		return api("POST", "/api/v1/calendar/fetch", nil)
	case "series-assign":
		if len(rest) < 2 {
			return fmt.Errorf("usage: stattii series-assign <source-uid> <person-id> [role]")
		}
		role := ""
		if len(rest) > 2 {
			role = rest[2]
		}
		return api("POST", "/api/v1/series-assignments", map[string]string{
			"source_uid": rest[0], "person_id": rest[1], "role": role,
		})
	case "event":
		return cmdEvent(rest)
	case "person":
		return cmdPerson(rest)
	case "assign":
		if len(rest) < 2 {
			return fmt.Errorf("usage: stattii assign <event-id> <person-id> [role]")
		}
		role := ""
		if len(rest) > 2 {
			role = rest[2]
		}
		return api("POST", "/api/v1/assignments", map[string]string{"event_id": rest[0], "person_id": rest[1], "role": role})
	case "broadcast":
		return cmdBroadcast(rest)
	case "webhook":
		return cmdWebhook(rest)
	case "proposal":
		return cmdProposal(rest)
	case "outbox":
		return cmdOutbox(rest)
	case "audit":
		fs := flag.NewFlagSet("audit", flag.ExitOnError)
		limit := fs.Int("limit", 200, "max entries")
		fs.Parse(rest)
		return api("GET", fmt.Sprintf("/api/v1/audit?limit=%d", *limit), nil)
	case "tick":
		return api("POST", "/api/v1/tick", nil)
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func cmdEvent(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stattii event list|create|show|confirm|cancel|move|links|responses|propagation")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return api("GET", "/api/v1/events", nil)
	case "create":
		fs := flag.NewFlagSet("event create", flag.ExitOnError)
		title := fs.String("title", "", "event title (required)")
		at := fs.String("at", "", "start time (required)")
		end := fs.String("end", "", "end time")
		location := fs.String("location", "", "location")
		note := fs.String("note", "", "note")
		ifUnconfirmed := fs.String("if-unconfirmed", "notify", "notify | cancel (dead-man-switch)")
		fs.Parse(rest)
		start, err := parseWhen(*at)
		if err != nil {
			return err
		}
		endT, err := parseWhen(*end)
		if err != nil {
			return err
		}
		return api("POST", "/api/v1/events", map[string]any{
			"title": *title, "location": *location, "note": *note,
			"starts_at": start, "ends_at": endT, "if_unconfirmed": *ifUnconfirmed,
		})
	case "show", "confirm", "reinstate", "responses", "propagation":
		if len(rest) < 1 {
			return fmt.Errorf("usage: stattii event %s <event-id>", sub)
		}
		id := rest[0]
		switch sub {
		case "show":
			return api("GET", "/api/v1/events/"+id, nil)
		case "confirm":
			return api("POST", "/api/v1/events/"+id+"/confirm", map[string]any{})
		case "reinstate":
			return api("POST", "/api/v1/events/"+id+"/reinstate", map[string]any{})
		case "responses":
			return api("GET", "/api/v1/events/"+id+"/responses", nil)
		default:
			return api("GET", "/api/v1/events/"+id+"/propagation", nil)
		}
	case "cancel":
		if len(rest) < 1 {
			return fmt.Errorf("usage: stattii event cancel <event-id> [--reason ...]")
		}
		fs := flag.NewFlagSet("event cancel", flag.ExitOnError)
		reason := fs.String("reason", "", "why the event is cancelled")
		fs.Parse(rest[1:])
		return api("POST", "/api/v1/events/"+rest[0]+"/cancel", map[string]string{"reason": *reason})
	case "move":
		if len(rest) < 1 {
			return fmt.Errorf("usage: stattii event move <event-id> --at ... [--end ...] [--note ...]")
		}
		fs := flag.NewFlagSet("event move", flag.ExitOnError)
		at := fs.String("at", "", "new start time (required)")
		end := fs.String("end", "", "new end time")
		note := fs.String("note", "", "note")
		fs.Parse(rest[1:])
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
		return api("POST", "/api/v1/events/"+rest[0]+"/move", map[string]any{
			"starts_at": start, "ends_at": endT, "note": *note,
		})
	case "links":
		if len(rest) < 2 {
			return fmt.Errorf("usage: stattii event links <event-id> <person-id>")
		}
		return api("POST", "/api/v1/events/"+rest[0]+"/links", map[string]string{"person_id": rest[1]})
	default:
		return fmt.Errorf("unknown event subcommand %q", sub)
	}
}

func cmdPerson(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stattii person list|add|test")
	}
	switch args[0] {
	case "list":
		return api("GET", "/api/v1/people", nil)
	case "test":
		if len(args) < 2 {
			return fmt.Errorf("usage: stattii person test <person-id>")
		}
		return api("POST", "/api/v1/people/"+args[1]+"/test-message", nil)
	case "add":
		fs := flag.NewFlagSet("person add", flag.ExitOnError)
		name := fs.String("name", "", "name (required)")
		trust := fs.String("trust", "respond", "respond | propose | direct")
		email := fs.String("email", "", "email address")
		telegram := fs.String("telegram", "", "telegram chat id")
		fs.Parse(args[1:])
		var channels []map[string]string
		if *email != "" {
			channels = append(channels, map[string]string{"kind": "email", "to": *email})
		}
		if *telegram != "" {
			channels = append(channels, map[string]string{"kind": "telegram", "to": *telegram})
		}
		return api("POST", "/api/v1/people", map[string]any{"name": *name, "trust": *trust, "channels": channels})
	default:
		return fmt.Errorf("unknown person subcommand %q", args[0])
	}
}

func cmdBroadcast(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stattii broadcast list|add|rm")
	}
	switch args[0] {
	case "list":
		return api("GET", "/api/v1/broadcasts", nil)
	case "add":
		fs := flag.NewFlagSet("broadcast add", flag.ExitOnError)
		name := fs.String("name", "", "label")
		kind := fs.String("kind", "", "email | telegram | webhook (required)")
		to := fs.String("to", "", "address / chat id / URL (required)")
		fs.Parse(args[1:])
		return api("POST", "/api/v1/broadcasts", map[string]string{"name": *name, "kind": *kind, "to": *to})
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: stattii broadcast rm <id>")
		}
		return api("DELETE", "/api/v1/broadcasts/"+args[1], nil)
	default:
		return fmt.Errorf("unknown broadcast subcommand %q", args[0])
	}
}

func cmdWebhook(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stattii webhook list|add|rm")
	}
	switch args[0] {
	case "list":
		return api("GET", "/api/v1/webhooks", nil)
	case "add":
		fs := flag.NewFlagSet("webhook add", flag.ExitOnError)
		url := fs.String("url", "", "target URL (required)")
		events := fs.String("events", "", "comma-separated filter (empty = all)")
		fs.Parse(args[1:])
		var evs []string
		if *events != "" {
			evs = strings.Split(*events, ",")
		}
		return api("POST", "/api/v1/webhooks", map[string]any{"url": *url, "events": evs})
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: stattii webhook rm <id>")
		}
		return api("DELETE", "/api/v1/webhooks/"+args[1], nil)
	default:
		return fmt.Errorf("unknown webhook subcommand %q", args[0])
	}
}

func cmdOutbox(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stattii outbox list [--pending] | retry <id>")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("outbox list", flag.ExitOnError)
		pending := fs.Bool("pending", false, "only undelivered items")
		fs.Parse(args[1:])
		path := "/api/v1/outbox"
		if *pending {
			path += "?pending=1"
		}
		return api("GET", path, nil)
	case "retry":
		if len(args) < 2 {
			return fmt.Errorf("usage: stattii outbox retry <id>")
		}
		return api("POST", "/api/v1/outbox/"+args[1]+"/retry", nil)
	default:
		return fmt.Errorf("unknown outbox subcommand %q", args[0])
	}
}

func cmdProposal(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stattii proposal list|accept|reject")
	}
	switch args[0] {
	case "list":
		return api("GET", "/api/v1/proposals", nil)
	case "accept", "reject":
		if len(args) < 2 {
			return fmt.Errorf("usage: stattii proposal %s <id>", args[0])
		}
		return api("POST", "/api/v1/proposals/"+args[1]+"/decide", map[string]bool{"accept": args[0] == "accept"})
	default:
		return fmt.Errorf("unknown proposal subcommand %q", args[0])
	}
}

// cmdOverview renders the operator's one-glance view: upcoming events,
// who is responsible, who answered what, and outbox/proposal health.
func cmdOverview(args []string) error {
	fs := flag.NewFlagSet("overview", flag.ExitOnError)
	all := fs.Bool("all", false, "include past events")
	fs.Parse(args)

	var ov core.Overview
	if err := apiJSON("/api/v1/overview", &ov); err != nil {
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
		if len(oe.Assignees) == 0 {
			fmt.Printf("    (nobody assigned — reminder waits, dead-man-switch does not)\n")
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

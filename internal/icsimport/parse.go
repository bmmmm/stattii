// SPDX-License-Identifier: GPL-3.0-or-later

// Package icsimport parses a foreign iCalendar feed and expands its
// events into concrete occurrences inside a time window. It is the
// inbound counterpart to package ics (outbound feed generation) and
// deliberately depends on nothing but the stdlib. In scope since the
// 2026-08-12 owner decision; the feed URL is operator configuration —
// never repository data, test fixtures are synthetic.
package icsimport

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Event is one parsed VEVENT. A non-zero RecurrenceID marks an override
// for that single occurrence of the series with the same UID.
type Event struct {
	UID          string
	Summary      string
	Location     string
	Start        time.Time
	End          time.Time
	AllDay       bool
	RRule        string
	ExDates      []time.Time
	RDates       []time.Time
	RecurrenceID time.Time
	Status       string
	Sequence     int
}

// Parse extracts the VEVENTs from raw iCalendar data. Events whose
// times cannot be parsed are returned as Skipped, never dropped.
func Parse(raw []byte) ([]Event, []Skipped) {
	var (
		events  []Event
		skipped []Skipped
		cur     *Event
		curErr  string
	)
	for _, line := range unfold(raw) {
		name, params, value := splitProp(line)
		switch name {
		case "BEGIN":
			if value == "VEVENT" {
				cur, curErr = &Event{}, ""
			}
		case "END":
			if value != "VEVENT" || cur == nil {
				continue
			}
			switch {
			case curErr != "":
				skipped = append(skipped, Skipped{UID: cur.UID, Summary: cur.Summary, Reason: curErr})
			case cur.UID == "" || cur.Start.IsZero():
				skipped = append(skipped, Skipped{UID: cur.UID, Summary: cur.Summary, Reason: "missing UID or DTSTART"})
			default:
				if cur.End.IsZero() {
					cur.End = cur.Start
					if cur.AllDay {
						cur.End = cur.Start.AddDate(0, 0, 1)
					}
				}
				events = append(events, *cur)
			}
			cur = nil
		}
		if cur == nil {
			continue
		}
		switch name {
		case "UID":
			cur.UID = value
		case "SUMMARY":
			cur.Summary = unescapeText(value)
		case "LOCATION":
			cur.Location = unescapeText(value)
		case "STATUS":
			cur.Status = strings.ToUpper(value)
		case "SEQUENCE":
			cur.Sequence, _ = strconv.Atoi(value)
		case "RRULE":
			cur.RRule = value
		case "DTSTART":
			t, allDay, err := parseICSTime(value, params)
			if err != nil {
				curErr = "DTSTART: " + err.Error()
				continue
			}
			cur.Start, cur.AllDay = t, allDay
		case "DTEND":
			t, _, err := parseICSTime(value, params)
			if err != nil {
				curErr = "DTEND: " + err.Error()
				continue
			}
			cur.End = t
		case "RECURRENCE-ID":
			t, _, err := parseICSTime(value, params)
			if err != nil {
				curErr = "RECURRENCE-ID: " + err.Error()
				continue
			}
			cur.RecurrenceID = t
		case "EXDATE":
			// EXDATE may carry a comma-separated list of date-times.
			for _, v := range strings.Split(value, ",") {
				t, _, err := parseICSTime(strings.TrimSpace(v), params)
				if err != nil {
					curErr = "EXDATE: " + err.Error()
					break
				}
				cur.ExDates = append(cur.ExDates, t)
			}
		case "RDATE":
			// RDATE adds single date-times to the recurrence set.
			// VALUE=PERIOD (start/end pairs) does not appear in the feeds
			// we import from; refusing it loudly beats mis-parsing it
			// silently (issue #11: silent gaps are the locked-door bug).
			if strings.EqualFold(params["VALUE"], "PERIOD") {
				curErr = "RDATE: PERIOD values are not supported"
				continue
			}
			for _, v := range strings.Split(value, ",") {
				t, _, err := parseICSTime(strings.TrimSpace(v), params)
				if err != nil {
					curErr = "RDATE: " + err.Error()
					break
				}
				cur.RDates = append(cur.RDates, t)
			}
		}
	}
	return events, skipped
}

// unfold splits raw data into logical lines: a line starting with space
// or tab continues the previous one (RFC 5545 folding). The current
// line is accumulated in a builder — appending to the last string of
// the slice instead would copy it once per continuation, which a feed
// of a million one-byte folds turns into quadratic work.
func unfold(raw []byte) []string {
	physical := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var (
		lines   []string
		cur     strings.Builder
		started bool
	)
	flush := func() {
		if started {
			lines = append(lines, cur.String())
			cur.Reset()
			started = false
		}
	}
	for _, l := range physical {
		if l == "" {
			continue
		}
		if (l[0] == ' ' || l[0] == '\t') && started {
			cur.WriteString(l[1:])
			continue
		}
		flush()
		cur.WriteString(l)
		started = true
	}
	flush()
	return lines
}

// splitProp splits "NAME;PARAM=A;OTHER="q:v":VALUE" into its parts,
// honouring quotes (param values may contain ':' and ';').
func splitProp(line string) (name string, params map[string]string, value string) {
	inQuote := false
	head := line
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case ':':
			if !inQuote {
				head, value = line[:i], line[i+1:]
				i = len(line)
			}
		}
	}
	parts := strings.Split(head, ";")
	name = strings.ToUpper(parts[0])
	params = map[string]string{}
	for _, p := range parts[1:] {
		if k, v, ok := strings.Cut(p, "="); ok {
			params[strings.ToUpper(k)] = strings.Trim(v, `"`)
		}
	}
	return name, params, value
}

func unescapeText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n', 'N':
			b.WriteByte('\n')
		default: // \, \; \\
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// parseICSTime handles the three iCalendar time shapes: date-only
// (all-day), UTC ("...Z") and local time with an optional TZID.
func parseICSTime(value string, params map[string]string) (t time.Time, allDay bool, err error) {
	if params["VALUE"] == "DATE" || len(value) == 8 {
		t, err = time.ParseInLocation("20060102", value, time.Local)
		return t, true, err
	}
	if strings.HasSuffix(value, "Z") {
		t, err = time.Parse("20060102T150405Z", value)
		return t, false, err
	}
	loc := time.Local
	if tzid := params["TZID"]; tzid != "" {
		loc, err = time.LoadLocation(tzid)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("unknown TZID %q", tzid)
		}
	}
	t, err = time.ParseInLocation("20060102T150405", value, loc)
	return t, false, err
}

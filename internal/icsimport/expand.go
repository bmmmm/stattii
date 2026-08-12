// SPDX-License-Identifier: GPL-3.0-or-later

package icsimport

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Occurrence is one concrete date a calendar event happens on. Key is
// stable across refetches — overrides keep the ORIGINAL start in the
// key so a moved single occurrence maps onto the same stattii event.
type Occurrence struct {
	Key      string // UID + "/" + original start, UTC RFC3339
	UID      string
	Summary  string
	Location string
	Start    time.Time
	End      time.Time
	AllDay   bool
}

// Skipped names an event or series the importer could not (or must
// not) expand — surfaced to the operator, never silently dropped.
type Skipped struct {
	UID     string
	Summary string
	Reason  string
}

func occurrenceKey(uid string, originalStart time.Time) string {
	return uid + "/" + originalStart.UTC().Format(time.RFC3339)
}

// Expand turns parsed events into occurrences within [from, to].
// Recurrence is resolved by enumerating from the series anchor, so
// COUNT and EXDATE stay correct even when the anchor lies in the past.
func Expand(events []Event, from, to time.Time) ([]Occurrence, []Skipped) {
	var (
		occs    []Occurrence
		skipped []Skipped
	)

	// Overrides (RECURRENCE-ID) replace single occurrences of their series.
	overrides := map[string]Event{}
	for _, e := range events {
		if !e.RecurrenceID.IsZero() {
			overrides[occurrenceKey(e.UID, e.RecurrenceID)] = e
		}
	}

	for _, e := range events {
		if !e.RecurrenceID.IsZero() {
			continue // handled via the override map
		}
		if e.Status == "CANCELLED" {
			skipped = append(skipped, Skipped{UID: e.UID, Summary: e.Summary, Reason: "cancelled in source"})
			continue
		}
		duration := e.End.Sub(e.Start)
		var starts []time.Time
		if e.RRule == "" {
			starts = []time.Time{e.Start}
		} else {
			var err error
			starts, err = expandRule(e, to)
			if err != nil {
				skipped = append(skipped, Skipped{UID: e.UID, Summary: e.Summary, Reason: err.Error()})
				continue
			}
		}
		for _, st := range starts {
			if excluded(st, e.ExDates) {
				continue
			}
			occ := Occurrence{
				Key: occurrenceKey(e.UID, st), UID: e.UID,
				Summary: e.Summary, Location: e.Location,
				Start: st, End: st.Add(duration), AllDay: e.AllDay,
			}
			if ov, ok := overrides[occ.Key]; ok {
				if ov.Status == "CANCELLED" {
					continue
				}
				occ.Summary, occ.Location, occ.AllDay = ov.Summary, ov.Location, ov.AllDay
				occ.Start, occ.End = ov.Start, ov.End
				if occ.End.IsZero() {
					occ.End = occ.Start.Add(duration)
				}
			}
			if occ.Start.Before(from) || occ.Start.After(to) {
				continue
			}
			occs = append(occs, occ)
		}
	}
	sort.Slice(occs, func(i, j int) bool { return occs[i].Start.Before(occs[j].Start) })
	return occs, skipped
}

func excluded(t time.Time, exdates []time.Time) bool {
	for _, x := range exdates {
		if t.Equal(x) {
			return true
		}
	}
	return false
}

// rule is the subset of RFC 5545 RRULE this importer understands —
// chosen to cover every pattern observed in real Nextcloud/Apple feeds
// (weekly/daily with UNTIL, monthly BYDAY ordinals incl. negatives,
// INTERVAL, BYSETPOS+BYMONTHDAY, yearly BYMONTH+BYDAY, COUNT).
type rule struct {
	Freq       string
	Interval   int
	Until      time.Time
	Count      int
	ByDay      []byDay
	ByMonth    []int
	ByMonthDay []int
	BySetPos   []int
}

type byDay struct {
	Ord     int // 0 = every, else ±n-th within the period
	Weekday time.Weekday
}

var weekdays = map[string]time.Weekday{
	"MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday, "SU": time.Sunday,
}

func parseRule(s string, loc *time.Location) (rule, error) {
	r := rule{Interval: 1}
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return r, fmt.Errorf("unsupported RRULE part %q", part)
		}
		switch strings.ToUpper(k) {
		case "FREQ":
			r.Freq = strings.ToUpper(v)
		case "INTERVAL":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return r, fmt.Errorf("bad INTERVAL %q", v)
			}
			r.Interval = n
		case "UNTIL":
			var t time.Time
			var err error
			switch {
			case strings.HasSuffix(v, "Z"):
				t, err = time.Parse("20060102T150405Z", v)
			case len(v) == 8:
				t, err = time.ParseInLocation("20060102", v, loc)
				t = t.AddDate(0, 0, 1).Add(-time.Second) // whole day inclusive
			default:
				t, err = time.ParseInLocation("20060102T150405", v, loc)
			}
			if err != nil {
				return r, fmt.Errorf("bad UNTIL %q", v)
			}
			r.Until = t
		case "COUNT":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return r, fmt.Errorf("bad COUNT %q", v)
			}
			r.Count = n
		case "BYDAY":
			for _, d := range strings.Split(v, ",") {
				d = strings.TrimSpace(d)
				ord := 0
				if len(d) > 2 {
					n, err := strconv.Atoi(d[:len(d)-2])
					if err != nil {
						return r, fmt.Errorf("bad BYDAY %q", d)
					}
					ord = n
				}
				wd, ok := weekdays[d[max(0, len(d)-2):]]
				if !ok {
					return r, fmt.Errorf("bad BYDAY %q", d)
				}
				r.ByDay = append(r.ByDay, byDay{Ord: ord, Weekday: wd})
			}
		case "BYMONTH":
			for _, m := range strings.Split(v, ",") {
				n, err := strconv.Atoi(m)
				if err != nil || n < 1 || n > 12 {
					return r, fmt.Errorf("bad BYMONTH %q", m)
				}
				r.ByMonth = append(r.ByMonth, n)
			}
		case "BYMONTHDAY":
			for _, m := range strings.Split(v, ",") {
				n, err := strconv.Atoi(m)
				if err != nil || n == 0 {
					return r, fmt.Errorf("bad BYMONTHDAY %q", m)
				}
				r.ByMonthDay = append(r.ByMonthDay, n)
			}
		case "BYSETPOS":
			for _, m := range strings.Split(v, ",") {
				n, err := strconv.Atoi(m)
				if err != nil || n == 0 {
					return r, fmt.Errorf("bad BYSETPOS %q", m)
				}
				r.BySetPos = append(r.BySetPos, n)
			}
		case "WKST":
			// week start only matters for WEEKLY+BYDAY with interval>1;
			// accepted and ignored (we use Monday, the RFC default).
		default:
			return r, fmt.Errorf("unsupported RRULE part %q", k)
		}
	}
	switch r.Freq {
	case "DAILY", "WEEKLY", "MONTHLY", "YEARLY":
	default:
		return r, fmt.Errorf("unsupported FREQ %q", r.Freq)
	}
	return r, nil
}

// expandRule enumerates the series from its anchor until the window end
// (or UNTIL/COUNT run out) and returns every start time. Times are
// constructed in the anchor's location, so wall-clock times survive DST.
func expandRule(e Event, windowEnd time.Time) ([]time.Time, error) {
	loc := e.Start.Location()
	r, err := parseRule(e.RRule, loc)
	if err != nil {
		return nil, err
	}
	end := windowEnd
	if !r.Until.IsZero() && r.Until.Before(end) {
		end = r.Until
	}

	h, mi, sec := e.Start.Clock()
	anchor := e.Start
	var out []time.Time
	add := func(t time.Time) bool { // false = enumeration budget exhausted
		if t.Before(anchor) || t.After(end) {
			return true
		}
		out = append(out, t)
		return r.Count == 0 || len(out) < r.Count
	}

	switch r.Freq {
	case "DAILY":
		for d, i := anchor, 0; !d.After(end); i++ {
			d = time.Date(anchor.Year(), anchor.Month(), anchor.Day()+i*r.Interval, h, mi, sec, 0, loc)
			if d.After(end) {
				break
			}
			if !add(d) {
				break
			}
		}
	case "WEEKLY":
		days := map[time.Weekday]bool{}
		for _, bd := range r.ByDay {
			days[bd.Weekday] = true
		}
		if len(days) == 0 {
			days[anchor.Weekday()] = true
		}
		anchorMonday := startOfISOWeek(anchor)
		for day := 0; ; day++ {
			d := time.Date(anchor.Year(), anchor.Month(), anchor.Day()+day, h, mi, sec, 0, loc)
			if d.After(end) {
				break
			}
			week := int(startOfISOWeek(d).Sub(anchorMonday).Hours() / (24 * 7))
			if week%r.Interval != 0 || !days[d.Weekday()] {
				continue
			}
			if !add(d) {
				break
			}
		}
	case "MONTHLY":
		y0, m0 := anchor.Year(), int(anchor.Month())
	monthly:
		for mIdx := 0; ; mIdx += r.Interval {
			y, m := y0+(m0-1+mIdx)/12, (m0-1+mIdx)%12+1
			periodStart := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, loc)
			if periodStart.After(end) {
				break
			}
			for _, d := range monthCandidates(r, anchor, y, time.Month(m), h, mi, sec, loc) {
				if !add(d) {
					break monthly
				}
			}
		}
	case "YEARLY":
		months := r.ByMonth
		if len(months) == 0 {
			months = []int{int(anchor.Month())}
		}
	yearly:
		for y := anchor.Year(); ; y += r.Interval {
			if time.Date(y, 1, 1, 0, 0, 0, 0, loc).After(end) {
				break
			}
			for _, m := range months {
				for _, d := range monthCandidates(r, anchor, y, time.Month(m), h, mi, sec, loc) {
					if !add(d) {
						break yearly
					}
				}
			}
		}
	}
	return out, nil
}

// monthCandidates resolves which days of one month a monthly/yearly
// rule selects, applying BYSETPOS as the final per-period filter.
func monthCandidates(r rule, anchor time.Time, y int, m time.Month, h, mi, sec int, loc *time.Location) []time.Time {
	daysInMonth := time.Date(y, m+1, 0, 0, 0, 0, 0, loc).Day()
	var cands []time.Time
	switch {
	case len(r.ByDay) > 0:
		for _, bd := range r.ByDay {
			if bd.Ord == 0 {
				for day := 1; day <= daysInMonth; day++ {
					if d := time.Date(y, m, day, h, mi, sec, 0, loc); d.Weekday() == bd.Weekday {
						cands = append(cands, d)
					}
				}
				continue
			}
			if d, ok := nthWeekday(y, m, bd.Weekday, bd.Ord, h, mi, sec, loc); ok {
				cands = append(cands, d)
			}
		}
	case len(r.ByMonthDay) > 0:
		for _, md := range r.ByMonthDay {
			day := md
			if md < 0 {
				day = daysInMonth + 1 + md
			}
			if day >= 1 && day <= daysInMonth {
				cands = append(cands, time.Date(y, m, day, h, mi, sec, 0, loc))
			}
		}
	default:
		if anchor.Day() <= daysInMonth {
			cands = append(cands, time.Date(y, m, anchor.Day(), h, mi, sec, 0, loc))
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Before(cands[j]) })
	if len(r.BySetPos) == 0 {
		return cands
	}
	var picked []time.Time
	for _, pos := range r.BySetPos {
		idx := pos - 1
		if pos < 0 {
			idx = len(cands) + pos
		}
		if idx >= 0 && idx < len(cands) {
			picked = append(picked, cands[idx])
		}
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].Before(picked[j]) })
	return picked
}

// nthWeekday returns the ord-th (negative: from the end) weekday of a
// month, keeping the anchor's wall-clock time.
func nthWeekday(y int, m time.Month, wd time.Weekday, ord, h, mi, sec int, loc *time.Location) (time.Time, bool) {
	daysInMonth := time.Date(y, m+1, 0, 0, 0, 0, 0, loc).Day()
	var matches []int
	for day := 1; day <= daysInMonth; day++ {
		if time.Date(y, m, day, 0, 0, 0, 0, loc).Weekday() == wd {
			matches = append(matches, day)
		}
	}
	idx := ord - 1
	if ord < 0 {
		idx = len(matches) + ord
	}
	if idx < 0 || idx >= len(matches) {
		return time.Time{}, false
	}
	return time.Date(y, m, matches[idx], h, mi, sec, 0, loc), true
}

func startOfISOWeek(t time.Time) time.Time {
	shift := (int(t.Weekday()) + 6) % 7 // Monday = 0
	y, m, d := t.AddDate(0, 0, -shift).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

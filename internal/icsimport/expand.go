// SPDX-License-Identifier: GPL-3.0-or-later

package icsimport

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The feed is foreign data, so expansion must be a dead end for anyone
// trying to burn CPU or memory with it. Three bounds enforce that:
// parse-time limits on the RRULE numbers, an enumeration budget every
// loop spends from, and a fast-forward that starts uncounted series at
// the window edge instead of at a far-past anchor.
const (
	// maxInterval and maxCount reject absurd RRULE numbers before they
	// reach the date arithmetic. Both sit far above anything a real
	// calendar emits — a rule beyond them is broken or hostile.
	maxInterval = 10_000
	maxCount    = 10_000

	// maxEventSteps bounds the enumeration steps one VEVENT may spend,
	// maxFeedSteps the whole Expand call so a feed cannot multiply the
	// per-event budget by its own length. Every loop an RRULE can drive
	// spends from this, and every appended occurrence costs a step, so
	// the budget caps run time and result size alike. This is the
	// backstop: it holds regardless of how clever the input is.
	maxEventSteps = 500_000
	maxFeedSteps  = 2_000_000
)

// errBudget aborts a VEVENT whose recurrence would outrun the budget.
// It is reported like any other expansion failure — loudly, as Skipped.
var errBudget = errors.New("recurrence too large: enumeration budget exhausted")

// budget meters enumeration work across one Expand call.
type budget struct {
	event, feed int
	hit         bool // true once a spend was denied
}

func newBudget() *budget { return &budget{event: maxEventSteps, feed: maxFeedSteps} }

// nextEvent hands the next VEVENT a fresh per-event allowance; the feed
// allowance keeps counting down across events.
func (b *budget) nextEvent() { b.event, b.hit = maxEventSteps, false }

// spend takes one step and reports false once the budget is exhausted.
func (b *budget) spend() bool {
	if b.event <= 0 || b.feed <= 0 {
		b.hit = true
		return false
	}
	b.event--
	b.feed--
	return true
}

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
// Recurrence is resolved from the series anchor, so COUNT and EXDATE
// stay correct even when the anchor lies in the past; where that is
// provably equivalent, enumeration fast-forwards to the window instead.
// Every series that hits a limit ends up in Skipped, never dropped.
func Expand(events []Event, from, to time.Time) ([]Occurrence, []Skipped) {
	var (
		occs    []Occurrence
		skipped []Skipped
	)

	// Overrides (RECURRENCE-ID) replace single occurrences of their
	// series. firstOverride remembers the earliest original start a
	// series is overridden at: an override may move an occurrence from
	// before the window into it, so the fast-forward must not skip past
	// it.
	overrides := map[string]Event{}
	firstOverride := map[string]time.Time{}
	for _, e := range events {
		if e.RecurrenceID.IsZero() {
			continue
		}
		overrides[occurrenceKey(e.UID, e.RecurrenceID)] = e
		if t, ok := firstOverride[e.UID]; !ok || e.RecurrenceID.Before(t) {
			firstOverride[e.UID] = e.RecurrenceID
		}
	}

	b := newBudget()
	for _, e := range events {
		if !e.RecurrenceID.IsZero() {
			continue // handled via the override map
		}
		if e.Status == "CANCELLED" {
			skipped = append(skipped, Skipped{UID: e.UID, Summary: e.Summary, Reason: "cancelled in source"})
			continue
		}
		duration := e.End.Sub(e.Start)
		b.nextEvent()
		var starts []time.Time
		if e.RRule == "" {
			starts = []time.Time{e.Start}
		} else {
			lower := from
			if t, ok := firstOverride[e.UID]; ok && t.Before(lower) {
				lower = t
			}
			var err error
			starts, err = expandRule(e, b, lower, to)
			if err != nil {
				skipped = append(skipped, Skipped{UID: e.UID, Summary: e.Summary, Reason: err.Error()})
				continue
			}
		}
		// RDATE adds occurrences on top of DTSTART/RRULE. Dedup by second
		// (iCalendar carries no sub-second precision — same reasoning as
		// exdateSet); the EXDATE filter below applies to RDATEs too, and
		// out-of-window RDATEs fall to the window check like any start.
		if len(e.RDates) > 0 {
			have := make(map[int64]bool, len(starts))
			for _, st := range starts {
				have[st.Unix()] = true
			}
			budgetOK := true
			for _, rd := range e.RDates {
				if !b.spend() {
					budgetOK = false
					break
				}
				if have[rd.Unix()] {
					continue
				}
				have[rd.Unix()] = true
				starts = append(starts, rd)
			}
			if !budgetOK {
				skipped = append(skipped, Skipped{UID: e.UID, Summary: e.Summary, Reason: errBudget.Error()})
				continue
			}
		}
		exdates := exdateSet(e.ExDates)
		for _, st := range starts {
			if exdates[st.Unix()] {
				continue
			}
			occ := Occurrence{
				Key: occurrenceKey(e.UID, st), UID: e.UID,
				Summary: e.Summary, Location: e.Location,
				Start: st, End: st.Add(duration),
			}
			if ov, ok := overrides[occ.Key]; ok {
				if ov.Status == "CANCELLED" {
					continue
				}
				occ.Summary, occ.Location = ov.Summary, ov.Location
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

// exdateSet indexes EXDATEs by second so the exclusion test stays O(1)
// per occurrence — a feed may carry thousands of them, and a linear
// scan per occurrence would be quadratic in attacker-chosen input.
// iCalendar times never carry sub-second precision, so the second
// identifies an instant exactly as time.Equal would.
func exdateSet(exdates []time.Time) map[int64]bool {
	if len(exdates) == 0 {
		return nil
	}
	set := make(map[int64]bool, len(exdates))
	for _, x := range exdates {
		set[x.Unix()] = true
	}
	return set
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
			if n > maxInterval {
				return r, fmt.Errorf("INTERVAL %q above the %d limit", v, maxInterval)
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
			if n > maxCount {
				return r, fmt.Errorf("COUNT %q above the %d limit", v, maxCount)
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

// expandRule enumerates the series until the window end (or UNTIL/COUNT
// run out) and returns every start time. Times are constructed in the
// anchor's location, so wall-clock times survive DST. windowStart is
// only a hint for how far enumeration may skip ahead — it never filters
// the result, that stays Expand's job.
func expandRule(e Event, b *budget, windowStart, windowEnd time.Time) ([]time.Time, error) {
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
	// COUNT counts from the anchor, so a counted series must be walked
	// from there to stay correct. Without COUNT every occurrence before
	// the window is discarded anyway, so enumeration may start at the
	// window edge — that is what keeps a year-1 DTSTART cheap.
	lower := anchor
	if r.Count == 0 && windowStart.After(lower) {
		lower = windowStart
	}

	var out []time.Time
	// add records one candidate start and reports false once enumeration
	// must stop — COUNT satisfied, or the budget ran out (b.hit tells
	// the two apart).
	add := func(t time.Time) bool {
		if !b.spend() {
			return false
		}
		if t.Before(anchor) || t.After(end) {
			return true
		}
		out = append(out, t)
		return r.Count == 0 || len(out) < r.Count
	}

	switch r.Freq {
	case "DAILY":
		// The first step at or before the window edge, computed instead
		// of enumerated; one whole interval of slack keeps it safe.
		i := max(0, daysBetween(anchor, lower)/r.Interval-1)
		for ; ; i++ {
			if !b.spend() {
				break
			}
			d := time.Date(anchor.Year(), anchor.Month(), anchor.Day()+i*r.Interval, h, mi, sec, 0, loc)
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
		// Week index and weekday are decided per day from absolute
		// dates, so skipping whole days ahead changes nothing; a week of
		// slack covers the interval alignment.
		day := max(0, daysBetween(anchor, lower)-7)
		for ; ; day++ {
			if !b.spend() {
				break
			}
			d := time.Date(anchor.Year(), anchor.Month(), anchor.Day()+day, h, mi, sec, 0, loc)
			if d.After(end) {
				break
			}
			week := daysBetween(anchorMonday, startOfISOWeek(d)) / 7
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
			if !b.spend() {
				break
			}
			y, m := y0+(m0-1+mIdx)/12, (m0-1+mIdx)%12+1
			periodStart := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, loc)
			if periodStart.After(end) {
				break
			}
			cands, ok := monthCandidates(r, anchor, y, time.Month(m), h, mi, sec, loc, b)
			if !ok {
				break
			}
			for _, d := range cands {
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
			if !b.spend() {
				break
			}
			if time.Date(y, 1, 1, 0, 0, 0, 0, loc).After(end) {
				break
			}
			for _, m := range months {
				cands, ok := monthCandidates(r, anchor, y, time.Month(m), h, mi, sec, loc, b)
				if !ok {
					break yearly
				}
				for _, d := range cands {
					if !add(d) {
						break yearly
					}
				}
			}
		}
	}
	if b.hit {
		return nil, errBudget
	}
	return out, nil
}

// monthCandidates resolves which days of one month a monthly/yearly
// rule selects, applying BYSETPOS as the final per-period filter. Every
// candidate it appends costs a budget step; ok is false once the budget
// is gone, and the caller must then abandon the whole series.
func monthCandidates(r rule, anchor time.Time, y int, m time.Month, h, mi, sec int, loc *time.Location, b *budget) ([]time.Time, bool) {
	daysInMonth := time.Date(y, m+1, 0, 0, 0, 0, 0, loc).Day()
	var cands []time.Time
	switch {
	case len(r.ByDay) > 0:
		for _, bd := range r.ByDay {
			if !b.spend() {
				return nil, false
			}
			if bd.Ord == 0 {
				for day := 1; day <= daysInMonth; day++ {
					d := time.Date(y, m, day, h, mi, sec, 0, loc)
					if d.Weekday() != bd.Weekday {
						continue
					}
					if !b.spend() {
						return nil, false
					}
					cands = append(cands, d)
				}
				continue
			}
			if d, ok := nthWeekday(y, m, bd.Weekday, bd.Ord, h, mi, sec, loc); ok {
				cands = append(cands, d)
			}
		}
	case len(r.ByMonthDay) > 0:
		for _, md := range r.ByMonthDay {
			if !b.spend() {
				return nil, false
			}
			day := md
			if md < 0 {
				day = daysInMonth + 1 + md
			}
			if day >= 1 && day <= daysInMonth {
				cands = append(cands, time.Date(y, m, day, h, mi, sec, 0, loc))
			}
		}
	default:
		if !b.spend() {
			return nil, false
		}
		if anchor.Day() <= daysInMonth {
			cands = append(cands, time.Date(y, m, anchor.Day(), h, mi, sec, 0, loc))
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Before(cands[j]) })
	if len(r.BySetPos) == 0 {
		return cands, true
	}
	var picked []time.Time
	for _, pos := range r.BySetPos {
		if !b.spend() {
			return nil, false
		}
		idx := pos - 1
		if pos < 0 {
			idx = len(cands) + pos
		}
		if idx >= 0 && idx < len(cands) {
			picked = append(picked, cands[idx])
		}
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].Before(picked[j]) })
	return picked, true
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

// daysBetween counts whole calendar days from a's date to b's date, b
// read in a's location. Subtracting the two times would not do: a
// time.Duration overflows past ~292 years and a degenerate feed may
// anchor a series in year 1, and a duration also miscounts across DST.
func daysBetween(a, b time.Time) int {
	const day = 86400
	midnightUTC := func(t time.Time) int64 {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / day
	}
	return int(midnightUTC(b.In(a.Location())) - midnightUTC(a))
}

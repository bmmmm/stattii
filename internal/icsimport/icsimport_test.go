// SPDX-License-Identifier: GPL-3.0-or-later

package icsimport

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// All fixtures are synthetic — real calendar data is operator data and
// never enters the repository.

func vevent(lines ...string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:test-uid\r\n" + strings.Join(lines, "\r\n") +
		"\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func expandOne(t *testing.T, ics string, from, to time.Time) ([]Occurrence, []Skipped) {
	t.Helper()
	events, skipped := Parse([]byte(ics))
	if len(skipped) > 0 {
		return nil, skipped
	}
	return Expand(events, from, to)
}

func TestParseFoldingEscapingAndTZID(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:u1\r\n" +
		"SUMMARY:Line one\\, with comma\r\n" +
		"LOCATION:Street 1\\nTown\r\n" +
		"X-APPLE-STRUCTURED-LOCATION;VALUE=URI;X-TITLE=\"has:colon\":geo:1\\,2\r\n" +
		"DTSTART;TZID=Europe/Berlin:20260901T2000\r\n 00\r\n" + // folded line
		"DTEND;TZID=Europe/Berlin:20260901T220000\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	events, skipped := Parse([]byte(raw))
	if len(skipped) != 0 || len(events) != 1 {
		t.Fatalf("events=%d skipped=%+v", len(events), skipped)
	}
	e := events[0]
	if e.Summary != "Line one, with comma" || e.Location != "Street 1\nTown" {
		t.Fatalf("unescaping failed: %+v", e)
	}
	want := time.Date(2026, 9, 1, 20, 0, 0, 0, berlin(t))
	if !e.Start.Equal(want) {
		t.Fatalf("folded TZID start = %v, want %v", e.Start, want)
	}
}

func TestParseAllDayAndUTC(t *testing.T) {
	events, _ := Parse([]byte(vevent(
		"DTSTART;VALUE=DATE:20260905", "DTEND;VALUE=DATE:20260906", "SUMMARY:AllDay")))
	if len(events) != 1 || !events[0].AllDay {
		t.Fatalf("all-day not detected: %+v", events)
	}
	events, _ = Parse([]byte(vevent("DTSTART:20260905T100000Z", "SUMMARY:UTC")))
	if len(events) != 1 || !events[0].Start.Equal(time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("utc start wrong: %+v", events)
	}
}

func TestParseUnknownTZIDIsSkippedLoudly(t *testing.T) {
	_, skipped := Parse([]byte(vevent("DTSTART;TZID=Nowhere/Land:20260901T200000", "SUMMARY:x")))
	if len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "TZID") {
		t.Fatalf("want loud skip, got %+v", skipped)
	}
}

func TestWeeklyWithUntilAndInterval(t *testing.T) {
	loc := berlin(t)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, 10, 15, 0, 0, 0, 0, loc)

	// Anchor Wed 2026-07-01 20:00, weekly until Sep 16 (inclusive).
	occs, sk := expandOne(t, vevent(
		"DTSTART;TZID=Europe/Berlin:20260701T200000",
		"DTEND;TZID=Europe/Berlin:20260701T220000",
		"RRULE:FREQ=WEEKLY;UNTIL=20260916T215959Z",
		"SUMMARY:Weekly"), from, to)
	if len(sk) != 0 {
		t.Fatalf("skipped: %+v", sk)
	}
	if len(occs) != 3 { // Sep 2, 9, 16
		t.Fatalf("weekly+until: want 3, got %d: %+v", len(occs), occs)
	}
	if occs[2].Start.Day() != 16 || occs[2].Start.Hour() != 20 {
		t.Fatalf("last occurrence wrong: %v", occs[2].Start)
	}

	// Every second week from Mon 2026-08-31.
	occs, _ = expandOne(t, vevent(
		"DTSTART;TZID=Europe/Berlin:20260831T190000",
		"RRULE:FREQ=WEEKLY;INTERVAL=2",
		"SUMMARY:Biweekly"), from, to)
	var days []int
	for _, o := range occs {
		days = append(days, o.Start.Day())
	}
	// Sep 14, 28, Oct 12 (Aug 31 is before the window).
	if len(days) != 3 || days[0] != 14 || days[1] != 28 || days[2] != 12 {
		t.Fatalf("biweekly days = %v", days)
	}
}

func TestMonthlyByDayOrdinals(t *testing.T) {
	loc := berlin(t)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, 10, 31, 23, 0, 0, 0, loc)

	// Second Monday: Sep 14, Oct 12.
	occs, _ := expandOne(t, vevent(
		"DTSTART;TZID=Europe/Berlin:20260713T180000",
		"RRULE:FREQ=MONTHLY;BYDAY=2MO",
		"SUMMARY:SecondMonday"), from, to)
	if len(occs) != 2 || occs[0].Start.Day() != 14 || occs[1].Start.Day() != 12 {
		t.Fatalf("2MO: %+v", occs)
	}

	// Last Wednesday: Sep 30, Oct 28.
	occs, _ = expandOne(t, vevent(
		"DTSTART;TZID=Europe/Berlin:20260729T180000",
		"RRULE:FREQ=MONTHLY;BYDAY=-1WE",
		"SUMMARY:LastWednesday"), from, to)
	if len(occs) != 2 || occs[0].Start.Day() != 30 || occs[1].Start.Day() != 28 {
		t.Fatalf("-1WE: %+v", occs)
	}

	// Every 2nd month, first Monday, anchored July → Sep 7 only in Sep window part.
	occs, _ = expandOne(t, vevent(
		"DTSTART;TZID=Europe/Berlin:20260706T100000",
		"RRULE:FREQ=MONTHLY;INTERVAL=2;BYDAY=1MO",
		"SUMMARY:BimonthlyFirstMonday"), from, to)
	if len(occs) != 1 || occs[0].Start.Day() != 7 || occs[0].Start.Month() != time.September {
		t.Fatalf("INTERVAL=2;1MO: %+v", occs)
	}
}

func TestMonthlyBySetPosMonthEnd(t *testing.T) {
	loc := berlin(t)
	// Last existing of 28/29/30 → Sep 30; Feb (2027) would pick 28.
	occs, sk := expandOne(t, vevent(
		"DTSTART;TZID=Europe/Berlin:20260830T120000",
		"RRULE:FREQ=MONTHLY;BYSETPOS=-1;BYMONTHDAY=28,29,30",
		"SUMMARY:MonthEnd"),
		time.Date(2026, 9, 1, 0, 0, 0, 0, loc),
		time.Date(2026, 9, 30, 23, 0, 0, 0, loc))
	if len(sk) != 0 || len(occs) != 1 || occs[0].Start.Day() != 30 {
		t.Fatalf("BYSETPOS month end: %+v %+v", occs, sk)
	}
}

func TestYearlyByMonthByDayAcrossDST(t *testing.T) {
	loc := berlin(t)
	// Last Sunday of March (DST switch day) — wall clock must hold.
	occs, sk := expandOne(t, vevent(
		"DTSTART;TZID=Europe/Berlin:20250330T100000",
		"RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU",
		"SUMMARY:MarchLastSunday"),
		time.Date(2027, 1, 1, 0, 0, 0, 0, loc),
		time.Date(2027, 12, 31, 0, 0, 0, 0, loc))
	if len(sk) != 0 || len(occs) != 1 {
		t.Fatalf("yearly: %+v %+v", occs, sk)
	}
	if occs[0].Start.Day() != 28 || occs[0].Start.Month() != time.March || occs[0].Start.Hour() != 10 {
		t.Fatalf("last Sunday March 2027 = %v, want Mar 28 10:00", occs[0].Start)
	}
}

func TestCountLimitsFromAnchorNotWindow(t *testing.T) {
	loc := berlin(t)
	// 5 daily occurrences from Aug 30 → last is Sep 3; a window starting
	// Sep 1 must only see Sep 1–3 (COUNT consumed by pre-window days).
	occs, _ := expandOne(t, vevent(
		"DTSTART;TZID=Europe/Berlin:20260830T080000",
		"RRULE:FREQ=DAILY;COUNT=5",
		"SUMMARY:Counted"),
		time.Date(2026, 9, 1, 0, 0, 0, 0, loc),
		time.Date(2026, 9, 30, 0, 0, 0, 0, loc))
	if len(occs) != 3 || occs[len(occs)-1].Start.Day() != 3 {
		t.Fatalf("COUNT window: %+v", occs)
	}
}

func TestExdateRemovesOccurrence(t *testing.T) {
	loc := berlin(t)
	occs, _ := expandOne(t, vevent(
		"DTSTART;TZID=Europe/Berlin:20260902T200000",
		"RRULE:FREQ=WEEKLY",
		"EXDATE;TZID=Europe/Berlin:20260909T200000",
		"SUMMARY:WithExdate"),
		time.Date(2026, 9, 1, 0, 0, 0, 0, loc),
		time.Date(2026, 9, 17, 0, 0, 0, 0, loc))
	if len(occs) != 2 { // Sep 2 and 16; Sep 9 excluded
		t.Fatalf("exdate: %+v", occs)
	}
	for _, o := range occs {
		if o.Start.Day() == 9 {
			t.Fatalf("excluded date still present: %v", o.Start)
		}
	}
}

func TestRecurrenceIDOverrideKeepsKey(t *testing.T) {
	loc := berlin(t)
	raw := "BEGIN:VCALENDAR\r\n" +
		"BEGIN:VEVENT\r\nUID:series\r\n" +
		"DTSTART;TZID=Europe/Berlin:20260902T200000\r\n" +
		"DTEND;TZID=Europe/Berlin:20260902T220000\r\n" +
		"RRULE:FREQ=WEEKLY\r\nSUMMARY:Series\r\nEND:VEVENT\r\n" +
		// Sep 9 moved to Sep 10 21:00 with a new title.
		"BEGIN:VEVENT\r\nUID:series\r\n" +
		"RECURRENCE-ID;TZID=Europe/Berlin:20260909T200000\r\n" +
		"DTSTART;TZID=Europe/Berlin:20260910T210000\r\n" +
		"DTEND;TZID=Europe/Berlin:20260910T230000\r\n" +
		"SUMMARY:Series (moved)\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	events, sk := Parse([]byte(raw))
	if len(sk) != 0 {
		t.Fatalf("skipped: %+v", sk)
	}
	occs, _ := Expand(events,
		time.Date(2026, 9, 1, 0, 0, 0, 0, loc),
		time.Date(2026, 9, 12, 0, 0, 0, 0, loc))
	if len(occs) != 2 {
		t.Fatalf("want Sep 2 + moved Sep 10, got %+v", occs)
	}
	moved := occs[1]
	if moved.Start.Day() != 10 || moved.Summary != "Series (moved)" {
		t.Fatalf("override not applied: %+v", moved)
	}
	wantKey := "series/" + time.Date(2026, 9, 9, 20, 0, 0, 0, loc).UTC().Format(time.RFC3339)
	if moved.Key != wantKey {
		t.Fatalf("override key = %q, want original-start key %q", moved.Key, wantKey)
	}
}

func TestUnsupportedRuleIsSkippedLoudly(t *testing.T) {
	loc := berlin(t)
	_, sk := expandOne(t, vevent(
		"DTSTART;TZID=Europe/Berlin:20260902T200000",
		"RRULE:FREQ=HOURLY",
		"SUMMARY:Hourly"),
		time.Date(2026, 9, 1, 0, 0, 0, 0, loc),
		time.Date(2026, 9, 30, 0, 0, 0, 0, loc))
	if len(sk) != 1 || !strings.Contains(sk[0].Reason, "FREQ") {
		t.Fatalf("want loud skip for HOURLY, got %+v", sk)
	}
}

// --- hostile / degenerate feeds ---------------------------------------
//
// The source feed is foreign data. Parsing and expansion must stay cheap
// no matter what it contains, and anything refused must be reported.

func TestFarPastAnchorDoesNotEnumerateHistory(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	// A daily series anchored in year 1 is ~740k occurrences before the
	// window. Enumerating them would cost seconds and megabytes per
	// VEVENT; fast-forwarding makes it cost the window.
	start := time.Now()
	occs, sk := expandOne(t, vevent(
		"DTSTART:00010101T000100Z",
		"RRULE:FREQ=DAILY",
		"SUMMARY:AncientDaily"), from, to)
	if len(sk) != 0 {
		t.Fatalf("daily year-1 anchor was skipped: %+v", sk)
	}
	if len(occs) != 61 { // Sep 1 .. Oct 31, both ends inclusive
		t.Fatalf("daily year-1 anchor: want 61 occurrences, got %d", len(occs))
	}

	occs, sk = expandOne(t, vevent(
		"DTSTART:00010101T000100Z",
		"RRULE:FREQ=WEEKLY",
		"SUMMARY:AncientWeekly"), from, to)
	if len(sk) != 0 {
		t.Fatalf("weekly year-1 anchor was skipped: %+v", sk)
	}
	if len(occs) != 8 { // Mondays Sep 7 .. Oct 26
		t.Fatalf("weekly year-1 anchor: want 8 occurrences, got %d", len(occs))
	}
	for _, o := range occs {
		if o.Start.Weekday() != time.Monday {
			t.Fatalf("weekly anchor drifted off its weekday: %v", o.Start)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("far-past anchors took %v — enumeration is not fast-forwarding", d)
	}
}

func TestAbsurdRRuleNumbersAreRejectedLoudly(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	cases := []struct{ name, rrule, want string }{
		// Overflows y += Interval, wraps the year, and never passes the
		// window end: a 100% CPU loop that outlives the client.
		{"interval overflow", "FREQ=YEARLY;INTERVAL=9223372036854775807", "INTERVAL"},
		{"interval above limit", "FREQ=DAILY;INTERVAL=10001", "INTERVAL"},
		{"interval unparsable", "FREQ=DAILY;INTERVAL=99999999999999999999999", "INTERVAL"},
		{"count above limit", "FREQ=DAILY;COUNT=10001", "COUNT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			occs, sk := expandOne(t, vevent(
				"DTSTART:20260901T100000Z", "RRULE:"+tc.rrule, "SUMMARY:Bomb"), from, to)
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("expansion took %v — the rule was not rejected", d)
			}
			if len(occs) != 0 {
				t.Fatalf("want no occurrences, got %d", len(occs))
			}
			if len(sk) != 1 || !strings.Contains(sk[0].Reason, tc.want) {
				t.Fatalf("want a loud skip naming %s, got %+v", tc.want, sk)
			}
		})
	}
	// The limits accept what a real calendar emits.
	if _, sk := expandOne(t, vevent(
		"DTSTART:20260901T100000Z", "RRULE:FREQ=DAILY;INTERVAL=10000;COUNT=10000",
		"SUMMARY:Legit"), from, to); len(sk) != 0 {
		t.Fatalf("limit values must still be accepted: %+v", sk)
	}
}

func TestEnumerationBudgetAbortsLoudly(t *testing.T) {
	// Nothing here is out of range on its own: every weekday of every
	// month since year 1. Only the budget stops it.
	start := time.Now()
	occs, sk := expandOne(t, vevent(
		"DTSTART:00010101T000100Z",
		"RRULE:FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR,SA,SU",
		"SUMMARY:BudgetBomb"),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC))
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("expansion took %v — the budget did not bite", d)
	}
	if len(occs) != 0 {
		t.Fatalf("an aborted series must yield nothing, got %d occurrences", len(occs))
	}
	if len(sk) != 1 || !strings.Contains(sk[0].Reason, "budget") {
		t.Fatalf("want a loud budget skip, got %+v", sk)
	}
}

func TestFastForwardKeepsOverrideBeforeWindow(t *testing.T) {
	loc := berlin(t)
	// The Aug 15 occurrence lies before the window but is moved into it.
	// Fast-forwarding to the window edge must not lose it.
	raw := "BEGIN:VCALENDAR\r\n" +
		"BEGIN:VEVENT\r\nUID:moved-in\r\n" +
		"DTSTART;TZID=Europe/Berlin:20260801T200000\r\n" +
		"DTEND;TZID=Europe/Berlin:20260801T220000\r\n" +
		"RRULE:FREQ=DAILY\r\nSUMMARY:Daily\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:moved-in\r\n" +
		"RECURRENCE-ID;TZID=Europe/Berlin:20260815T200000\r\n" +
		"DTSTART;TZID=Europe/Berlin:20260905T210000\r\n" +
		"DTEND;TZID=Europe/Berlin:20260905T230000\r\n" +
		"SUMMARY:Moved in\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	events, sk := Parse([]byte(raw))
	if len(sk) != 0 {
		t.Fatalf("skipped: %+v", sk)
	}
	occs, sk := Expand(events,
		time.Date(2026, 9, 1, 0, 0, 0, 0, loc),
		time.Date(2026, 9, 10, 23, 0, 0, 0, loc))
	if len(sk) != 0 {
		t.Fatalf("skipped: %+v", sk)
	}
	var moved *Occurrence
	for i, o := range occs {
		if o.Summary == "Moved in" {
			moved = &occs[i]
		}
	}
	if moved == nil {
		t.Fatalf("override from before the window was lost: %+v", occs)
	}
	wantKey := "moved-in/" + time.Date(2026, 8, 15, 20, 0, 0, 0, loc).UTC().Format(time.RFC3339)
	if moved.Key != wantKey || moved.Start.Day() != 5 || moved.Start.Hour() != 21 {
		t.Fatalf("override wrong: key=%q start=%v", moved.Key, moved.Start)
	}
}

func TestBiweeklyAcrossDSTSwitch(t *testing.T) {
	loc := berlin(t)
	// Anchor Mon 2027-03-15; the second occurrence lands after the
	// spring-forward Sunday, so the week index must be counted in
	// calendar days, not in hours.
	occs, sk := expandOne(t, vevent(
		"DTSTART;TZID=Europe/Berlin:20270315T200000",
		"DTEND;TZID=Europe/Berlin:20270315T220000",
		"RRULE:FREQ=WEEKLY;INTERVAL=2",
		"SUMMARY:BiweeklyDST"),
		time.Date(2027, 3, 1, 0, 0, 0, 0, loc),
		time.Date(2027, 4, 30, 0, 0, 0, 0, loc))
	if len(sk) != 0 {
		t.Fatalf("skipped: %+v", sk)
	}
	var days []int
	for _, o := range occs {
		if o.Start.Hour() != 20 {
			t.Fatalf("wall clock drifted across DST: %v", o.Start)
		}
		days = append(days, o.Start.Day())
	}
	// Mar 15, Mar 29, Apr 12, Apr 26.
	if len(days) != 4 || days[0] != 15 || days[1] != 29 || days[2] != 12 || days[3] != 26 {
		t.Fatalf("biweekly across DST = %v, want [15 29 12 26]", days)
	}
}

func TestFoldedLinesStayLinear(t *testing.T) {
	// 200k one-byte continuations: appending to the previous string
	// would copy it every time and burn minutes on a 600 kB feed.
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:folded\r\nSUMMARY:x")
	for range 200_000 {
		b.WriteString("\r\n y")
	}
	b.WriteString("\r\nDTSTART:20260901T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	start := time.Now()
	events, sk := Parse([]byte(b.String()))
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("unfolding took %v — it is still quadratic", d)
	}
	if len(sk) != 0 || len(events) != 1 || len(events[0].Summary) != 200_001 {
		t.Fatalf("folded summary wrong: events=%d skipped=%+v", len(events), sk)
	}
}

func TestManyExdatesStayLinear(t *testing.T) {
	// A daily series plus 50k EXDATEs: a linear scan per occurrence
	// would be a quadratic cross product. One of them must still bite.
	var ex strings.Builder
	ex.WriteString("EXDATE:20260905T100000Z")
	noise := time.Date(1900, 1, 1, 11, 0, 0, 0, time.UTC) // 11:00 never matches
	for i := range 50_000 {
		ex.WriteString("," + noise.AddDate(0, 0, i).Format("20060102T150405Z"))
	}
	start := time.Now()
	occs, sk := expandOne(t, vevent(
		"DTSTART:20260901T100000Z", "RRULE:FREQ=DAILY", ex.String(), "SUMMARY:Excluded"),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC))
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("exdate matching took %v — it is still quadratic", d)
	}
	if len(sk) != 0 || len(occs) != 60 { // Sep 1 .. Oct 31 minus Sep 5
		t.Fatalf("exdate set wrong: %d occs, %+v", len(occs), sk)
	}
	for _, o := range occs {
		if o.Start.Month() == time.September && o.Start.Day() == 5 {
			t.Fatalf("excluded date still present: %v", o.Start)
		}
	}
}

func TestLargeHonestFeedFitsTheBudget(t *testing.T) {
	// Headroom check: 5000 weekly series anchored ten years back, the
	// production 60-day window. The feed-wide budget must not turn an
	// honest calendar into a wall of skips.
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	for i := range 5000 {
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:u%d\r\nDTSTART:20160104T100000Z\r\n"+
			"DTEND:20160104T110000Z\r\nRRULE:FREQ=WEEKLY\r\nSUMMARY:S%d\r\nEND:VEVENT\r\n", i, i)
	}
	b.WriteString("END:VCALENDAR\r\n")

	start := time.Now()
	occs, sk := expandOne(t, b.String(),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC))
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("honest feed took %v", d)
	}
	if len(sk) != 0 {
		t.Fatalf("honest feed hit a limit: %+v", sk[:min(3, len(sk))])
	}
	if len(occs) != 5000*8 { // 8 Mondays in the window, per series
		t.Fatalf("honest feed: want 40000 occurrences, got %d", len(occs))
	}
}

func TestCancelledSourceEventProducesNoOccurrence(t *testing.T) {
	loc := berlin(t)
	occs, sk := expandOne(t, vevent(
		"DTSTART;TZID=Europe/Berlin:20260905T200000",
		"STATUS:CANCELLED",
		"SUMMARY:Gone"),
		time.Date(2026, 9, 1, 0, 0, 0, 0, loc),
		time.Date(2026, 9, 30, 0, 0, 0, 0, loc))
	if len(occs) != 0 || len(sk) != 1 {
		t.Fatalf("cancelled source: occs=%+v sk=%+v", occs, sk)
	}
}

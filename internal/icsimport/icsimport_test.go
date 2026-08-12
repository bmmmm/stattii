// SPDX-License-Identifier: GPL-3.0-or-later

package icsimport

import (
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

// SPDX-License-Identifier: GPL-3.0-or-later

package icsimport

import (
	"strings"
	"testing"
	"time"
)

// RDATE adds occurrences to a series — issue #11: without support they
// went silently missing, the exact locked-door gap this product exists
// to close (an occurrence the source knows about, stattii never saw).

func TestRDATEAddsAndDedupes(t *testing.T) {
	loc := berlin(t)
	ics := vevent(
		"SUMMARY:Training",
		"DTSTART;TZID=Europe/Berlin:20260901T200000", // a Tuesday
		"DTEND;TZID=Europe/Berlin:20260901T220000",
		"RRULE:FREQ=WEEKLY",
		// One extra Saturday slot, plus a duplicate of the regular
		// Sep 8 hit — the duplicate must not double the occurrence.
		"RDATE;TZID=Europe/Berlin:20260905T180000,20260908T200000",
	)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, 9, 14, 23, 59, 0, 0, loc)
	occs, skipped := expandOne(t, ics, from, to)
	if len(skipped) != 0 {
		t.Fatalf("skipped: %+v", skipped)
	}
	if len(occs) != 3 { // Sep 1, Sep 5 (RDATE), Sep 8 — deduped
		t.Fatalf("want 3 occurrences, got %d: %+v", len(occs), occs)
	}
	sat := time.Date(2026, 9, 5, 18, 0, 0, 0, loc)
	if !occs[1].Start.Equal(sat) {
		t.Fatalf("RDATE occurrence missing: %+v", occs[1])
	}
	if !occs[1].End.Equal(sat.Add(2 * time.Hour)) {
		t.Fatalf("RDATE occurrence must inherit the series duration: %+v", occs[1])
	}
}

func TestRDATEOnlySeries(t *testing.T) {
	loc := berlin(t)
	// No RRULE: DTSTART plus the RDATEs is the whole recurrence set.
	ics := vevent(
		"SUMMARY:Special",
		"DTSTART;TZID=Europe/Berlin:20260901T200000",
		"DTEND;TZID=Europe/Berlin:20260901T210000",
		"RDATE;TZID=Europe/Berlin:20260910T200000",
		"RDATE;TZID=Europe/Berlin:20261001T200000", // outside the window
	)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, 9, 30, 23, 59, 0, 0, loc)
	occs, skipped := expandOne(t, ics, from, to)
	if len(skipped) != 0 {
		t.Fatalf("skipped: %+v", skipped)
	}
	if len(occs) != 2 {
		t.Fatalf("want DTSTART + in-window RDATE = 2, got %d: %+v", len(occs), occs)
	}
}

func TestRDATERemovedByExdate(t *testing.T) {
	loc := berlin(t)
	ics := vevent(
		"SUMMARY:Special",
		"DTSTART;TZID=Europe/Berlin:20260901T200000",
		"DTEND;TZID=Europe/Berlin:20260901T210000",
		"RDATE;TZID=Europe/Berlin:20260910T200000",
		"EXDATE;TZID=Europe/Berlin:20260910T200000",
	)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, 9, 30, 23, 59, 0, 0, loc)
	occs, skipped := expandOne(t, ics, from, to)
	if len(skipped) != 0 {
		t.Fatalf("skipped: %+v", skipped)
	}
	if len(occs) != 1 {
		t.Fatalf("EXDATE must remove the RDATE occurrence, got %d: %+v", len(occs), occs)
	}
}

func TestRDATEPeriodIsLoudlySkipped(t *testing.T) {
	ics := vevent(
		"SUMMARY:Special",
		"DTSTART:20260901T200000Z",
		"RDATE;VALUE=PERIOD:20260905T180000Z/20260905T200000Z",
	)
	events, skipped := Parse([]byte(ics))
	if len(events) != 0 || len(skipped) != 1 {
		t.Fatalf("PERIOD RDATE must skip the event loudly: events=%+v skipped=%+v", events, skipped)
	}
	if !strings.Contains(skipped[0].Reason, "PERIOD") {
		t.Fatalf("skip reason does not name the cause: %q", skipped[0].Reason)
	}
}

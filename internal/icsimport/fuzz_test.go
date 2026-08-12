// SPDX-License-Identifier: GPL-3.0-or-later

package icsimport

import (
	"testing"
	"time"
)

// The feed is foreign data, so the importer has to survive arbitrary
// bytes: no panic out of Parse, no panic out of Expand, and no run that
// outlives the enumeration budget. All corpus entries are synthetic.

// fuzzWall is the wall-clock ceiling one Parse+Expand round may take.
// It is far above the budgeted worst case and only fires when a bound
// stops holding.
const fuzzWall = 10 * time.Second

func fuzzSeeds() [][]byte {
	seeds := []string{
		// Plain single event.
		"BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\nDTSTART:20260905T100000Z\r\n" +
			"DTEND:20260905T110000Z\r\nSUMMARY:Plain\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		// Weekly series with UNTIL, EXDATE and a folded line.
		"BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:b\r\n" +
			"DTSTART;TZID=Europe/Berlin:20260902T2000\r\n 00\r\n" +
			"RRULE:FREQ=WEEKLY;INTERVAL=2;UNTIL=20261231T000000Z\r\n" +
			"EXDATE;TZID=Europe/Berlin:20260916T200000\r\n" +
			"SUMMARY:Weekly\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		// Monthly BYSETPOS + BYMONTHDAY, and a yearly BYDAY ordinal.
		"BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:c\r\nDTSTART:20260830T120000Z\r\n" +
			"RRULE:FREQ=MONTHLY;BYSETPOS=-1;BYMONTHDAY=28,29,30\r\nEND:VEVENT\r\n" +
			"BEGIN:VEVENT\r\nUID:d\r\nDTSTART:20250330T100000Z\r\n" +
			"RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		// Series plus a RECURRENCE-ID override.
		"BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:e\r\nDTSTART:20260902T200000Z\r\n" +
			"RRULE:FREQ=WEEKLY\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:e\r\n" +
			"RECURRENCE-ID:20260909T200000Z\r\nDTSTART:20260910T210000Z\r\n" +
			"SUMMARY:Moved\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		// Attack shape 1: an anchor in year 1 — ~740k occurrences before
		// any window, enumerated before anything is discarded.
		"BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:f\r\nDTSTART:00010101T000100Z\r\n" +
			"RRULE:FREQ=DAILY\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		// Attack shape 2: an INTERVAL that overflows the year arithmetic
		// so the loop never reaches the window end.
		"BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:g\r\nDTSTART:20260901T100000Z\r\n" +
			"RRULE:FREQ=YEARLY;INTERVAL=9223372036854775807\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		// Every weekday of every month since year 1 — nothing out of
		// range on its own, only the iteration budget stops it.
		"BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:h\r\nDTSTART:00010101T000100Z\r\n" +
			"RRULE:FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR,SA,SU\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		// Degenerate shapes: empty rule parts, stray folds, no VEVENT.
		"BEGIN:VEVENT\r\nUID:i\r\nDTSTART:20260901\r\nRRULE:\r\nEXDATE:\r\nEND:VEVENT\r\n",
		" \r\n\t\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\n",
	}
	out := make([][]byte, 0, len(seeds))
	for _, s := range seeds {
		out = append(out, []byte(s))
	}
	return out
}

func FuzzParse(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		events, _ := Parse(raw)
		for _, e := range events {
			// Parse promises these two — everything else is Skipped.
			if e.UID == "" || e.Start.IsZero() {
				t.Fatalf("event without UID or DTSTART escaped Parse: %+v", e)
			}
		}
	})
}

func FuzzExpand(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 60)
	f.Fuzz(func(t *testing.T, raw []byte) {
		events, _ := Parse(raw)
		started := time.Now()
		occs, _ := Expand(events, from, to)
		if d := time.Since(started); d > fuzzWall {
			t.Fatalf("Expand took %v on %d bytes — a bound stopped holding", d, len(raw))
		}
		for _, o := range occs {
			if o.Start.Before(from) || o.Start.After(to) {
				t.Fatalf("occurrence outside the window: %v", o.Start)
			}
		}
	})
}

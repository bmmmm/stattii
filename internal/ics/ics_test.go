// SPDX-License-Identifier: GPL-3.0-or-later

package ics

import (
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

func TestFeed(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	events := []core.Event{
		{
			ID: "ev_1", Title: "Board, games; night", Location: "Hall 3",
			StartsAt: now.Add(48 * time.Hour), EndsAt: now.Add(50 * time.Hour),
			Status: core.StatusCancelled, CancelReason: "storm", Seq: 2,
		},
		{
			ID: "ev_2", Title: strings.Repeat("Very long title ", 12),
			StartsAt: now.Add(72 * time.Hour), Status: core.StatusConfirmed, Seq: 1,
		},
	}
	feed := Feed("test-cal", events, now)

	for _, want := range []string{
		"BEGIN:VCALENDAR", "END:VCALENDAR",
		"UID:ev_1@stattii",
		"STATUS:CANCELLED", "STATUS:CONFIRMED",
		"SEQUENCE:2",
		`SUMMARY:Board\, games\; night`,
		"DTSTART:20260814T120000Z",
		"Cancelled: storm",
	} {
		if !strings.Contains(feed, want) {
			t.Errorf("feed missing %q", want)
		}
	}

	for i, l := range strings.Split(feed, "\r\n") {
		if len(l) > 75 {
			t.Errorf("line %d exceeds 75 octets (%d): %q", i, len(l), l)
		}
	}
	if !strings.HasSuffix(feed, "\r\n") {
		t.Error("feed must end with CRLF")
	}
	if strings.Contains(strings.ReplaceAll(feed, "\r\n", ""), "\n") {
		t.Error("bare LF in feed")
	}
}

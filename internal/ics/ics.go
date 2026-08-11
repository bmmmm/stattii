// SPDX-License-Identifier: GPL-3.0-or-later

// Package ics generates the outbound iCalendar feed. Generation only —
// parsing foreign feeds is deliberately out of scope. Note that calendar
// apps poll subscribed feeds slowly (Google: ~12-24h); the feed is the
// passive baseline, never the short-notice cancellation channel.
package ics

import (
	"strconv"
	"strings"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

const stampFmt = "20060102T150405Z"

// Feed renders all events as one VCALENDAR.
func Feed(name string, events []core.Event, now time.Time) string {
	var b strings.Builder
	line(&b, "BEGIN:VCALENDAR")
	line(&b, "VERSION:2.0")
	line(&b, "PRODID:-//stattii//stattii//EN")
	line(&b, "CALSCALE:GREGORIAN")
	if name != "" {
		line(&b, "X-WR-CALNAME:"+escape(name))
	}
	for _, e := range events {
		line(&b, "BEGIN:VEVENT")
		line(&b, "UID:"+e.ID+"@stattii")
		line(&b, "DTSTAMP:"+now.UTC().Format(stampFmt))
		line(&b, "DTSTART:"+e.StartsAt.UTC().Format(stampFmt))
		if !e.EndsAt.IsZero() {
			line(&b, "DTEND:"+e.EndsAt.UTC().Format(stampFmt))
		}
		line(&b, "SUMMARY:"+escape(e.Title))
		if e.Location != "" {
			line(&b, "LOCATION:"+escape(e.Location))
		}
		desc := e.Note
		if e.CancelReason != "" {
			if desc != "" {
				desc += "\n"
			}
			desc += "Cancelled: " + e.CancelReason
		}
		if desc != "" {
			line(&b, "DESCRIPTION:"+escape(desc))
		}
		line(&b, "SEQUENCE:"+strconv.Itoa(e.Seq))
		line(&b, "STATUS:"+status(e.Status))
		line(&b, "END:VEVENT")
	}
	line(&b, "END:VCALENDAR")
	return b.String()
}

func status(s core.EventStatus) string {
	switch s {
	case core.StatusConfirmed:
		return "CONFIRMED"
	case core.StatusCancelled:
		return "CANCELLED"
	default:
		return "TENTATIVE"
	}
}

// escape per RFC 5545: backslash, semicolon, comma, newline.
func escape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`)
	return r.Replace(s)
}

// line writes a content line folded at 74 octets (RFC 5545 limit is 75),
// breaking only at rune boundaries.
func line(b *strings.Builder, s string) {
	octets := 0
	for _, r := range s {
		n := len(string(r))
		if octets+n > 74 {
			b.WriteString("\r\n ")
			octets = 1 // the leading space counts
		}
		b.WriteRune(r)
		octets += n
	}
	b.WriteString("\r\n")
}

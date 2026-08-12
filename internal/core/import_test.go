// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/icsimport"
)

func occ(uid, title string, start time.Time, dur time.Duration) icsimport.Occurrence {
	return icsimport.Occurrence{
		Key: uid + "/" + start.UTC().Format(time.RFC3339), UID: uid,
		Summary: title, Start: start, End: start.Add(dur),
	}
}

func TestSyncCreatesAndInheritsSeriesAssignment(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	p := mustPerson(t, svc, "ana", TrustRespond)
	if _, err := svc.AssignSeries("series-1", p.ID, "host"); err != nil {
		t.Fatal(err)
	}

	now := *clock
	until := now.Add(60 * 24 * time.Hour)
	rep := svc.SyncCalendar([]icsimport.Occurrence{
		occ("series-1", "Weekly Thing", now.Add(40*time.Hour), 2*time.Hour),
		occ("series-1", "Weekly Thing", now.Add(7*24*time.Hour+40*time.Hour), 2*time.Hour),
		occ("other", "One-off", now.Add(30*time.Hour), time.Hour),
	}, nil, until)
	if rep.Created != 3 || rep.Unchanged != 0 {
		t.Fatalf("report: %+v", rep)
	}

	// Both series events inherited ana; the one-off did not.
	ov := svc.Overview()
	assigned := 0
	for _, oe := range ov.Events {
		if oe.Event.SourceUID == "series-1" {
			if len(oe.Assignees) != 1 || oe.Assignees[0].Name != "ana" || oe.Assignees[0].Role != "host" {
				t.Fatalf("series inheritance failed: %+v", oe)
			}
			assigned++
		}
	}
	if assigned != 2 {
		t.Fatalf("want 2 series events, got %d", assigned)
	}

	// The reminder inside the lead window fires for the inherited assignee.
	svc.Tick(now)
	if got := fake.byPurposeTo("ana@test.local"); len(got) != 1 {
		t.Fatalf("want 1 reminder for the 40h occurrence, got %d", len(got))
	}

	// Re-sync with identical data is a no-op.
	rep = svc.SyncCalendar([]icsimport.Occurrence{
		occ("series-1", "Weekly Thing", now.Add(40*time.Hour), 2*time.Hour),
		occ("series-1", "Weekly Thing", now.Add(7*24*time.Hour+40*time.Hour), 2*time.Hour),
		occ("other", "One-off", now.Add(30*time.Hour), time.Hour),
	}, nil, until)
	if rep.Created != 0 || rep.Unchanged != 3 || rep.Moved != 0 {
		t.Fatalf("resync not idempotent: %+v", rep)
	}
}

func TestSyncMoveRunsFullTransaction(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	p := mustPerson(t, svc, "ana", TrustRespond)
	now := *clock
	until := now.Add(60 * 24 * time.Hour)
	start := now.Add(40 * time.Hour)

	svc.SyncCalendar([]icsimport.Occurrence{occ("u1", "Session", start, 2*time.Hour)}, nil, until)
	ev := svc.Events()[0]
	if err := svc.Assign(ev.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	svc.Tick(now) // reminder out

	// Source moved the occurrence by a day — same key (original start).
	moved := occ("u1", "Session", start, 2*time.Hour)
	moved.Start = start.Add(24 * time.Hour)
	moved.End = moved.Start.Add(2 * time.Hour)
	rep := svc.SyncCalendar([]icsimport.Occurrence{moved}, nil, until)
	if rep.Moved != 1 {
		t.Fatalf("report: %+v", rep)
	}
	got, _ := svc.EventByID(ev.ID)
	if !got.StartsAt.Equal(moved.Start) {
		t.Fatalf("event not moved: %v", got.StartsAt)
	}
	svc.Tick(now) // deliver the fan-out the sync enqueued
	// The move fan-out reached the assignee (subject carries "Moved").
	var sawMove bool
	for _, m := range fake.byPurposeTo("ana@test.local") {
		if strings.Contains(strings.ToLower(m.Subject), "moved") {
			sawMove = true
		}
	}
	if !sawMove {
		t.Fatalf("no moved notification, got %+v", fake.byPurposeTo("ana@test.local"))
	}
}

func TestSyncVanishedIsReportedNeverCancelled(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	now := *clock
	until := now.Add(60 * 24 * time.Hour)
	a := occ("u1", "Stays", now.Add(48*time.Hour), time.Hour)
	b := occ("u2", "Disappears", now.Add(72*time.Hour), time.Hour)
	svc.SyncCalendar([]icsimport.Occurrence{a, b}, nil, until)

	rep := svc.SyncCalendar([]icsimport.Occurrence{a}, nil, until)
	if len(rep.Vanished) != 1 || !strings.Contains(rep.Vanished[0], "Disappears") {
		t.Fatalf("vanished not reported: %+v", rep)
	}
	for _, e := range svc.Events() {
		if e.Status == StatusCancelled {
			t.Fatalf("a feed glitch cancelled an event: %+v", e)
		}
	}
}

func TestSyncCancelledHereConflictsWithSourceMove(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	now := *clock
	until := now.Add(60 * 24 * time.Hour)
	a := occ("u1", "Cancelled here", now.Add(48*time.Hour), time.Hour)
	svc.SyncCalendar([]icsimport.Occurrence{a}, nil, until)
	ev := svc.Events()[0]
	if _, err := svc.CancelEvent(ev.ID, "", "we said no", "admin"); err != nil {
		t.Fatal(err)
	}

	// Same time again: stays quiet. Moved in source: conflict, no resurrect.
	rep := svc.SyncCalendar([]icsimport.Occurrence{a}, nil, until)
	if len(rep.Conflicts) != 0 {
		t.Fatalf("unchanged cancelled event must not conflict: %+v", rep)
	}
	m := a
	m.Start = a.Start.Add(2 * time.Hour)
	m.End = m.Start.Add(time.Hour)
	rep = svc.SyncCalendar([]icsimport.Occurrence{m}, nil, until)
	if len(rep.Conflicts) != 1 {
		t.Fatalf("want conflict: %+v", rep)
	}
	if got, _ := svc.EventByID(ev.ID); got.Status != StatusCancelled || !got.StartsAt.Equal(a.Start) {
		t.Fatalf("cancelled event was touched: %+v", got)
	}
}

func TestFetchCalendarEndToEnd(t *testing.T) {
	fake := &fakeNotifier{}
	store, err := NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	feed := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:f1\r\n" +
		"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\n" +
		"SUMMARY:Fetched\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(feed))
	}))
	defer srv.Close()

	svc, err := NewService(store, Config{
		BaseURL: "http://test.local", CalendarSource: srv.URL,
		CalendarWindow: 60 * 24 * time.Hour,
	}, fake)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now })
	svc.SetCalendarClient(srv.Client())

	rep, err := svc.FetchCalendar(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 1 {
		t.Fatalf("report: %+v", rep)
	}
	events := svc.Events()
	if len(events) != 1 || events[0].SourceUID != "f1" || events[0].Title != "Fetched" {
		t.Fatalf("events: %+v", events)
	}

	// No source configured is a loud error.
	svc2, _ := NewService(store, Config{BaseURL: "http://x"}, fake)
	if _, err := svc2.FetchCalendar(context.Background()); err == nil {
		t.Fatal("missing calendar_source must error")
	}
}

func TestSyncEmptyFeedIsSuspect(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	now := *clock
	until := now.Add(60 * 24 * time.Hour)
	a := occ("u1", "Stays", now.Add(48*time.Hour), time.Hour)
	svc.SyncCalendar([]icsimport.Occurrence{a}, nil, until)

	// A feed that suddenly reports nothing is a broken feed until proven
	// otherwise — no vanished verdicts, a suspect flag instead.
	rep := svc.SyncCalendar(nil, nil, until)
	if !rep.Suspect {
		t.Fatalf("empty feed against a populated window must be suspect: %+v", rep)
	}
	if len(rep.Vanished) != 0 {
		t.Fatalf("suspect fetch must not report vanished events: %+v", rep)
	}
}

// An end-only time change is not a move: no re-confirmation cycle, no
// "MOVED Old: X / New: X" fan-out — a quiet update like a title edit.
func TestSyncEndOnlyChangeIsQuietUpdate(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	now := *clock
	until := now.Add(60 * 24 * time.Hour)
	a := occ("u1", "Session", now.Add(48*time.Hour), time.Hour)
	svc.SyncCalendar([]icsimport.Occurrence{a}, nil, until)
	ev := svc.Events()[0]
	if _, err := svc.ConfirmEvent(ev.ID, "", "api"); err != nil {
		t.Fatal(err)
	}

	longer := a
	longer.End = a.Start.Add(2 * time.Hour)
	rep := svc.SyncCalendar([]icsimport.Occurrence{longer}, nil, until)
	if rep.Updated != 1 || rep.Moved != 0 {
		t.Fatalf("report: %+v", rep)
	}
	got, _ := svc.EventByID(ev.ID)
	if got.Status != StatusConfirmed {
		t.Fatalf("end-only change reset the confirmation: %v", got.Status)
	}
	if !got.EndsAt.Equal(longer.End) {
		t.Fatal("end time not updated")
	}
	for _, o := range svc.OutboxItems(false) {
		if o.Purpose == "moved" {
			t.Fatalf("end-only change fanned out a MOVED notice: %+v", o)
		}
	}
}

// The created webhook fires with the import identity already set —
// consumers must be able to tell imported events from hand-created ones.
func TestImportCreatedWebhookCarriesSource(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	if _, err := svc.AddWebhook("https://consumer.example/hook", []string{"event.created"}); err != nil {
		t.Fatal(err)
	}
	now := *clock
	until := now.Add(60 * 24 * time.Hour)
	svc.SyncCalendar([]icsimport.Occurrence{occ("u1", "Session", now.Add(48*time.Hour), time.Hour)}, nil, until)
	saw := false
	for _, o := range svc.OutboxItems(false) {
		if o.Purpose != "webhook" || o.Subject != "event.created" {
			continue
		}
		saw = true
		var env struct {
			Data Event `json:"data"`
		}
		if err := json.Unmarshal([]byte(o.Body), &env); err != nil {
			t.Fatal(err)
		}
		if env.Data.SourceUID != "u1" || env.Data.SourceKey == "" {
			t.Fatalf("webhook payload missing source identity: %+v", env.Data)
		}
	}
	if !saw {
		t.Fatal("no event.created webhook enqueued")
	}
}

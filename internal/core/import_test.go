// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/icsimport"
)

// stubFeed answers calendar fetches in-process — no listener, so these
// tests run inside the sandbox.
type stubFeed struct {
	mu     sync.Mutex
	status int
	body   string
	calls  int
}

func (f *stubFeed) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return &http.Response{
		StatusCode: f.status, Header: http.Header{}, Request: r,
		Body: io.NopCloser(strings.NewReader(f.body)),
	}, nil
}

func (f *stubFeed) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *stubFeed) set(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func icsWith(uids ...string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	for i, uid := range uids {
		b.WriteString("BEGIN:VEVENT\r\nUID:" + uid + "\r\n")
		day := 15 + i
		b.WriteString("DTSTART:202608" + string(rune('0'+day/10)) + string(rune('0'+day%10)) + "T100000Z\r\n")
		b.WriteString("DTEND:202608" + string(rune('0'+day/10)) + string(rune('0'+day%10)) + "T110000Z\r\n")
		b.WriteString("SUMMARY:" + uid + "\r\nEND:VEVENT\r\n")
	}
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

// newFeedService is newTestService plus a configured source served by
// the stub and, when every > 0, the automatic fetcher armed.
func newFeedService(t *testing.T, fake *fakeNotifier, feed *stubFeed, every time.Duration) (*Service, *time.Time) {
	t.Helper()
	store, err := NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(store, Config{
		BaseURL: "http://test.local", AdminNotify: &Address{Kind: "email", To: "admin@test.local"},
		CalendarSource: "https://feed.test/cal.ics", CalendarWindow: 60 * 24 * time.Hour,
		CalendarFetchEvery: every,
	}, fake)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := &now
	svc.SetClock(func() time.Time { return *clock })
	svc.SetCalendarClient(&http.Client{Transport: feed})
	svc.logf = t.Logf
	return svc, clock
}

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

// The importer runs the move transaction with no note: the MOVED body
// never carried "calendar update", but it overwrote whatever the
// operator had written on the event.
func TestImportMoveKeepsOperatorNote(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	now := *clock
	until := now.Add(60 * 24 * time.Hour)
	start := now.Add(90 * time.Hour)
	svc.SyncCalendar([]icsimport.Occurrence{occ("u1", "Session", start, time.Hour)}, nil, until)
	ev := svc.Events()[0]
	if _, err := svc.MoveEvent(ev.ID, start.Add(time.Hour), time.Time{}, "bring the spare keys", "admin"); err != nil {
		t.Fatal(err)
	}

	moved := occ("u1", "Session", start, time.Hour)
	moved.Start = start.Add(48 * time.Hour)
	moved.End = moved.Start.Add(time.Hour)
	if rep := svc.SyncCalendar([]icsimport.Occurrence{moved}, nil, until); rep.Moved != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if got, _ := svc.EventByID(ev.ID); got.Note != "bring the spare keys" {
		t.Fatalf("source move overwrote the operator note: %q", got.Note)
	}
}

// A rescheduled series is thirty moves in one sync. Each empty fan-out is
// audited, the admin is paged once per fetch, and the report lists them.
func TestImportMovesDoNotFloodAdmin(t *testing.T) {
	fake := &fakeNotifier{}
	svc, clock := newTestService(t, fake)
	now := *clock
	until := now.Add(60 * 24 * time.Hour)
	var occs []icsimport.Occurrence
	for i := range 3 {
		occs = append(occs, occ("series", "Weekly", now.Add(time.Duration(72+i*168)*time.Hour), time.Hour))
	}
	svc.SyncCalendar(occs, nil, until)
	for i := range occs {
		occs[i].Start = occs[i].Start.Add(2 * time.Hour)
		occs[i].End = occs[i].Start.Add(time.Hour)
	}
	rep := svc.SyncCalendar(occs, nil, until)
	if rep.Moved != 3 || len(rep.Silent) != 3 {
		t.Fatalf("report: %+v", rep)
	}
	svc.Tick(now)
	pages := 0
	for _, m := range fake.byPurposeTo("admin@test.local") {
		if strings.Contains(strings.ToLower(m.Subject), "nobody was told") {
			pages++
		}
	}
	if pages != 1 {
		t.Fatalf("want exactly 1 page for 3 silent moves, got %d", pages)
	}
	if got := svc.LastImport(); got == nil || len(got.Silent) != 3 {
		t.Fatalf("LastImport does not carry the silent list: %+v", got)
	}
	if auditCount(t, svc, "propagation.empty") != 3 {
		t.Fatal("each silent move must still be audited")
	}
}

// A vanished occurrence is an open decision, not a line in one report:
// the marker sticks with its first timestamp across fetches, the admin
// is paged on the transition only, and the reminder tells the
// responsible what happened.
func TestVanishedIsStickyAcrossFetches(t *testing.T) {
	fake := &fakeNotifier{}
	feed := &stubFeed{status: 200, body: icsWith("stays", "goes")}
	svc, clock := newFeedService(t, fake, feed, 0)
	if _, err := svc.FetchCalendar(context.Background()); err != nil {
		t.Fatal(err)
	}
	feed.set(200, icsWith("stays"))
	if _, err := svc.FetchCalendar(context.Background()); err != nil {
		t.Fatal(err)
	}
	var goes Event
	for _, e := range svc.Events() {
		if e.SourceUID == "goes" {
			goes = e
		}
	}
	if goes.VanishedAt.IsZero() || !goes.VanishedAt.Equal(*clock) {
		t.Fatalf("vanished marker not set on first disappearance: %+v", goes)
	}
	first := goes.VanishedAt

	*clock = clock.Add(time.Hour)
	if _, err := svc.FetchCalendar(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.EventByID(goes.ID)
	if !got.VanishedAt.Equal(first) {
		t.Fatalf("marker must keep its first timestamp, got %v want %v", got.VanishedAt, first)
	}
	if got.Status == StatusCancelled {
		t.Fatal("a vanished event was cancelled by the import")
	}
	svc.Tick(*clock)
	pages := adminMessages(fake, "Gone from the calendar")
	if len(pages) != 1 || !strings.Contains(pages[0].Body, "goes") {
		t.Fatalf("want exactly 1 page across 2 vanished fetches, got %d: %+v", len(pages), pages)
	}
	if vs := svc.VanishedEvents(); len(vs) != 1 || vs[0].ID != goes.ID {
		t.Fatalf("VanishedEvents: %+v", vs)
	}

	// The reminder to the responsible carries the note.
	ana := mustPerson(t, svc, "ana", TrustRespond)
	if err := svc.Assign(goes.ID, ana.ID, ""); err != nil {
		t.Fatal(err)
	}
	*clock = goes.StartsAt.Add(-40 * time.Hour)
	svc.Tick(*clock)
	rem := fake.byPurposeTo("ana@test.local")
	if len(rem) != 1 || !strings.Contains(rem[0].Body, "disappeared from the source calendar") {
		t.Fatalf("reminder lacks the vanished note: %+v", rem)
	}
}

// An empty result is a broken feed until proven otherwise: no markers.
func TestSuspectFetchLeavesVanishedAtUntouched(t *testing.T) {
	fake := &fakeNotifier{}
	feed := &stubFeed{status: 200, body: icsWith("a", "b")}
	svc, clock := newFeedService(t, fake, feed, 0)
	if _, err := svc.FetchCalendar(context.Background()); err != nil {
		t.Fatal(err)
	}
	feed.set(200, "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")
	rep, err := svc.FetchCalendar(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Suspect {
		t.Fatalf("empty feed must be suspect: %+v", rep)
	}
	for _, e := range svc.Events() {
		if !e.VanishedAt.IsZero() {
			t.Fatalf("suspect fetch set a vanished marker: %+v", e)
		}
	}
	svc.Tick(*clock)
	if len(adminMessages(fake, "Gone from the calendar")) != 0 {
		t.Fatal("suspect fetch paged about vanished events")
	}
}

func TestVanishedClearsOnReappear(t *testing.T) {
	fake := &fakeNotifier{}
	feed := &stubFeed{status: 200, body: icsWith("a", "b")}
	svc, _ := newFeedService(t, fake, feed, 0)
	svc.FetchCalendar(context.Background())
	feed.set(200, icsWith("a"))
	svc.FetchCalendar(context.Background())
	if len(svc.VanishedEvents()) != 1 {
		t.Fatal("setup: b should be vanished")
	}
	feed.set(200, icsWith("a", "b"))
	svc.FetchCalendar(context.Background())
	if vs := svc.VanishedEvents(); len(vs) != 0 {
		t.Fatalf("reappeared event still marked: %+v", vs)
	}
	if auditCount(t, svc, "import.reappeared") != 1 {
		t.Fatal("import.reappeared not audited once")
	}
}

func TestCancelledEventNeverMarkedVanished(t *testing.T) {
	fake := &fakeNotifier{}
	feed := &stubFeed{status: 200, body: icsWith("a", "b")}
	svc, _ := newFeedService(t, fake, feed, 0)
	svc.FetchCalendar(context.Background())
	var b Event
	for _, e := range svc.Events() {
		if e.SourceUID == "b" {
			b = e
		}
	}
	if _, err := svc.CancelEvent(b.ID, "", "off", "admin"); err != nil {
		t.Fatal(err)
	}
	feed.set(200, icsWith("a"))
	svc.FetchCalendar(context.Background())
	got, _ := svc.EventByID(b.ID)
	if !got.VanishedAt.IsZero() || len(svc.VanishedEvents()) != 0 {
		t.Fatalf("cancelled event marked vanished: %+v", got)
	}
}

// Fetch failures page once per episode, audit every time, and announce
// the recovery — three 500s are one problem, not three.
func TestImportFailurePagesOncePerEpisode(t *testing.T) {
	fake := &fakeNotifier{}
	feed := &stubFeed{status: 500}
	svc, clock := newFeedService(t, fake, feed, time.Hour)
	ctx := context.Background()
	for range 3 {
		svc.fetchOnce(ctx)
	}
	svc.Tick(*clock)
	if n := len(adminMessages(fake, "Calendar fetch failing")); n != 1 {
		t.Fatalf("want 1 failure page for 3 failed fetches, got %d", n)
	}
	if auditCount(t, svc, "import.failed") != 3 {
		t.Fatal("every failed fetch must be audited")
	}
	feed.set(200, icsWith("a"))
	svc.fetchOnce(ctx)
	svc.Tick(*clock)
	if n := len(adminMessages(fake, "Calendar fetch recovered")); n != 1 {
		t.Fatalf("want 1 recovery page, got %d", n)
	}
	if auditCount(t, svc, "import.recovered") != 1 {
		t.Fatal("recovery not audited")
	}
	// A new episode pages again.
	feed.set(500, "")
	svc.fetchOnce(ctx)
	svc.Tick(*clock)
	if n := len(adminMessages(fake, "Calendar fetch failing")); n != 2 {
		t.Fatalf("second episode not paged: %d", n)
	}
}

// Shutdown mid-fetch is not a failure: no audit, no page, and the loop
// returns.
func TestCalendarFetcherStopsOnContextCancel(t *testing.T) {
	fake := &fakeNotifier{}
	feed := &stubFeed{status: 200, body: icsWith("a")}
	svc, _ := newFeedService(t, fake, feed, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		svc.RunCalendarFetcher(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fetcher did not stop on a cancelled context")
	}
	if auditCount(t, svc, "import.failed") != 0 {
		t.Fatal("a cancelled context was booked as a fetch failure")
	}
	if len(svc.OutboxItems(true)) != 0 {
		t.Fatal("a cancelled context paged the admin")
	}

	// And an unarmed fetcher (no interval) returns at once, without a
	// single request.
	feed2 := &stubFeed{status: 200, body: icsWith("a")}
	svc2, _ := newFeedService(t, fake, feed2, 0)
	done2 := make(chan struct{})
	go func() {
		svc2.RunCalendarFetcher(context.Background())
		close(done2)
	}()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("fetcher without interval did not return")
	}
	if feed2.count() != 0 {
		t.Fatal("fetcher without interval fetched")
	}
}

func TestFetchEveryRequiresSource(t *testing.T) {
	store, err := NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(store, Config{CalendarFetchEvery: time.Hour}, &fakeNotifier{}); err == nil {
		t.Fatal("calendar_fetch_every without calendar_source must be refused")
	}
}

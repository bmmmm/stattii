// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The credentials below are sentinels: shaped like the real thing,
// impossible to collide with one. A transport failure (dead port,
// connection refused) is what makes them leak — Go renders the whole
// request URL into the *url.Error. An application-level rejection
// (HTTP 401/500) stays clean, so a stub server would prove nothing.
const (
	sentinelTokenPath  = "/bot111111:SENTINELdoNOTusePLACEHOLDERvalue/sendMessage"
	sentinelTokenValue = "111111:SENTINELdoNOTusePLACEHOLDERvalue"
	sentinelHookURL    = "http://127.0.0.1:1/services/SENTINELwebhookSECRETpath"
	sentinelHookSecret = "SENTINELwebhookSECRETpath"
	sentinelFeedURL    = "http://127.0.0.1:1/s/SENTINELfeedSECRETpath.ics"
	sentinelFeedSecret = "SENTINELfeedSECRETpath"
	deadPort           = "http://127.0.0.1:1"
)

// transportError performs a real request against a dead port and returns
// the resulting *url.Error — the genuine article, not a hand-built
// string that could pass for the wrong reason.
func transportError(t *testing.T, rawURL string) error {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(rawURL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("want a transport failure for %s, got a response", rawURL)
	}
	return err
}

// transportFailer fails every send with a per-kind transport error.
type transportFailer struct{ errs map[string]error }

func (f *transportFailer) Send(kind, to string, m Message) error { return f.errs[kind] }

func auditDump(t *testing.T, svc *Service) string {
	t.Helper()
	entries, err := svc.Audit(200)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.Kind)
		b.WriteString(" ")
		b.Write(e.Data)
		b.WriteString("\n")
	}
	return b.String()
}

func mustNotContain(t *testing.T, what, haystack, secret string) {
	t.Helper()
	if strings.Contains(haystack, secret) {
		t.Fatalf("credential leaked into %s:\n%s", what, haystack)
	}
}

// TestOutboxTransportErrorHidesCredentials — M1 and M12 on the delivery
// path: a bot token in a failing send and a webhook target URL must
// reach neither LastError, nor the delivery.fail audit entry, nor the
// escalation body, while host and failure kind survive.
func TestOutboxTransportErrorHidesCredentials(t *testing.T) {
	fake := &transportFailer{errs: map[string]error{
		"telegram": transportError(t, deadPort+sentinelTokenPath),
		"webhook":  transportError(t, sentinelHookURL),
	}}
	store, err := NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(store, Config{
		BaseURL:       "http://test.local",
		EscalateAfter: 10 * time.Minute,
		AdminNotify:   &Address{Kind: "email", To: "admin@test.local"},
	}, fake)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now })
	svc.logf = t.Logf

	svc.state.Outbox = append(svc.state.Outbox,
		OutboxItem{ID: "ob_tg", Purpose: "reminder", Kind: "telegram", To: "12345",
			Subject: "Please confirm", Body: "Tuesday Session", CreatedAt: now},
		OutboxItem{ID: "ob_hook", Purpose: "webhook", Kind: "webhook", To: sentinelHookURL,
			Subject: "event.cancelled", Body: "{}", CreatedAt: now},
	)

	svc.Tick(now)

	for _, o := range svc.state.Outbox {
		mustNotContain(t, "LastError of "+o.ID, o.LastError, sentinelTokenValue)
		mustNotContain(t, "LastError of "+o.ID, o.LastError, sentinelHookSecret)
		if o.LastError == "" {
			t.Fatalf("%s: no error recorded — the send did not fail as intended", o.ID)
		}
		if !strings.Contains(o.LastError, "127.0.0.1:1") {
			t.Fatalf("%s: redaction destroyed the host:\n%s", o.ID, o.LastError)
		}
		t.Logf("operator sees for %s: %s", o.ID, o.LastError)
	}

	log := auditDump(t, svc)
	mustNotContain(t, "the audit trail", log, sentinelTokenValue)
	mustNotContain(t, "the audit trail", log, sentinelHookSecret)
	if !strings.Contains(log, "delivery.fail") {
		t.Fatalf("no delivery.fail audit entry:\n%s", log)
	}

	// Past EscalateAfter the stuck items page the admin — that body
	// carries both LastError and the target address.
	later := now.Add(11 * time.Minute)
	svc.SetClock(func() time.Time { return later })
	svc.Tick(later)

	escalations := 0
	for _, o := range svc.state.Outbox {
		if o.Purpose != "escalation" {
			continue
		}
		escalations++
		mustNotContain(t, "the escalation body", o.Subject+"\n"+o.Body, sentinelTokenValue)
		mustNotContain(t, "the escalation body", o.Subject+"\n"+o.Body, sentinelHookSecret)
		t.Logf("admin reads: %s", o.Body)
	}
	if escalations != 2 {
		t.Fatalf("want 2 escalations, got %d", escalations)
	}
	mustNotContain(t, "the audit trail after escalation", auditDump(t, svc), sentinelTokenValue)
	mustNotContain(t, "the audit trail after escalation", auditDump(t, svc), sentinelHookSecret)
}

// TestCalendarSourceParseFailureHidesSourceURL — M2 on the other error
// return in FetchCalendar: a source that passes the http(s) prefix check
// but does not parse (a trailing newline on a copy-pasted config value
// is the realistic trigger) fails before any request is made. That error
// goes straight to the operator — POST /api/v1/calendar/fetch renders it
// into the JSON body, POST /admin/calendar/fetch onto the error page —
// and it never passes noteImportResultLocked, so the bookkeeping's
// redaction never sees it. A malformed URL is also exactly the shape a
// URL-parsing scrub cannot cut down, so the address must be dropped
// whole and only the reason kept.
func TestCalendarSourceParseFailureHidesSourceURL(t *testing.T) {
	// Every shape carries the same sentinel secret path.
	shapes := map[string]string{
		"trailing newline":   sentinelFeedURL + "\n",
		"control char":       "http://127.0.0.1:1/s/" + sentinelFeedSecret + "\x00.ics",
		"del char":           "http://127.0.0.1:1/s/" + sentinelFeedSecret + "\x7f.ics",
		"bad escape in path": "http://127.0.0.1:1/s/%zz" + sentinelFeedSecret + ".ics",
		"invalid port":       "http://127.0.0.1:xx/s/" + sentinelFeedSecret + ".ics",
		"bad escape in host": "http://12%zz7.0.0.1:1/s/" + sentinelFeedSecret + ".ics",
	}
	for name, src := range shapes {
		t.Run(name, func(t *testing.T) {
			store, err := NewJSONStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			svc, err := NewService(store, Config{
				BaseURL:        "http://test.local",
				CalendarSource: src,
				CalendarWindow: 60 * 24 * time.Hour,
				AdminNotify:    &Address{Kind: "email", To: "admin@test.local"},
			}, &fakeNotifier{})
			if err != nil {
				t.Fatal(err)
			}
			svc.logf = t.Logf

			_, err = svc.FetchCalendar(context.Background())
			if err == nil {
				t.Fatal("a source that does not parse must fail the fetch")
			}
			mustNotContain(t, "the returned fetch error", err.Error(), sentinelFeedSecret)
			t.Logf("operator sees: %s", err.Error())
		})
	}
}

// TestCalendarFetchFailureHidesSourceURL — M2: a secret iCal address is
// the credential, so a failing automatic fetch must not write it into
// the audit trail, the returned error, or the "Calendar fetch failing"
// mail.
func TestCalendarFetchFailureHidesSourceURL(t *testing.T) {
	store, err := NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(store, Config{
		BaseURL:        "http://test.local",
		CalendarSource: sentinelFeedURL,
		CalendarWindow: 60 * 24 * time.Hour,
		AdminNotify:    &Address{Kind: "email", To: "admin@test.local"},
	}, &fakeNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now })
	svc.logf = t.Logf
	svc.SetCalendarClient(&http.Client{Timeout: 5 * time.Second})

	// The automatic path first: audit entry plus the admin page. The
	// sinks are asserted before the returned error so that neutering the
	// redaction shows THEM going red, not only the error string.
	svc.fetchOnce(context.Background())

	log := auditDump(t, svc)
	mustNotContain(t, "the audit trail", log, sentinelFeedSecret)
	if !strings.Contains(log, "import.failed") {
		t.Fatalf("no import.failed audit entry:\n%s", log)
	}

	found := false
	for _, o := range svc.state.Outbox {
		if o.Purpose != "escalation" {
			continue
		}
		found = true
		mustNotContain(t, "the escalation body", o.Subject+"\n"+o.Body, sentinelFeedSecret)
		t.Logf("admin reads: %s", o.Body)
	}
	if !found {
		t.Fatal("the failing fetch paged nobody")
	}

	// The manual path returns the error to the panel and the API.
	if _, err := svc.FetchCalendar(context.Background()); err == nil {
		t.Fatal("want a transport failure against a dead port")
	} else {
		mustNotContain(t, "the returned fetch error", err.Error(), sentinelFeedSecret)
		if !strings.Contains(err.Error(), "127.0.0.1:1") {
			t.Fatalf("redaction destroyed the host:\n%s", err.Error())
		}
	}
}

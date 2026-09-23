// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"fmt"
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
		// Fails the http(s) prefix check, whose error must not echo the
		// value: redactURLs only knows http(s), so this one would pass it.
		"foreign scheme": "ftp://127.0.0.1:1/s/" + sentinelFeedSecret + ".ics",
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

// TestRedactURLsCutsEveryShape — the scrub itself: a scheme in capitals
// (url.Parse accepts it, so it validates and delivers) and a URL that
// does not parse (a bare % in the path) must both lose their secret; a
// bracket around the URL is not part of it, an IPv6 host's brackets are.
func TestRedactURLsCutsEveryShape(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"upper-case scheme": {`Post "HTTPS://hooks.example/services/T0/B0/SECRET": status 500`,
			`Post "https://hooks.example/[redacted]": status 500`},
		"bad escape in path": {`parse "http://hooks.example/%zzSECRET": invalid URL escape "%zz"`,
			`parse "http://[redacted]": invalid URL escape "%zz"`},
		"bad escape, capitals, punctuation": {`see HTTP://h/%SECRET.`, `see http://[redacted].`},
		"no host":                           {`http:///SECRET`, `http://[redacted]`},
		"bracketed":                         {`[https://h/a/SECRET]`, `[https://h/[redacted]]`},
		"ipv6 host":                         {`http://[::1]:8080/SECRET`, `http://[::1]:8080/[redacted]`},
		"bracketed ipv6":                    {`[https://[::1]/SECRET]`, `[https://[::1]/[redacted]]`},
		"host only stays":                   {`dial https://h: refused`, `dial https://h: refused`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := redactURLs(c.in); got != c.want {
				t.Fatalf("redactURLs(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// newRedactService is a service whose sends fail through fake and whose
// stuck items page an admin after ten minutes.
func newRedactService(t *testing.T, fake Notifier, now time.Time) *Service {
	t.Helper()
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
	svc.SetClock(func() time.Time { return now })
	svc.logf = t.Logf
	return svc
}

// TestWebhookTargetShapesTheScrubMissedStayHidden — #22 items 1 and 2
// end to end: a webhook target in capitals, one that fails
// http.NewRequest, one that is no http(s) URL at all, and one whose path
// holds a character the URL regex stops at, all used to reach LastError,
// the delivery.fail audit entry and the escalation mail in full.
// AddWebhook checks for empty only, so every one of them can be stored.
func TestWebhookTargetShapesTheScrubMissedStayHidden(t *testing.T) {
	requestErr := func(target string) error {
		t.Helper()
		_, err := http.NewRequest(http.MethodPost, target, nil)
		if err == nil {
			t.Fatalf("want http.NewRequest to reject %q", target)
		}
		return err
	}
	upper := "HTTPS://127.0.0.1:1/services/" + sentinelHookSecret
	badEscape := "http://127.0.0.1:1/services/%zz" + sentinelHookSecret
	paren := "http://127.0.0.1:1/services/a)" + sentinelHookSecret
	schemeless := "hooks.example/services/" + sentinelHookSecret
	schemelessSpace := "hooks.example/services/a b/" + sentinelHookSecret
	backslash := "127.0.0.1:1/services/a\\" + sentinelHookSecret
	httpBackslash := "http://127.0.0.1:1/services/a\\" + sentinelHookSecret
	ftpUser := "ftp://user:pw@127.0.0.1:1/services/" + sentinelHookSecret
	ftpEmptyPort := "ftp://127.0.0.1:/services/" + sentinelHookSecret
	quoteInQuery := "https://127.0.0.1:1/a b?x=\"" + sentinelHookSecret
	shapes := map[string]struct {
		to  string
		err error
	}{
		// The channel's own non-2xx error echoes the target as given.
		"upper-case scheme": {upper, fmt.Errorf("webhook %s: status %d", upper, 500)},
		"bad escape":        {badEscape, requestErr(badEscape)},
		// The regex ends the URL at ')' and leaves the rest standing.
		"paren in path": {paren, transportError(t, paren)},
		// "unsupported protocol scheme": the client echoes the target.
		"no scheme": {schemeless, transportError(t, schemeless)},
		// ... re-serialised by url.Parse, so the space reads %20.
		"no scheme, space in path": {schemelessSpace, transportError(t, schemelessSpace)},
		// A parse error quotes it with %q, so the backslash is doubled.
		"no scheme, backslash": {backslash, requestErr(backslash)},
		// The regex also ends at '\\'; the status error echoes it raw.
		"backslash in path": {httpBackslash, fmt.Errorf("webhook %s: status %d", httpBackslash, 500)},
		// The client masks the password and drops an empty port, and
		// the url.Error %q-quotes its own re-serialisation: spellings
		// that match the target by value in none of its forms.
		"ftp with userinfo": {ftpUser, transportError(t, ftpUser)},
		"ftp, empty port":   {ftpEmptyPort, transportError(t, ftpEmptyPort)},
		"quote in query":    {quoteInQuery, transportError(t, quoteInQuery)},
	}
	for name, sh := range shapes {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
			svc := newRedactService(t, &transportFailer{errs: map[string]error{"webhook": sh.err}}, now)
			svc.state.Outbox = append(svc.state.Outbox, OutboxItem{ID: "ob_hook", Purpose: "webhook",
				Kind: "webhook", To: sh.to, Subject: "event.cancelled", Body: "{}", CreatedAt: now})

			svc.Tick(now)
			o := svc.outboxItemLocked("ob_hook")
			if o.LastError == "" {
				t.Fatal("no error recorded — the send did not fail as intended")
			}
			mustNotContain(t, "LastError", o.LastError, sentinelHookSecret)
			mustNotContain(t, "the audit trail", auditDump(t, svc), sentinelHookSecret)

			later := now.Add(11 * time.Minute)
			svc.SetClock(func() time.Time { return later })
			svc.Tick(later)
			escalations := 0
			for _, e := range svc.state.Outbox {
				if e.Purpose == "escalation" {
					escalations++
					mustNotContain(t, "the escalation body", e.Subject+"\n"+e.Body, sentinelHookSecret)
				}
			}
			if escalations != 1 {
				t.Fatalf("want 1 escalation, got %d", escalations)
			}
		})
	}
}

// TestWebhookErrorKeepsItsReason — the structural cut keeps what the
// operator needs: the operation and the failure, with the target reduced
// to its host — even for a one-letter target that a by-value search
// would smear across the whole message.
func TestWebhookErrorKeepsItsReason(t *testing.T) {
	got := redactDeliveryErr("webhook", "e", transportError(t, "e")).Error()
	if want := `Get "[redacted]": unsupported protocol scheme ""`; got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	got = redactDeliveryErr("webhook", sentinelHookURL, transportError(t, sentinelHookURL)).Error()
	if !strings.HasPrefix(got, `Get "http://127.0.0.1:1": `) || !strings.Contains(got, "connect") {
		t.Fatalf("lost the host or the failure kind: %q", got)
	}
}

// TestWebhookCreatedAuditHidesTargetURL — #22 item 3: registration wrote
// the target URL, which delivery treats as the credential, into the audit
// trail in clear. The record keeps scheme and host only, and drops a
// target that does not parse whole.
func TestWebhookCreatedAuditHidesTargetURL(t *testing.T) {
	// want is what the record keeps: which host, or nothing at all.
	targets := map[string]struct{ target, want string }{
		"plain":             {sentinelHookURL, "http://127.0.0.1:1"},
		"upper-case scheme": {"HTTPS://127.0.0.1:1/services/" + sentinelHookSecret, "https://127.0.0.1:1"},
		"bad escape":        {"http://127.0.0.1:1/services/%zz" + sentinelHookSecret, redactedMark},
		"no scheme":         {"127.0.0.1:1/services/" + sentinelHookSecret, redactedMark},
		"scheme-relative":   {"//127.0.0.1:1/services/" + sentinelHookSecret, redactedMark},
		"empty port":        {"http://127.0.0.1:/services/" + sentinelHookSecret, "http://127.0.0.1"},
	}
	for name, tc := range targets {
		t.Run(name, func(t *testing.T) {
			svc := newRedactService(t, &fakeNotifier{}, time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC))
			if _, err := svc.AddWebhook(tc.target, nil); err != nil {
				t.Fatal(err)
			}
			log := auditDump(t, svc)
			if !strings.Contains(log, "webhook.created") {
				t.Fatalf("no webhook.created audit entry:\n%s", log)
			}
			mustNotContain(t, "the audit trail", log, sentinelHookSecret)
			if !strings.Contains(log, `"url":"`+tc.want+`"`) {
				t.Fatalf("want the record to keep %q:\n%s", tc.want, log)
			}
			t.Logf("audit reads: %s", log)
		})
	}
}

// TestSMTPErrorKeepsItsHelpLink — #22 item 4: an e-mail target is not a
// credential, and the SMTP server's answer names the reason with a link.
// The panel is the only place the operator sees it while mail is down,
// so it must arrive whole.
func TestSMTPErrorKeepsItsHelpLink(t *testing.T) {
	const reason = "535 5.7.8 Username and Password not accepted. " +
		"https://support.google.com/mail/?p=BadCredentials"
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	svc := newRedactService(t, &transportFailer{errs: map[string]error{"email": errors.New(reason)}}, now)
	svc.state.Outbox = append(svc.state.Outbox, OutboxItem{ID: "ob_mail", Purpose: "reminder",
		Kind: "email", To: "alex@example.org", Subject: "Please confirm", Body: "b", CreatedAt: now})

	svc.Tick(now)
	if got := svc.outboxItemLocked("ob_mail").LastError; got != reason {
		t.Fatalf("the SMTP reason was altered:\n got %q\nwant %q", got, reason)
	}
	if log := auditDump(t, svc); !strings.Contains(log, "?p=BadCredentials") {
		t.Fatalf("delivery.fail lost the SMTP help link:\n%s", log)
	}
}

// TestImportBookkeepingRedactsWhatReachesIt — #22 item 5, the second line
// of defence: whatever error reaches the import bookkeeping unredacted
// must still not land in the audit trail or the admin mail.
func TestImportBookkeepingRedactsWhatReachesIt(t *testing.T) {
	svc := newRedactService(t, &fakeNotifier{}, time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC))
	raw := fmt.Errorf("fetch calendar: Get %q: dial tcp 127.0.0.1:1: connect: connection refused", sentinelFeedURL)

	svc.mu.Lock()
	svc.noteImportResultLocked(context.Background(), raw)
	svc.mu.Unlock()

	log := auditDump(t, svc)
	if !strings.Contains(log, "import.failed") {
		t.Fatalf("no import.failed audit entry:\n%s", log)
	}
	mustNotContain(t, "the audit trail", log, sentinelFeedSecret)
	paged := false
	for _, o := range svc.state.Outbox {
		if o.Purpose == "escalation" {
			paged = true
			mustNotContain(t, "the escalation body", o.Subject+"\n"+o.Body, sentinelFeedSecret)
		}
	}
	if !paged {
		t.Fatal("the failure paged nobody")
	}
}

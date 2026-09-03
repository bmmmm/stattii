// SPDX-License-Identifier: GPL-3.0-or-later

package channel

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

// sentinelToken is a made-up bot token: it must never match a real one,
// and it must never be redacted away by accident either.
const sentinelToken = "111111:SENTINELdoNOTusePLACEHOLDERvalue"

// deadPort is a port nothing listens on, so every request against it
// fails at the transport layer (connection refused). That is the shape
// that leaks: Go renders the full request URL — token included — into
// the *url.Error it returns. An application-level Telegram rejection
// (HTTP 401, ok:false) stays clean, so a stub server would prove
// nothing here.
const deadPort = "http://127.0.0.1:1"

// assertRedacted is the whole point of these checks: the credential is
// gone, but the operator can still see which host failed and how.
func assertRedacted(t *testing.T, err error, secret string) {
	t.Helper()
	if err == nil {
		t.Fatal("want a transport error against a dead port, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("credential leaked into the error string:\n%s", err.Error())
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("redaction destroyed the host — operator cannot tell what failed:\n%s", err.Error())
	}
	if !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "connect") {
		t.Fatalf("redaction destroyed the failure kind:\n%s", err.Error())
	}
	t.Logf("operator sees: %s", err.Error())
}

// TestTelegramSendTransportErrorHidesToken — the send path (M1).
func TestTelegramSendTransportErrorHidesToken(t *testing.T) {
	tg := &Telegram{Token: sentinelToken, BaseURL: deadPort}
	err := tg.Send("12345", core.Message{Subject: "s", Body: "b"})
	assertRedacted(t, err, sentinelToken)
}

// TestTelegramPollerTransportErrorHidesToken — the poll path (M1). Both
// legs carry the token in the URL and both errors reach the process log.
func TestTelegramPollerTransportErrorHidesToken(t *testing.T) {
	p := &TelegramPoller{
		Token:   sentinelToken,
		BaseURL: deadPort,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	ctx := context.Background()

	_, err := p.getUpdates(ctx, -1, 0)
	assertRedacted(t, err, sentinelToken)

	assertRedacted(t, p.answer(ctx, "cb1", "text"), sentinelToken)
}

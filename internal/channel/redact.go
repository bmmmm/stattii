// SPDX-License-Identifier: GPL-3.0-or-later

package channel

import (
	"errors"
	"net/url"
	"strings"
)

// redactedMark replaces a credential in an error message. It is visible
// on purpose: an operator must be able to tell "hidden" from "empty".
const redactedMark = "[redacted]"

// redact removes a credential from an error before it leaves the
// channel. A transport failure (DNS, TLS, refused, timeout) yields a
// *url.Error, and that error renders the whole request URL — for the Bot
// API the URL carries the token in its path. From there it would flow
// into LastError, state.json, audit.jsonl, the admin timeline, the
// escalation mail, the process log and GET /api/v1/outbox. An
// application-level rejection never carries it, which is why this is an
// outage artifact and has to be cut off at the boundary, not at the
// sinks.
//
// Host, endpoint and failure kind survive — the operator still sees
// which host failed and how. The result deliberately does not wrap the
// original: an Unwrap would hand the un-redacted message straight back
// to whoever asks for it.
func redact(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	msg := err.Error()
	for _, form := range []string{secret, url.PathEscape(secret), url.QueryEscape(secret)} {
		msg = strings.ReplaceAll(msg, form, redactedMark)
	}
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

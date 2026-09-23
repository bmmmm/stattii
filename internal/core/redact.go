// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// redactedMark replaces the credential-bearing part of a URL. Visible on
// purpose: an operator must be able to tell "hidden" from "empty".
const redactedMark = "[redacted]"

// absURL matches an absolute http(s) URL as it appears inside an error
// message ("Get \"https://host/secret\": dial tcp …") or in an address
// field. The closing quote, whitespace and brackets end the match. The
// scheme is case-insensitive like url.Parse: "HTTPS://…" validates and
// delivers, so it must not slip past here.
var absURL = regexp.MustCompile(`(?i)https?://[^\s"'<>\\)]+`)

// redactURLs cuts every URL in s down to scheme://host, plus a marker
// when something was dropped. The path and query are the credential for
// the sources stattii talks to — a Slack or Discord webhook URL, a
// Google/Apple/Nextcloud "secret address" calendar feed, a Bot API token
// in the path — while userinfo is a credential by definition. Scheme and
// host stay because the operator still has to see WHICH host failed, and
// the surrounding error text (the failure kind) is untouched.
func redactURLs(s string) string {
	return absURL.ReplaceAllStringFunc(s, func(raw string) string {
		// Sentence punctuation glued to the end is not part of the URL,
		// nor is a closing bracket that opens nowhere inside it (an IPv6
		// host keeps its own).
		trimmed := strings.TrimRight(raw, ".,;:")
		if strings.HasSuffix(trimmed, "]") && strings.Count(trimmed, "]") > strings.Count(trimmed, "[") {
			trimmed = strings.TrimRight(trimmed[:len(trimmed)-1], ".,;:")
		}
		u, err := url.Parse(trimmed)
		if err != nil || u.Host == "" {
			// Fail closed: a URL that does not parse cannot be cut down
			// to its host, and it is exactly what a transport or request
			// error echoes — so the whole address goes, only the scheme
			// (matched literally above, never a secret) stays.
			scheme := strings.ToLower(raw[:strings.Index(raw, "://")])
			return scheme + "://" + redactedMark + raw[len(trimmed):]
		}
		out := u.Scheme + "://" + u.Host
		path := u.EscapedPath()
		if (path != "" && path != "/") || u.RawQuery != "" || u.User != nil {
			out += "/" + redactedMark
		}
		return out + raw[len(trimmed):]
	})
}

// redactTarget reduces an address that is itself a credential (a webhook
// target) to scheme://host for a record. Unlike redactURLs it does not
// search for a URL inside free text: whatever does not parse as an
// absolute URL is dropped whole instead of passed through.
func redactTarget(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return redactedMark
	}
	return u.Scheme + "://" + strings.TrimSuffix(u.Host, ":")
}

// redactTargetIn cuts a target that is itself the credential out of a
// free-text msg by value — the webhook channel's status error echoes it
// raw — then runs redactURLs over the rest. Searching for the known
// target is what makes this independent of its shape: a schemeless or
// ftp:// target, or a ')' or a space in the path, is no http(s) URL to
// the regex. A *url.Error, whose URL Go re-spells (re-serialised,
// password masked, empty port dropped, %q-quoted), never gets here —
// redactDeliveryErr rebuilds that one structurally.
func redactTargetIn(msg, target string) string {
	if target != "" {
		msg = strings.ReplaceAll(msg, target, redactTarget(target))
	}
	return redactURLs(msg)
}

// redactAddress renders a delivery target for a record: a webhook target
// is the credential and keeps scheme+host only; a mail address or chat
// id is no URL and passes redactURLs untouched.
func redactAddress(kind, to string) string {
	if kind == "webhook" {
		return redactTarget(to)
	}
	return redactURLs(to)
}

// redactDeliveryErr scrubs a failed send's error for the kinds whose
// delivery carries a credential: a webhook target is one, and the Bot
// API URL carries the bot token (the telegram channel cuts it already;
// this is the second line). An e-mail error stays whole — the SMTP
// answer's help link ("535 5.7.8 … https://support.google.com/mail/…")
// is the operator's only diagnosis while mail itself is down, and the
// panel is the only place it reaches them.
func redactDeliveryErr(kind, to string, err error) error {
	var msg string
	switch kind {
	case "webhook":
		// A request or transport failure is a *url.Error, and its URL is
		// the target in whichever spelling Go chose (re-serialised,
		// password masked, empty port dropped) — so it is replaced as a
		// field, not searched for. The reason beside it stays.
		if ue, ok := err.(*url.Error); ok {
			msg = ue.Op + " " + strconv.Quote(redactTarget(to)) + ": " + redactURLs(ue.Err.Error())
		} else {
			msg = redactTargetIn(err.Error(), to)
		}
	case "telegram":
		msg = redactURLs(err.Error())
	default:
		return err
	}
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

// redactErr is redactURLs on an error. It deliberately does not wrap the
// original: an Unwrap would hand the un-redacted message back to whoever
// asks for it.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	msg := redactURLs(err.Error())
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

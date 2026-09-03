// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// redactedMark replaces the credential-bearing part of a URL. Visible on
// purpose: an operator must be able to tell "hidden" from "empty".
const redactedMark = "[redacted]"

// absURL matches an absolute http(s) URL as it appears inside an error
// message ("Get \"https://host/secret\": dial tcp …") or in an address
// field. The closing quote, whitespace and brackets end the match.
var absURL = regexp.MustCompile(`https?://[^\s"'<>\\)]+`)

// redactURLs cuts every URL in s down to scheme://host, plus a marker
// when something was dropped. The path and query are the credential for
// the sources stattii talks to — a Slack or Discord webhook URL, a
// Google/Apple/Nextcloud "secret address" calendar feed, a Bot API token
// in the path — while userinfo is a credential by definition. Scheme and
// host stay because the operator still has to see WHICH host failed, and
// the surrounding error text (the failure kind) is untouched.
func redactURLs(s string) string {
	return absURL.ReplaceAllStringFunc(s, func(raw string) string {
		// Sentence punctuation glued to the end is not part of the URL.
		trimmed := strings.TrimRight(raw, ".,;:")
		u, err := url.Parse(trimmed)
		if err != nil || u.Host == "" {
			return raw
		}
		out := u.Scheme + "://" + u.Host
		path := u.EscapedPath()
		if (path != "" && path != "/") || u.RawQuery != "" || u.User != nil {
			out += "/" + redactedMark
		}
		return out + raw[len(trimmed):]
	})
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

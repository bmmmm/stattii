// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import (
	"regexp"
	"strings"
	"testing"
)

var tmplPostForm = regexp.MustCompile(`(?s)<form method="post" action="([^"]*)"[^>]*>(.*?)</form>`)

// The rendered sweep in admin_csrf_test.go only sees the forms a given
// state produces. This one reads the templates themselves, so a form
// behind a condition no test happens to trigger — a retry button, a
// calendar fetch — cannot slip through without its token.
func TestEveryTemplateFormCarriesTheCSRFToken(t *testing.T) {
	forms := 0
	for _, tp := range adminTmpl.Templates() {
		if tp.Tree == nil || tp.Tree.Root == nil {
			continue
		}
		for _, m := range tmplPostForm.FindAllStringSubmatch(tp.Tree.Root.String(), -1) {
			action, body := m[1], m[2]
			// The public token pages ride along in this template set
			// (adminTmpl clones it). They carry no cookie: the URL's
			// token IS the authority, so there is no ambient authority
			// for a foreign site to borrow — CSRF does not apply.
			if !strings.HasPrefix(action, "/admin/") {
				continue
			}
			if action == "/admin/login" {
				continue // no session to bind to yet; SameSite covers it
			}
			forms++
			if !strings.Contains(body, `{{template "csrf"`) {
				t.Errorf("%s: form %q renders no csrf token", tp.Name(), action)
			}
		}
	}
	// The panel had 20 mutating forms when this was written. A drop means
	// the markup moved and this test stopped looking at it.
	if forms < 20 {
		t.Fatalf("only %d mutating forms found in the templates — fix the pattern", forms)
	}
}

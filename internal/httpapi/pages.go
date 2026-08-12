// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import (
	"bytes"
	"errors"
	"html/template"
	"log"
	"net/http"

	"github.com/bmmmm/stattii/internal/core"
)

type actionData struct {
	View     core.ActionView
	Token    string
	Done     bool
	Conflict bool // tried to act on an already-cancelled event
	Proposed bool // a move proposal was just filed
}

type portalData struct {
	View  core.PortalView
	Token string
}

var tmpl = template.Must(template.New("").Parse(`
{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex">
<title>stattii</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:36rem;margin:2rem auto;padding:0 1rem;color:#1a1a1a;background:#fafafa}
@media(prefers-color-scheme:dark){body{color:#eaeaea;background:#111}.card{background:#1c1c1c !important;border-color:#333 !important}}
.card{background:#fff;border:1px solid #ddd;border-radius:8px;padding:1rem 1.25rem;margin:1rem 0}
h1{font-size:1.3rem}h2{font-size:1.05rem;margin:.2rem 0}
.badge{display:inline-block;padding:.1rem .5rem;border-radius:99px;font-size:.8rem;font-weight:600}
.scheduled{background:#fff3cd;color:#664d03}.confirmed{background:#d1e7dd;color:#0f5132}.cancelled{background:#f8d7da;color:#842029}
button{font:inherit;padding:.5rem 1.1rem;border-radius:6px;border:1px solid #888;cursor:pointer;background:#fff}
button.yes{background:#198754;border-color:#198754;color:#fff}
button.no{background:#dc3545;border-color:#dc3545;color:#fff}
form{display:inline-block;margin:.25rem .25rem .25rem 0}
input,textarea{font:inherit;padding:.35rem;border:1px solid #aaa;border-radius:4px;max-width:100%}
.muted{color:#777;font-size:.85rem}
details{margin:.5rem 0}
</style></head><body>{{end}}

{{define "status"}}<span class="badge {{.}}">{{.}}</span>{{end}}

{{define "event"}}
<h2>{{.Title}} {{template "status" .Status}}</h2>
<p>{{.StartsAt.Format "Mon, 02 Jan 2006 15:04"}}{{if .Location}} · {{.Location}}{{end}}</p>
{{if .Note}}<p class="muted">{{.Note}}</p>{{end}}
{{if .CancelReason}}<p class="muted">Reason: {{.CancelReason}}</p>{{end}}
{{end}}

{{define "action"}}{{template "head"}}
<h1>Hello {{.View.Person.Name}}</h1>
<div class="card">{{template "event" .View.Event}}</div>
{{if .Proposed}}
  <p><strong>Your new-time proposal was sent.</strong> The organizer decides; you will be notified.</p>
{{else if .Done}}
  {{if .Conflict}}<p>This event was <strong>already cancelled</strong> — no change possible.</p>
  {{else if eq .View.Action "confirm"}}<p><strong>Thanks — recorded as taking place.</strong></p>
  {{else}}<p><strong>Recorded: the event is cancelled.</strong> Everyone involved is being notified.</p>{{end}}
{{else if eq .View.Event.Status "cancelled"}}
  <p>This event is <strong>cancelled</strong>. Nothing to do.</p>
{{else}}
  {{if .View.Decided}}<p class="muted">You last answered: <strong>{{.View.Decided.Action}}</strong> ({{.View.Decided.At.Format "02 Jan 15:04"}}). You can change your answer until the event.</p>{{end}}
  <form method="post" action="/a/{{.Token}}">
    {{if eq .View.Action "confirm"}}<button class="yes" type="submit">Yes, this event takes place</button>
    {{else}}<button class="no" type="submit">Cancel this event</button>{{end}}
  </form>
  <p class="muted">One click on the button above is enough. This link is personal.</p>
  <details><summary>Suggest a new time instead</summary>
    <form method="post" action="/a/{{.Token}}/propose">
      <label>New start <input type="datetime-local" name="starts_at" required></label>
      <label>New end <input type="datetime-local" name="ends_at"></label>
      <input name="note" placeholder="Why?">
      <button type="submit">Send proposal</button>
    </form>
    <p class="muted">A proposal does not change anything by itself — the organizer decides.</p>
  </details>
{{end}}
</body></html>{{end}}

{{define "portal"}}{{template "head"}}
<h1>{{.View.Person.Name}} — your events</h1>
{{if not .View.Items}}<p>No upcoming events.</p>{{end}}
{{range .View.Items}}
<div class="card">
  {{template "event" .Event}}
  {{if .Response}}<p class="muted">Your answer: <strong>{{.Response.Action}}</strong> ({{.Response.At.Format "02 Jan 15:04"}})</p>{{end}}
  {{if ne .Event.Status "cancelled"}}
  <form method="post" action="/p/{{$.Token}}/respond">
    <input type="hidden" name="event_id" value="{{.Event.ID}}">
    <input type="hidden" name="action" value="confirm">
    <button class="yes" type="submit">Takes place</button>
  </form>
  <form method="post" action="/p/{{$.Token}}/respond">
    <input type="hidden" name="event_id" value="{{.Event.ID}}">
    <input type="hidden" name="action" value="cancel">
    <button class="no" type="submit">Cancel</button>
  </form>
  {{if $.View.CanPropose}}
  <details><summary>Move this event{{if not $.View.Direct}} (proposal){{end}}</summary>
    <form method="post" action="/p/{{$.Token}}/submit">
      <input type="hidden" name="kind" value="move">
      <input type="hidden" name="event_id" value="{{.Event.ID}}">
      <label>New start <input type="datetime-local" name="starts_at" required></label>
      <label>New end <input type="datetime-local" name="ends_at"></label>
      <input name="note" placeholder="Why?">
      <button type="submit">{{if $.View.Direct}}Move{{else}}Propose move{{end}}</button>
    </form>
  </details>
  {{end}}
  {{end}}
</div>
{{end}}
{{if .View.CanPropose}}
<div class="card">
  <h2>New event{{if not .View.Direct}} (proposal){{end}}</h2>
  <form method="post" action="/p/{{.Token}}/submit">
    <input type="hidden" name="kind" value="create">
    <label>Title <input name="title" required></label>
    <label>Start <input type="datetime-local" name="starts_at" required></label>
    <label>End <input type="datetime-local" name="ends_at"></label>
    <input name="note" placeholder="Note">
    <button type="submit">{{if .View.Direct}}Create{{else}}Propose{{end}}</button>
  </form>
</div>
{{end}}
<p class="muted">This page is personal — do not share the link.</p>
</body></html>{{end}}

{{define "error"}}{{template "head"}}
<h1>Nothing here</h1>
<p>{{.}}</p>
<p class="muted">If you expected an event page, ask the organizer for a fresh link.</p>
</body></html>{{end}}
`))

// renderTo executes into a buffer first: a template failure mid-render must
// become a clean 500, not corrupt HTML behind an already-written status.
func renderTo(w http.ResponseWriter, t *template.Template, name string, status int, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("stattii: render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	renderTo(w, tmpl, name, http.StatusOK, data)
}

func (s *Server) renderError(w http.ResponseWriter, err error) {
	msg := "This link is unknown."
	status := http.StatusNotFound
	switch {
	case errors.Is(err, core.ErrGone):
		msg = "This link has expired or was revoked."
		status = http.StatusGone
	case errors.Is(err, core.ErrForbidden):
		msg = "Your access level does not allow this."
		status = http.StatusForbidden
	case errors.Is(err, core.ErrNotFound):
	default:
		// Everything reaching this branch is validation text written for
		// recipients ("title is required") — keep it, but log it so a
		// future internal error leaking through gets noticed.
		log.Printf("stattii: public error page: %v", err)
		msg = err.Error()
		status = http.StatusBadRequest
	}
	renderTo(w, tmpl, "error", status, msg)
}

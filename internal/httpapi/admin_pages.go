// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import "html/template"

// adminTmpl clones the public template set so "head", "status", "event"
// and "error" render identically on both surfaces.
var adminTmpl = template.Must(template.Must(tmpl.Clone()).Parse(`
{{define "admin_nav"}}
<p class="muted"><a href="/admin">Events</a> · <a href="/admin/people">People</a>
<form method="post" action="/admin/logout" style="float:right"><button type="submit">Log out</button></form></p>
{{end}}

{{define "admin_login"}}{{template "head"}}
<h1>stattii admin</h1>
<div class="card">
<form method="post" action="/admin/login">
  <label>Admin token <input type="password" name="token" autofocus required></label>
  <button class="yes" type="submit">Log in</button>
</form>
{{if .}}<p><strong>{{.}}</strong></p>{{end}}
<p class="muted">The token lives in the data dir (admin-token) or STATTII_ADMIN_TOKEN.</p>
</div>
</body></html>{{end}}

{{define "admin_assignee"}}
{{if eq .Action "confirm"}}✓{{else if eq .Action "cancel"}}✗{{else}}–{{end}}
{{.Name}}{{if .Role}} ({{.Role}}){{end}}
{{- if .Action}} — {{.Action}} via {{.Via}}, {{.At.Format "02 Jan 15:04"}}{{else}} — no response yet{{end}}
{{end}}

{{define "admin_overview"}}{{template "head"}}
<h1>stattii admin</h1>
{{template "admin_nav"}}

{{if or .Proposals .Pending}}
<div class="card">
<h2>Needs attention</h2>
{{range .Proposals}}
  <p>Proposal <strong>{{.Kind}}</strong>{{if .Title}} „{{.Title}}"{{end}}{{if .EventID}} for {{.EventID}}{{end}}
  {{if not .StartsAt.IsZero}} → {{.StartsAt.Format "Mon, 02 Jan 15:04"}}{{end}}
  {{if .Note}}<span class="muted">({{.Note}})</span>{{end}}
  <form method="post" action="/admin/proposals/{{.ID}}"><input type="hidden" name="decision" value="accept"><button class="yes" type="submit">Accept</button></form>
  <form method="post" action="/admin/proposals/{{.ID}}"><input type="hidden" name="decision" value="reject"><button class="no" type="submit">Reject</button></form></p>
{{end}}
{{range .Pending}}
  <p>Undelivered {{.Purpose}} → {{.To}} ({{.Attempts}} attempts)
  <form method="post" action="/admin/outbox/{{.ID}}/retry"><button type="submit">Retry now</button></form></p>
{{end}}
</div>
{{end}}

{{range .Ov.Events}}
<div class="card">
  <h2><a href="/admin/event/{{.Event.ID}}">{{.Event.Title}}</a> {{template "status" .Event.Status}}</h2>
  <p>{{.Event.StartsAt.Format "Mon, 02 Jan 2006 15:04"}}{{if .Event.Location}} · {{.Event.Location}}{{end}}
  {{if not .Event.ReminderSentAt.IsZero}}<span class="muted"> · reminder sent</span>{{end}}</p>
  {{if not .Assignees}}<p class="muted">nobody assigned — the reminder waits</p>{{end}}
  {{range .Assignees}}<p>{{template "admin_assignee" .}}</p>{{end}}
</div>
{{end}}
{{if .Hidden}}<p class="muted">{{.Hidden}} past event(s) hidden — <a href="/admin?all=1">show all</a></p>{{end}}

<div class="card">
<h2>New event</h2>
<form method="post" action="/admin/events">
  <label>Title <input name="title" required></label>
  <label>Start <input type="datetime-local" name="starts_at" required></label>
  <label>End <input type="datetime-local" name="ends_at"></label>
  <label>Location <input name="location"></label>
  <input name="note" placeholder="Note">
  <label>If unconfirmed <select name="if_unconfirmed"><option value="notify">notify</option><option value="cancel">cancel (dead-man-switch)</option></select></label>
  <button class="yes" type="submit">Create</button>
</form>
</div>

<p class="muted">outbox: {{.Ov.Outbox.Delivered}} delivered · {{.Ov.Outbox.Pending}} pending · {{.Ov.Outbox.Failed}} failed · people: {{.Ov.People}}</p>
</body></html>{{end}}

{{define "admin_event"}}{{template "head"}}
<h1>stattii admin</h1>
{{template "admin_nav"}}

<div class="card">
{{template "event" .Ev.Event}}
{{if not .Ev.Assignees}}<p class="muted">nobody assigned — the reminder waits</p>{{end}}
{{range .Ev.Assignees}}<p>{{template "admin_assignee" .}}</p>{{end}}
</div>

<div class="card">
<h2>Actions</h2>
{{if eq .Ev.Event.Status "cancelled"}}
  <form method="post" action="/admin/event/{{.Ev.Event.ID}}/reinstate"><button class="yes" type="submit">Reinstate</button></form>
{{else}}
  <form method="post" action="/admin/event/{{.Ev.Event.ID}}/confirm"><button class="yes" type="submit">Confirm</button></form>
  <form method="post" action="/admin/event/{{.Ev.Event.ID}}/cancel">
    <input name="reason" placeholder="Reason">
    <button class="no" type="submit">Cancel event</button>
  </form>
  <details><summary>Move</summary>
    <form method="post" action="/admin/event/{{.Ev.Event.ID}}/move">
      <label>New start <input type="datetime-local" name="starts_at" required></label>
      <label>New end <input type="datetime-local" name="ends_at"></label>
      <input name="note" placeholder="Why?">
      <button type="submit">Move</button>
    </form>
  </details>
{{end}}
<details><summary>Assign someone</summary>
  <form method="post" action="/admin/event/{{.Ev.Event.ID}}/assign">
    <select name="person_id">{{range .People}}<option value="{{.ID}}">{{.Name}} ({{.Trust}})</option>{{end}}</select>
    <input name="role" placeholder="Role (optional)">
    <button type="submit">Assign</button>
  </form>
</details>
</div>

{{if .Responses}}
<div class="card">
<h2>Responses</h2>
{{range .Responses}}<p>{{.At.Format "02 Jan 15:04"}} — {{.PersonID}}: <strong>{{.Action}}</strong> via {{.Via}}</p>{{end}}
</div>
{{end}}

{{if .Propagation.Total}}
<div class="card">
<h2>Propagation</h2>
<p>{{.Propagation.Delivered}}/{{.Propagation.Total}} delivered{{if .Propagation.Failed}} · <strong>{{.Propagation.Failed}} FAILED</strong>{{end}}{{if .Propagation.Complete}} · complete{{end}}</p>
{{range .Propagation.Items}}<p class="muted">{{.Purpose}} → {{.To}} ({{.Kind}}), {{if .Delivered}}delivered {{.DeliveredAt.Format "02 Jan 15:04"}}{{else}}{{.Attempts}} attempts{{end}}</p>{{end}}
</div>
{{end}}
</body></html>{{end}}

{{define "admin_people"}}{{template "head"}}
<h1>stattii admin</h1>
{{template "admin_nav"}}

{{range .People}}
<div class="card">
  <h2>{{.Name}} <span class="badge scheduled">{{.Trust}}</span></h2>
  {{range .Channels}}<p class="muted">{{.Kind}}: {{.To}}</p>{{end}}
  <p class="muted">Portal: <a href="{{.PortalURL}}">{{.PortalURL}}</a></p>
</div>
{{end}}

<div class="card">
<h2>Add person</h2>
<form method="post" action="/admin/people">
  <label>Name <input name="name" required></label>
  <label>Trust <select name="trust"><option>respond</option><option>propose</option><option>direct</option></select></label>
  <label>Email <input name="email" type="email"></label>
  <label>Telegram chat id <input name="telegram"></label>
  <button class="yes" type="submit">Add</button>
</form>
</div>
</body></html>{{end}}
`))

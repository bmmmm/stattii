// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import "html/template"

// adminTmpl clones the public template set so "head", "status", "event"
// and "error" render identically on both surfaces. admin_head layers the
// admin-only styles (chips, timeline) on top.
var adminTmpl = template.Must(template.Must(tmpl.Clone()).Parse(`
{{define "admin_head"}}{{template "head"}}<style>
.chip{display:inline-block;border:1px solid #ccc;border-radius:99px;padding:.05rem .6rem;margin:.1rem .15rem 0 0;font-size:.9rem}
.chip.ok{border-color:#198754;color:#0f5132;background:#d1e7dd}
.chip.bad{border-color:#dc3545;color:#842029;background:#f8d7da}
@media(prefers-color-scheme:dark){.chip{border-color:#555}.chip.ok{background:#143;color:#9fd8b5}.chip.bad{background:#411;color:#e8a0a8}}
.tl{list-style:none;padding-left:.25rem;margin:.2rem 0 .6rem}
.tl li{padding:.05rem 0}
.bad{color:#c0392b}
</style>{{end}}

{{define "admin_nav"}}
<p class="muted"><a href="/admin">Events</a> · <a href="/admin/people">People</a>
<form method="post" action="/admin/logout" style="float:right"><button type="submit">Log out</button></form></p>
{{end}}

{{define "admin_login"}}{{template "admin_head"}}
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

{{define "admin_chip"}}<span class="chip {{if eq .Action "confirm"}}ok{{else if eq .Action "cancel"}}bad{{end}}">{{if eq .Action "confirm"}}✓{{else if eq .Action "cancel"}}✗{{else}}–{{end}} {{.Name}}{{if .Role}} ({{.Role}}){{end}}</span>{{end}}

{{define "admin_overview"}}{{template "admin_head"}}
<h1>stattii admin</h1>
{{template "admin_nav"}}

{{if .Calendar}}
<p><form method="post" action="/admin/calendar/fetch"><button class="yes" type="submit">Fetch calendar now</button></form>
{{with .LastImport}}<span class="muted">last fetch {{.FetchedAt.Format "02 Jan 15:04"}} — {{.Created}} new · {{.Moved}} moved · {{.Updated}} updated · {{.Unchanged}} unchanged</span>{{end}}</p>
{{end}}

{{$imp := .LastImport}}
{{if or .Proposals .Pending (and $imp (or $imp.Vanished $imp.Conflicts $imp.Skipped))}}
<div class="card">
<h2>Needs attention</h2>
{{if $imp}}
{{range $imp.Vanished}}<p class="bad">Gone from the calendar (NOT auto-cancelled — cancel it yourself if real): {{.}}</p>{{end}}
{{range $imp.Conflicts}}<p class="bad">Import conflict: {{.}}</p>{{end}}
{{range $imp.Skipped}}<p class="muted">Import skipped: {{.}}</p>{{end}}
{{end}}
{{range .Proposals}}
  <p>Proposal <strong>{{.Kind}}</strong>{{if .Title}} „{{.Title}}"{{end}}{{if .EventID}} for {{.EventID}}{{end}}
  {{if not .StartsAt.IsZero}} → {{.StartsAt.Format "Mon, 02 Jan 15:04"}}{{end}}
  {{if .Note}}<span class="muted">({{.Note}})</span>{{end}}
  <form method="post" action="/admin/proposals/{{.ID}}"><input type="hidden" name="decision" value="accept"><button class="yes" type="submit">Accept</button></form>
  <form method="post" action="/admin/proposals/{{.ID}}"><input type="hidden" name="decision" value="reject"><button class="no" type="submit">Reject</button></form></p>
{{end}}
{{range .Pending}}
  <p class="bad">Undelivered {{.Purpose}} → {{.To}} ({{.Attempts}} attempts)
  <form method="post" action="/admin/outbox/{{.ID}}/retry"><button type="submit">Retry now</button></form></p>
{{end}}
</div>
{{end}}

{{range .Ov.Events}}
<div class="card">
  <h2><a href="/admin/event/{{.Event.ID}}">{{.Event.Title}}</a> {{template "status" .Event.Status}}</h2>
  <p>{{.Event.StartsAt.Format "Mon, 02 Jan 2006 15:04"}}{{if .Event.Location}} · {{.Event.Location}}{{end}}</p>
  <p>{{if .Assignees}}{{range .Assignees}}{{template "admin_chip" .}}{{end}}
  {{else}}<span class="muted">nobody assigned — the reminder waits</span>{{end}}</p>
</div>
{{end}}
{{if not .Ov.Events}}<p class="muted">No upcoming events.</p>{{end}}
{{if .Hidden}}<p class="muted">{{.Hidden}} past event(s) hidden — <a href="/admin?all=1">show all</a></p>{{end}}

<div class="card">
<details><summary><strong>New event</strong></summary>
<form method="post" action="/admin/events">
  <label>Title <input name="title" required></label>
  <label>Start <input type="datetime-local" name="starts_at" required></label>
  <label>End <input type="datetime-local" name="ends_at"></label>
  <label>Location <input name="location"></label>
  <input name="note" placeholder="Note">
  <label>If unconfirmed <select name="if_unconfirmed"><option value="notify">notify</option><option value="cancel">cancel (dead-man-switch)</option></select></label>
  <button class="yes" type="submit">Create</button>
</form>
</details>
</div>

{{if .Recent}}
<div class="card">
<h2>Recent messages</h2>
{{range .Recent}}<p class="{{if .Bad}}bad{{else}}muted{{end}}">{{.At.Format "02 Jan 15:04"}} — {{.Text}}</p>{{end}}
</div>
{{end}}

<p class="muted">outbox: {{.Ov.Outbox.Delivered}} delivered · {{.Ov.Outbox.Pending}} pending · {{.Ov.Outbox.Failed}} failed · people: {{.Ov.People}}</p>
</body></html>{{end}}

{{define "admin_event"}}{{template "admin_head"}}
<h1>stattii admin</h1>
{{template "admin_nav"}}

<div class="card">
{{template "event" .Ev.Event}}
{{if .Ev.Event.SourceUID}}<p class="muted">↻ imported series occurrence</p>{{end}}
{{if not .Tracks}}<p class="muted">nobody assigned — the reminder waits</p>{{end}}
{{range .Tracks}}
  <p><strong>{{.A.Name}}</strong>{{if .A.Role}} ({{.A.Role}}){{end}} <span class="muted">· trust: {{.A.Trust}}</span></p>
  <ul class="tl">
  {{range .Entries}}<li class="{{if .Bad}}bad{{else if .Muted}}muted{{end}}">{{.At.Format "02 Jan 15:04"}}&nbsp; {{.Icon}} {{.Text}}</li>
  {{else}}<li class="muted">nothing sent yet — the reminder goes out {{$.Ev.Event.StartsAt.Format "02 Jan"}} minus the reminder lead</li>{{end}}
  {{if not .A.Action}}<li class="muted">… no answer yet</li>{{end}}
  </ul>
{{end}}
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
    {{if .Ev.Event.SourceUID}}<label><input type="checkbox" name="series" value="1"> whole series (every imported occurrence, incl. future fetches)</label>{{end}}
    <button type="submit">Assign</button>
  </form>
</details>
</div>

{{if .Propagation.Total}}
<div class="card">
<h2>Propagation (broadcasts &amp; fan-out)</h2>
<p>{{.Propagation.Delivered}}/{{.Propagation.Total}} delivered{{if .Propagation.Failed}} · <strong class="bad">{{.Propagation.Failed}} FAILED</strong>{{end}}{{if .Propagation.Complete}} · complete{{end}}</p>
{{range .Propagation.Items}}<p class="muted">{{.Purpose}} → {{.To}} ({{.Kind}}), {{if .Delivered}}delivered {{.DeliveredAt.Format "02 Jan 15:04"}}{{else}}{{.Attempts}} attempts{{end}}</p>{{end}}
</div>
{{end}}
</body></html>{{end}}

{{define "admin_people"}}{{template "admin_head"}}
<h1>stattii admin</h1>
{{template "admin_nav"}}

{{range .People}}
<div class="card">
  <h2>{{.Name}} <span class="badge scheduled">{{.Trust}}</span></h2>
  {{range .Channels}}<p class="muted">{{.Kind}}: {{.To}}</p>{{end}}
  {{if .LastMsg}}<p class="{{if .LastBad}}bad{{else}}muted{{end}}">{{.LastMsg}}</p>{{end}}
  <p class="muted">Portal: <a href="{{.PortalURL}}">{{.PortalURL}}</a></p>
  <form method="post" action="/admin/people/{{.ID}}/test"><button type="submit">Send test message</button></form>
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

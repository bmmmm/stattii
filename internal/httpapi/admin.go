// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

// The web admin lives on the admin listener only (see AdminHandler) and
// authenticates with the same admin token as the API: the operator pastes
// it once into a login form, it is kept in an HttpOnly cookie and verified
// constant-time on every request. SameSite=Strict plus POST-only mutations
// is the CSRF baseline; GET never mutates here either.

const adminCookie = "stattii_admin"

func (s *Server) registerAdminUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", s.adminAuth(s.adminOverview))
	mux.HandleFunc("POST /admin/login", s.adminLogin)
	mux.HandleFunc("POST /admin/logout", s.adminLogout)
	mux.HandleFunc("GET /admin/event/{id}", s.adminAuth(s.adminEvent))
	mux.HandleFunc("POST /admin/event/{id}/confirm", s.adminAuth(s.adminEventConfirm))
	mux.HandleFunc("POST /admin/event/{id}/cancel", s.adminAuth(s.adminEventCancel))
	mux.HandleFunc("POST /admin/event/{id}/reinstate", s.adminAuth(s.adminEventReinstate))
	mux.HandleFunc("POST /admin/event/{id}/move", s.adminAuth(s.adminEventMove))
	mux.HandleFunc("POST /admin/event/{id}/assign", s.adminAuth(s.adminEventAssign))
	mux.HandleFunc("POST /admin/events", s.adminAuth(s.adminEventCreate))
	mux.HandleFunc("GET /admin/people", s.adminAuth(s.adminPeople))
	mux.HandleFunc("POST /admin/people", s.adminAuth(s.adminPeopleAdd))
	mux.HandleFunc("POST /admin/people/{id}/test", s.adminAuth(s.adminPeopleTest))
	mux.HandleFunc("POST /admin/calendar/fetch", s.adminAuth(s.adminCalendarFetch))
	mux.HandleFunc("POST /admin/proposals/{id}", s.adminAuth(s.adminProposalDecide))
	mux.HandleFunc("POST /admin/outbox/{id}/retry", s.adminAuth(s.adminOutboxRetry))
}

func (s *Server) adminAuthed(r *http.Request) bool {
	c, err := r.Cookie(adminCookie)
	return err == nil && s.adminToken != "" &&
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.adminToken)) == 1
}

func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.adminAuthed(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			adminTmpl.ExecuteTemplate(w, "admin_login", nil)
			return
		}
		next(w, r)
	}
}

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	tok := r.FormValue("token")
	if s.adminToken == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminToken)) != 1 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		adminTmpl.ExecuteTemplate(w, "admin_login", "That token is not right.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: tok, Path: "/admin",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		// Secure also works on http://127.0.0.1 tunnels — browsers treat
		// loopback as a trustworthy origin.
		Secure: true,
		MaxAge: int((30 * 24 * time.Hour).Seconds()),
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Path: "/admin", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ---- views ------------------------------------------------------------

// adminTimelineEntry is one line in the per-recipient tracking story:
// sent → delivered → answered, oldest first.
type adminTimelineEntry struct {
	At    time.Time
	Icon  string // "→" sent · "✓" delivered/confirmed · "✗" cancelled · "!" trouble
	Text  string
	Bad   bool
	Muted bool
}

// timelineFor merges one person's outbox items and responses for one
// event into that story. Everything shown here really happened — we
// track only our own deliveries and answer clicks, no pixels.
func timelineFor(eventID, personID string, outbox []core.OutboxItem, responses []core.Response) []adminTimelineEntry {
	var es []adminTimelineEntry
	for _, o := range outbox {
		if o.EventID != eventID || o.PersonID != personID {
			continue
		}
		es = append(es, adminTimelineEntry{At: o.CreatedAt, Icon: "→", Muted: true,
			Text: fmt.Sprintf("%s sent via %s to %s", o.Purpose, o.Kind, o.To)})
		switch {
		case o.Delivered():
			es = append(es, adminTimelineEntry{At: o.DeliveredAt, Icon: "✓", Muted: true,
				Text: fmt.Sprintf("delivered (attempt %d)", o.Attempts)})
		case o.Attempts > 0:
			es = append(es, adminTimelineEntry{At: o.NextAttempt, Icon: "!", Bad: true,
				Text: fmt.Sprintf("undelivered after %d attempt(s): %s", o.Attempts, o.LastError)})
		}
		if !o.EscalatedAt.IsZero() {
			es = append(es, adminTimelineEntry{At: o.EscalatedAt, Icon: "!", Bad: true, Text: "escalated to admin"})
		}
	}
	for _, r := range responses {
		if r.EventID != eventID || r.PersonID != personID {
			continue
		}
		icon, bad := "✓", false
		if r.Action == core.ActionCancel {
			icon, bad = "✗", true
		}
		es = append(es, adminTimelineEntry{At: r.At, Icon: icon, Bad: bad,
			Text: fmt.Sprintf("answered: %s via %s", r.Action, r.Via)})
	}
	sort.Slice(es, func(i, j int) bool { return es[i].At.Before(es[j].At) })
	return es
}

type adminRecentMsg struct {
	At   time.Time
	Text string
	Bad  bool
}

type adminOverviewData struct {
	Ov         core.Overview
	Hidden     int // past events not shown (use ?all=1)
	All        bool
	Proposals  []core.Proposal   // open only
	Pending    []core.OutboxItem // undelivered
	People     []core.Person
	Recent     []adminRecentMsg // last messages, newest first
	Calendar   bool             // a source feed is configured
	LastImport *core.ImportReport
}

func (s *Server) adminOverview(w http.ResponseWriter, r *http.Request) {
	d := adminOverviewData{Ov: s.svc.Overview(), All: r.URL.Query().Get("all") == "1", People: s.svc.People(),
		Calendar: s.svc.CalendarConfigured(), LastImport: s.svc.LastImport()}
	if !d.All {
		now := time.Now()
		var keep []core.OverviewEvent
		for _, oe := range d.Ov.Events {
			if oe.Event.StartsAt.After(now.Add(-24*time.Hour)) || oe.Event.EndsAt.After(now) {
				keep = append(keep, oe)
			} else {
				d.Hidden++
			}
		}
		d.Ov.Events = keep
	}
	for _, p := range s.svc.Proposals() {
		if p.DecidedAt.IsZero() {
			d.Proposals = append(d.Proposals, p)
		}
	}
	d.Pending = s.svc.OutboxItems(true)

	// Recent messages — the tracking feed: newest first, names not ids.
	name := map[string]string{}
	for _, p := range d.People {
		name[p.ID] = p.Name
	}
	title := map[string]string{}
	for _, oe := range d.Ov.Events {
		title[oe.Event.ID] = oe.Event.Title
	}
	items := s.svc.OutboxItems(false)
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	for _, o := range items {
		if len(d.Recent) == 10 {
			break
		}
		who := o.To
		if n, ok := name[o.PersonID]; ok {
			who = n
		}
		what := o.Purpose
		if t, ok := title[o.EventID]; ok {
			what += " · " + t
		}
		m := adminRecentMsg{At: o.CreatedAt}
		switch {
		case o.Delivered():
			m.Text = fmt.Sprintf("%s → %s (%s): delivered", what, who, o.Kind)
		case o.Attempts > 0:
			m.Text = fmt.Sprintf("%s → %s (%s): UNDELIVERED after %d attempt(s)", what, who, o.Kind, o.Attempts)
			m.Bad = true
		default:
			m.Text = fmt.Sprintf("%s → %s (%s): queued", what, who, o.Kind)
		}
		d.Recent = append(d.Recent, m)
	}
	s.renderAdmin(w, "admin_overview", d)
}

type adminTrack struct {
	A       core.OverviewAssignee
	Entries []adminTimelineEntry
}

type adminEventData struct {
	Ev          core.OverviewEvent
	Tracks      []adminTrack
	Propagation core.PropagationStatus
	People      []core.Person
}

func (s *Server) adminEvent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ov := s.svc.Overview()
	var found *core.OverviewEvent
	for i := range ov.Events {
		if ov.Events[i].Event.ID == id {
			found = &ov.Events[i]
			break
		}
	}
	if found == nil {
		s.renderError(w, core.ErrNotFound)
		return
	}
	d := adminEventData{Ev: *found, People: s.svc.People()}
	outbox := s.svc.OutboxItems(false)
	responses := s.svc.Responses(id)
	for _, a := range found.Assignees {
		d.Tracks = append(d.Tracks, adminTrack{A: a, Entries: timelineFor(id, a.PersonID, outbox, responses)})
	}
	d.Propagation, _ = s.svc.Propagation(id)
	s.renderAdmin(w, "admin_event", d)
}

// adminAct wraps a mutation and redirects back to the event page.
func (s *Server) adminAct(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil && !errors.Is(err, core.ErrCancelled) {
		s.renderError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/event/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) adminEventConfirm(w http.ResponseWriter, r *http.Request) {
	_, err := s.svc.ConfirmEvent(r.PathValue("id"), "", "admin")
	s.adminAct(w, r, err)
}

func (s *Server) adminEventCancel(w http.ResponseWriter, r *http.Request) {
	_, err := s.svc.CancelEvent(r.PathValue("id"), "", r.FormValue("reason"), "admin")
	s.adminAct(w, r, err)
}

func (s *Server) adminEventReinstate(w http.ResponseWriter, r *http.Request) {
	_, err := s.svc.ReinstateEvent(r.PathValue("id"), "admin")
	s.adminAct(w, r, err)
}

func (s *Server) adminEventMove(w http.ResponseWriter, r *http.Request) {
	start, err := time.ParseInLocation("2006-01-02T15:04", r.FormValue("starts_at"), time.Local)
	if err != nil {
		s.renderError(w, errors.New("invalid start time"))
		return
	}
	var end time.Time
	if v := r.FormValue("ends_at"); v != "" {
		if end, err = time.ParseInLocation("2006-01-02T15:04", v, time.Local); err != nil {
			s.renderError(w, errors.New("invalid end time"))
			return
		}
	}
	_, err = s.svc.MoveEvent(r.PathValue("id"), start, end, r.FormValue("note"), "admin")
	s.adminAct(w, r, err)
}

func (s *Server) adminEventAssign(w http.ResponseWriter, r *http.Request) {
	// "series" assigns the whole imported series, not just this event.
	if r.FormValue("series") == "1" {
		e, err := s.svc.EventByID(r.PathValue("id"))
		if err != nil || e.SourceUID == "" {
			s.renderError(w, core.ErrNotFound)
			return
		}
		_, err = s.svc.AssignSeries(e.SourceUID, r.FormValue("person_id"), r.FormValue("role"))
		s.adminAct(w, r, err)
		return
	}
	s.adminAct(w, r, s.svc.Assign(r.PathValue("id"), r.FormValue("person_id"), r.FormValue("role")))
}

func (s *Server) adminCalendarFetch(w http.ResponseWriter, r *http.Request) {
	if _, err := s.svc.FetchCalendar(r.Context()); err != nil {
		s.renderError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminEventCreate(w http.ResponseWriter, r *http.Request) {
	start, err := time.ParseInLocation("2006-01-02T15:04", r.FormValue("starts_at"), time.Local)
	if err != nil {
		s.renderError(w, errors.New("invalid start time"))
		return
	}
	var end time.Time
	if v := r.FormValue("ends_at"); v != "" {
		if end, err = time.ParseInLocation("2006-01-02T15:04", v, time.Local); err != nil {
			s.renderError(w, errors.New("invalid end time"))
			return
		}
	}
	e, err := s.svc.CreateEvent(core.EventInput{
		Title: r.FormValue("title"), Location: r.FormValue("location"),
		Note: r.FormValue("note"), StartsAt: start, EndsAt: end,
		IfUnconfirmed: r.FormValue("if_unconfirmed"),
	})
	if err != nil {
		s.renderError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/event/"+e.ID, http.StatusSeeOther)
}

type adminPeopleData struct {
	People []adminPerson
}

type adminPerson struct {
	core.Person
	PortalURL string
	LastMsg   string // most recent message to this person, any event
	LastBad   bool
}

func (s *Server) adminPeople(w http.ResponseWriter, r *http.Request) {
	var d adminPeopleData
	items := s.svc.OutboxItems(false)
	for _, p := range s.svc.People() {
		u, _ := s.svc.PersonPortalURL(p.ID)
		ap := adminPerson{Person: p, PortalURL: u}
		var last *core.OutboxItem
		for i := range items {
			if items[i].PersonID == p.ID && (last == nil || items[i].CreatedAt.After(last.CreatedAt)) {
				last = &items[i]
			}
		}
		if last != nil {
			switch {
			case last.Delivered():
				ap.LastMsg = fmt.Sprintf("last message: %s via %s, delivered %s",
					last.Purpose, last.Kind, last.DeliveredAt.Local().Format("02 Jan 15:04"))
			case last.Attempts > 0:
				ap.LastMsg = fmt.Sprintf("last message: %s via %s, UNDELIVERED (%d attempts)",
					last.Purpose, last.Kind, last.Attempts)
				ap.LastBad = true
			default:
				ap.LastMsg = fmt.Sprintf("last message: %s via %s, queued", last.Purpose, last.Kind)
			}
		}
		d.People = append(d.People, ap)
	}
	s.renderAdmin(w, "admin_people", d)
}

func (s *Server) adminPeopleTest(w http.ResponseWriter, r *http.Request) {
	if _, err := s.svc.SendTest(r.PathValue("id")); err != nil {
		s.renderError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/people", http.StatusSeeOther)
}

func (s *Server) adminPeopleAdd(w http.ResponseWriter, r *http.Request) {
	var channels []core.Address
	if v := r.FormValue("email"); v != "" {
		channels = append(channels, core.Address{Kind: "email", To: v})
	}
	if v := r.FormValue("telegram"); v != "" {
		channels = append(channels, core.Address{Kind: "telegram", To: v})
	}
	_, err := s.svc.AddPerson(r.FormValue("name"), core.TrustLevel(r.FormValue("trust")), channels)
	if err != nil {
		s.renderError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/people", http.StatusSeeOther)
}

func (s *Server) adminProposalDecide(w http.ResponseWriter, r *http.Request) {
	_, err := s.svc.DecideProposal(r.PathValue("id"), r.FormValue("decision") == "accept")
	if err != nil {
		s.renderError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminOutboxRetry(w http.ResponseWriter, r *http.Request) {
	if _, err := s.svc.RetryOutbox(r.PathValue("id")); err != nil {
		s.renderError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) renderAdmin(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminTmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

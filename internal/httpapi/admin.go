// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import (
	"crypto/subtle"
	"errors"
	"net/http"
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

type adminOverviewData struct {
	Ov        core.Overview
	Hidden    int // past events not shown (use ?all=1)
	All       bool
	Proposals []core.Proposal   // open only
	Pending   []core.OutboxItem // undelivered
	People    []core.Person
}

func (s *Server) adminOverview(w http.ResponseWriter, r *http.Request) {
	d := adminOverviewData{Ov: s.svc.Overview(), All: r.URL.Query().Get("all") == "1", People: s.svc.People()}
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
	s.renderAdmin(w, "admin_overview", d)
}

type adminEventData struct {
	Ev          core.OverviewEvent
	Responses   []core.Response
	Propagation core.PropagationStatus
	People      []core.Person
	Links       map[string]string // set after "links" action via query params
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
	d := adminEventData{Ev: *found, Responses: s.svc.Responses(id), People: s.svc.People()}
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
	s.adminAct(w, r, s.svc.Assign(r.PathValue("id"), r.FormValue("person_id"), r.FormValue("role")))
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
}

func (s *Server) adminPeople(w http.ResponseWriter, r *http.Request) {
	var d adminPeopleData
	for _, p := range s.svc.People() {
		u, _ := s.svc.PersonPortalURL(p.ID)
		d.People = append(d.People, adminPerson{Person: p, PortalURL: u})
	}
	s.renderAdmin(w, "admin_people", d)
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

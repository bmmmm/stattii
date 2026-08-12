// SPDX-License-Identifier: GPL-3.0-or-later

// Package httpapi exposes stattii over HTTP. Public surface: tokenized
// action pages, the personal portal, and the ICS feed. Everything else
// lives under /api/v1 behind a bearer token.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bmmmm/stattii/internal/core"
	"github.com/bmmmm/stattii/internal/ics"
)

type Server struct {
	svc            *core.Service
	adminToken     string
	limiter        *limiter
	calName        string
	trustedProxies []*net.IPNet
}

// New builds the HTTP surface. trustedProxies lists the reverse proxies in
// front of the server (CIDRs); requests arriving from one of them have their
// client IP taken from X-Forwarded-For instead of the socket peer — without
// this every visitor behind the proxy shares one rate-limit bucket.
func New(svc *core.Service, adminToken, calName string, trustedProxies []*net.IPNet) *Server {
	if calName == "" {
		calName = "stattii"
	}
	return &Server{svc: svc, adminToken: adminToken, limiter: newLimiter(30, time.Minute),
		calName: calName, trustedProxies: trustedProxies}
}

// ParseTrustedProxies parses a comma-separated list of CIDRs or bare IPs.
// Empty input means no proxy is trusted (X-Forwarded-For is ignored).
func ParseTrustedProxies(s string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, fmt.Errorf("trusted_proxies: %q is not an IP or CIDR", part)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			part = fmt.Sprintf("%s/%d", ip, bits)
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: %w", err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public, rate-limited. GET never mutates — mail scanners prefetch links.
	mux.HandleFunc("GET /a/{token}", s.public(s.actionPage))
	mux.HandleFunc("POST /a/{token}", s.public(s.actionApply))
	mux.HandleFunc("POST /a/{token}/propose", s.public(s.actionPropose))
	mux.HandleFunc("GET /p/{token}", s.public(s.portalPage))
	mux.HandleFunc("POST /p/{token}/respond", s.public(s.portalRespond))
	mux.HandleFunc("POST /p/{token}/submit", s.public(s.portalSubmit))
	mux.HandleFunc("GET /feed.ics", s.feed)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})

	// Admin API.
	api := map[string]http.HandlerFunc{
		"GET /api/v1/events":                  s.listEvents,
		"POST /api/v1/events":                 s.createEvent,
		"GET /api/v1/events/{id}":             s.getEvent,
		"POST /api/v1/events/{id}/confirm":    s.confirmEvent,
		"POST /api/v1/events/{id}/cancel":     s.cancelEvent,
		"POST /api/v1/events/{id}/move":       s.moveEvent,
		"POST /api/v1/events/{id}/reinstate":  s.reinstateEvent,
		"GET /api/v1/outbox":                  s.listOutbox,
		"POST /api/v1/outbox/{id}/retry":      s.retryOutbox,
		"POST /api/v1/events/{id}/links":      s.makeLinks,
		"GET /api/v1/events/{id}/responses":   s.eventResponses,
		"GET /api/v1/events/{id}/propagation": s.propagation,
		"GET /api/v1/people":                  s.listPeople,
		"POST /api/v1/people":                 s.createPerson,
		"POST /api/v1/assignments":            s.assign,
		"GET /api/v1/proposals":               s.listProposals,
		"POST /api/v1/proposals/{id}/decide":  s.decideProposal,
		"GET /api/v1/broadcasts":              s.listBroadcasts,
		"POST /api/v1/broadcasts":             s.createBroadcast,
		"DELETE /api/v1/broadcasts/{id}":      s.deleteBroadcast,
		"GET /api/v1/webhooks":                s.listWebhooks,
		"POST /api/v1/webhooks":               s.createWebhook,
		"DELETE /api/v1/webhooks/{id}":        s.deleteWebhook,
		"GET /api/v1/audit":                   s.audit,
		"POST /api/v1/tick":                   s.tick,
	}
	for pattern, h := range api {
		mux.HandleFunc(pattern, s.auth(h))
	}
	return mux
}

// ---- middleware -----------------------------------------------------------

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.adminToken == "" || !ok ||
			subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminToken)) != 1 {
			jsonError(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		next(w, r)
	}
}

func (s *Server) public(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(s.clientIP(r)) {
			http.Error(w, "too many requests — try again in a minute", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// clientIP is the rate-limit key. When the socket peer is a trusted proxy,
// X-Forwarded-For is walked right to left and the first hop we do not run
// ourselves is the client — anything further left is client-supplied and
// spoofable. Otherwise the peer address is used as-is.
func (s *Server) clientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if !s.trustedProxy(ip) {
		return ip
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop != "" && !s.trustedProxy(hop) {
			return hop
		}
	}
	return ip
}

func (s *Server) trustedProxy(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	for _, n := range s.trustedProxies {
		if n.Contains(p) {
			return true
		}
	}
	return false
}

// ---- public pages ---------------------------------------------------------

func (s *Server) actionPage(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.ResolveAction(r.PathValue("token"))
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.render(w, "action", actionData{View: v, Token: r.PathValue("token")})
}

func (s *Server) actionApply(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.ApplyAction(r.PathValue("token"))
	if err != nil && !errors.Is(err, core.ErrCancelled) {
		s.renderError(w, err)
		return
	}
	s.render(w, "action", actionData{View: v, Token: r.PathValue("token"), Done: true, Conflict: errors.Is(err, core.ErrCancelled)})
}

func (s *Server) actionPropose(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
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
	if _, err := s.svc.ProposeMoveViaLink(token, start, end, r.FormValue("note")); err != nil {
		s.renderError(w, err)
		return
	}
	v, err := s.svc.ResolveAction(token)
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.render(w, "action", actionData{View: v, Token: token, Proposed: true})
}

func (s *Server) portalPage(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.Portal(r.PathValue("token"))
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.render(w, "portal", portalData{View: v, Token: r.PathValue("token")})
}

func (s *Server) portalRespond(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	err := s.svc.PortalRespond(token, r.FormValue("event_id"), core.ActionKind(r.FormValue("action")))
	if err != nil && !errors.Is(err, core.ErrCancelled) {
		s.renderError(w, err)
		return
	}
	http.Redirect(w, r, "/p/"+token, http.StatusSeeOther)
}

func (s *Server) portalSubmit(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	var start, end time.Time
	var err error
	if v := r.FormValue("starts_at"); v != "" {
		start, err = time.ParseInLocation("2006-01-02T15:04", v, time.Local)
		if err != nil {
			s.renderError(w, errors.New("invalid start time"))
			return
		}
	}
	if v := r.FormValue("ends_at"); v != "" {
		end, err = time.ParseInLocation("2006-01-02T15:04", v, time.Local)
		if err != nil {
			s.renderError(w, errors.New("invalid end time"))
			return
		}
	}
	applied, err := s.svc.PortalSubmit(token, r.FormValue("kind"), r.FormValue("event_id"),
		r.FormValue("title"), r.FormValue("note"), start, end)
	if err != nil && !errors.Is(err, core.ErrCancelled) {
		s.renderError(w, err)
		return
	}
	suffix := "?proposed=1"
	if applied {
		suffix = "?applied=1"
	}
	http.Redirect(w, r, "/p/"+token+suffix, http.StatusSeeOther)
}

func (s *Server) feed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Write([]byte(ics.Feed(s.calName, s.svc.Events(), time.Now())))
}

// ---- admin API ------------------------------------------------------------

type eventReq struct {
	Title         string    `json:"title"`
	Location      string    `json:"location"`
	Note          string    `json:"note"`
	Reason        string    `json:"reason"`
	StartsAt      time.Time `json:"starts_at"`
	EndsAt        time.Time `json:"ends_at"`
	IfUnconfirmed string    `json:"if_unconfirmed"`
}

func (s *Server) listEvents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Events())
}

func (s *Server) createEvent(w http.ResponseWriter, r *http.Request) {
	var in eventReq
	if !readJSON(w, r, &in) {
		return
	}
	e, err := s.svc.CreateEvent(core.EventInput{
		Title: in.Title, Location: in.Location, Note: in.Note,
		StartsAt: in.StartsAt, EndsAt: in.EndsAt, IfUnconfirmed: in.IfUnconfirmed,
	})
	respond(w, e, err, http.StatusCreated)
}

func (s *Server) reinstateEvent(w http.ResponseWriter, r *http.Request) {
	e, err := s.svc.ReinstateEvent(r.PathValue("id"), "api")
	respond(w, e, err, http.StatusOK)
}

func (s *Server) listOutbox(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.OutboxItems(r.URL.Query().Get("pending") == "1"))
}

func (s *Server) retryOutbox(w http.ResponseWriter, r *http.Request) {
	o, err := s.svc.RetryOutbox(r.PathValue("id"))
	respond(w, o, err, http.StatusOK)
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	e, err := s.svc.EventByID(r.PathValue("id"))
	respond(w, e, err, http.StatusOK)
}

func (s *Server) confirmEvent(w http.ResponseWriter, r *http.Request) {
	e, err := s.svc.ConfirmEvent(r.PathValue("id"), "", "api")
	respond(w, e, err, http.StatusOK)
}

func (s *Server) cancelEvent(w http.ResponseWriter, r *http.Request) {
	var in eventReq
	if !readJSON(w, r, &in) {
		return
	}
	e, err := s.svc.CancelEvent(r.PathValue("id"), "", in.Reason, "api")
	respond(w, e, err, http.StatusOK)
}

func (s *Server) moveEvent(w http.ResponseWriter, r *http.Request) {
	var in eventReq
	if !readJSON(w, r, &in) {
		return
	}
	e, err := s.svc.MoveEvent(r.PathValue("id"), in.StartsAt, in.EndsAt, in.Note, "api")
	respond(w, e, err, http.StatusOK)
}

func (s *Server) makeLinks(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PersonID string `json:"person_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	c, x, err := s.svc.GenerateLinks(r.PathValue("id"), in.PersonID)
	respond(w, map[string]string{"confirm_url": c, "cancel_url": x}, err, http.StatusOK)
}

func (s *Server) eventResponses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Responses(r.PathValue("id")))
}

func (s *Server) propagation(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.Propagation(r.PathValue("id"))
	respond(w, p, err, http.StatusOK)
}

func (s *Server) listPeople(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.People())
}

func (s *Server) createPerson(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string          `json:"name"`
		Trust    core.TrustLevel `json:"trust"`
		Channels []core.Address  `json:"channels"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	p, err := s.svc.AddPerson(in.Name, in.Trust, in.Channels)
	if err != nil {
		respond(w, nil, err, 0)
		return
	}
	portal, _ := s.svc.PersonPortalURL(p.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"person": p, "portal_url": portal})
}

func (s *Server) assign(w http.ResponseWriter, r *http.Request) {
	var in struct {
		EventID  string `json:"event_id"`
		PersonID string `json:"person_id"`
		Role     string `json:"role"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	err := s.svc.Assign(in.EventID, in.PersonID, in.Role)
	respond(w, map[string]string{"status": "assigned"}, err, http.StatusOK)
}

func (s *Server) listProposals(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Proposals())
}

func (s *Server) decideProposal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Accept bool `json:"accept"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	p, err := s.svc.DecideProposal(r.PathValue("id"), in.Accept)
	respond(w, p, err, http.StatusOK)
}

func (s *Server) listBroadcasts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Broadcasts())
}

func (s *Server) createBroadcast(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		To   string `json:"to"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	b, err := s.svc.AddBroadcast(in.Name, in.Kind, in.To)
	respond(w, b, err, http.StatusCreated)
}

func (s *Server) deleteBroadcast(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]string{"status": "deleted"}, s.svc.DeleteBroadcast(r.PathValue("id")), http.StatusOK)
}

func (s *Server) listWebhooks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Webhooks())
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	wh, err := s.svc.AddWebhook(in.URL, in.Events)
	respond(w, wh, err, http.StatusCreated)
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]string{"status": "deleted"}, s.svc.DeleteWebhook(r.PathValue("id")), http.StatusOK)
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	entries, err := s.svc.Audit(limit)
	respond(w, entries, err, http.StatusOK)
}

func (s *Server) tick(w http.ResponseWriter, _ *http.Request) {
	s.svc.Tick(time.Now().UTC())
	writeJSON(w, http.StatusOK, map[string]string{"status": "ticked"})
}

// ---- helpers --------------------------------------------------------------

func respond(w http.ResponseWriter, data any, err error, okStatus int) {
	if err != nil {
		switch {
		case errors.Is(err, core.ErrNotFound):
			jsonError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, core.ErrGone):
			jsonError(w, http.StatusGone, err.Error())
		case errors.Is(err, core.ErrCancelled):
			jsonError(w, http.StatusConflict, err.Error())
		case errors.Is(err, core.ErrForbidden):
			jsonError(w, http.StatusForbidden, err.Error())
		default:
			jsonError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, okStatus, data)
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("stattii: encode response: %v", err)
	}
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

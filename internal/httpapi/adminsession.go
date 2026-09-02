// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import (
	"sync"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

// The admin session store. Until v0.7.1 the session cookie WAS the admin
// bearer token: anything that could read the cookie (a stray extension, a
// shared screenshot, a proxy log) held full API access. Now the cookie
// carries a random id that means nothing outside this process, and the
// token itself never leaves the login form.
//
// In memory on purpose: sessions are cheap to re-create (paste the token
// again) and a restart forcing a fresh login is the conservative
// direction. Nothing here belongs in state.json — a persisted session is
// a credential at rest, which is exactly what invariant 5 avoids for
// action links.
type adminSession struct {
	csrf    string
	expires time.Time
}

type sessionStore struct {
	mu   sync.Mutex
	byID map[string]adminSession
	ttl  time.Duration
	now  func() time.Time
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{byID: map[string]adminSession{}, ttl: ttl, now: time.Now}
}

// create mints a session id and its CSRF token. The two are separate
// secrets: the id lives in an HttpOnly cookie the page cannot read, the
// CSRF token is rendered into forms — swapping their roles would put the
// session id into the DOM.
func (st *sessionStore) create() (id string, sess adminSession) {
	id = core.NewToken()
	sess = adminSession{csrf: core.NewToken(), expires: st.now().Add(st.ttl)}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked()
	st.byID[id] = sess
	return id, sess
}

func (st *sessionStore) lookup(id string) (adminSession, bool) {
	if id == "" {
		return adminSession{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	sess, ok := st.byID[id]
	if !ok {
		return adminSession{}, false
	}
	if !st.now().Before(sess.expires) {
		delete(st.byID, id)
		return adminSession{}, false
	}
	return sess, true
}

// drop is the logout: the session dies on the server, not just in the
// browser. A cookie the client keeps must stop working.
func (st *sessionStore) drop(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.byID, id)
}

func (st *sessionStore) sweepLocked() {
	now := st.now()
	for id, sess := range st.byID {
		if !now.Before(sess.expires) {
			delete(st.byID, id)
		}
	}
}

func (st *sessionStore) count() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.byID)
}

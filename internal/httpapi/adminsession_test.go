// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import (
	"testing"
	"time"
)

// The store is the whole security property of the login: ids are random,
// sessions expire, logout drops them, and an expired one is gone from
// memory instead of lingering.
func TestSessionStoreLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	st := newSessionStore(time.Hour)
	st.now = func() time.Time { return now }

	id, sess := st.create()
	if id == "" || sess.csrf == "" || id == sess.csrf {
		t.Fatalf("session id and csrf must be two distinct secrets: %q / %q", id, sess.csrf)
	}
	other, _ := st.create()
	if other == id {
		t.Fatal("two logins got the same session id")
	}

	got, ok := st.lookup(id)
	if !ok || got.csrf != sess.csrf {
		t.Fatalf("lookup: %+v %v", got, ok)
	}
	if _, ok := st.lookup(""); ok {
		t.Fatal("an empty cookie resolved to a session")
	}
	if _, ok := st.lookup("nope"); ok {
		t.Fatal("an unknown id resolved to a session")
	}

	// Expiry is a hard edge: exactly at the deadline the session is over.
	now = now.Add(time.Hour)
	if _, ok := st.lookup(id); ok {
		t.Fatal("an expired session still resolves")
	}
	if st.count() > 1 {
		t.Fatalf("expired session not dropped from memory: %d", st.count())
	}

	// Logout drops the other one; a fresh create sweeps what is left.
	st.drop(other)
	if _, ok := st.lookup(other); ok {
		t.Fatal("a dropped session still resolves")
	}
	if st.count() != 0 {
		t.Fatalf("store not empty after logout: %d", st.count())
	}
}

// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/core"
	"github.com/bmmmm/stattii/internal/httpapi"
)

type failingStore struct{ saveErr error }

func (f *failingStore) Load() (*core.State, error)               { return &core.State{}, nil }
func (f *failingStore) Save(*core.State) error                   { return f.saveErr }
func (f *failingStore) Audit(string, any) error                  { return nil }
func (f *failingStore) ReadAudit(int) ([]core.AuditEntry, error) { return nil, nil }

// TestHealthzReportsPersistFailure: a store that stopped persisting must
// turn BOTH health probes red — monitoring and the deploy verify rely on
// /healthz saying "ok" only when acknowledged mutations are durable.
func TestHealthzReportsPersistFailure(t *testing.T) {
	fs := &failingStore{}
	svc, err := core.NewService(fs, core.Config{BaseURL: "http://x.local"}, nullNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	s := httpapi.New(svc, testToken, "test", nil)
	pub, admin := s.PublicHandler(), s.AdminHandler()

	if w := do(t, pub, "GET", "/healthz", "", ""); w.Code != http.StatusOK || w.Body.String() != "ok\n" {
		t.Fatalf("healthy probe: %d %q", w.Code, w.Body.String())
	}

	fs.saveErr = errors.New("disk full")
	if _, err := svc.CreateEvent(core.EventInput{Title: "x", StartsAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string]http.Handler{"public": pub, "admin": admin} {
		if w := do(t, h, "GET", "/healthz", "", ""); w.Code != http.StatusInternalServerError {
			t.Fatalf("%s healthz with broken store: got %d, want 500", name, w.Code)
		}
	}
}

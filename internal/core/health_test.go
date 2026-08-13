// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"errors"
	"testing"
	"time"
)

// failingStore simulates a broken data dir: Save/Audit fail on command.
type failingStore struct {
	saveErr, auditErr error
}

func (f *failingStore) Load() (*State, error)               { return &State{}, nil }
func (f *failingStore) Save(*State) error                   { return f.saveErr }
func (f *failingStore) Audit(string, any) error             { return f.auditErr }
func (f *failingStore) ReadAudit(int) ([]AuditEntry, error) { return nil, nil }

func newFailingService(t *testing.T, fs *failingStore) *Service {
	t.Helper()
	svc, err := NewService(fs, Config{
		BaseURL:     "http://test.local",
		AdminNotify: &Address{Kind: "email", To: "admin@test.local"},
	}, &fakeNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	svc.logf = t.Logf
	return svc
}

func adminEscalations(svc *Service) int {
	n := 0
	for _, o := range svc.OutboxItems(false) {
		if o.To == "admin@test.local" {
			n++
		}
	}
	return n
}

// TestStoreFailureSurfacesOnceAndHeals pins issue #8: a failed persist
// must flip /healthz-visible health and page the admin exactly once per
// failure episode — CancelEvent returning success on a full disk while
// nothing was written is the audit trail lying about durability.
func TestStoreFailureSurfacesOnceAndHeals(t *testing.T) {
	fs := &failingStore{}
	svc := newFailingService(t, fs)

	if !svc.PersistHealthy() {
		t.Fatal("fresh service must be healthy")
	}
	fs.saveErr = errors.New("disk full")
	mustEvent(t, svc, 40*time.Hour)
	if svc.PersistHealthy() {
		t.Fatal("failed save must flip health")
	}
	if got := adminEscalations(svc); got != 1 {
		t.Fatalf("want exactly 1 admin escalation on first failure, got %d", got)
	}
	mustEvent(t, svc, 41*time.Hour) // still broken — no escalation spam
	if got := adminEscalations(svc); got != 1 {
		t.Fatalf("one episode escalates once, got %d", got)
	}

	fs.saveErr = nil
	mustEvent(t, svc, 42*time.Hour)
	if !svc.PersistHealthy() {
		t.Fatal("successful save must heal")
	}
	fs.saveErr = errors.New("disk full again")
	mustEvent(t, svc, 43*time.Hour)
	if got := adminEscalations(svc); got != 2 {
		t.Fatalf("a new episode escalates again, got %d", got)
	}
}

// TestAuditFailureAloneFlipsHealth pins the two-bit logic: a healthy
// state save must not mask a broken audit trail (silent holes in
// audit.jsonl), and the still-broken audit must not re-escalate on
// every mutation.
func TestAuditFailureAloneFlipsHealth(t *testing.T) {
	fs := &failingStore{auditErr: errors.New("permission denied")}
	svc := newFailingService(t, fs)

	mustEvent(t, svc, 40*time.Hour)
	if svc.PersistHealthy() {
		t.Fatal("failed audit write must flip health even while saves succeed")
	}
	if got := adminEscalations(svc); got != 1 {
		t.Fatalf("want 1 escalation, got %d", got)
	}
	mustEvent(t, svc, 41*time.Hour)
	if got := adminEscalations(svc); got != 1 {
		t.Fatalf("save success must not reset the audit episode, got %d escalations", got)
	}
}

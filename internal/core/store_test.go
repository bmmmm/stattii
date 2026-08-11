// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	st, err := NewJSONStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Events) != 0 {
		t.Fatal("fresh store not empty")
	}

	loaded.Events = append(loaded.Events, Event{
		ID: "ev_1", Title: "X", StartsAt: time.Date(2026, 8, 18, 19, 0, 0, 0, time.UTC),
		Status: StatusScheduled,
	})
	if err := st.Save(loaded); err != nil {
		t.Fatal(err)
	}

	again, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Events) != 1 || again.Events[0].Title != "X" {
		t.Fatalf("roundtrip lost data: %+v", again.Events)
	}

	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state.json mode = %o, want 600 (contains tokens)", info.Mode().Perm())
	}
}

func TestAuditAppendAndTornLine(t *testing.T) {
	dir := t.TempDir()
	st, err := NewJSONStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := st.Audit(k, map[string]string{"k": k}); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a crash mid-write: a torn, non-JSON last line.
	f, err := os.OpenFile(filepath.Join(dir, "audit.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"at":"2026-`)
	f.Close()

	all, err := st.ReadAudit(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 entries (torn line skipped), got %d", len(all))
	}
	last2, err := st.ReadAudit(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(last2) != 2 || last2[0].Kind != "b" || last2[1].Kind != "c" {
		t.Fatalf("limit wrong: %+v", last2)
	}
}

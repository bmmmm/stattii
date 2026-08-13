// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAuditRotation pins issue #8's second half: audit.jsonl must not
// grow forever. One previous generation is kept; ReadAudit keeps
// working on the live file.
func TestAuditRotation(t *testing.T) {
	dir := t.TempDir()
	st, err := NewJSONStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.maxAuditBytes = 400
	for i := 0; i < 50; i++ {
		if err := st.Audit("test", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.jsonl.1")); err != nil {
		t.Fatal("no rotated generation written")
	}
	entries, err := st.ReadAudit(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || len(entries) >= 50 {
		t.Fatalf("live file after rotation holds %d entries, want a recent subset", len(entries))
	}
}

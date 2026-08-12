// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestListenAdminTCPAndUnix(t *testing.T) {
	ln, err := listenAdmin("127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp form: %v", err)
	}
	ln.Close()

	// Unix socket paths are length-limited (~104 bytes on darwin), so a
	// short base dir instead of t.TempDir().
	dir, err := os.MkdirTemp("/tmp", "stattii-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "admin.sock")

	// A stale file at the path (crashed process) must not block the bind.
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err = listenAdmin(path)
	if err != nil {
		t.Fatalf("stale file not replaced: %v", err)
	}
	defer ln.Close()

	fi, err := os.Stat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected a socket at %s (mode %v, err %v)", path, fi.Mode(), err)
	}
	if perm := fi.Mode().Perm(); perm != 0o660 {
		t.Fatalf("socket perm = %o, want 660", perm)
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.Close()
}

// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Store persists the State plus an append-only audit trail. The first
// backend is plain JSON files; SQLite can slot in behind the same
// interface if volume ever demands it.
type Store interface {
	Load() (*State, error)
	Save(*State) error
	Audit(kind string, data any) error
	ReadAudit(limit int) ([]AuditEntry, error)
}

type AuditEntry struct {
	At   time.Time       `json:"at"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// JSONStore keeps state.json (atomic rewrite) and audit.jsonl (append-only)
// in one directory. Files are 0600 — they contain capability tokens.
type JSONStore struct {
	dir string
}

func NewJSONStore(dir string) (*JSONStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	return &JSONStore{dir: dir}, nil
}

func (j *JSONStore) statePath() string { return filepath.Join(j.dir, "state.json") }
func (j *JSONStore) auditPath() string { return filepath.Join(j.dir, "audit.jsonl") }

func (j *JSONStore) Load() (*State, error) {
	raw, err := os.ReadFile(j.statePath())
	if os.IsNotExist(err) {
		return &State{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", j.statePath(), err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w (restore from backup or fix by hand)", j.statePath(), err)
	}
	return &st, nil
}

func (j *JSONStore) Save(st *State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(j.dir, "state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), j.statePath())
}

func (j *JSONStore) Audit(kind string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	line, err := json.Marshal(AuditEntry{At: time.Now().UTC(), Kind: kind, Data: raw})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(j.auditPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

func (j *JSONStore) ReadAudit(limit int) ([]AuditEntry, error) {
	f, err := os.Open(j.auditPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var e AuditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // a torn last line after a crash is expected, skip it
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

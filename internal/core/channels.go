// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"fmt"
	"sort"
	"strings"
)

// Channels stored before their format check existed are structurally
// reachable and formally broken: Address.Usable says "there is something
// to try", so the scheduler keeps counting the person, while Validate —
// which only AddPerson and UpdatePerson ever ran — would reject the
// address on sight. JSONStore.Load is a plain unmarshal, so nothing
// looks at old data again.
//
// The consequence is not a hole (delivery fails and escalates) but a late
// signal: the "Nobody can be reached" alarm is blind to exactly the state
// it was built for. So the state gets a name and two pushes, and nothing
// else — no migration, no auto-removal, no quarantine flag. Removing a
// channel to tidy the list would make someone silently unreachable, which
// is the failure mode this product exists against.

// ChannelProblem is one stored address that fails its own format check.
// Assigned counts the person's upcoming events — zero means nothing is
// riding on it right now.
type ChannelProblem struct {
	PersonID string `json:"person_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	To       string `json:"to"`
	Problem  string `json:"problem"`
	Assigned int    `json:"assigned"`
}

// ChannelProblems lists them, computed live on every call. Never cached:
// a cached list would keep naming an address the operator has already
// repaired, and a stale alarm teaches people to ignore alarms.
func (s *Service) ChannelProblems() []ChannelProblem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channelProblemsLocked()
}

func (s *Service) channelProblemsLocked() []ChannelProblem {
	upcoming := s.upcomingByPersonLocked()
	var out []ChannelProblem
	for i := range s.state.People {
		p := &s.state.People[i]
		for _, ch := range p.Channels {
			if !ch.Usable() {
				continue // blank placeholder — Reachable already ignores it
			}
			problem := ch.Problem()
			if problem == "" {
				continue
			}
			out = append(out, ChannelProblem{
				PersonID: p.ID, Name: p.Name, Kind: ch.Kind, To: ch.To,
				Problem: problem, Assigned: upcoming[p.ID],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Assigned != out[j].Assigned {
			return out[i].Assigned > out[j].Assigned // the urgent ones first
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// upcomingByPersonLocked counts each person's live, still-future events in
// one pass. Overview() had to be de-quadratised once already: EventsFor
// and Assignees are linear scans, so calling either per person is
// O(people × assignments).
func (s *Service) upcomingByPersonLocked() map[string]int {
	now := s.now()
	live := make(map[string]bool, len(s.state.Events))
	for i := range s.state.Events {
		e := &s.state.Events[i]
		if e.Status != StatusCancelled && e.StartsAt.After(now) {
			live[e.ID] = true
		}
	}
	out := make(map[string]int)
	for _, a := range s.state.Assignments {
		if live[a.EventID] {
			out[a.PersonID]++
		}
	}
	return out
}

// noteChannelProblemsLocked reports suspect channels once per process —
// the importFailed pattern: in-memory only, a restart is a fresh episode.
// There is nothing to re-scan for, either: bad data of this kind is
// written by hand or predates the check, and the next mutation through
// AddPerson/UpdatePerson validates it anyway.
//
// Silent unless someone with a suspect channel has upcoming events. That
// is the threshold noteUnreachablePersonLocked uses: an address nothing
// depends on right now is not worth waking anyone for.
func (s *Service) noteChannelProblemsLocked() bool {
	if s.channelsScanned {
		return false
	}
	s.channelsScanned = true
	problems := s.channelProblemsLocked()
	if len(problems) == 0 {
		return false
	}
	pressing := 0
	var lines []string
	for _, c := range problems {
		s.auditLocked("channel.invalid", map[string]any{
			"person_id": c.PersonID, "kind": c.Kind, "problem": c.Problem,
			"assigned": c.Assigned,
		})
		if c.Assigned > 0 {
			pressing++
		}
		lines = append(lines, fmt.Sprintf("- %s — %s: %s (%s, %d upcoming event(s))",
			c.Name, c.Kind, c.To, c.Problem, c.Assigned))
	}
	if pressing == 0 {
		return true // audited for the record, but nobody needs waking
	}
	s.notifyAdminLocked(fmt.Sprintf("Stored channels look broken (%d)", len(problems)),
		fmt.Sprintf("These addresses are stored but do not pass their own format check:\n%s\n\n"+
			"Nothing was removed and nobody was marked unreachable — asks still go out over them, "+
			"and a real delivery failure still escalates as usual. Fix them under /admin/people.",
			strings.Join(lines, "\n")))
	return true
}

// anyValidChannel reports whether at least one of these people has a
// channel that passes its format check.
func anyValidChannel(ps []*Person) bool {
	for _, p := range ps {
		if p.HasValidChannel() {
			return true
		}
	}
	return false
}

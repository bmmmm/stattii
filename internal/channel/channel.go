// SPDX-License-Identifier: GPL-3.0-or-later

// Package channel implements the outbound delivery adapters. One interface,
// two uses: person notifications and audience broadcasts.
package channel

import (
	"fmt"
	"sort"

	"github.com/bmmmm/stattii/internal/core"
)

// Sender delivers one message to one address of its kind.
type Sender interface {
	Kind() string
	Send(to string, m core.Message) error
}

// Registry routes by channel kind and satisfies core.Notifier.
type Registry struct {
	senders map[string]Sender
}

func NewRegistry(senders ...Sender) *Registry {
	r := &Registry{senders: map[string]Sender{}}
	for _, s := range senders {
		r.senders[s.Kind()] = s
	}
	return r
}

func (r *Registry) Kinds() []string {
	kinds := make([]string, 0, len(r.senders))
	for k := range r.senders {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

func (r *Registry) Send(kind, to string, m core.Message) error {
	s, ok := r.senders[kind]
	if !ok {
		return fmt.Errorf("channel %q not configured (configured: %v) — see README for the STATTII_* env vars", kind, r.Kinds())
	}
	return s.Send(to, m)
}

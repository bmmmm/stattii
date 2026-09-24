// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"fmt"
	"time"
)

// ---- telegram onboarding (#13) ---------------------------------------------
//
// Telegram delivery needs each person's chat id, and asking every person
// for a number they cannot see in the app does not scale. Instead the
// operator hands over a deep link, https://t.me/<bot>?start=<token>: the
// person taps it, Telegram opens the bot chat and sends "/start <token>",
// and the getUpdates poller binds the chat that sent it.
//
// The token is a capability like every other one here (invariant 5):
// random, looked up in the store, revocable — minting a new one for the
// person replaces the old — single-use, and dead after seven days.

// TelegramOnboardTTL is how long a handed-over onboarding link binds.
const TelegramOnboardTTL = 7 * 24 * time.Hour

// TelegramOnboarding is one outstanding onboarding capability. A used one
// is removed, so a second /start with it finds nothing.
type TelegramOnboarding struct {
	Token     string    `json:"token"`
	PersonID  string    `json:"person_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// TelegramOnboardView is what the operator hands over. URL stays empty
// until the poller has learned the bot's username; Start is what the
// link makes Telegram send, and works typed into the bot chat by hand.
type TelegramOnboardView struct {
	PersonID  string    `json:"person_id"`
	URL       string    `json:"url,omitempty"`
	Start     string    `json:"start"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SetTelegramBot records the bot's username (from getMe), which the
// onboarding links are built on.
func (s *Service) SetTelegramBot(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.telegramBot = username
}

func (s *Service) telegramOnboardViewLocked(o TelegramOnboarding) TelegramOnboardView {
	v := TelegramOnboardView{PersonID: o.PersonID, Start: "/start " + o.Token, ExpiresAt: o.ExpiresAt}
	if s.telegramBot != "" {
		v.URL = "https://t.me/" + s.telegramBot + "?start=" + o.Token
	}
	return v
}

// CreateTelegramOnboarding mints a person's onboarding link, replacing
// any earlier one — handing out a new link is also how a leaked one is
// revoked.
func (s *Service) CreateTelegramOnboarding(personID string) (TelegramOnboardView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Person(personID) == nil {
		return TelegramOnboardView{}, ErrNotFound
	}
	now := s.now()
	dropWhere(&s.state.TelegramOnboardings, func(o TelegramOnboarding) bool { return o.PersonID == personID })
	o := TelegramOnboarding{Token: NewToken(), PersonID: personID, CreatedAt: now, ExpiresAt: now.Add(TelegramOnboardTTL)}
	s.state.TelegramOnboardings = append(s.state.TelegramOnboardings, o)
	s.auditLocked("telegram.onboarding_created", map[string]any{"person_id": personID, "expires_at": o.ExpiresAt})
	s.saveLocked()
	return s.telegramOnboardViewLocked(o), nil
}

// TelegramOnboardings lists the links that still bind, by person — for
// the panel, which shows each one next to its person until it is used.
func (s *Service) TelegramOnboardings() map[string]TelegramOnboardView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := map[string]TelegramOnboardView{}
	for _, o := range s.state.TelegramOnboardings {
		if now.Before(o.ExpiresAt) {
			out[o.PersonID] = s.telegramOnboardViewLocked(o)
		}
	}
	return out
}

// BindTelegram handles "/start <token>" from the bot chat chatID, sent by
// the Telegram user fromID (both decimal). Only a private chat binds:
// there the chat id is the sender's own user id, the one shape
// VerifyTelegramActor can later check a button press against. A group
// is refused without spending the token, so the person can still open
// the link themselves.
//
// Unknown, used and expired tokens all return ErrNotFound/ErrGone and
// change nothing; the poller answers none of them (a second /start is
// ignored). A bound chat gets one confirmation through the outbox
// (invariant 2), sent by the next delivery pass.
func (s *Service) BindTelegram(token, chatID, fromID string) error {
	if token == "" || chatID != fromID || !isPrivateChatID(chatID) {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i := range s.state.TelegramOnboardings {
		if s.state.TelegramOnboardings[i].Token == token {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrNotFound
	}
	o := s.state.TelegramOnboardings[idx]
	// Single use: gone whatever happens next.
	s.state.TelegramOnboardings = append(s.state.TelegramOnboardings[:idx], s.state.TelegramOnboardings[idx+1:]...)
	now := s.now()
	p := s.state.Person(o.PersonID)
	if !now.Before(o.ExpiresAt) || p == nil {
		s.saveLocked()
		return ErrGone
	}
	// The chat the person just proved is THE telegram channel: it takes
	// the first telegram slot — the one the panel form and `person set`
	// edit (PatchChannels) — instead of hiding next to it. A wrong,
	// hand-typed id is thereby fixed, not kept failing beside the right
	// one, and a chat bound through a leaked link is visible in that slot
	// and replaced by the person's own next /start.
	from := channelKinds(p.Channels)
	slot := -1
	for i, ch := range p.Channels {
		if ch.Kind == "telegram" {
			slot = i
			break
		}
	}
	changed := true
	switch {
	case slot < 0:
		p.Channels = append(p.Channels, Address{Kind: "telegram", To: chatID})
	case p.Channels[slot].To == chatID:
		changed = false
	default:
		p.Channels[slot].To = chatID
	}
	if changed {
		// Kinds only — addresses do not belong in the audit trail.
		s.auditLocked("person.updated", map[string]any{
			"person_id": p.ID, "fields": []string{"channels"}, "via": "telegram",
			"channels_from": from, "channels_to": channelKinds(p.Channels),
		})
	}
	s.enqueueLocked(OutboxItem{
		Purpose: "onboarding", PersonID: p.ID, Kind: "telegram", To: chatID,
		Subject: "stattii",
		Body: fmt.Sprintf("Connected, %s: confirmation asks and cancellations for your events "+
			"will reach you in this chat.", p.Name),
	})
	s.saveLocked()
	return nil
}

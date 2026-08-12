// SPDX-License-Identifier: GPL-3.0-or-later

package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

func TestTelegramSendRendersInlineButtons(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			captured, _ = io.ReadAll(r.Body)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := &Telegram{Token: "TOK", BaseURL: srv.URL}
	err := tg.Send("12345", core.Message{
		Subject: "Please confirm",
		Body:    "Tuesday Session",
		Buttons: []core.Button{
			{Label: "✅ Takes place", Data: "tok-confirm"},
			{Label: "❌ Cancel event", Data: "tok-cancel"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		ChatID      string `json:"chat_id"`
		ReplyMarkup struct {
			InlineKeyboard [][]struct {
				Text         string `json:"text"`
				CallbackData string `json:"callback_data"`
			} `json:"inline_keyboard"`
		} `json:"reply_markup"`
	}
	if err := json.Unmarshal(captured, &req); err != nil {
		t.Fatalf("sendMessage payload: %v\n%s", err, captured)
	}
	if req.ChatID != "12345" {
		t.Fatalf("chat_id = %q", req.ChatID)
	}
	kb := req.ReplyMarkup.InlineKeyboard
	if len(kb) != 1 || len(kb[0]) != 2 || kb[0][0].CallbackData != "tok-confirm" || kb[0][1].CallbackData != "tok-cancel" {
		t.Fatalf("inline keyboard wrong: %+v", kb)
	}
}

func TestPollerAppliesCallback(t *testing.T) {
	var served atomic.Bool
	answered := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			if r.URL.Query().Get("offset") == "-1" {
				// Seed call: nothing pending before Run started.
				w.Write([]byte(`{"ok":true,"result":[]}`))
				return
			}
			if served.CompareAndSwap(false, true) {
				w.Write([]byte(`{"ok":true,"result":[{"update_id":7,"callback_query":{"id":"cb1","data":"tok-confirm"}}]}`))
				return
			}
			w.Write([]byte(`{"ok":true,"result":[]}`))
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			raw, _ := io.ReadAll(r.Body)
			var in struct {
				ID   string `json:"callback_query_id"`
				Text string `json:"text"`
			}
			json.Unmarshal(raw, &in)
			select {
			case answered <- in.Text:
			default:
			}
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	var applied atomic.Value
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Wait for Run to actually exit before the test returns: t.Logf from a
	// goroutine that outlives the test is a data race (and can panic).
	defer func() { <-done }()
	defer cancel()
	p := &TelegramPoller{
		Token:   "TOK",
		BaseURL: srv.URL,
		Logf:    t.Logf,
		Apply: func(token string) (string, error) {
			applied.Store(token)
			return "recorded", nil
		},
	}
	go func() {
		p.Run(ctx)
		close(done)
	}()

	select {
	case text := <-answered:
		if text != "recorded" {
			t.Fatalf("answer text = %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poller never answered the callback")
	}
	if got, _ := applied.Load().(string); got != "tok-confirm" {
		t.Fatalf("applied token = %q", got)
	}
}

// TestPollerSeedOffsetDropsStaleUpdate covers the restart-replay fix: a
// callback query that was already queued at Telegram before Run starts
// (e.g. a cancel-click on an event later reinstated) must never reach
// Apply. A callback that arrives after the seed call, however, must be
// applied normally.
func TestPollerSeedOffsetDropsStaleUpdate(t *testing.T) {
	var seedServed atomic.Bool
	var freshServed atomic.Bool
	applied := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			if r.URL.Query().Get("offset") == "-1" {
				seedServed.Store(true)
				// A callback query that was already queued when Run started.
				w.Write([]byte(`{"ok":true,"result":[{"update_id":5,"callback_query":{"id":"cb-stale","data":"tok-stale"}}]}`))
				return
			}
			if !seedServed.Load() {
				t.Errorf("getUpdates reached with offset=%q before the seed call", r.URL.Query().Get("offset"))
			}
			if freshServed.CompareAndSwap(false, true) {
				// A callback query that arrives after the seed.
				w.Write([]byte(`{"ok":true,"result":[{"update_id":6,"callback_query":{"id":"cb-fresh","data":"tok-fresh"}}]}`))
				return
			}
			w.Write([]byte(`{"ok":true,"result":[]}`))
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { <-done }()
	defer cancel()
	p := &TelegramPoller{
		Token:   "TOK",
		BaseURL: srv.URL,
		Logf:    t.Logf,
		Apply: func(token string) (string, error) {
			applied <- token
			return "recorded", nil
		},
	}
	go func() {
		p.Run(ctx)
		close(done)
	}()

	select {
	case tok := <-applied:
		if tok != "tok-fresh" {
			t.Fatalf("applied token = %q, want tok-fresh (the stale seeded update must be dropped)", tok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poller never applied the fresh callback")
	}

	select {
	case tok := <-applied:
		t.Fatalf("stale update was applied: %q", tok)
	case <-time.After(200 * time.Millisecond):
		// Expected: the stale update never shows up.
	}
}

// TestPollerLogsAnswerFailure covers the answer() fix: a non-OK HTTP status
// or a Telegram {"ok":false} envelope from answerCallbackQuery must surface
// as a logged error (via the caller's existing p.Logf call) rather than
// being silently swallowed, and must not panic the poller.
func TestPollerLogsAnswerFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"okFalseEnvelope", http.StatusOK, `{"ok":false,"description":"Bad Request: query is too old"}`},
		{"httpBadRequest", http.StatusBadRequest, `{"ok":false,"description":"Bad Request: query is too old"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var served atomic.Bool
			logs := make(chan string, 8)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/getUpdates"):
					if r.URL.Query().Get("offset") == "-1" {
						w.Write([]byte(`{"ok":true,"result":[]}`))
						return
					}
					if served.CompareAndSwap(false, true) {
						w.Write([]byte(`{"ok":true,"result":[{"update_id":9,"callback_query":{"id":"cb-fail","data":"tok-fail"}}]}`))
						return
					}
					w.Write([]byte(`{"ok":true,"result":[]}`))
				case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
					w.WriteHeader(tc.status)
					w.Write([]byte(tc.body))
				}
			}))
			defer srv.Close()

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			defer func() { <-done }()
			defer cancel()
			p := &TelegramPoller{
				Token:   "TOK",
				BaseURL: srv.URL,
				Logf: func(format string, args ...any) {
					select {
					case logs <- format:
					default:
					}
				},
				Apply: func(token string) (string, error) {
					return "recorded", nil
				},
			}
			go func() {
				p.Run(ctx)
				close(done)
			}()

			select {
			case msg := <-logs:
				if !strings.Contains(msg, "telegram answer") {
					t.Fatalf("unexpected log message: %q", msg)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("poller never logged the answerCallbackQuery failure")
			}
		})
	}
}

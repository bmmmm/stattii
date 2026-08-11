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
	go p.Run(ctx)

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

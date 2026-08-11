// SPDX-License-Identifier: GPL-3.0-or-later

package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// TelegramPoller long-polls getUpdates and turns inline-button callbacks
// (callback_data = action-link token) into applied actions. Duplicate
// deliveries are harmless: confirm and cancel are idempotent in core.
type TelegramPoller struct {
	Token   string
	BaseURL string                             // default api.telegram.org
	Apply   func(token string) (string, error) // returns user-facing result text
	Logf    func(format string, args ...any)
	client  *http.Client
}

type tgUpdate struct {
	UpdateID      int `json:"update_id"`
	CallbackQuery *struct {
		ID   string `json:"id"`
		Data string `json:"data"`
	} `json:"callback_query"`
}

func (p *TelegramPoller) Run(ctx context.Context) {
	if p.Token == "" || p.Apply == nil {
		return
	}
	if p.BaseURL == "" {
		p.BaseURL = "https://api.telegram.org"
	}
	if p.Logf == nil {
		p.Logf = log.Printf
	}
	if p.client == nil {
		// Must outlive the long-poll timeout below.
		p.client = &http.Client{Timeout: 60 * time.Second}
	}
	offset := 0
	for ctx.Err() == nil {
		updates, err := p.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.Logf("stattii: telegram poll: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.CallbackQuery == nil {
				continue
			}
			text, err := p.Apply(u.CallbackQuery.Data)
			if err != nil {
				text = "This action is no longer possible: " + err.Error()
			}
			if err := p.answer(ctx, u.CallbackQuery.ID, text); err != nil {
				p.Logf("stattii: telegram answer: %v", err)
			}
		}
	}
}

func (p *TelegramPoller) getUpdates(ctx context.Context, offset int) ([]tgUpdate, error) {
	q := url.Values{
		"timeout":         {"50"},
		"offset":          {strconv.Itoa(offset)},
		"allowed_updates": {`["callback_query"]`},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.BaseURL+"/bot"+p.Token+"/getUpdates?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool       `json:"ok"`
		Description string     `json:"description"`
		Result      []tgUpdate `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode getUpdates: %w", err)
	}
	if !out.OK {
		return nil, fmt.Errorf("telegram: %s", out.Description)
	}
	return out.Result, nil
}

func (p *TelegramPoller) answer(ctx context.Context, callbackID, text string) error {
	payload, err := json.Marshal(map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.BaseURL+"/bot"+p.Token+"/answerCallbackQuery", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

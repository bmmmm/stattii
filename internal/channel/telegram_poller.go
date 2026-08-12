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
// (callback_data = action-link token) into applied actions. Run seeds its
// offset (see seedOffset) before entering the poll loop instead of starting
// at 0: Telegram retains unconfirmed updates for up to 24h, so a plain
// restart would replay every callback query queued since the process died.
// A replayed cancel-click would re-run the cancel transaction, including
// outward fan-out, on an event an admin may have since reinstated. Dropping
// a stale click on restart is safe — the person can just click again — so
// that is the direction we bias toward.
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
	offset, err := p.seedOffset(ctx)
	if err != nil {
		// ctx was canceled while retrying the seed; nothing left to do.
		return
	}
	for ctx.Err() == nil {
		updates, err := p.getUpdates(ctx, offset, 50*time.Second)
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

// seedOffset determines the offset Run should start polling from, without
// applying any callback that was already queued before this process started.
// getUpdates with offset=-1 (and timeout=0, so it returns immediately)
// yields only the single newest pending update, if any — see the Bot API
// docs on drop-pending-updates semantics. We advance past it without
// calling Apply, which both confirms it (Telegram won't resend it or
// anything older) and discards it. If the seed call itself fails, retrying
// with backoff (rather than falling back to offset 0) is required: offset 0
// is exactly the replay bug this function exists to prevent.
func (p *TelegramPoller) seedOffset(ctx context.Context) (int, error) {
	for {
		updates, err := p.getUpdates(ctx, -1, 0)
		if err == nil {
			offset := 0
			for _, u := range updates {
				if u.UpdateID+1 > offset {
					offset = u.UpdateID + 1
				}
			}
			return offset, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		p.Logf("stattii: telegram seed offset: %v", err)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (p *TelegramPoller) getUpdates(ctx context.Context, offset int, timeout time.Duration) ([]tgUpdate, error) {
	q := url.Values{
		"timeout":         {strconv.Itoa(int(timeout / time.Second))},
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
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("answerCallbackQuery: HTTP %d: %s", resp.StatusCode, body)
	}
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("decode answerCallbackQuery: %w", err)
	}
	if !out.OK {
		return fmt.Errorf("telegram: %s", out.Description)
	}
	return nil
}

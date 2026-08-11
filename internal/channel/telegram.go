// SPDX-License-Identifier: GPL-3.0-or-later

package channel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

// Telegram sends plain messages via the Bot API. The address is a chat id.
type Telegram struct {
	Token   string
	BaseURL string // overridable for tests; default api.telegram.org
	client  *http.Client
}

func (t *Telegram) Kind() string { return "telegram" }

func (t *Telegram) Send(to string, m core.Message) error {
	if t.Token == "" {
		return fmt.Errorf("telegram not configured: set STATTII_TELEGRAM_TOKEN")
	}
	base := t.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	if t.client == nil {
		t.client = &http.Client{Timeout: 10 * time.Second}
	}
	text := m.Body
	if m.Subject != "" {
		text = m.Subject + "\n\n" + m.Body
	}
	payload, err := json.Marshal(map[string]any{
		"chat_id":                  to,
		"text":                     text,
		"disable_web_page_preview": true,
	})
	if err != nil {
		return err
	}
	resp, err := t.client.Post(base+"/bot"+t.Token+"/sendMessage", "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("telegram: decode response: %w", err)
	}
	if !out.OK {
		return fmt.Errorf("telegram: %s", out.Description)
	}
	return nil
}

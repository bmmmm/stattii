// SPDX-License-Identifier: GPL-3.0-or-later

package channel

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

// Webhook POSTs the message body to the target URL. Used both for
// subscriber webhooks (pre-signed JSON in Body/Headers) and for broadcast
// targets of kind "webhook" (plain JSON with subject/body).
type Webhook struct {
	client *http.Client
}

func (w *Webhook) Kind() string { return "webhook" }

func (w *Webhook) Send(to string, m core.Message) error {
	if w.client == nil {
		w.client = &http.Client{Timeout: 10 * time.Second}
	}
	body := m.Body
	if len(m.Headers) == 0 {
		// Broadcast target: wrap subject+body in a minimal JSON envelope.
		body = fmt.Sprintf(`{"subject":%q,"body":%q}`, m.Subject, m.Body)
	}
	req, err := http.NewRequest(http.MethodPost, to, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range m.Headers {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook %s: status %d", to, resp.StatusCode)
	}
	return nil
}

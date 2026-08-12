// SPDX-License-Identifier: GPL-3.0-or-later

package channel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bmmmm/stattii/internal/core"
)

// TestWebhookBroadcastEnvelopeIsValidJSON guards against the original bug:
// building the envelope with fmt.Sprintf("%q", ...) emits Go string escapes
// (e.g. \x01) that are not valid JSON escapes.
func TestWebhookBroadcastEnvelopeIsValidJSON(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := &Webhook{}
	// \x01 is a control byte %q would render as \x01 (invalid JSON); the
	// quote exercises normal JSON string escaping too.
	msg := core.Message{Subject: "control\x01char \"quoted\"", Body: "body\x01here"}
	if err := wh.Send(srv.URL, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !json.Valid(received) {
		t.Fatalf("envelope is not valid JSON: %q", received)
	}
	var decoded struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(received, &decoded); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if decoded.Subject != msg.Subject {
		t.Errorf("subject = %q, want %q", decoded.Subject, msg.Subject)
	}
	if decoded.Body != msg.Body {
		t.Errorf("body = %q, want %q", decoded.Body, msg.Body)
	}
}

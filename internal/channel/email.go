// SPDX-License-Identifier: GPL-3.0-or-later

package channel

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

// Email sends via SMTP. STARTTLS is used opportunistically when the server
// offers it; PLAIN auth is only attempted once the session is encrypted (or
// the server is localhost). The whole session — dial, EHLO, optional
// STARTTLS, auth, and the message itself — is bounded by timeouts so a
// stalling peer cannot block the caller (and the service's global mutex)
// forever.
type Email struct {
	Host string
	Port string
	User string
	Pass string
	From string

	// dialTimeout and sessionTimeout default to 10s/30s in Send when zero.
	// Unexported so tests can shorten them without widening the public API.
	dialTimeout    time.Duration
	sessionTimeout time.Duration
}

func (e *Email) Kind() string { return "email" }

func (e *Email) Send(to string, m core.Message) error {
	if e.Host == "" || e.From == "" {
		return fmt.Errorf("smtp not configured: set STATTII_SMTP_HOST and STATTII_SMTP_FROM")
	}
	port := e.Port
	if port == "" {
		port = "587"
	}
	dialTimeout := e.dialTimeout
	if dialTimeout == 0 {
		dialTimeout = 10 * time.Second
	}
	sessionTimeout := e.sessionTimeout
	if sessionTimeout == 0 {
		sessionTimeout = 30 * time.Second
	}

	addr := e.Host + ":" + port
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	defer conn.Close()
	// One deadline for the entire session (connect through QUIT) so a peer
	// that accepts the TCP connection and then goes silent at any step
	// still bounds the call.
	if err := conn.SetDeadline(time.Now().Add(sessionTimeout)); err != nil {
		return fmt.Errorf("smtp set deadline: %w", err)
	}

	client, err := smtp.NewClient(conn, e.Host)
	if err != nil {
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer client.Close()

	// Extension implicitly sends EHLO (falling back to HELO) before
	// reporting the server's capabilities.
	isTLS := false
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: e.Host}); err != nil {
			return fmt.Errorf("smtp STARTTLS: %w", err)
		}
		isTLS = true
	}

	if e.User != "" && e.Pass != "" {
		// Refusing beats silently sending unauthenticated: credentials on a
		// plaintext connection mean either a config mistake or a STARTTLS
		// downgrade in progress — both must be loud.
		if !isTLS && !isLocalhostAddr(e.Host) {
			return fmt.Errorf("smtp auth: server %s offers no STARTTLS — refusing to authenticate over plaintext", e.Host)
		}
		auth := smtp.PlainAuth("", e.User, e.Pass, e.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := client.Mail(e.From); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("smtp RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(buildMessage(e.From, to, e.Host, m)); err != nil {
		return fmt.Errorf("smtp write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close body: %w", err)
	}

	return client.Quit()
}

// buildMessage assembles the RFC 5322 message, including Date and
// Message-ID headers — both matter for deliverability with mail filters.
func buildMessage(from, to, host string, m core.Message) []byte {
	msg := strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + mime.QEncoding.Encode("utf-8", m.Subject),
		"Date: " + time.Now().Format(time.RFC1123Z),
		"Message-ID: " + newMessageID(host),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		m.Body,
	}, "\r\n")
	return []byte(msg)
}

// newMessageID builds a random RFC 5322 message identifier scoped to host.
func newMessageID(host string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read practically never fails; fall back to a
		// timestamp-based id rather than erroring the whole send.
		return fmt.Sprintf("<%d@%s>", time.Now().UnixNano(), host)
	}
	return fmt.Sprintf("<%s@%s>", hex.EncodeToString(buf), host)
}

// isLocalhostAddr mirrors net/smtp's PlainAuth localhost allowance, so
// auth-over-plaintext is permitted in exactly the same cases the stdlib
// PlainAuth implementation would have permitted it.
func isLocalhostAddr(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

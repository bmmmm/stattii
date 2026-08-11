// SPDX-License-Identifier: GPL-3.0-or-later

package channel

import (
	"fmt"
	"mime"
	"net/smtp"
	"strings"

	"github.com/bmmmm/stattii/internal/core"
)

// Email sends via SMTP (STARTTLS when the server offers it).
type Email struct {
	Host string
	Port string
	User string
	Pass string
	From string
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
	var auth smtp.Auth
	if e.User != "" {
		auth = smtp.PlainAuth("", e.User, e.Pass, e.Host)
	}
	msg := strings.Join([]string{
		"From: " + e.From,
		"To: " + to,
		"Subject: " + mime.QEncoding.Encode("utf-8", m.Subject),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		m.Body,
	}, "\r\n")
	return smtp.SendMail(e.Host+":"+port, auth, e.From, []string{to}, []byte(msg))
}

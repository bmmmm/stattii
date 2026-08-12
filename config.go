// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
)

// fileConfig is config.json: everything lives in the project folder, one
// place. Lines whose first non-space characters are "//" are comments.
// Unknown keys are rejected loudly so typos cannot be silently ignored.
// Precedence at startup: explicit serve flags > config.json > STATTII_* env.
type fileConfig struct {
	Listen string `json:"listen"`
	// Admin API + web admin bind here — a separate listener so the public
	// one can never serve management routes. Keep it off public interfaces
	// (default 127.0.0.1:8789; reach it via SSH tunnel, VPN or an
	// internally-restricted reverse proxy).
	AdminListen   string `json:"admin_listen"`
	BaseURL       string `json:"base_url"`
	CalName       string `json:"cal_name"`
	DataDir       string `json:"data_dir"`
	ReminderLead  string `json:"reminder_lead"`
	DeadlineLead  string `json:"deadline_lead"`
	EscalateAfter string `json:"escalate_after"`
	Tick          string `json:"tick"`
	AdminNotify   string `json:"admin_notify"` // "email:addr" | "telegram:chat-id"
	// Comma-separated CIDRs of reverse proxies in front of stattii. When the
	// TCP peer matches, the rate-limit client IP comes from X-Forwarded-For
	// (rightmost hop not in this list) — otherwise everyone behind the proxy
	// shares one bucket. Empty = direct exposure, header ignored.
	TrustedProxies string `json:"trusted_proxies"`
	// Foreign ICS feed to import events from (operator data — never in
	// the repo). webcal:// is rewritten to https://. Fetch is manual:
	// POST /api/v1/calendar/fetch, the panel button, or `stattii
	// calendar fetch`.
	CalendarSource string `json:"calendar_source"`
	// How far ahead recurring series are materialised (Go duration).
	CalendarWindow string         `json:"calendar_window"`
	Email          emailConfig    `json:"email"`
	Telegram       telegramConfig `json:"telegram"`
}

type emailConfig struct {
	SMTPHost string `json:"smtp_host"`
	SMTPPort string `json:"smtp_port"`
	SMTPUser string `json:"smtp_user"`
	// Either put the credential here (config.json is gitignored, keep it
	// chmod 600) or leave it empty and set the env var named in
	// smtp_pass_env (default STATTII_SMTP_PASS).
	SMTPPass    string `json:"smtp_pass"`
	SMTPPassEnv string `json:"smtp_pass_env"`
	From        string `json:"from"`
}

type telegramConfig struct {
	Token    string `json:"token"`
	TokenEnv string `json:"token_env"`
}

// loadFileConfig reads and parses path. A missing file is only an error
// when the user pointed at it explicitly.
func loadFileConfig(path string, explicit bool) (fileConfig, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if explicit {
			return fileConfig{}, fmt.Errorf("config %s not found (copy config.example.json or one of examples/)", path)
		}
		return fileConfig{}, nil
	}
	if err != nil {
		return fileConfig{}, err
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		log.Printf("stattii: warning: %s is group/world readable — chmod 600 (it may contain credentials)", path)
	}
	var c fileConfig
	dec := json.NewDecoder(bytes.NewReader(stripComments(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return fileConfig{}, fmt.Errorf("parse %s: %w (comments must be on their own lines, keys must match config.example.json)", path, err)
	}
	return c, nil
}

// stripComments blanks full-line // comments but keeps the newlines, so
// JSON error offsets still point at the right line.
func stripComments(b []byte) []byte {
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines[i] = ""
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (c *fileConfig) smtpPass() string {
	if c.Email.SMTPPass != "" {
		return c.Email.SMTPPass
	}
	return os.Getenv(firstOf(c.Email.SMTPPassEnv, "STATTII_SMTP_PASS"))
}

func (c *fileConfig) telegramToken() string {
	if c.Telegram.Token != "" {
		return c.Telegram.Token
	}
	return os.Getenv(firstOf(c.Telegram.TokenEnv, "STATTII_TELEGRAM_TOKEN"))
}

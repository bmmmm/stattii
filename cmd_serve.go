// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bmmmm/stattii/internal/channel"
	"github.com/bmmmm/stattii/internal/core"
	"github.com/bmmmm/stattii/internal/httpapi"
)

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", ":8788", "listen address")
	dataDir := fs.String("data", "./data", "data directory (state.json, audit.jsonl, admin-token)")
	baseURL := fs.String("base-url", "", "public base URL used in links (default http://localhost:<port>)")
	calName := fs.String("cal-name", "stattii", "calendar name in the ICS feed")
	reminderLead := fs.Duration("reminder-lead", 48*time.Hour, "how long before start the confirm/cancel ask goes out")
	deadlineLead := fs.Duration("deadline-lead", 24*time.Hour, "unanswered this close to start fires deadline.passed")
	escalateAfter := fs.Duration("escalate-after", 10*time.Minute, "undelivered outbox items page the admin after this long")
	tickEvery := fs.Duration("tick", time.Minute, "scheduler interval")
	fs.Parse(args)

	if *baseURL == "" {
		addr := *listen
		if strings.HasPrefix(addr, ":") {
			addr = "localhost" + addr
		}
		*baseURL = "http://" + addr
	}

	store, err := core.NewJSONStore(*dataDir)
	if err != nil {
		log.Fatalf("stattii: %v", err)
	}

	cfg := core.Config{
		BaseURL:       *baseURL,
		ReminderLead:  *reminderLead,
		DeadlineLead:  *deadlineLead,
		EscalateAfter: *escalateAfter,
		AdminNotify:   parseAdminNotify(os.Getenv("STATTII_ADMIN_NOTIFY")),
	}

	registry := channel.NewRegistry(
		&channel.Email{
			Host: os.Getenv("STATTII_SMTP_HOST"),
			Port: os.Getenv("STATTII_SMTP_PORT"),
			User: os.Getenv("STATTII_SMTP_USER"),
			Pass: os.Getenv("STATTII_SMTP_PASS"),
			From: os.Getenv("STATTII_SMTP_FROM"),
		},
		&channel.Telegram{Token: os.Getenv("STATTII_TELEGRAM_TOKEN")},
		&channel.Webhook{},
	)

	svc, err := core.NewService(store, cfg, registry)
	if err != nil {
		log.Fatalf("stattii: %v", err)
	}

	adminToken, err := loadAdminToken(*dataDir)
	if err != nil {
		log.Fatalf("stattii: %v", err)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           httpapi.New(svc, adminToken, *calName).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.RunScheduler(ctx, *tickEvery)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("stattii %s listening on %s (base %s, data %s)", versionString(), *listen, *baseURL, *dataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("stattii: %v", err)
	}
}

// loadAdminToken reads STATTII_ADMIN_TOKEN, falling back to <data>/admin-token,
// which is generated on first run. The token is never printed — only its path.
func loadAdminToken(dataDir string) (string, error) {
	if t := os.Getenv("STATTII_ADMIN_TOKEN"); t != "" {
		return t, nil
	}
	path := filepath.Join(dataDir, "admin-token")
	raw, err := os.ReadFile(path)
	if err == nil {
		return strings.TrimSpace(string(raw)), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	token := core.NewToken()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	log.Printf("stattii: admin token generated at %s (export STATTII_TOKEN=$(cat %s) for the CLI)", path, path)
	return token, nil
}

// parseAdminNotify turns "telegram:12345" or "email:me@example.org" into an
// escalation target.
func parseAdminNotify(spec string) *core.Address {
	if spec == "" {
		return nil
	}
	kind, to, ok := strings.Cut(spec, ":")
	if !ok || kind == "" || to == "" {
		log.Printf("stattii: ignoring malformed STATTII_ADMIN_NOTIFY %q (want kind:address, e.g. telegram:12345)", spec)
		return nil
	}
	return &core.Address{Kind: kind, To: to}
}

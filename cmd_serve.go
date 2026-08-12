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
	cfgPath := fs.String("config", "config.json", "config file (JSON with // comment lines; see config.example.json)")
	listen := fs.String("listen", ":8788", "public listen address (token pages, portal, feed)")
	adminListen := fs.String("admin-listen", "127.0.0.1:8789", "admin listen address (API + web admin) — keep it off the public interface")
	dataDir := fs.String("data", "./data", "data directory (state.json, audit.jsonl, admin-token)")
	baseURL := fs.String("base-url", "", "public base URL used in links (default http://localhost:<port>)")
	calName := fs.String("cal-name", "stattii", "calendar name in the ICS feed")
	reminderLead := fs.Duration("reminder-lead", 48*time.Hour, "how long before start the confirm/cancel ask goes out")
	deadlineLead := fs.Duration("deadline-lead", 24*time.Hour, "unanswered this close to start fires deadline.passed")
	escalateAfter := fs.Duration("escalate-after", 10*time.Minute, "undelivered outbox items page the admin after this long")
	tickEvery := fs.Duration("tick", time.Minute, "scheduler interval")
	trustedProxies := fs.String("trusted-proxies", "", "comma-separated CIDRs of reverse proxies; rate-limit client IP then comes from X-Forwarded-For")
	fs.Parse(args)

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	fc, err := loadFileConfig(*cfgPath, set["config"])
	if err != nil {
		log.Fatalf("stattii: %v", err)
	}
	parseDur := func(field, v string) time.Duration {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("stattii: config %s: %s = %q is not a duration (use forms like 48h, 30m)", *cfgPath, field, v)
		}
		return d
	}
	// Explicit flags win; otherwise the config file fills the gaps.
	if !set["listen"] && fc.Listen != "" {
		*listen = fc.Listen
	}
	if !set["admin-listen"] {
		if fc.AdminListen != "" {
			*adminListen = fc.AdminListen
		} else if v := os.Getenv("STATTII_ADMIN_LISTEN"); v != "" {
			*adminListen = v
		}
	}
	if !set["data"] && fc.DataDir != "" {
		*dataDir = fc.DataDir
	}
	if !set["base-url"] && fc.BaseURL != "" {
		*baseURL = fc.BaseURL
	}
	if !set["cal-name"] && fc.CalName != "" {
		*calName = fc.CalName
	}
	if !set["reminder-lead"] && fc.ReminderLead != "" {
		*reminderLead = parseDur("reminder_lead", fc.ReminderLead)
	}
	if !set["deadline-lead"] && fc.DeadlineLead != "" {
		*deadlineLead = parseDur("deadline_lead", fc.DeadlineLead)
	}
	if !set["escalate-after"] && fc.EscalateAfter != "" {
		*escalateAfter = parseDur("escalate_after", fc.EscalateAfter)
	}
	if !set["tick"] && fc.Tick != "" {
		*tickEvery = parseDur("tick", fc.Tick)
	}
	if !set["trusted-proxies"] && fc.TrustedProxies != "" {
		*trustedProxies = fc.TrustedProxies
	}
	if *trustedProxies == "" {
		*trustedProxies = os.Getenv("STATTII_TRUSTED_PROXIES")
	}
	trusted, err := httpapi.ParseTrustedProxies(*trustedProxies)
	if err != nil {
		log.Fatalf("stattii: %v", err)
	}

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
		AdminNotify:   parseAdminNotify(firstOf(fc.AdminNotify, os.Getenv("STATTII_ADMIN_NOTIFY"))),
	}

	tgToken := fc.telegramToken()
	registry := channel.NewRegistry(
		&channel.Email{
			Host: firstOf(fc.Email.SMTPHost, os.Getenv("STATTII_SMTP_HOST")),
			Port: firstOf(fc.Email.SMTPPort, os.Getenv("STATTII_SMTP_PORT")),
			User: firstOf(fc.Email.SMTPUser, os.Getenv("STATTII_SMTP_USER")),
			Pass: fc.smtpPass(),
			From: firstOf(fc.Email.From, os.Getenv("STATTII_SMTP_FROM")),
		},
		&channel.Telegram{Token: tgToken},
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

	api := httpapi.New(svc, adminToken, *calName, trusted)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           api.PublicHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// The admin surface is a separate listener on purpose: the public
	// server cannot route to it, so a proxy misconfiguration cannot
	// expose management. Default binds to loopback.
	adminSrv := &http.Server{
		Addr:              *adminListen,
		Handler:           api.AdminHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.RunScheduler(ctx, *tickEvery)
	if tgToken != "" {
		poller := &channel.TelegramPoller{
			Token: tgToken,
			Apply: func(token string) (string, error) {
				v, err := svc.ApplyAction(token)
				if err != nil {
					return "", err
				}
				if v.Action == core.ActionConfirm {
					return "Recorded: the event takes place.", nil
				}
				return "Recorded: the event is cancelled — everyone is being notified.", nil
			},
		}
		go poller.Run(ctx)
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		adminSrv.Shutdown(shutdownCtx)
	}()

	go func() {
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("stattii: admin listener: %v", err)
		}
	}()

	log.Printf("stattii %s listening on %s (admin %s, base %s, data %s)", versionString(), *listen, *adminListen, *baseURL, *dataDir)
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
		log.Printf("stattii: ignoring malformed admin_notify %q (want kind:address, e.g. telegram:12345)", spec)
		return nil
	}
	return &core.Address{Kind: kind, To: to}
}

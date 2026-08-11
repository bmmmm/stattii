# stattii

Does the event actually take place? — a secure, minimal attestation layer
over an event calendar.

Events usually happen; sometimes they get cancelled on short notice — and
the worst failure mode is *cancelled internally, never communicated
outward*: people standing in front of a locked door. stattii closes that
gap:

- Before each event, the responsible people receive **two personal links**
  (confirm / cancel) over their configured channels (email, Telegram, …).
- Every click is **tracked per recipient** and appended to an audit log.
- A cancellation is a **propagation transaction**: it is only "done" when
  every outward channel (Telegram channel, mailing list, website webhook,
  ICS feed) has confirmed delivery. Stuck deliveries page the admin.
- Automations hang off **HMAC-signed webhooks** and an **ICS feed**.
- Everything is steerable through a **REST API**; the CLI and the recipient
  pages are thin skins over it.

Single static Go binary, stdlib only — zero dependencies. State lives in a
JSON file behind a store interface (SQLite can slot in later; at a few
hundred events a year it never has to).

## Quick start

```sh
go build .
./stattii serve --data ./data --base-url https://events.example.org
# admin token is generated at ./data/admin-token on first run

export STATTII_URL=http://localhost:8788
export STATTII_TOKEN=$(cat ./data/admin-token)

stattii person add --name "Ana" --trust respond --email ana@example.org
stattii event create --title "Tuesday Session" --at 2026-08-18T19:00 --location "Hall 3"
stattii assign <event-id> <person-id>
```

48 hours before start (configurable via `--reminder-lead`), Ana gets the
confirm/cancel pair. Clicking opens a page with a single button — the GET
never mutates anything, because corporate mail scanners prefetch links; only
the POST acts.

## Trust model

Per person, `--trust` decides how much power their portal link carries:

| Level     | May do |
|-----------|--------|
| `respond` | answer confirm/cancel per assigned event |
| `propose` | + propose cancel/move/create — applied after admin approval |
| `direct`  | + cancel/move/create applying immediately |

Every person gets a long-lived portal link (`/p/<token>`) listing their
events; per-event action links (`/a/<token>`) expire with the event.

## Cancellation propagation

`stattii event cancel <id> --reason "storm"` (or a responsible person's
click) flips the status **and** fans out to all broadcast targets and
assignees through a persistent outbox with retries and exponential backoff.

```sh
stattii event propagation <id>   # {total, delivered, pending, failed, complete}
```

Items undelivered after `--escalate-after` (default 10 min) page the admin
(`STATTII_ADMIN_NOTIFY=telegram:<chat-id>` or `email:<addr>`). If nobody
answers the reminder at all, `deadline.passed` fires (webhook + admin ping)
`--deadline-lead` (default 24h) before start.

## Configuration

Flags on `serve`: `--listen`, `--data`, `--base-url`, `--cal-name`,
`--reminder-lead`, `--deadline-lead`, `--escalate-after`, `--tick`.

Environment (secrets never travel as flags):

| Variable | Purpose |
|----------|---------|
| `STATTII_ADMIN_TOKEN` | bearer token for `/api/v1` (default: generated at `<data>/admin-token`) |
| `STATTII_SMTP_HOST/PORT/USER/PASS/FROM` | email channel |
| `STATTII_TELEGRAM_TOKEN` | Telegram bot channel (addresses are chat ids) |
| `STATTII_ADMIN_NOTIFY` | escalation target, `kind:address` |
| `STATTII_URL`, `STATTII_TOKEN` | used by the CLI client |

## API

Everything the CLI does is plain REST under `/api/v1` (bearer auth):
events (`create/confirm/cancel/move/links/responses/propagation`), people,
assignments, broadcasts, webhooks, proposals, audit. Webhook payloads are
signed: `X-Stattii-Signature: sha256=<hex hmac of body>` with the
per-subscription secret returned on registration.

Public surface (rate-limited, token-scoped): `/a/<token>`, `/p/<token>`,
`/feed.ics`, `/healthz`. Note calendar apps poll ICS feeds slowly (Google:
~12–24 h) — the feed is the passive baseline; short-notice cancellations
travel through the active channels above.

## License

GPL-3.0-or-later — see [LICENSE](LICENSE).

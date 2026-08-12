# Roadmap

## Done

- **v0.1.0** — core: tokenized confirm/cancel links (GET renders, POST acts),
  trust levels (respond/propose/direct) with personal portals, cancellation
  as propagation transaction (outbox, retries, escalation), reminder +
  deadline scheduler, HMAC-signed webhooks, ICS feed, REST API + CLI.
- **v0.2.0** — dead-man-switch (`if_unconfirmed=cancel`), reinstate, outbox
  list/retry, trust-independent move proposals from action pages, Telegram
  inline buttons + `getUpdates` poller.
- **v0.3.0** — project-folder configuration: `config.json` (commented JSON,
  loud on typos), ready-made blanks in `examples/` (Gmail, generic SMTP,
  Telegram), AGENTS.md.

## Phase 2 — production go-live (next session)

Focus decided 2026-08-12: **email + links first, Telegram last.**

1. **Deploy** to the home infra: pick host (garage/nutc), reverse proxy +
   TLS, real `base_url`, service unit / compose entry, backup of the data
   dir (state.json + audit.jsonl are the whole world). → issue #2
2. **Email go-live**: copy a blank from `examples/`, fill mail access,
   send a real reminder round-trip to yourself; check spam placement
   (SPF/DKIM of the sending account matter more than the code).
3. **Real data**: enter the actual people (+ trust levels) and the real
   event series via CLI/API; define broadcast targets (mailing list,
   website webhook) for the propagation fan-out.
4. **First live cycle** observed end-to-end: reminder → click → feed/webhook.
5. **Telegram** (deliberately last): BotFather token, chat-id onboarding
   per person (see examples/config.telegram.json), verify one-tap buttons.

## Later / maybe

- Web dashboard as a second head over the API (the API already carries
  everything; the dashboard must stay a thin skin).
- Recurring events / series sugar (expand-on-create; ICS stays RRULE-free).
- SQLite store backend behind the `Store` interface — only if volume ever
  demands it (a few hundred events/year will not).
- Localization hooks — English UI confirmed OK for now (2026-08-12).
- Admin token scopes (read-only vs. admin API tokens).

## Learnings (why things are the way they are)

- **The core feature is the propagation guarantee**, not the status flip —
  origin story: internally cancelled, never announced, people at a locked
  door. Everything outward-facing goes through the persistent outbox.
- **GET must never mutate**: corporate mail scanners prefetch links and
  would confirm/cancel events. One-tap exists only where a real callback
  protocol does (Telegram inline buttons).
- **ICS is the passive baseline, never the cancellation channel** — Google
  Calendar polls subscribed feeds only every ~12–24 h.
- **JSON store over SQLite** was the right cut: Go SQLite means CGO or a
  huge transpiled dependency; the data is kilobytes. The `Store` interface
  keeps the door open.
- **Proposals are trust-free by design**: they never apply by themselves,
  so even respond-level people may counter a cancellation with a new time.
- Tests: `internal/channel` binds real listeners (`httptest.NewServer`) —
  needs a sandbox bypass locally, runs clean in CI. Prove new assertions
  can go red before trusting them (done for the GET-mutation guard).

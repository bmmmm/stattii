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
- **v0.4.0** — admin surface: dedicated admin listener (`admin_listen`,
  default loopback) carrying `/api/v1` + the server-rendered `/admin` web
  UI (cookie login), `GET /api/v1/overview` + `stattii overview`; the
  public listener keeps only the token pages and the feed.
- **v0.4.1** — unix-socket support for `admin_listen` (any value with `/`).
- **v0.5.0** — calendar import: fetch a foreign ICS feed
  (`calendar_source`), expand recurrence into `calendar_window`, sync
  occurrences as events (start changes run the move transaction; vanished
  is reported, never cancelled), series responsibles, per-recipient
  tracking timeline + test messages in the panel, `/api/v1` contract test.

## Phase 2 — production go-live

Focus decided 2026-08-12: **email + links first, Telegram last.**

1. ~~**Deploy**~~ — done 2026-08-12: garage, public at `https://stattii.6bm.de`
   via the garage-wb Cloudflare Tunnel → Traefik (LE cert). Runs as
   `stattii:local` built on garage by `~/servers/garage/scripts/
   rebuild-stattii.sh`; compose + policy live in the servers repo. Data is a
   bind mount under `~/docker/stattii/` (nightly restic sweep). → issue #2
2. ~~**Email go-live**~~ — done 2026-08-12: Gmail app password, reminder +
   cancellation round-trip delivered cross-provider (Gmail → brtsz.de),
   confirm-via-link proven end to end. Spam placement: inbox.
3. **Real data**: ~~event series~~ — since 2026-08-12 the events come
   from the calendar source (`calendar_source`, manual fetch via panel
   button / `stattii calendar fetch` / API): ICS import with recurrence
   expansion into a 60-day window, auto-move with fan-out on source time
   changes, vanished-is-reported-never-cancelled. Remaining: enter the
   actual people (+ trust levels), set series responsibles
   (`series-assign` / panel checkbox), define broadcast targets.
4. ~~**First live cycle**~~ — observed 2026-08-12 with test data:
   reminder → click → confirm → feed, then cancel → propagation complete
   (delivered 1/1). Repeat once with real data as part of step 3.
5. **Telegram** (deliberately last): BotFather token, chat-id onboarding
   per person (see examples/config.telegram.json), verify one-tap buttons.

## Later / maybe

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
- **Behind a reverse proxy the limiter needs `trusted_proxies`** (found at
  go-live): the rate-limit key was `RemoteAddr`, which behind Traefik is
  always the proxy — every recipient would share one 30/min bucket. The
  fix walks X-Forwarded-For right-to-left past trusted hops; direct
  clients still cannot spoof it.
- **Reminders wait for assignees** (found live): the scheduler ticked in
  the seconds between event.created and the first assignment and burned
  the one-shot reminder on zero recipients. The deadline pass deliberately
  does NOT wait — an unstaffed `if_unconfirmed=cancel` event must still
  auto-cancel.
- **Gmail self-send is invisible**: SMTP-submitting from your own Gmail
  address to itself lands only in Sent/All Mail, never the inbox — a
  self-round-trip "did not arrive" is Gmail dedup, not a delivery failure.
  Test deliverability cross-provider.

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
- **Party invitations** — one shared `/i/<token>` link per
  event: invitees self-register (name, optional email, yes/no; answering
  again under the same name updates), aggregate-only public page, guest
  list + link management in panel/API/CLI (`event invite` / `event
  guests`), guests with an address join the cancel/move/reinstate fan-out.
- **v0.7.0** — flow gaps from the 2026-09-02 review: assigned ≠ reachable
  (channel validation, `Person.Reachable`, the deadline no longer waits
  for an ask that cannot go out, one early "nobody can be reached" page);
  a fan-out with zero recipients is an alarm (`propagation.empty`, "Nobody
  was told" page + red panel card from `FanOutAt/FanOutCount`); cancel
  notices say who cancelled, link cancels take a reason; the auto-cancel
  reason is recipient-facing. Calendar: `calendar_fetch_every` polling
  with once-per-episode failure paging, sticky `VanishedAt` marker
  (panel, page on transition, note in the reminder), the importer keeps
  operator notes. People: `PATCH /people/{id}` / `person set`, unassign
  (event + future series), CI runs govulncheck.

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
   changes, vanished-is-reported-never-cancelled; since v0.7.0 the fetch
   can poll by itself (`calendar_fetch_every`, off in prod until switched
   on deliberately) and people are editable. Remaining: enter the actual
   people (+ trust levels, `person set` for corrections), set series
   responsibles (`series-assign` / panel checkbox), define broadcast
   targets — until then every cancellation raises "Nobody was told".
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
- Guest double-opt-in (confirm-your-address mail before joining the
  notice list): v1 accepts that link possession is the only gate and
  leans on rate limit + cap + address dedup + write-once addresses;
  a per-audience notice wording would ride along with this.

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
- **Assigned ≠ reachable** (review 2026-09-02): the reminder waited for a
  person with a channel, the deadline waited for the reminder as soon as
  anyone was assigned — one channel-less responsible disarmed the
  dead-man-switch completely. The scheduler now keys on
  `Person.Reachable`, and a blank `{"kind":"","to":""}` no longer counts
  as a channel.
- **A fan-out with zero recipients is an alarm.** Prod had no broadcast
  targets, so a cancellation produced 0 outbox items, a hidden panel card
  and no page — a green status nobody heard about. Every propagation
  transaction now audits `propagation.empty` and pages "Nobody was told".
- **Panel truth comes from persisted event fields, not the outbox**:
  `Propagation.Total == 0` also happens after retention pruning of a
  correctly told cancellation, so "nobody was told" reads
  `FanOutAt/FanOutCount` on the event; likewise "vanished" is
  `Event.VanishedAt`, not a line in the last import report.
- **Guest fan-out is status-blind**: a party guest who left an address gets
  cancel/move/reinstate notices regardless of their yes/no — the decliner
  declined the *old* date, and a "no" who shows up anyway is the classic
  locked-door victim. Consequence: guests gate `propagation.complete`, so
  RSVP validates addresses strictly (reject now, never fail forever).
  Guests are deliberately not People — no portal token, no trust level,
  and the reminder/deadline scheduler never sees them.

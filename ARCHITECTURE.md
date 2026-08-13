# Architecture

Why things are the way they are. The rules agents must not break live in
[AGENTS.md](AGENTS.md); the operator manual is [README.md](README.md); the
history of each decision is in [ROADMAP.md](ROADMAP.md). This file is the
map: the flows, the store, the identity model — and the limits that are
deliberate decisions, not oversights.

The one guarantee everything serves: **nobody stands in front of a locked
door.** A cancellation is not done when the status flips; it is done when
every outward recipient has provably been told.

## Flows: one process, two listeners

```
INBOUND
═══════
PUBLIC LISTENER (rate-limited per client IP)
  responsibles → GET/POST /a/<token>   one-click confirm/cancel
  responsibles → /p/<token>            portal: respond/move/create per trust
  party guests → /i/<token>            RSVP (name, optional email, yes/no)
  calendar apps → GET /feed.ics        read-only, unauthenticated

ADMIN LISTENER (separate mux — bind it internally)
  operator → /admin (cookie) + /api/v1 (bearer) + CLI

POLLED (outbound connections, no open port needed)
  Telegram bot   ← getUpdates poller   inline-button taps → ApplyAction
  source calendar ← calendar fetch     ICS import (moves yes, cancel NEVER)

                          │
                          ▼
                 ┌──────────────────┐
                 │   core.Service   │  every mutation: audit + persist,
                 │   fanOutLocked   │  one mutex; cancel/move/reinstate
                 └────────┬─────────┘  are propagation transactions
                          ▼
                 ┌──────────────────┐  retries with backoff,
                 │      OUTBOX      │  escalation to the admin,
                 └──┬─────┬──────┬──┘  delivery proof (`propagation`)
              email │ telegram │ webhook
                    ▼         ▼      ▼
OUTBOUND RECIPIENTS
═══════════════════
  assignees   reminders (with action links), propagation, verdicts, tests
  broadcasts  propagation only (audience-facing targets)
  guests      propagation only, status-blind, one notice per address,
              never a reminder
  webhooks    signed JSON events (ids and status, no third-party PII)
  admin       escalations, deadline pings, auto-cancel notices
```

The two listeners are separate muxes by construction, not by middleware:
a reverse-proxy mistake cannot expose management routes on the public
side, because they were never registered there. `fanOutLocked` is the
single choke point for outward propagation — the dead-man switch, the
calendar importer, portal actions, and party guests all inherit it.

## The store: one JSON document

There is no database. The whole world is a `State` struct in memory,
guarded by one mutex, written atomically on every mutation:
`state.json` via temp file → fsync → rename → directory fsync (survives
power loss), plus `audit.jsonl` as an append-only journal of every
mutation. Both files are 0600 — they contain capability tokens.

The "tables" are slices linked by ids: events, people, assignments,
links, responses, proposals, broadcasts, webhooks, outbox, series
assignments, invites, guests. Every lookup is a linear scan. The `Store`
interface is the exit hatch if volume ever demands SQLite; a few hundred
events a year never will.

## Identity: token possession, three tiers

There are no accounts. Two kinds of keys, strictly separated:

- **Ids** (`ev_`, `pe_`, `gu_`, `ob_`, …) are internal references —
  short random hex with a prefix, fine to show in the API and panel.
- **Tokens** (128-bit crypto/rand hex) are capabilities — never decoded,
  never JWT, resolved only by looking them up in the store, revocable
  via `RevokedAt`. Expiry is computed live from the event (`EndsAt`, or
  start + 6h) and never stored — a stored copy would silently go stale
  after every move.

| Token | resolves to | grants |
|---|---|---|
| action link | (event, person, action) | one click = one authenticated answer |
| portal token | person | all their assigned events, powers per trust level |
| invite link | event only — deliberately no person | the RSVP form; identity is the typed name |
| admin token | — (config secret) | everything, bearer or cookie, compared constant-time |

(`Webhook.Secret` is not a lookup token — it is the HMAC key signing
outbound payloads.)

The three tiers: the **operator** holds the admin token; **responsibles**
are operator-created People with strong per-event attribution (the click
itself is the identity — no login, which is why response rates work);
**guests** sit behind one shared link with nothing but a typed name —
deliberately weak, deliberately frictionless.

Signals join back through the same ids: `Response` (event, person, via),
`Guest` (event, normalised name as upsert key), `OutboxItem` (event,
person **or** guest, purpose) — from which the per-event `propagation`
summary, the per-person timeline, and the per-guest delivery marker are
all derived. Telegram button taps carry the action-link token in the
callback data and run through the same lookup as a link click.

## Deliberate limits

The difference between simple and naive is whether the limits are
documented decisions. These are the decisions:

- **It does not scale, on purpose.** Every mutation rewrites the whole
  state file, every lookup is O(n), and sends run under the global lock
  (bounded by per-session timeouts). At club scale — hundreds of events,
  dozens of people — this is unmeasurable and buys a system with no
  cache-coherency, no migration, and no race-condition class at all. At
  ten thousand guests it is the first thing to break; the `Store`
  interface is the planned exit.
- **Guest identity is just a name.** Right calibration for a shared
  party link among acquaintances; too weak for public events. The sharp
  edges are filed off — write-once addresses, address-deduped fan-out,
  rate limit, cap, oracle-free responses — but there is no ownership
  proof. Double-opt-in is a roadmap item, not an accident.
- **Tokens at rest are plaintext** in `state.json` (0600, encrypted
  backups). Whoever can read the file owns the host anyway; hashing
  them at rest is backlog, not a hole.
- **Portal tokens never expire on their own.** A durable capability in a
  mailbox is the UX trade-off that makes the portal usable; killing a
  leaked one is an explicit operator act — rotate it (panel button,
  `person rotate-portal`), and action links revoke the same way
  (`event revoke-links`).
- **The ICS feed is unauthenticated** and lists all events — it is the
  passive baseline for calendar apps, and its URL must be treated like
  a token. Short-notice cancellations never rely on it.

# AGENTS.md — stattii

Working notes for AI agents (and humans) in this repo. Read this before
changing anything.

## What this is

A secure, minimal attestation layer over an event calendar: responsible
people confirm/cancel events via tokenized links; cancellations propagate
outward with delivery proof. Full product description: [README.md](README.md).
Origin: an event was once cancelled internally but never communicated —
people stood in front of a locked door. That failure mode drives the design.

## Architecture (read in this order)

| Piece | Role |
|-------|------|
| `internal/core` | domain types, JSON store (`state.json` atomic + `audit.jsonl` append-only), `Service` (every mutation audits + persists under one mutex), scheduler `Tick` |
| `internal/channel` | `Sender` interface: email (SMTP), telegram (send + `getUpdates` poller for inline-button callbacks), webhook |
| `internal/httpapi` | TWO muxes: public token surface (`/a/`, `/p/`, `/feed.ics`, rate limiter) and the admin listener (`/api/v1` behind bearer auth + `/admin` web UI behind cookie login) |
| `internal/ics` | outbound ICS feed generation |
| `internal/icsimport` | inbound: parses the configured foreign feed + expands recurrence into a window (owner decision 2026-08-12 — import IS in scope now). The feed URL is operator data: host config only, never in the repo; test fixtures are synthetic |
| root `package main` | `serve` + thin CLI client over the REST API; `config.go` loads `config.json` |

Design rationale and the deliberate limits (scaling, guest identity,
tokens at rest): [ARCHITECTURE.md](ARCHITECTURE.md).

## Invariants — do not break

1. **GET never mutates.** Mail scanners prefetch links; only POST acts.
   Guarded by `TestActionPageFlow` — keep that test meaningful.
2. **Every outbound message goes through the outbox.** No direct sends —
   retries, delivery proof, and escalation depend on it.
3. **Cancel / move / reinstate are propagation transactions** (`fanOutLocked`):
   a status flip without outward fan-out — to every broadcast target, every
   assignee channel, and every party guest who left an address — recreates
   the locked-door bug. Guest fan-out is status-blind (a decliner still
   needs the move notice); guarded by `TestGuestsGetCancellationFanOut`.
   A fan-out that reaches **nobody** escalates (`nobodyToldLocked`: audit
   `propagation.empty`, admin page, red panel card from the event's own
   `FanOutAt`/`FanOutCount` — never from the prunable outbox); guarded by
   `TestCancelWithNoRecipientsPagesAdmin`. Assigned ≠ reachable ≠ sound:
   the scheduler waits only for people with a usable channel
   (`Person.Reachable`); guarded by `TestDeadlineFiresForUnreachableAssignees`.
   Reachability stays **structural** — `Address.Usable` asks "is there
   something to try", `Address.Validate` asks "does it look right", and
   the two must never be merged. Validate is in places stricter than the
   channel (`parseEmail` wants a dot in the domain, so `root@garage`
   fails it while internal SMTP delivers it), so letting it decide would
   auto-cancel `if_unconfirmed=cancel` events over a typo, without one
   message ever going out. A suspect stored address is therefore
   *reported*, never acted on: `Service.ChannelProblems` (live, never
   cached), one `channel.invalid` page per process, and one
   `staffing.channels_broken` page at the moment an ask goes out over
   nothing but suspect channels. Guarded by
   `TestLegacyChannelStaysReachable`, `TestBrokenChannelStillGetsTheAsk`
   and `TestAdminPanelShowsBrokenChannels`.
   Deleting is not a second way out: `DeleteEvent` refuses a scheduled
   event that has not happened yet and `DeletePerson` refuses while they
   are responsible for one (cancelled events included — a reinstate
   brings the assignees back) — cancel, or unassign, first. Also refused
   while the event's last fan-out is still queued or retrying: the
   `propagation` view hangs off the event and is the proof the notices
   arrived. An imported occurrence is refused outright, whatever
   `calendar_source` currently says: the sync recreates a missing row as
   *scheduled*, so deleting a cancelled one would undo the cancellation
   the import itself would never undo (invariant 9) — the row is the
   tombstone. Both take the rows that only existed for the subject and
   leave the outbox alone (an undelivered notice still has to go out);
   `Response` rows stay too, person id and all — who attested is a fact
   about the EVENT. Guarded by
   `TestDeleteEventRefusesALiveOne`,
   `TestDeleteEventClearsItsOwnRowsButKeepsTheProof`,
   `TestDeletePersonRefusesWhileStillResponsible`,
   `TestDeleteRefusesWhileTheFanOutIsStillOnItsWay`,
   `TestDeletePersonRefusesForACancelledFutureEvent` and
   `TestDeleteRefusesWhatTheImportWouldRecreate`.
4. **stdlib only.** Any new dependency needs a stated justification.
5. **Tokens are random, DB-looked-up, revocable.** Never JWT, never decodable.
   The admin session cookie is one of them: a random id resolved against
   the in-memory `sessionStore`, **never the admin token itself** — that
   equivalence made every cookie leak a full API credential. Every
   mutating panel form carries the session's CSRF token, checked in
   `adminAuth`; guarded by `TestAdminCookieIsNotTheAPIToken`,
   `TestCSRFTokenIsBoundToItsSession` and
   `TestEveryPanelFormCarriesTheCSRFToken` (which fails on a new form
   that forgets `{{template "csrf" $.CSRF}}`).
6. **Secrets never in tracked files.** `config.json` is gitignored; the
   shipped blanks (`config.example.json`, `examples/`) carry placeholders
   only, and `TestShippedConfigBlanksParse` keeps them valid.
7. UI strings are English (owner decision 2026-08-12).
8. **Management routes exist only on the admin listener.** `PublicHandler`
   carries the token surface and nothing else — never register `/api/v1`
   or `/admin` there. Guarded by `TestAdminAPIAbsentFromPublic`.
9. **The calendar import never cancels.** A feed glitch must not send
   cancellation mail: occurrences that disappear from the source are
   marked (`Event.VanishedAt`, sticky until they reappear; `import.vanished`
   audit, one admin page per fetch, panel attention), the operator decides.
   The reachable assignees are asked once per disappearance
   (`askVanishedLocked`, purpose `vanished`, audit `vanished.asked`) — the
   one-shot reminder cannot carry that question after it has gone out;
   guarded by `TestVanishedAsksTheResponsibleOnce` and
   `TestSuspectFetchAsksNobody`.
   The deadline may still auto-cancel a vanished `if_unconfirmed=cancel`
   event — that is the dead-man-switch deciding, not the import.
   Time changes DO run the full move transaction (owner decision).
10. **The source feed URL is user/project data** — config on the host,
   never committed anywhere, and never baked into tests.
11. **Nothing waits on the network under `s.mu`.** A delivery pass
   collects the due outbox items locked (`collectOutboxLocked`), sends
   unlocked (`deliver`), and books the outcome locked
   (`recordDeliveries`, by item id — indices go stale). Never call
   `notify.Send` from a `…Locked` function again: one hanging peer would
   stall both listeners for its full timeout. In-flight ids sit in
   `Service.sending` (its value counts mid-flight re-arms) and, keyed by
   recipient, in `Service.sendingTo` — and the RECIPIENT is what a
   concurrent pass consults. Three callers overlap here (scheduler,
   `POST /tick`, `SendTest`), so that one map does two jobs: no item goes
   out twice, and nothing overtakes a parked message to the same address
   (a reinstatement delivered past a stuck cancellation arrives as "it is
   back on", then "it is off"). Within one pass, several messages to one
   recipient are fine — that pass sends them in order. A booked failure
   yields to the operator's `RetryOutbox` (never inferred from `Attempts`
   — a Retry on a queued item leaves that at 0), and the booking clock is
   read after the sends, so a 30s timeout neither stamps `DeliveredAt`
   early nor eats the backoff. A sender that panics becomes
   a failed attempt, not an item parked in flight forever: `net/http`
   recovers handler panics, and `SendTest`/`tick` deliver from one.
   Consequence for `channel`: `Sender.Send` is now called concurrently
   and must be safe for it — hence the one shared `sendClient`. Guarded
   by `TestDeliveryRunsOutsideTheLock`,
   `TestRetryDuringAnInFlightSendSurvives`,
   `TestAPanickingChannelIsAFailedAttempt`,
   `TestOverlappingPassesSendAnItemOnlyOnce`,
   `TestASecondPassDoesNotOvertakeAParkedRecipient`, and
   `TestNotifySendHasOneCallerOnly`, which parses this package and fails
   on a second call site whatever it is named.

## Build & test

```sh
gofmt -l . && go vet ./... && go test ./... && go build .
```

On the owner's machine the sandbox cache env (GOCACHE/GOMODCACHE/GOPROXY/GOSUMDB)
comes from the untracked `.claude/settings.local.json` — no manual exports
needed. Elsewhere (CI has no sandbox) plain `go` commands just work; if you
hit a blocked-cache error in a sandboxed shell, set:

```sh
export GOCACHE="$HOME/.cache/claudii/go-build" GOMODCACHE="$HOME/.cache/claudii/gomod" \
       GOPROXY=direct GOSUMDB=off PATH="/opt/homebrew/bin:$PATH"
```

Some tests bind real listeners (`internal/channel`, `internal/core`'s
`TestFetchCalendarEndToEnd`, root `cmd_serve`) — inside the Claude sandbox
they fail with "operation not permitted" and need a bypass; CI has no
sandbox and is fine. New fetch tests must NOT bind: use the in-process
`stubFeed` RoundTripper via `SetCalendarClient` (see `import_test.go`).

## Conventions

- The CLI is one table (`clientGroups`/`clientLeaves` in
  `cmd_client.go`): a command is a row — name, spec, help, run. The spec
  doubles as the usage line and the arity contract, so an argument a
  command does not know is an error instead of being dropped, and every
  group answers `--help`. Adding a command means adding a row, never a
  new switch. `TestClientDispatchTable` pins method, path AND body for
  each row, `TestClientUsageStrings` the wording of every usage error. A
  positional that starts with a dash needs `--` in front of it, which
  escapes exactly that one argument.
- Config precedence: explicit `serve` flags > `config.json` > `STATTII_*` env
  (the env tier exists only for the fallbacks listed in the README — most
  keys are flags/config only). Config files are JSON with full-line `//`
  comments; unknown keys fail loudly.
- Commits as `bmmmm <hi@brtsz.de>`; tag releases `vX.Y.Z`. Dual-remote
  since 2026-08-12: push branches AND tags to both `origin` (the private
  Forgejo) and `github` (public mirror, pre-push leak gate installed).
  Dependabot/CodeQL PRs on GitHub are signals only — fix locally, push to
  both remotes, never merge in the GitHub UI (`dependabot-adopt` skill).
- Time handling: state is UTC; recipient-facing formatting uses the event's
  stored time. `datetime-local` form inputs parse in server-local time.

## State & roadmap

Current phase, plan, and recorded learnings: [ROADMAP.md](ROADMAP.md).
Actionable next work lives in Forgejo issues.

**Production** (since 2026-08-12): `https://stattii.6bm.de` on garage —
Cloudflare Tunnel → Traefik → `stattii:local`, rebuilt + redeployed by
`~/servers/garage/scripts/rebuild-stattii.sh` (source must be clean and
pushed). Operator config lives on the host at `~/docker/stattii/`
(config.json + .env, never committed); state is bind-mounted there and
covered by the nightly restic sweep.

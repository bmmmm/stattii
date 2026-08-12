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

## Invariants — do not break

1. **GET never mutates.** Mail scanners prefetch links; only POST acts.
   Guarded by `TestActionPageFlow` — keep that test meaningful.
2. **Every outbound message goes through the outbox.** No direct sends —
   retries, delivery proof, and escalation depend on it.
3. **Cancel / move / reinstate are propagation transactions** (`fanOutLocked`):
   a status flip without outward fan-out recreates the locked-door bug.
4. **stdlib only.** Any new dependency needs a stated justification.
5. **Tokens are random, DB-looked-up, revocable.** Never JWT, never decodable.
6. **Secrets never in tracked files.** `config.json` is gitignored; the
   shipped blanks (`config.example.json`, `examples/`) carry placeholders
   only, and `TestShippedConfigBlanksParse` keeps them valid.
7. UI strings are English (owner decision 2026-08-12).
8. **Management routes exist only on the admin listener.** `PublicHandler`
   carries the token surface and nothing else — never register `/api/v1`
   or `/admin` there. Guarded by `TestAdminAPIAbsentFromPublic`.
9. **The calendar import never cancels.** A feed glitch must not send
   cancellation mail: occurrences that disappear from the source are
   reported (`import.vanished`, panel attention), the operator decides.
   Time changes DO run the full move transaction (owner decision).
10. **The source feed URL is user/project data** — config on the host,
   never committed anywhere, and never baked into tests.

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

`internal/channel` tests bind real `httptest` listeners — inside the Claude
sandbox they may need a bypass; CI has no sandbox and is fine.

## Conventions

- Config precedence: explicit `serve` flags > `config.json` > `STATTII_*` env.
  Config files are JSON with full-line `//` comments; unknown keys fail loudly.
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

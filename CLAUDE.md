# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`healthz` is a single-binary Go service that periodically checks HTTP endpoints and PostgreSQL health queries, stores state/history in SQLite, sends email alerts (down / recovered / flapping / daily digest), and serves a small authenticated JSON API. All behavior is driven by a JSON config file; README.md is the user-facing reference for every config field, and docs/deployment.md covers production setup and operations.

## Commands

```sh
make test                      # go test ./...
make build                     # go build -o healthz ./cmd/healthz
make fmt                       # gofmt -w cmd internal
make check-config              # validate config.json without starting
make run                       # go run ./cmd/healthz -config config.json

go test ./internal/monitor -run TestName -v   # single test
go vet ./...                                  # CI runs this too
```

CI (`.github/workflows/ci.yml`) runs `go test`, `go vet`, and a `CGO_ENABLED=0` build. The build must stay CGO-free — that is why SQLite uses the pure-Go `modernc.org/sqlite` driver; don't introduce cgo dependencies.

The PostgreSQL integration test in `internal/check/postgres_test.go` spins up a throwaway cluster with `initdb`/`pg_ctl` (found on `PATH` or under `/usr/lib/postgresql/*/bin`) and skips itself when they're absent.

Running locally needs either `HEALTHZ_API_TOKEN` set or a personal token in the DB, plus the SMTP password env var when SMTP is enabled with a username (see `.env.example`; load `.env` with `set -a; source .env; set +a`). `config.json`, `.env`, and `data/` are gitignored local files.

Releases: `./release.sh patch|minor|major|vX.Y.Z` from a clean `main` pushes a semver tag, which triggers `.github/workflows/release.yml` (tests, then Linux/macOS `amd64`/`arm64` binaries + `SHA256SUMS` attached to a GitHub release, version injected via `-X main.version`). This repo is the open-source project only: it contains no deployment of any particular instance, so don't add deploy workflows, server names, or host-specific scripts here.

## Architecture

Wiring happens in `cmd/healthz/main.go`: load + validate config → verify required env vars → open SQLite store → start HTTP server, monitor `Runner`, store maintenance (history pruning), and optional `DigestRunner` as goroutines sharing one signal-cancelled context. `healthz token ...` subcommands are dispatched before flag parsing (`cmd/healthz/tokens.go`) and operate directly on the same SQLite DB.

- **`internal/config`** — JSON schema, `applyDefaults`, and strict `Validate` (unknown fields and trailing data are rejected). The service refuses to start on any invalid config; there is no hot reload. New endpoint options need defaults + validation here and docs in README's endpoint table. Alert subject templates have a fixed per-type placeholder whitelist validated here.
- **`internal/check`** — `Checker` interface returning `Result{Healthy, StatusCode, Duration, Err}`. `Router` dispatches on `endpoint.Type` (`http`, `postgres`, `ping`). Adding a new check type means: a constant + validation branch in config, a checker here, and registering it in `NewRouter`. Postgres checks open a fresh connection per check, run in a READ ONLY transaction with `statement_timeout`, force `QueryExecModeExec` (PgBouncer-safe) unless the DSN overrides it, and must never surface the DSN in errors. Ping checks make no network call: `EvaluatePing` judges the pings `StatusStore` recorded (via `PingStateSource`) against the endpoint's cron `schedule` or `period` plus `grace`.
- **`internal/monitor`** — the core:
  - `monitor.go`: one goroutine per endpoint; checks for one endpoint never overlap. `processResultWithOptions` is the alert state machine (threshold, repeat interval, recovery, `recheck_before_alert` confirmation, flapping).
  - `status.go`: `StatusStore` is the single owner of SQLite — persisted endpoint state (failures, alert flags, outage start), check history, state transitions for flapping, digest checkpoints, and the HTTP handler for `/healthz`, `/status`, `/history`. Alert state is persisted so restarts don't re-send outage emails; the runner's in-memory `endpointState` is refreshed from `RecordCheck` each cycle.
  - `ping.go`: the unauthenticated `/ping/<key>[/start|/fail|/<exit code>]` handler (key from `ping_key_env`, looked up by SHA-256, never logged), ping persistence, and `PingState`. After recording a ping it calls `Runner.Wake` so the endpoint is re-evaluated immediately; `main.go` mounts it on `/ping/` beside `StatusStore.Handler`.
  - `tokens.go`: personal API tokens (SHA-256 hashed, revocable, checked per request without restart) and the auth middleware that also accepts the env bootstrap token.
  - `digest.go`: daily digest scheduling in a configured IANA timezone, retried every 15 min on failure; stats cover the span since the last *successfully delivered* digest.
- **`internal/alert`** — `Sender` interface + SMTP implementation (`starttls` / `implicit` / `none`). A message's `Recipients` overrides the global `smtp.to`; header values are sanitized against CRLF injection.

### SQLite schema migrations

`migrateSchema` in `internal/monitor/status.go` uses `PRAGMA user_version` (`schemaVersion` const). The `CREATE TABLE IF NOT EXISTS` statements always describe the **latest** schema (so fresh DBs get everything), and version-gated `ALTER TABLE` blocks upgrade older DBs. When changing the schema: update the CREATE statement, bump `schemaVersion`, and add an ALTER block for existing databases at the older versions. A DB with a newer version than the binary supports is refused.

### Testing patterns

Tests use in-memory SQLite (`NewStatusStore` / `OpenStatusStore(":memory:", ...)`) and fakes such as `recordingSender` and `sequenceChecker` in `internal/monitor/monitor_test.go`. Time-dependent store methods take an explicit `now`/`checkedAt` argument rather than calling `time.Now()` internally, which keeps tests deterministic — follow that convention.

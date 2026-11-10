# healthz

`healthz` is a small, self-contained Go service that checks websites, APIs, and PostgreSQL queries, receives pings from scheduled jobs, and sends email when they fail. Each endpoint has its own check frequency and timeout. Configuration is a JSON file, so adding a new endpoint does not require changing Go code.

## Install

Download a prebuilt binary for Linux or macOS (`amd64` or `arm64`) from the [releases page](https://github.com/runeQ/healthz/releases). Each archive contains the `healthz` binary, this README, and `config.example.json`, and `SHA256SUMS` lists their checksums. The binaries are statically linked and need nothing else installed.

Or install with Go 1.27 or later:

```sh
go install github.com/runeQ/healthz/cmd/healthz@latest
```

`healthz -version` prints the installed version.

## Quick start

Building from source requires Go 1.27 or later. Production deployments run the native binary directly, for example with systemd; see [Deployment](#deployment).

```sh
cp config.example.json config.json
export HEALTHZ_SMTP_PASSWORD='your-smtp-app-password'
export HEALTHZ_API_TOKEN="$(openssl rand -hex 32)"
go run ./cmd/healthz -config config.json
```

Validate changes without starting the monitor:

```sh
go run ./cmd/healthz -config config.json -check-config
```

The liveness endpoint is public, while status and history require the API token:

- `GET http://localhost:8080/healthz` — confirms the monitor process is running.
- `GET http://localhost:8080/status` — shows the latest result for every endpoint; requires authentication.
- `GET http://localhost:8080/history?endpoint=Main%20website&period=24h` — calculates historical availability and latency; requires authentication.
- `GET|POST http://localhost:8080/ping/<key>` — the ping address that scheduled jobs report to; the secret key in the URL is the credential. See [Job pings](#job-pings).

Send the token as a Bearer credential:

```sh
curl -H "Authorization: Bearer $HEALTHZ_API_TOKEN" \
  http://localhost:8080/status
```

By default, the server listens on `127.0.0.1:8080` and reads an optional bootstrap token from `HEALTHZ_API_TOKEN`. Configure a different environment variable name if needed:

```json
"server": {
  "listen": "127.0.0.1:8080",
  "auth_token_env": "HEALTHZ_API_TOKEN"
}
```

The bootstrap token must be at least 32 characters with no whitespace; `openssl rand -hex 32` gives 64. The service starts when either the bootstrap environment variable is non-empty or SQLite contains at least one unexpired, unrevoked personal token. Keep the loopback binding when accessing the API locally, over SSH, or through a TLS reverse proxy. Binding to `:8080` exposes the server on every interface; only do so intentionally on a trusted private network or behind suitable firewall and TLS protection, because direct HTTP sends Bearer tokens without transport encryption.

### Personal API tokens

Issue independently revocable credentials for people, scripts, and devices with the local token CLI. The name is required attribution metadata; the email and expiry are optional:

```sh
go run ./cmd/healthz token create -config config.json \
  -name "Alice laptop" \
  -email "alice@example.com" \
  -expires-in 90d
```

The plaintext token is written once to standard output and is never stored. SQLite retains its public ID, name, optional email, SHA-256 hash, creation and optional expiry times, revocation time, and last-used time. A 32-byte cryptographically random secret makes a fast hash appropriate here; user-chosen passwords are not accepted as API tokens.

Omit both expiry flags for a token that does not expire, or provide an exact UTC timestamp:

```sh
go run ./cmd/healthz token create -config config.json \
  -name "Temporary automation" \
  -expires-at 2026-12-31T23:59:59Z
```

List token metadata and revoke a token by its public ID:

```sh
go run ./cmd/healthz token list -config config.json
go run ./cmd/healthz token revoke -config config.json -id TOKEN_ID
```

Issuing and revoking tokens takes effect on the next request without restarting healthz. Expired, revoked, malformed, and unknown credentials all receive the same `401 Unauthorized` response. Successful personal-token requests update the last-used timestamp and produce a structured log entry containing the token ID, name, optional email, method, and path; secrets and Authorization headers are never logged by healthz. An email describes who received a token but cannot prove who used it if the credential was shared.

When Caddy terminates public TLS, it can pass each caller's Bearer token through unchanged:

```caddyfile
status.example.com {
  reverse_proxy 127.0.0.1:8080
}
```

Do not configure Caddy to replace `Authorization` with one shared backend token when using personalized tokens.

#### Test a personal token

Paste the token into a hidden shell prompt. The variable name contains ordinary underscores—do not escape them:

```sh
read -rsp 'API token: ' healthz_api_token
echo
```

Test the status endpoint locally on the server:

```sh
curl --fail-with-body \
  -H "Authorization: Bearer $healthz_api_token" \
  http://127.0.0.1:8080/status
```

For history, set `endpoint_name` to an exact `name` returned by `/status`. Using `--data-urlencode` safely handles spaces, parentheses, and other characters:

```sh
endpoint_name='Main website'
curl --fail-with-body --get \
  -H "Authorization: Bearer $healthz_api_token" \
  --data-urlencode "endpoint=$endpoint_name" \
  --data-urlencode 'period=24h' \
  http://127.0.0.1:8080/history
```

Test through Caddy by replacing the example hostname with the deployed hostname:

```sh
curl --fail-with-body \
  -H "Authorization: Bearer $healthz_api_token" \
  https://status.example.com/status
```

A valid active token returns `200 OK`; invalid, expired, and revoked tokens return `401 Unauthorized`. The local liveness endpoint does not require a token:

```sh
curl --fail-with-body http://127.0.0.1:8080/healthz
```

Remove the token from the shell when finished:

```sh
unset healthz_api_token
```

## Add an endpoint

Add another object to the `endpoints` array in `config.json`, validate the file, and restart the process:

```json
{
  "name": "Customer portal",
  "url": "https://portal.example.com/health",
  "method": "GET",
  "frequency": "45s",
  "timeout": "8s",
  "expected_statuses": [200],
  "body_contains": "healthy",
  "failure_threshold": 3,
  "headers": {
    "Authorization": "Bearer replace-me"
  }
}
```

`frequency` controls the base time the monitor waits after a check completes before starting the next one. Durations use Go notation: `500ms`, `30s`, `5m`, or `1h`. Checks for different endpoints run concurrently, but a slow check never overlaps the next check for the same endpoint.

Endpoint options:

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | Unique name used in logs and email subjects. |
| `type` | no | `http` (default), `postgres`, or `ping`; see [Database checks](#database-checks) and [Job pings](#job-pings). |
| `url` | http | An `http://` or `https://` URL without credentials. Query values are shown as `REDACTED` in the API, alert emails, and check errors. |
| `dsn_env` | postgres | Environment variable containing the PostgreSQL connection string. |
| `query` | postgres | One read-only SQL statement returning a health row. |
| `ping_key_env` | ping | Environment variable containing the secret key in the job's ping address. |
| `schedule` | ping | Cron expression for when the job runs, such as `30 2 * * *`. Use either this or `period`. |
| `timezone` | no | Ping only: IANA time zone for `schedule`; defaults to `UTC`. |
| `period` | ping | Expected time between runs, such as `1h`, for jobs without a fixed schedule. Use either this or `schedule`. |
| `grace` | no | Ping only: how late a ping may arrive, and how long a started run may take; defaults to `1h`. |
| `frequency` | yes | Time between checks. Ping endpoints default to `1m`. |
| `schedule_jitter` | no | Random additional delay that spreads checks over time. |
| `method` | no | HTTP only: `GET` (default) or `HEAD`. |
| `timeout` | no | Per-check timeout, including connecting; defaults to `10s`. |
| `expected_statuses` | no | HTTP only: exact accepted codes; empty means any `200`–`399` response. |
| `body_contains` | no | HTTP only: text that must occur in the first 1 MiB of a GET response. |
| `headers` | no | HTTP only: extra request headers. They follow redirects only to the same host or its subdomains, and never from HTTPS to HTTP. Avoid storing secrets in a committed config file. |
| `failure_threshold` | no | Consecutive failures before alerting; overrides the global value. Ping endpoints default to `1`. |
| `recheck_before_alert` | no | Immediately confirms the failure that would otherwise declare a new outage. |
| `flapping` | no | Detects rapid healthy/unhealthy transitions within a time window. |
| `alert_to` | no | One or more recipients for this endpoint; overrides the global SMTP list. |
| `alert_subjects` | no | Custom subject templates for outage, recovery, and flapping messages. |

Restart is deliberate: the service validates the entire configuration on startup and never runs with a partially applied configuration.

### Database checks

A `postgres` endpoint runs a query and alerts when it reports a problem. Use it for conditions that only the data can show, such as a background worker whose heartbeat row has stopped updating. healthz connects outbound, so the monitored servers need no open port:

```json
{
  "name": "Background workers",
  "type": "postgres",
  "dsn_env": "HEALTHZ_APP_DSN",
  "query": "SELECT count(*) > 0 AND bool_and(updated_at > now() - interval '3 minutes'), string_agg(name || ' last seen ' || date_trunc('second', now() - updated_at) || ' ago', ', ') FILTER (WHERE updated_at <= now() - interval '3 minutes') FROM app.worker_heartbeats",
  "frequency": "1m",
  "timeout": "10s"
}
```

The query must return exactly one row:

| Column | Type | Meaning |
| --- | --- | --- |
| 1 | `boolean` | `true` when healthy. `false` or `NULL` is a failed check. |
| 2 (optional) | `text` | Explanation used as the failure reason in logs, history, and outage email, e.g. `worker:office last seen 00:07:12 ago`. Truncated to 500 bytes. |

Zero rows, several rows, other column types, connection failures, and SQL errors are also failed checks. Write the query so that missing data is unhealthy; for example, `count(*) > 0 AND bool_and(...)` fails when no heartbeat rows exist, where `bool_and` alone would return `NULL` without an explanation.

Every check opens a new connection, runs the query in a `READ ONLY` transaction with `statement_timeout` set to the endpoint timeout, rolls back, and disconnects. A check therefore cannot write data, and a long query is cancelled by the server as well as by healthz. Exactly one statement is accepted. Queries use the extended protocol without cached prepared statements (`default_query_exec_mode=exec`), which works through PgBouncer transaction pooling; an explicit `default_query_exec_mode` in the DSN is respected.

Keep credentials out of `config.json`: put the DSN in the variable named by `dsn_env`, such as in `/etc/healthz/healthz.env`, and use a dedicated login that can only `SELECT` the tables the query reads. healthz refuses to start when a configured DSN variable is missing or empty, and never logs the DSN. Status, history, digests, and email identify the target as `postgres:$VARIABLE`.

A new connection can occasionally be slow to establish. Keep the default `failure_threshold` of 2 or enable `recheck_before_alert` so one slow connection does not send an outage email.

### Job pings

A `ping` endpoint watches a scheduled job, such as a nightly backup, that cannot be checked from outside. Instead of healthz polling the job, the job calls its ping address when it starts, when it finishes, and when it fails. healthz alerts when a ping says the run failed, when a started run doesn't finish in time, or when an expected ping doesn't arrive, which also catches a job that never ran. It works like healthchecks.io's ping URLs, without an external service:

```json
{
  "name": "Nightly database backup",
  "type": "ping",
  "ping_key_env": "HEALTHZ_BACKUP_PING_KEY",
  "schedule": "30 2 * * *",
  "timezone": "UTC",
  "grace": "3h",
  "alert_to": ["operations@example.com"]
}
```

Choose a `grace` comfortably longer than the job's longest normal run, so a slow night doesn't cause a false alarm; for a backup that takes about half an hour, 3 hours is generous. For a job without a fixed schedule, replace `schedule` and `timezone` with `period`, for example `"period": "6h"`: a ping is then expected within `period` plus `grace` of the previous one.

**1. Create the key.** The ping address is `/ping/<key>`, and anyone who knows the key can send pings, so treat it as a secret. Generate one and add it to the healthz environment file (`/etc/healthz/healthz.env` in production), then restart healthz:

```sh
openssl rand -hex 16
```

```text
HEALTHZ_BACKUP_PING_KEY=<output of openssl rand -hex 16>
```

Keys must be 32 to 128 letters, digits, `-`, or `_`, and each ping endpoint needs its own key. healthz refuses to start when a key is missing or invalid and never logs keys. Status, history, and email identify the target as `ping:$VARIABLE`.

**2. Give the job its ping address.** Put the full URL in the job's own environment file, such as `/etc/db-backup.env`, and keep it out of git:

```bash
HEALTHCHECK_URL=https://status.example.com/ping/<key>
```

Don't add a trailing `/`. The job appends the event to the URL:

| Request | Meaning |
| --- | --- |
| `/ping/<key>` | The run succeeded. |
| `/ping/<key>/start` | The run started. |
| `/ping/<key>/fail` | The run failed. |
| `/ping/<key>/<exit status>` | `0` succeeded; `1`–`255` failed. Lets a script report `curl -fsS "$HEALTHCHECK_URL/$?"`. |

`GET`, `HEAD`, and `POST` are all accepted, a recorded ping returns `200 OK` with the body `OK`, and an unknown key returns `404`. Rejected pings are logged at most once a minute (without the key), with a count of those suppressed in between, so anonymous requests cannot flood the log. This is the same URL format as healthchecks.io, so a script written for healthchecks.io works unchanged with the healthz URL.

**3. Make the address reachable.** healthz listens on `127.0.0.1:8080` by default, which is enough when the job runs on the healthz host itself (use `http://127.0.0.1:8080/ping/<key>`). For jobs on other servers, publish only the ping path through the TLS reverse proxy, so the key is encrypted in transit:

```caddyfile
status.example.com {
  handle /ping/* {
    reverse_proxy 127.0.0.1:8080
  }
}
```

**4. Test it.**

```bash
curl -fsS https://status.example.com/ping/<key>/fail   # prints OK and sends a [DOWN] email
curl -fsS https://status.example.com/ping/<key>        # marks the job healthy and sends [RECOVERED]
```

Check that the alert actually reached its recipients. After that, the next run reports on its own.

How a ping endpoint decides its state, re-evaluated every `frequency` and immediately after each ping:

- A failure ping keeps the endpoint unhealthy until the next success ping.
- After a start ping, the run has `grace` to report success or failure; past that, the endpoint is unhealthy.
- Otherwise the next ping is due at the first scheduled time after the last success ping (or `last + period`), and is missing once `grace` has also passed. A new ping endpoint expects its first ping from the time it was first configured, so a job that never runs is caught too.

Because a single missed or failed run is already conclusive, ping endpoints alert on the first failed evaluation unless `failure_threshold` is set. `repeat_interval`, `send_recovery`, `alert_to`, `alert_subjects`, and `flapping` behave as for other endpoints; the recovery subject's `{status}` is `0`. `/status` includes `last_ping_at`, `last_ping_start_at`, and `last_ping_fail_at`. Pings are stored in SQLite, so restarts lose nothing, but a ping sent while healthz itself is down is lost and can show up as a missing ping. Renaming the `ping_key_env` variable resets the endpoint's ping history.

## Alert behavior

The global `alerts.failure_threshold` prevents one brief network error from paging you. With a value of `2`, email is sent after the second consecutive failure. Only one email is sent for an outage unless `repeat_interval` is non-zero. A successful check resets the failure count and, when `send_recovery` is true, sends a recovery message.

If SMTP delivery fails, the monitor logs the error and retries on the next endpoint check. Set `smtp.enabled` to `false` for local testing; checks still run and alerts appear in logs, but no mail is delivered.

SMTP TLS modes are:

- `starttls` — normal choice for port 587 and the default.
- `implicit` — normal choice for port 465.
- `none` — only for a trusted local relay; credentials would otherwise travel unencrypted.

The SMTP password is read from the environment variable named by `smtp.password_env`, keeping it out of `config.json`. The service refuses to start if that variable is missing. Many mail providers require an app password rather than the account password.

### Per-endpoint recipients

The global `smtp.to` array accepts one or more recipients and is used by default. Add `alert_to` to route one endpoint's outage reminders and recovery messages to a different group:

```json
{
  "name": "Customer portal",
  "url": "https://portal.example.com/health",
  "frequency": "1m",
  "alert_to": [
    "portal-team@example.com",
    "operations@example.com"
  ]
}
```

A non-empty `alert_to` list replaces the global list for that endpoint; it is not merged automatically. Omit it or use an empty array to fall back to `smtp.to`. Recipient addresses must be valid and unique within an endpoint. Daily status digests always use the global `smtp.to` recipients.

### Custom alert subjects

Customize subjects independently for each endpoint and alert type:

```json
"alert_subjects": {
  "down": "[CRITICAL] {name} failed {failures} checks",
  "recovered": "[RESOLVED] {name} returned HTTP {status}",
  "flapping": "[UNSTABLE] {name}: {transitions} changes in {window}"
}
```

Available placeholders are deliberately limited:

| Subject | Placeholders |
| --- | --- |
| `down` | `{name}`, `{failures}` |
| `recovered` | `{name}`, `{status}` |
| `flapping` | `{name}`, `{transitions}`, `{window}` |

Each field is optional and falls back to the built-in subject when omitted. Templates are validated during startup: unknown or mismatched placeholders, unmatched braces, newlines, and subjects longer than 200 bytes are rejected. Daily digest subjects are not affected by endpoint templates.

### Important endpoint protections

Important endpoints can confirm an outage, stagger their schedule, and detect rapid state changes:

```json
{
  "name": "Checkout API",
  "url": "https://api.example.com/checkout/health",
  "frequency": "30s",
  "schedule_jitter": "5s",
  "recheck_before_alert": true,
  "flapping": {
    "enabled": true,
    "transitions": 4,
    "window": "10m"
  }
}
```

When `recheck_before_alert` is enabled, the check that reaches the failure threshold is recorded and then repeated immediately. The outage email is sent only if that confirmation also fails. A successful confirmation resets the consecutive failure count. Existing-outage reminder emails do not trigger another confirmation.

`schedule_jitter` adds a random delay from zero up to the configured value before the initial check and after each normal `frequency` interval. This spreads endpoints and multiple monitor instances over time without allowing checks to run more frequently than configured. Jitter cannot be negative or exceed the endpoint frequency.

Flapping counts changes between consecutive healthy and unhealthy results. Once the configured number of transitions occurs within `window`, healthz sends one `[FLAPPING]` email using the endpoint's recipient routing and exposes `flapping: true` through `/status`. Failed email delivery is retried on later checks. The state and transition history survive restarts; flapping clears automatically once the transition count falls below the threshold. Enabling flapping without explicit values defaults to four transitions in ten minutes.

## Daily status digest

Enable a daily email so recipients can see the monitor is running:

```json
"status_digest": {
  "enabled": true,
  "send_at": "08:00",
  "timezone": "Europe/Copenhagen"
}
```

`send_at` uses 24-hour `HH:MM` notation in the configured IANA timezone. Daylight-saving changes are handled automatically. The email contains each endpoint's current state, passed and failed check counts since the previous successfully delivered digest, check availability, last check and latency, and monitor uptime.

If delivery fails, the digest is retried every 15 minutes and its statistics are retained. The first digest covers the period since the database was created; later reports cover the period since the last successful digest. Digest checkpoints and statistics survive service restarts.

A missing digest is a useful warning, but it still depends on someone noticing it. For automatic detection when the entire host or network is down, use a separate external heartbeat or dead-man's-switch service as well.

## Persistent state and history

SQLite stores the latest endpoint state, active outage start, consecutive failure count, last alert and recovery, flapping transitions, digest checkpoint, and individual check results with latency:

```json
"storage": {
  "path": "data/healthz.db",
  "history_retention_days": 30
}
```

This prevents routine restarts from producing duplicate outage emails. If an endpoint was already down and alerted before a restart, monitoring resumes with the existing failure count and alert state. A recovery after restart still produces the expected recovery email.

Database schema upgrades run automatically on startup. Existing databases are migrated in place to add flapping state, the personal API-token table, and job-ping state as needed.

Check history is retained for the configured number of days and pruned daily. The history API returns counts, availability, and average/minimum/maximum latency for an endpoint and period:

```sh
endpoint_name='Main website'
curl --fail-with-body --get \
  -H "Authorization: Bearer $HEALTHZ_API_TOKEN" \
  --data-urlencode "endpoint=$endpoint_name" \
  --data-urlencode 'period=168h' \
  http://localhost:8080/history
```

The database is created with owner-only permissions. Back it up with a SQLite-aware backup tool, or stop the service before copying the database files. As with any SMTP integration, there is a very small crash window after an SMTP server accepts a message but before SQLite records that delivery; routine restarts are deduplicated, but exactly-once email delivery cannot be guaranteed across those two independent systems.

### Why the CGO-free SQLite driver?

The service uses [`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite), a pure-Go SQLite driver. This allows `CGO_ENABLED=0` builds that produce a self-contained executable with no runtime dependency on SQLite, a C compiler, libc development packages, or shared SQLite libraries. It also keeps GitHub Actions cross-compilation for Linux `amd64` and `arm64` straightforward. The database file remains a standard SQLite database that ordinary SQLite tools can inspect.

The tradeoff is a larger Go dependency graph, longer compilation, and potentially different performance from the commonly used CGO-based `github.com/mattn/go-sqlite3` driver. Healthz has a small, low-volume database workload, so simple static builds and reliable deployment are more valuable here than maximizing SQLite throughput.

## Deployment

[docs/deployment.md](docs/deployment.md) describes one production setup: a release binary run by a hardened systemd service on Ubuntu, upgrading to new releases with rollback, updating the configuration, routine operations, and inspecting the database with `sqlite3`. Adapt it to your own environment; healthz itself only needs the binary, a configuration file, its environment variables, and a writable directory for the database.

## Build and test

```sh
make test
make build
./healthz -config config.json
```

## Contributing, security, and license

Bug reports and pull requests are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md). Please report security vulnerabilities privately as described in [SECURITY.md](SECURITY.md), not in public issues.

healthz is released under the [MIT License](LICENSE).

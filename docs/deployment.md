# Deployment

This guide describes one production setup for healthz: the native binary from a GitHub release, run by a hardened systemd service on Ubuntu. Adapt the paths, accounts, and network rules to your own environment. How new releases reach your server (by hand, configuration management, or your own CI) is up to you; [Upgrade to a new release](#upgrade-to-a-new-release) shows the steps any such process needs.

## Install as a systemd service

The supplied [systemd unit](../deploy/healthz.service) runs the native healthz binary directly as a dedicated unprivileged user, restarts it after failures, waits for networking, protects the host filesystem, and only permits writes beneath `/var/lib/healthz`.

The production artifact is a self-contained native binary; Docker is not required, and the server does not need Go, a C compiler, or SQLite libraries installed. Use a prebuilt release, or build it yourself with Go 1.27 or later.

The native installation uses these locations:

| Purpose | Location |
| --- | --- |
| Binary | `/usr/local/bin/healthz` |
| Configuration | `/etc/healthz/config.json` |
| Secret environment | `/etc/healthz/healthz.env` |
| SQLite database | `/var/lib/healthz/healthz.db` |
| systemd unit | `/etc/systemd/system/healthz.service` |

### Create the service account

`healthz` runs the application. It is a system account with no interactive shell, password, or SSH access. Log in with an existing Ubuntu administrator account and run:

```sh
sudo useradd --system --user-group --no-create-home \
  --home-dir /var/lib/healthz --shell /usr/sbin/nologin healthz
sudo install -d -o healthz -g healthz -m 0700 /var/lib/healthz
```

Do not add the account to the `sudo` group. Confirm the account and permissions:

```sh
id healthz
sudo -u healthz test -w /var/lib/healthz
```

The final command produces no output when the state directory is writable by the service account.

### Install the service

Prepare `config.json` on a trusted workstation. Keep the API bound to loopback unless it will be protected by a trusted private network or an authenticated TLS reverse proxy, and use an absolute database path under the directory writable by the systemd service:

```json
"server": {
  "listen": "127.0.0.1:8080",
  "auth_token_env": "HEALTHZ_API_TOKEN"
},
"storage": {
  "path": "/var/lib/healthz/healthz.db",
  "history_retention_days": 30
}
```

Validate the configuration before uploading it:

```sh
go run ./cmd/healthz -config config.json -check-config
```

Check the server architecture from the workstation:

```sh
ssh your-admin@your-server uname -m
```

Use `amd64` for `x86_64` and `arm64` for `aarch64`. Download that release from the [releases page](https://github.com/runeQ/healthz/releases) and verify it against `SHA256SUMS` before installing it:

```sh
version=v0.4.0
arch=amd64
name="healthz_${version}_linux_${arch}"
base="https://github.com/runeQ/healthz/releases/download/$version"
curl -fsSLO "$base/$name.tar.gz"
curl -fsSLO "$base/SHA256SUMS"
grep -F "./$name.tar.gz" SHA256SUMS | sha256sum --check --strict -
tar -xzf "$name.tar.gz"
cp "$name/healthz" healthz
```

To build it yourself instead:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o healthz ./cmd/healthz
```

Upload the binary and provisioning files using the existing Ubuntu administrator account:

```sh
scp healthz config.json deploy/healthz.service your-admin@your-server:/tmp/
```

Log in to the server as that administrator and install the uploaded files:

```sh
sudo install -d -o root -g healthz -m 0750 /etc/healthz
sudo install -o root -g root -m 0755 /tmp/healthz /usr/local/bin/healthz
sudo install -o root -g root -m 0644 /tmp/healthz.service \
  /etc/systemd/system/healthz.service
sudo install -o root -g healthz -m 0640 /tmp/config.json /etc/healthz/config.json
sudoedit /etc/healthz/healthz.env
sudo chown root:healthz /etc/healthz/healthz.env
sudo chmod 0640 /etc/healthz/healthz.env
```

The environment file uses systemd's `KEY=value` format:

```text
HEALTHZ_SMTP_PASSWORD=replace-with-an-app-password
HEALTHZ_API_TOKEN=replace-with-a-long-random-token
```

Generate the bootstrap API token with `openssl rand -hex 32`; healthz refuses to start with a bootstrap token shorter than 32 characters. The environment file is readable only by root and the `healthz` group; do not store either secret in `config.json`. Once at least one personal token exists, `HEALTHZ_API_TOKEN` can be removed and the service restarted; personal-token creation, expiry, and revocation do not require a restart.

Validate that the installed configuration is readable by the service account, then start the service:

```sh
sudo -u healthz /usr/local/bin/healthz \
  -config /etc/healthz/config.json -check-config

sudo systemctl daemon-reload
sudo systemctl enable --now healthz.service
systemctl status healthz.service --no-pager
journalctl -u healthz.service -n 100 --no-pager
curl --fail http://127.0.0.1:8080/healthz
```

To verify the authenticated status endpoint without putting the token in shell history, enter the same token stored in `healthz.env` when prompted:

```sh
read -rsp 'API token: ' healthz_api_token; echo
curl --fail -H "Authorization: Bearer $healthz_api_token" \
  http://127.0.0.1:8080/status
unset healthz_api_token
```

Follow subsequent service logs with `journalctl -u healthz.service -f`.

Manage personal tokens on the running server as the service account so the SQLite file retains the correct ownership. The service may remain running while these commands execute:

```sh
sudo -u healthz /usr/local/bin/healthz token create \
  -config /etc/healthz/config.json \
  -name "Deployment script" \
  -email "operations@example.com" \
  -expires-in 90d

sudo -u healthz /usr/local/bin/healthz token list \
  -config /etc/healthz/config.json

sudo -u healthz /usr/local/bin/healthz token revoke \
  -config /etc/healthz/config.json -id TOKEN_ID
```

## Upgrade to a new release

Upgrades replace only the binary; configuration, credentials, and the SQLite database stay as they are, and schema changes are migrated automatically on startup. Download and verify the new release as described above, upload the binary, then install it atomically and keep the previous one for rollback:

```sh
scp healthz your-admin@your-server:/tmp/healthz.new

ssh your-admin@your-server
sudo cp -p /usr/local/bin/healthz /usr/local/bin/healthz.previous
sudo install -o root -g root -m 0755 /tmp/healthz.new /usr/local/bin/healthz.next
sudo mv -f /usr/local/bin/healthz.next /usr/local/bin/healthz
rm /tmp/healthz.new
sudo systemctl restart healthz.service
systemctl is-active healthz.service && /usr/local/bin/healthz -version
```

If the service does not stay active, restore the previous binary:

```sh
sudo mv -f /usr/local/bin/healthz.previous /usr/local/bin/healthz
sudo systemctl restart healthz.service
```

A database migrated by a newer version is refused by an older binary, so back up `/var/lib/healthz/healthz.db` before upgrading across a release that changes the schema. When `deploy/healthz.service` changes, reinstall it and run `sudo systemctl daemon-reload` before restarting.

If you automate upgrades over SSH, give the automation account a single sudo rule for a fixed, argument-free install script rather than general sudo access, and have that script read the uploaded file with the automation account's own permissions, not root's.

### Update the production configuration

Upload an updated `config.json` to `/tmp`, install it with the correct ownership and permissions, validate it as the service user, and restart healthz so it reads the new configuration:

```sh
go run ./cmd/healthz -config config.json -check-config

scp config.json your-admin@your-server:/tmp/config.json

ssh your-admin@your-server
sudo install -o root -g healthz -m 0640 \
  /tmp/config.json /etc/healthz/config.json

sudo -u healthz /usr/local/bin/healthz \
  -config /etc/healthz/config.json -check-config

sudo systemctl restart healthz.service
systemctl status healthz.service --no-pager
```

Uploading directly to `/etc/healthz/config.json` generally does not work because the file is root-owned. Confirm that the local file contains the complete desired production configuration before installing it: this replaces the server's entire configuration, not only the changed endpoint.

### Production operations

- Permit the server's required outbound HTTPS traffic to monitored endpoints and outbound SMTP traffic to the configured mail server.
- Back up `/var/lib/healthz/healthz.db` with a SQLite-aware backup tool, or stop the service before copying the database files.
- Keep the API on loopback unless access is intentionally protected. No inbound application port is required when it is accessed locally or over SSH.
- Use a separate external heartbeat or dead-man's-switch service to detect failure of the entire healthz host or its network, since healthz cannot send an alert while its own host is unavailable.

### Inspect the database with sqlite3

Healthz does not need SQLite installed (see [Why the CGO-free SQLite driver?](../README.md#why-the-cgo-free-sqlite-driver)). The `sqlite3` command-line tool is optional and only useful for debugging, for example to see why an endpoint keeps failing. Install it on the server with:

```sh
sudo apt install sqlite3
```

The database files are owned by `healthz` with owner-only permissions, so run the tool as that user. Always pass `-readonly` so an inspection can never modify the database while the service is running:

```sh
sudo -u healthz sqlite3 -readonly -header -column /var/lib/healthz/healthz.db
```

That opens an interactive prompt; type queries ending in `;` and leave with `.quit`. Timestamps are stored as Unix nanoseconds, so the queries below convert them to UTC with `datetime(x / 1000000000, 'unixepoch')`. To run a query without the prompt, pass it on standard input:

```sh
sudo -u healthz sqlite3 -readonly -header -column /var/lib/healthz/healthz.db <<'SQL'
SELECT name, healthy, consecutive_failures AS failures, alert_active AS alerted,
       datetime(outage_started_ns / 1000000000, 'unixepoch') AS outage_started_utc,
       substr(last_error, 1, 60) AS last_error
FROM endpoints ORDER BY healthy, name;
SQL
```

Useful queries:

```sql
-- Latest failed checks for one endpoint, with the failure reason.
SELECT datetime(checked_at_ns / 1000000000, 'unixepoch') AS checked_utc,
       latency_ns / 1000000 AS latency_ms, substr(error, 1, 100) AS error
FROM checks
WHERE endpoint_name = 'Main website' AND healthy = 0
ORDER BY checked_at_ns DESC LIMIT 30;

-- Failed checks per endpoint per day over the last week.
SELECT endpoint_name, date(checked_at_ns / 1000000000, 'unixepoch') AS day, count(*) AS failed
FROM checks
WHERE healthy = 0 AND checked_at_ns > (unixepoch() - 7 * 86400) * 1000000000
GROUP BY endpoint_name, day ORDER BY day DESC, failed DESC;

-- Most common failure reasons.
SELECT count(*) AS failed, endpoint_name, substr(error, 1, 100) AS error
FROM checks WHERE healthy = 0
GROUP BY endpoint_name, error ORDER BY failed DESC LIMIT 20;

-- Recent healthy/unhealthy state changes (what flapping detection counts).
SELECT endpoint_name, datetime(changed_at_ns / 1000000000, 'unixepoch') AS changed_utc, healthy
FROM state_transitions ORDER BY changed_at_ns DESC LIMIT 30;
```

The tables are `endpoints` (current state per endpoint), `checks` (every check result, kept for `history_retention_days`), `state_transitions`, `metadata`, and `api_tokens`; `.schema` prints their full definitions. Failure reasons are also in the service log, which needs no extra tools: `journalctl -u healthz | grep "check failed"`.

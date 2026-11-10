# Security policy

## Reporting a vulnerability

Please don't report security problems in public issues or pull requests. Report them privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability**. Include what an attacker can do, the affected version (`healthz -version`), and steps or a configuration that reproduce it.

You'll get an acknowledgement within a few days. Once a fix is released, the advisory is published with credit to the reporter unless you prefer to stay anonymous.

## Supported versions

Security fixes are made for the latest release only. Upgrading is a binary swap: configuration and the SQLite database carry over, and schema migrations run automatically on startup.

## Scope

Reports are especially welcome for:

- bypassing API authentication on `/status` or `/history`, or recovering API tokens or ping keys;
- sending, suppressing, or altering alerts without valid credentials, including header injection into email;
- revealing secrets read from the environment, such as SMTP passwords or PostgreSQL connection strings, in logs, status output, or email;
- writing to a monitored PostgreSQL database through a `postgres` check.

The ping address `/ping/<key>` is unauthenticated by design: the secret key in the URL is the credential, as with healthchecks.io. Anyone who knows a key can report pings for that endpoint, so keep keys out of version control. The `/healthz` liveness endpoint is intentionally public.

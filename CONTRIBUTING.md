# Contributing

Bug reports, feature requests, and pull requests are welcome. For anything larger than a small fix, please open an issue first to agree on the approach.

## Development

Requires Go 1.27 or later.

```sh
make test        # go test ./...
go vet ./...
make fmt         # gofmt
make build
```

CI runs `go test`, `go vet`, and a `CGO_ENABLED=0` build on every pull request. Keep the build CGO-free: healthz ships as one static binary, which is why it uses the pure-Go `modernc.org/sqlite` driver.

The PostgreSQL integration test starts a throwaway cluster with `initdb` and `pg_ctl` when they're on `PATH` or under `/usr/lib/postgresql/*/bin`, and skips itself otherwise.

## Guidelines

- Every behavior is driven by the JSON configuration. A new endpoint option needs a default and validation in `internal/config`, and a row in the README's endpoint table.
- Store methods that depend on time take an explicit time argument instead of calling `time.Now()`, so tests stay deterministic.
- Schema changes update the `CREATE TABLE` statements, bump `schemaVersion`, and add an `ALTER TABLE` migration for existing databases (see `migrateSchema` in `internal/monitor/status.go`).
- Never log or return secrets such as tokens, ping keys, passwords, or connection strings, including in error messages.
- Add tests for new behavior and update the README when user-facing behavior changes.

## Releases

Maintainers release from a clean `main` with `./release.sh patch|minor|major|vX.Y.Z`. The pushed tag builds Linux and macOS binaries and publishes a GitHub release.

By contributing, you agree that your contributions are licensed under the [MIT License](LICENSE).

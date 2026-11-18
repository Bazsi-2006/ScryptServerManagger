# Development

## Building and running

```bash
make build
```

```bash
./dist/server-manager init --config ./.dev/config.json --data-dir ./.dev/data --development
```

```bash
./dist/server-manager admin create --config ./.dev/config.json --username admin
```

```bash
./dist/server-manager serve --config ./.dev/config.json
```

The frontend in `web/static/` is plain ES modules and CSS with no build step;
it is embedded with `go:embed`, so rebuild the binary to see changes.

## Architecture in one page

```text
HTTP request ──► api (middleware: headers, CORS/CSRF, auth, rate limit)
                  │  authorization: perm.Mask per (user, server)
                  ▼
               manager ── one `server` per definition:
                  │        state machine · single operation slot · monitor loop
                  │        console.Buffer (ring + subscribers)
                  ▼
               backend.Backend  (custom | tmux | systemd | docker)
                  Probe / Start / Stop / Kill / Restart / Snapshot / Follow / SendInput

manager ──► events.Hub ──► SSE subscribers (filtered per subscriber)
api/manager ──► audit.Recorder ──► SQLite
```

- **`backend`** knows how to control one server and report its *native*
  liveness (`Probe`). It knows nothing about users or readiness policy.
- **`manager`** owns policy: the state machine, readiness, timeouts, the
  per-server operation slot (the lock that prevents conflicting operations),
  background monitoring and state reconciliation.
- **`events.Hub`** attaches an audience to every event and decides per
  subscriber whether to deliver it; subscriber identities are reloaded
  whenever authorization data changes.
- **`api`** enforces authentication and authorization for every route and
  maps typed errors to stable error codes.

### Adding an execution backend

1. Add its configuration struct to `internal/servers/definition.go`, with
   normalisation and validation (validate every identifier that will reach a
   command line).
2. Implement `backend.Backend` in `internal/backend/<name>.go` and register it
   in `backend.New`. Return `ErrUnsupported` for actions that do not exist and
   `ErrUnavailable` when the tooling cannot be reached; make `Probe` return
   `LifeUnknown` with a useful detail rather than guessing.
3. Add its fields to the form in `web/static/js/pages/serverform.js` and to
   `openapi.yaml`.

Nothing in `manager` or `api` needs to change.

### Database migrations

Add `internal/db/migrations/NNNN_description.sql` with the next number. Files
are embedded, applied in order at startup, each in a transaction, and recorded
in `schema_migrations`. Never edit a migration that has been released.

## Tests

```bash
make test
```

```bash
make test-race
```

| Area | Where | Notes |
|---|---|---|
| Permission bitmask and implications | `internal/perm` | |
| Password hashing, JWT, tokens | `internal/auth` | |
| Rate limiting, cooldowns | `internal/ratelimit` | |
| Configuration validation, secrets | `internal/config` | |
| Migrations, token rotation, invitations, audit queries | `internal/db` | real SQLite files |
| Console buffer, sanitizer, tailer | `internal/console` | |
| Definition validation, store, shipped examples | `internal/servers` | |
| Backup path security | `internal/backup` | traversal and symlink escapes |
| Event authorization, replay | `internal/events` | |
| Command argument handling, environment scrubbing, readiness checks | `internal/backend` | |
| Custom backend | `internal/backend` | starts real shell scripts: stdin, PTY, signals, re-adoption |
| Lifecycle, concurrency, timeouts, restart recovery | `internal/manager` | real processes |
| Full API: auth flow, invitations, per-server authorization, SSE, backups, settings | `internal/api` | complete application over HTTP |
| Embedded frontend serving, CLI bootstrap | `internal/app`, `cmd/server-manager` | |

No game server is needed anywhere: tests use small shell scripts as servers.

### Requirements of the integration tests

| Backend | What the tests use | Requirement |
|---|---|---|
| custom | real processes (`/bin/sh` scripts) | a POSIX shell; `/dev/ptmx` for the PTY test (skipped if unavailable) |
| tmux | a **real tmux server** on a private socket (`tmux -L smtest…`), cleaned up afterwards | the `tmux` binary; tests are skipped when it is missing |
| docker | an in-process fake of the Docker Engine API on a Unix socket | none (no Docker daemon needed) |
| systemd | stand-in `systemctl` / `journalctl` / `sudo` scripts that mimic the real output and record their invocations | none (no systemd needed) |

To exercise the Docker and systemd backends against the real thing, run the
application on a suitable host and point a server definition at an actual
container or unit:

- **Docker**: a host with a running daemon and a throwaway container, e.g.
  `docker create --name sm-test alpine sh -c 'echo ready; sleep 3600'`, with
  the service user in the `docker` group.
- **systemd**: a host booted with systemd and a unit the test user may
  control — a user unit is easiest (`systemctl --user`, definition with
  `"scope": "user"`), which needs no extra privileges.

## Conventions

- `gofmt`, `go vet` clean (`make check`).
- Errors shown to users are written as messages for users; internal errors
  are logged and replaced by a generic message.
- Nothing secret in logs, audit details or API responses.
- Commands are argument arrays. If you find yourself building a shell string,
  stop.

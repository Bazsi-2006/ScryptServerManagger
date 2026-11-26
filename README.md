# Server Manager

A self-hosted web application for managing game servers — or almost any other
service — on a Linux host, through one interface.

Server Manager does not need to understand what it manages. You tell it how a
server is started, stopped and observed (a script, a tmux session, a systemd
unit or a Docker container), and it gives you a multi-user web UI with
status monitoring, a live console, backups, per-server permissions and an
audit trail.

It ships as **one statically linked executable** with the web interface
embedded. No language runtime, no Node.js, no external database.

## Features

- **Four execution backends**
  - *Custom commands*: any executable or script, with separate start and stop
    commands; foreground processes and "start script launches it and exits"
    servers are both supported.
  - *tmux*: create/attach sessions, live output via `pipe-pane`, commands typed
    into the pane.
  - *systemd*: system or user units; state from systemd, output from journald.
  - *Docker*: existing containers through the Engine API; state, logs and
    optional `exec` helpers.
- **Lifecycle that tells STARTING from RUNNING**, with three readiness methods
  (console output match, TCP/UDP port probe, status script), operation
  locking, timeouts, and state reconciliation after restarts. Monitoring runs
  in the background whether or not anyone has the UI open.
- **Live console** over Server-Sent Events with resume-without-duplicates,
  plus console input where the backend has a safe channel for it.
- **Closed multi-user system**: invitation-only registration, Argon2id
  passwords, short-lived JWT access tokens, rotating refresh tokens in
  `HttpOnly` cookies with server-side sessions and reuse detection.
- **Per-server permission bitmask** (START, STOP, CONSOLE_READ, CONSOLE_WRITE,
  CONFIG, BACKUP_CREATE, BACKUP_DOWNLOAD) with implied permissions, plus
  per-action grants, enforced on every API route and on every event.
- **Backups**: run a configured backup command, or type a backup command
  such as `/backup start` into a tmux server's console (reported as
  *submitted* unless a completion message confirms it); list and download
  files with traversal- and symlink-safe access. Starting backups and
  downloading them are separate permissions.
- **Custom actions**: administrator-defined buttons per server ("Save World",
  "Update Server", …) that run a fixed script or type a fixed tmux command,
  granted to users one by one — without giving them console access.
- **Concurrency limits**: a global maximum of running servers plus a limit
  per category, enforced atomically, with live usage on the dashboard.
- **Email notifications** (opt-in, verified addresses only) when servers
  become ready, stop, crash or fail to start/stop, and **email password
  reset** for regular users (never for administrators).
- **Real-time UI**: status, permission and configuration changes reach every
  connected browser without reloads.
- **Host monitoring** (CPU, memory) from `/proc`, refreshed every 5 seconds.
- **Audit log** in SQLite with a filterable viewer; structured application
  logs kept separate.
- **Security hardening**: no shell is ever involved in running your commands,
  strict CSP, CSRF protection, rate limits and per-server cooldowns, secrets
  kept out of configuration files, logs and API responses.

## Quick start

Requirements: Linux (x86_64 or ARM64) and Go 1.26+ to build. At runtime only
the tools for the backends you use (`tmux`, `systemctl`/`journalctl`, a Docker
socket).

```bash
make build            # produces dist/server-manager
```

Try it locally in development mode (emails are printed to the log):

```bash
./dist/server-manager init --config ./dev/config.json --data-dir ./dev/data \
    --listen 127.0.0.1:8080 --development
```

```bash
./dist/server-manager admin create --config ./dev/config.json --username admin
```

```bash
./dist/server-manager serve --config ./dev/config.json
```

Open <http://127.0.0.1:8080>, sign in, and add a server under
**Servers → Add server**.

For a real installation (dedicated user, systemd unit, HTTPS reverse proxy)
follow [docs/installation.md](docs/installation.md).

## Documentation

| Document | Contents |
|---|---|
| [docs/installation.md](docs/installation.md) | Building, installing, first administrator, systemd, reverse proxy, upgrades |
| [docs/configuration.md](docs/configuration.md) | `config.json` reference and the server definition format for all backends |
| [docs/permissions.md](docs/permissions.md) | Running unprivileged: file permissions, sudoers, polkit, journal, Docker, tmux |
| [docs/administration.md](docs/administration.md) | Users, invitations, permissions, SMTP, audit log, troubleshooting |
| [docs/security.md](docs/security.md) | Security model, guarantees and known limitations |
| [docs/api.md](docs/api.md) | API overview and examples; the OpenAPI document is served at `/api/v1/openapi.yaml` |
| [docs/development.md](docs/development.md) | Project layout, tests and their requirements, adding a backend |

## Project layout

```text
cmd/server-manager/     command-line entry point (serve, init, admin create, check-config, ...)
internal/
  app/                  wiring, HTTP server, graceful shutdown
  api/                  HTTP API, SSE endpoints, middleware, openapi.yaml
  auth/                 Argon2id hashing, JWT, random tokens
  perm/                 permission bitmask
  db/                   SQLite store and migrations (db/migrations/*.sql)
  config/               global configuration (JSON) and secret resolution
  servers/              server definition model, validation, one-file-per-server store
  backend/              execution backends: custom, tmux, systemd, docker
  manager/              lifecycle state machine, operations, monitoring, console access
  console/              bounded line buffer, terminal sanitizer, file tailer
  events/               authorization-aware publish/subscribe hub for SSE
  backup/               safe access to backup directories
  monitor/              host CPU and memory sampling
  mail/                 SMTP delivery (or log output in development)
  audit/                audit trail recorder
  notify/               server activity email notifications
  ratelimit/            token buckets and cooldowns
  logging/              application log setup
web/static/             frontend (plain ES modules and CSS, embedded at build time)
examples/               sample configuration, server definitions and scripts
deploy/                 systemd unit, Caddy/nginx, sudoers, polkit, logrotate examples
scripts/                build and install helpers
docs/                   documentation
```

## What it deliberately does not do

It does not install or update game servers, speak game-specific protocols,
replace Docker Compose, schedule jobs, or implement its own backup format. It
runs the commands you configure and manages access to them.

## Tests

```bash
make test
```

The suite covers every module and includes integration tests that start real
processes. tmux tests need the `tmux` binary (skipped otherwise); Docker and
systemd are exercised against stand-ins, so neither is required. Details in
[docs/development.md](docs/development.md).

# WARNING!:
The whole project is vibe coded! Use it at your own risk!

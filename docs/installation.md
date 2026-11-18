# Installation

## 1. Build the executable

You need the Go toolchain (1.26 or newer) on the build machine. Nothing else:
the frontend is plain JavaScript embedded at compile time and SQLite is
compiled in as pure Go, so there is no C compiler or Node.js step.

```bash
make build
```

This writes `dist/server-manager` for the machine you are on. To build
release binaries for both Linux architectures:

```bash
./scripts/build.sh
```

The results (`dist/server-manager-linux-amd64`, `dist/server-manager-linux-arm64`)
are statically linked and run on any modern Linux distribution without
installing a runtime. Copy the one that matches the target host.

## 2. Install

On a systemd-based host the installer does the routine part:

```bash
sudo ./scripts/install.sh dist/server-manager-linux-amd64
```

It creates the unprivileged `server-manager` user, the layout below and the
systemd unit. It never overwrites an existing configuration.

| Path | Purpose |
|---|---|
| `/usr/local/bin/server-manager` | the executable |
| `/etc/server-manager/config.json` | application configuration |
| `/etc/server-manager/servers/` | one JSON file per server |
| `/etc/server-manager/server-manager.env` | secrets passed as environment variables (root-only) |
| `/var/lib/server-manager/` | database, signing key, runtime capture files |

All paths are configurable (`paths` in `config.json`, `--config` on the
command line or `SM_CONFIG`).

### Manual installation

```bash
sudo useradd --system --home-dir /var/lib/server-manager --shell /usr/sbin/nologin server-manager
```

```bash
sudo install -m 0755 dist/server-manager-linux-amd64 /usr/local/bin/server-manager
```

```bash
sudo install -d -m 0750 -o server-manager -g server-manager /etc/server-manager /etc/server-manager/servers
```

```bash
sudo install -d -m 0700 -o server-manager -g server-manager /var/lib/server-manager
```

```bash
sudo -u server-manager server-manager init --config /etc/server-manager/config.json
```

```bash
sudo install -m 0644 deploy/server-manager.service /etc/systemd/system/
```

`init` writes a default configuration (production mode, listening on
`127.0.0.1:8080`), creates the directories, the database and the JWT signing
key (`/var/lib/server-manager/secrets/jwt.key`, mode 0600).

> **Which user should it run as?** A dedicated unprivileged user. If your game
> servers already run as, say, `minecraft`, the simplest secure setup is often
> to run Server Manager as that same user (`SM_USER=minecraft` for the
> installer and `User=` in the unit), because tmux sessions and files then
> just work. See [permissions.md](permissions.md) for the alternatives. Do not
> run it as root.

### Installing under /opt instead

Every location comes from the configuration file, and relative paths in it
are resolved against the directory that holds it, so the whole installation
can live in one directory. A layout that keeps the program read-only for the
service account:

| Path | Owner, mode | Contents |
|---|---|---|
| `/opt/server-manager/bin/server-manager` | root, 0755 | the program |
| `/opt/server-manager/etc/` | service user, 0750 | `config.json`, `servers/`, optional `server-manager.env` |
| `/opt/server-manager/data/` | service user, 0700 | database, signing key, runtime files |

The service user must be able to write `etc/` (settings saved from the web
UI are written there), but should not be able to replace its own program,
which is why the program sits in a separate root-owned `bin/`.

```bash
sudo useradd --system --home-dir /opt/server-manager --shell /usr/sbin/nologin server-manager
```

```bash
sudo install -d -m 0755 /opt/server-manager /opt/server-manager/bin
```

```bash
sudo install -m 0755 server-manager /opt/server-manager/bin/server-manager
```

```bash
sudo install -d -m 0750 -o server-manager -g server-manager /opt/server-manager/etc
```

```bash
sudo install -d -m 0700 -o server-manager -g server-manager /opt/server-manager/data
```

```bash
sudo -u server-manager /opt/server-manager/bin/server-manager init --config /opt/server-manager/etc/config.json --data-dir ../data
```

`--data-dir ../data` is stored relative to `config.json`, so the directory
can be moved or copied as a whole later. Use
[deploy/server-manager-opt.service](../deploy/server-manager-opt.service) as
the unit:

```bash
sudo install -m 0644 deploy/server-manager-opt.service /etc/systemd/system/server-manager.service
```

```bash
sudo systemctl daemon-reload
```

The command-line tools look for `/etc/server-manager/config.json` unless told
otherwise, so pass `--config /opt/server-manager/etc/config.json` to every
command (or export `SM_CONFIG=/opt/server-manager/etc/config.json`). Nothing
is written outside `/opt/server-manager` except the systemd unit file; logs go
to the journal.

## 3. Configure

Edit `/etc/server-manager/config.json`. At minimum set the address users will
use:

```json
"public_url": "https://servers.example.com"
```

`public_url` is used in invitation and verification links and is required in
production. The full reference is in [configuration.md](configuration.md);
[examples/config.json](../examples/config.json) is a complete sample.

Validate what you have at any time:

```bash
sudo -u server-manager server-manager check-config --config /etc/server-manager/config.json
```

It checks the configuration, every server definition, whether configured
executables exist and are executable, and whether required tools are present.
It exits non-zero on errors.

## 4. Create the first administrator

There are no default credentials and no web-based setup page. The first
account is created from a shell on the host:

```bash
sudo -u server-manager server-manager admin create --config /etc/server-manager/config.json --username alice
```

You are prompted for the password (not echoed). For automation, pipe it in
with `--password-stdin`. Further users join through invitations created in the
web UI.

Forgot the password? `server-manager user set-password --username alice`
(this also signs that account out everywhere).

## 5. Start the service

```bash
sudo systemctl enable --now server-manager
```

```bash
journalctl -u server-manager -f
```

Useful facts about the provided unit:

- `KillMode=process` — only the manager is stopped when the unit stops.
  Servers it started keep running and are re-adopted on the next start, so
  you can restart or upgrade the manager without taking game servers down.
  (Servers configured with `on_manager_exit: "stop"`, and servers attached to
  a PTY, are shut down gracefully instead.)
- `systemctl reload server-manager` re-reads the server definition files and
  reopens the log file.
- Secrets come from `/etc/server-manager/server-manager.env`.

## 6. Put HTTPS in front

Server Manager listens on loopback by default and expects a TLS-terminating
reverse proxy. Ready-made configurations:

- [deploy/Caddyfile](../deploy/Caddyfile) (automatic certificates)
- [deploy/nginx.conf](../deploy/nginx.conf)

What matters for any proxy:

1. Pass the original `Host` header and set `X-Forwarded-For` and
   `X-Forwarded-Proto`.
2. Do not buffer responses (Server-Sent Events) and allow long-lived
   connections.
3. List the proxy's address in `trusted_proxies` (the default covers a proxy
   on the same machine). Forwarded headers from any other address are
   ignored, so clients cannot spoof their IP.

With `public_url` set to an `https://` address the session cookie is marked
`Secure` automatically.

## 7. SMTP (optional)

Without SMTP, administrators get an invitation link to pass on themselves.
To have invitations and email verifications sent automatically:

```json
"smtp": {
  "host": "smtp.example.com",
  "port": 587,
  "username": "server-manager@example.com",
  "from": "Server Manager <server-manager@example.com>",
  "tls": "starttls"
}
```

The password is not stored in `config.json`. Provide it one of two ways:

- environment: add `SM_SMTP_PASSWORD=...` to
  `/etc/server-manager/server-manager.env` (mode 0600, owned by root), or
- file: put it in a file readable only by the service user and set
  `"password_file"` to its path.

`tls` is `starttls` (port 587; the connection fails if the server does not
offer STARTTLS), `tls` (implicit TLS, port 465) or `none` (only for a relay
on localhost). Use **Settings → Send a test email** to verify.

## Upgrading

1. Build or download the new executable.
2. Replace `/usr/local/bin/server-manager`.
3. `sudo systemctl restart server-manager`.

Database migrations are embedded in the executable and applied automatically
on start (each in its own transaction). An older executable refuses to open a
database created by a newer one rather than risk damaging it. To apply
migrations without starting the server: `server-manager migrate`.

Running servers are not interrupted by the restart (see `KillMode` above).

## Backing up Server Manager itself

Everything the application owns is in three places:

- `/etc/server-manager/` — configuration and server definitions,
- `/var/lib/server-manager/server-manager.db` (plus `-wal`/`-shm` while
  running) — users, sessions, permissions, audit log,
- `/var/lib/server-manager/secrets/jwt.key` — the signing key.

Copy them while the service is stopped, or use `sqlite3 server-manager.db
".backup copy.db"` for a consistent online copy of the database.

## Uninstalling

```bash
sudo systemctl disable --now server-manager
```

Then remove the unit file, the executable and the two directories. Your
servers' own files are never touched by Server Manager.

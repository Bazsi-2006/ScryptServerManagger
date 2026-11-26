# Configuration

Server Manager keeps its configuration in JSON files and its data in SQLite:

| What | Where |
|---|---|
| Global settings | `config.json` |
| Server definitions | one file per server: `<servers_dir>/<id>.json` |
| Users, sessions, invitations, permissions, audit log, operation history | SQLite database |
| Secrets (JWT key, SMTP password) | environment variables or protected files — never the JSON |

Both kinds of JSON file reject unknown fields, so a typo is reported instead of
silently ignored. Durations are strings such as `"30s"`, `"5m"`, `"1h"`.

Check everything with:

```bash
server-manager check-config --config /etc/server-manager/config.json
```

## Global settings (`config.json`)

Most settings can also be changed under **Settings** in the web UI; they are
written back to this file and applied immediately. The settings marked
*restart* are only read at startup and can only be changed in the file.

A complete sample is in [examples/config.json](../examples/config.json).
Omitted fields take the defaults shown.

### Top level

| Field | Default | Meaning |
|---|---|---|
| `mode` | `"production"` | `"production"` or `"development"`. Development prints outgoing email to the log when SMTP is not configured. *restart* |
| `listen` | `"127.0.0.1:8080"` | Address to listen on. Loopback by default, for use behind a reverse proxy. *restart* |
| `public_url` | `""` | External base URL, e.g. `https://servers.example.com`. Used for links in emails; required for them in production. |
| `trusted_proxies` | `["127.0.0.1/32","::1/128"]` | Addresses whose `X-Forwarded-For` / `X-Forwarded-Proto` headers are believed. |

### `paths` (*restart*)

| Field | Default |
|---|---|
| `data_dir` | `/var/lib/server-manager` |
| `database` | `<data_dir>/server-manager.db` |
| `servers_dir` | `servers/` next to the configuration file |
| `log_dir` | `/var/log/server-manager` |
| `secrets_dir` | `<data_dir>/secrets` |
| `runtime_dir` | `<data_dir>/runtime` (console capture files, stdin FIFOs) |

Relative paths are resolved against the directory of the configuration file.

### `auth`

| Field | Default | Meaning |
|---|---|---|
| `access_token_ttl` | `10m` | Lifetime of access tokens (30s–1h). |
| `refresh_token_ttl` | `720h` | A session ends after this long without use. |
| `invitation_ttl` | `72h` | Default validity of invitations. |
| `email_change_ttl` | `24h` | Validity of email verification links. |
| `password_reset_ttl` | `1h` | Validity of emailed password reset links (5m–24h). Email reset is never available to administrators. |
| `min_password_length` | `10` | Minimum password length (8–128). |
| `cookie_secure` | `"auto"` | `auto` = Secure when served over HTTPS; `always`; `never`. |
| `jwt_secret_file` | `<secrets_dir>/jwt.key` | Signing key file; generated on first start. *restart* |
| `argon2` | `{"memory_kib":65536,"iterations":3,"parallelism":2}` | Argon2id cost. Existing hashes are upgraded at the next sign-in. *restart* |

### `smtp`

| Field | Default | Meaning |
|---|---|---|
| `host`, `port` | `""`, `587` | SMTP server. Email is enabled when `host` and `from` are set. |
| `username` | `""` | Login name (empty = no authentication). |
| `password_file` | `""` | File containing the password. `SM_SMTP_PASSWORD` takes precedence. File-only. |
| `from` | `""` | Sender, e.g. `Server Manager <sm@example.com>`. |
| `tls` | `"starttls"` | `starttls` (required, fails closed), `tls` (implicit), `none`. |

### `rate_limits`

Each limit is `{"requests": N, "window": "1m"}` — a token bucket holding at most
N requests that refills over the window.

| Field | Default | Keyed by |
|---|---|---|
| `login` | 5 / 1m | client address, and separately the account |
| `refresh` | 30 / 1m | client address |
| `api` | 600 / 1m | user (address for unauthenticated calls) |
| `server_start` | 6 / 1m | user + server |
| `server_stop` | 6 / 1m | user + server (a restart counts against both) |
| `console_command` | 60 / 1m | user + server |
| `backup_create` | 3 / 5m | user + server |
| `action_run` | 20 / 1m | user + server (custom actions) |
| `password_reset` | 5 / 15m | client address, and separately the email address (reset requests); user (verification emails) |
| `server_cooldown` | `3s` | Minimum pause after a start/stop/restart of a server before the next one, for everybody. `0s` disables it. A server can override it. |

Rate limits are in addition to — never instead of — operation locking and
permission checks.

### `monitoring`, `console`, `backups`, `logging`, `audit`, `security`

| Field | Default | Meaning |
|---|---|---|
| `monitoring.host_metrics_interval` | `5s` | How often host CPU/memory are sampled and pushed. |
| `monitoring.host_metrics_for_users` | `true` | Show host metrics to non-administrators. |
| `monitoring.status_poll_interval` | `5s` | How often each server's real state is checked. |
| `console.buffer_lines` | `2000` | Lines kept in memory per server. |
| `console.snapshot_lines` | `300` | Lines sent when a console is opened. |
| `console.max_line_bytes` | `4096` | Longer lines are split. |
| `console.max_command_length` | `1024` | Longest accepted console command. |
| `console.max_log_file_bytes` | `16777216` | Capture files are truncated once fully read past this size. |
| `backups.max_download_bytes` | `0` | Largest downloadable file (0 = unlimited). |
| `backups.max_concurrent_downloads` | `4` | Simultaneous downloads. |
| `logging.level` | `"info"` | `debug`, `info`, `warn`, `error`. |
| `logging.format` | `"text"` | `text` or `json`. *restart* |
| `logging.file` | `""` | Optional log file in addition to stderr. *restart* |
| `audit.retention_days` | `0` | Delete audit entries older than this (0 = keep forever). |
| `security.max_request_bytes` | `1048576` | Largest accepted request body. |
| `security.config_permission_full_access` | `false` | See [security.md](security.md#the-config-permission). Leave off. |

### `limits` and `categories` — concurrent servers

```json
"limits": { "max_concurrent_servers": 5 },
"categories": [
  { "id": "minecraft", "name": "Minecraft", "description": "Java servers", "max_concurrent": 2 },
  { "id": "source", "name": "Source Engine", "max_concurrent": 3 }
]
```

| Field | Default | Meaning |
|---|---|---|
| `limits.max_concurrent_servers` | `0` | How many servers may be active at once across the whole installation (0 = unlimited). |
| `categories[].id` | — | Permanent identifier (`a-z`, `0-9`, `_`, `-`); server definitions refer to it in `category`. |
| `categories[].name` | — | Display name, unique (case-insensitive). |
| `categories[].max_concurrent` | `0` | Limit for the servers of this category (0 = unlimited). |

Both are usually managed on the **Limits** page. Both limits apply at the same
time: a start is refused when either is reached. A server counts while it is
STARTING, RUNNING or STOPPING and keeps its slot until it is confirmed
stopped; a server in ERROR or UNKNOWN keeps whatever the last conclusive
observation said (a server whose state cannot be determined is never assumed
to have stopped). Servers started outside Server Manager count as soon as
monitoring sees them. The check and the reservation of the slot happen
atomically, so two simultaneous starts can never take the same last slot.
Limits apply to administrators too; an administrator can explicitly override
a limit from the dialog that explains it, and the override is audited.

A definition's `category` may also contain a category *name* (definitions
written before categories were managed do); it is matched case-insensitively.
Creating or renaming a category rewrites such definitions to use the ID.
Category values that match no managed category still group servers on the
dashboard but have no limit.

### `notifications`

| Field | Default | Meaning |
|---|---|---|
| `notifications.enabled` | `true` | Global switch for server activity emails. Users still opt in individually in their profile. |
| `notifications.cooldown` | `10m` | Repeated emails of the same kind about the same server within this time are suppressed (and counted in the next one). `0s` disables the cooldown. |

Notifications use the SMTP settings above (and are printed to the log in
development mode). Nothing is sent when SMTP is not configured.

### `tools` (*restart*)

| Field | Default |
|---|---|
| `tmux`, `systemctl`, `journalctl`, `sudo` | the bare program names (found through `PATH`) |
| `docker_socket` | `/var/run/docker.sock` |

### Secrets

| Secret | Source (first match wins) |
|---|---|
| JWT signing key | `SM_JWT_SECRET` (≥ 32 characters), else `auth.jwt_secret_file` (auto-generated, must be mode 0600) |
| SMTP password | `SM_SMTP_PASSWORD`, else `smtp.password_file` |

With the provided systemd unit, put environment secrets in
`/etc/server-manager/server-manager.env` (root-owned, mode 0600). Variables
starting with `SM_` are stripped from the environment of every command the
manager runs, so your servers never see them.

## Server definitions

A definition describes one server. Create and edit them in the web UI
(**Servers → Add server**, or a server's **Settings** tab, which also has an
"Edit as JSON" view), or write the files by hand and choose **Settings →
Reload from disk** (or `systemctl reload server-manager`).

The file name must be `<id>.json`. Files are written with mode 0600 because
they may contain environment variables.

Examples for every backend are in [examples/servers/](../examples/servers).

### Common fields

| Field | Meaning |
|---|---|
| `id` | Permanent identifier: 1–63 characters of `a-z`, `0-9`, `_`, `-`. |
| `name` | Display name. |
| `description` | Optional. |
| `category` | Optional category ID (see [`categories`](#limits-and-categories--concurrent-servers)); groups servers and decides which concurrency limit applies. Only administrators can change it. |
| `backend` | `custom`, `tmux`, `systemd` or `docker`. |
| `working_dir` | Absolute working directory for the server's commands. |
| `env` | Extra environment variables for every command of this server. Not shown in the web form (set variables in your start scripts or service configuration instead); existing values keep working and can still be edited in the JSON view. |
| `readiness` | How to tell STARTING from RUNNING (below). |
| `console` | Console input method (below). |
| `backup` | Optional backups: a directory plus a command or a tmux console command (below). |
| `actions` | Optional custom actions users can be allowed to run (below). |
| `timeouts` | `start` (2m), `stop` (1m), `backup` (1h), `command` (15s). |
| `cooldown` | Overrides `rate_limits.server_cooldown` for this server. |
| `status_poll_interval` | Overrides `monitoring.status_poll_interval`. |

**Commands** are always written as an executable plus an argument list:

```json
{ "command": "/opt/gameservers/example/start.sh", "args": ["--port", "27015"] }
```

`command` is an absolute path, or a bare program name looked up in `PATH`
(`java`). Relative paths are rejected. Each element of `args` is passed to the
program exactly as written — no shell is involved, so quotes, spaces, `;`, `$`
and friends have no special meaning. If you want shell features, make the
command a script.

Every command receives `SM_SERVER_ID` and `SM_SERVER_NAME` in its environment.

### `custom` — scripts and commands

```json
"custom": {
  "mode": "foreground",
  "start": { "command": "/opt/app/start.sh", "args": ["--port", "27015"] },
  "stop":  { "command": "/opt/app/stop.sh" },
  "stop_input": "",
  "stop_signal": "SIGTERM",
  "kill_on_timeout": true,
  "pid_file": "",
  "log_file": "",
  "on_manager_exit": "keep"
}
```

`mode` chooses how the server is tracked:

- **`foreground`** (default) — the start command *is* the server. The manager
  starts it in its own session, records its PID and start time, and knows
  directly whether it is alive. Its stdout and stderr go to a capture file
  that feeds the console. Because output goes to a file and input (if
  enabled) through a FIFO, the server survives a restart of the manager and
  is picked up again afterwards.
- **`forking`** — the start command launches the server in the background and
  exits. The manager does **not** conclude that the server stopped when the
  script ends. It needs another way to know whether the server is alive:
  either `pid_file`, or a `port`/`script` readiness check (which then serves
  as the liveness signal too). One of the two is required.

Stopping, in order of preference:

1. `stop` command, if configured (required for forking servers without a PID
   file);
2. `stop_input` — text written to the server's console (e.g. `"stop"`), for
   foreground servers with `console.input` `stdin` or `pty`;
3. `stop_signal` sent to the server's process group (or to the PID from
   `pid_file`).

If the server is still alive when `timeouts.stop` expires and
`kill_on_timeout` is true (the default), it is killed with its whole process
group and the stop is reported as "was killed". With `kill_on_timeout: false`
the stop fails and the server keeps its real state.

`log_file` names an additional file whose new lines are shown in the console —
useful for forking servers that write their own log. It is never modified.

`on_manager_exit`: `keep` (default) leaves the server running when the manager
stops; `stop` shuts it down gracefully first.

### `tmux` — tmux sessions

```json
"tmux": {
  "session": "mc-survival",
  "window": "", "pane": "",
  "start": { "command": "/usr/bin/java", "args": ["-jar", "paper.jar", "--nogui"] },
  "stop_text": "stop",
  "stop_keys": [],
  "kill_on_timeout": true,
  "width": 200, "height": 50,
  "socket_name": "", "socket_path": ""
}
```

- The server is **running when the session exists** (and the pane's process
  has not exited). If you start your server from a shell inside the session,
  the session outlives the server; add a readiness/status check or run the
  server as the session's command, as above.
- `start` is the command of a new detached session. Omit it for an
  **attach-only** session that something else starts: the manager then
  observes it, streams its output, sends commands and can stop it, but offers
  no Start button.
- Stopping sends `stop_keys` (tmux key names such as `C-c`), then types
  `stop_text` followed by Enter. If neither is set, or the session is still
  there after `timeouts.stop` and `kill_on_timeout` is true, the session is
  killed.
- `window` / `pane` select a pane (default: the active one). Session names are
  matched exactly, never by prefix.
- Output is captured with `tmux pipe-pane` from the moment the manager first
  sees the session, whether or not anyone is watching; the pane's existing
  scrollback is loaded as history.
- The tmux server must belong to the user the manager runs as (see
  [permissions.md](permissions.md#tmux)). `socket_name` (`tmux -L`) or
  `socket_path` (`tmux -S`) select a non-default tmux server.

### `systemd` — existing units

```json
"systemd": { "unit": "valheim.service", "scope": "system", "use_sudo": true, "allow_restart": true }
```

State comes from `systemctl show` (`ActiveState`): `activating` is STARTING,
`deactivating` is STOPPING, `failed` is ERROR, a missing or masked unit is
UNKNOWN. Output comes from `journalctl`. `scope: "user"` uses
`systemctl --user`. `use_sudo` runs the start/stop/restart calls as
`sudo -n systemctl …` (queries never use sudo). Required privileges are
described in [permissions.md](permissions.md#systemd).

Console input is not available for systemd units unless you configure a
helper command (below).

### `docker` — existing containers

```json
"docker": { "container": "minecraft", "allow_restart": true }
```

The container must already exist (create it with `docker run`/Compose). State
comes from the Engine API; a container health check in `starting` keeps the
server in STARTING. A removed container or an unreachable daemon shows as
UNKNOWN with an explanation. Stopping uses Docker's own stop with
`timeouts.stop` as the grace period.

### `readiness` — when is a starting server ready?

A server is STARTING from the moment a start is requested until its readiness
condition holds, then RUNNING. If that takes longer than `timeouts.start` the
start fails with an explanatory message.

| `method` | Ready when |
|---|---|
| `none` (default) | the backend reports the server alive (process running, session exists, unit active, container running and not `health: starting`) |
| `console` | a line of console output contains `console.pattern` (or matches it as a regular expression with `"regex": true`). Only output produced by the current start attempt counts. |
| `port` | the port answers (see below) |
| `script` | the status script exits 0 |

```json
"readiness": { "method": "console", "console": { "pattern": "Done \\(", "regex": true } }
```

```json
"readiness": {
  "method": "port",
  "port": { "host": "127.0.0.1", "port": 25565, "protocol": "tcp", "timeout": "2s" },
  "interval": "2s"
}
```

- **TCP**: ready when a connection can be opened.
- **UDP**: an open UDP port proves nothing, so you must give a probe datagram
  in `udp_payload_hex` that the server answers; the port is ready when any
  reply arrives. (For Source-engine servers the A2S_INFO query works; see
  [examples/servers/source_engine.json](../examples/servers/source_engine.json).)

```json
"readiness": {
  "method": "script",
  "script": { "command": "/opt/app/check-status.sh", "args": [], "timeout": "5s" },
  "interval": "3s"
}
```

Status script convention: exit **0** = ready/running, exit **1** = not ready,
**anything else** (other codes, a timeout, a missing script) = the check
failed, shown as UNKNOWN with the reason.

**Readiness is not liveness.** Whether the process/session/unit/container is
alive always comes from the backend where it can. A port or script check only
shows that *something* answers; it cannot prove it is the process the manager
started. The one case where the manager has nothing better is a forking
custom server without a PID file — there the port/script check is used for
both, and that limitation applies.

### `console` — sending commands

| `input` | Available for | How a command is delivered |
|---|---|---|
| *(empty)* | all | backend default: `tmux` for tmux servers, otherwise none |
| `none` | all | console input disabled |
| `stdin` | custom, foreground | written to the server's standard input |
| `pty` | custom, foreground | the server runs on a pseudo-terminal; written to it. Such servers cannot outlive the manager and are stopped when it exits. |
| `tmux` | tmux | typed into the pane literally, then Enter |
| `command` | all | runs `input_command` on the host |
| `docker_exec` | docker | runs `input_command` inside the container |

For `command` and `docker_exec`, write `{command}` in an argument where the
console line should go. It is substituted as part of that single argument:

```json
"console": {
  "input": "docker_exec",
  "input_command": { "command": "rcon-cli", "args": ["{command}"] }
}
```

Without a `{command}` placeholder the line is sent on the helper's standard
input (host helpers) or appended as the last argument (`docker_exec`).

Console commands are data for your server's console. They are never executed
by a shell on the host. Be aware of what the *target* does with them: a tmux
pane sitting at a shell prompt will run what is typed into it.

### `backup`

```json
"backup": {
  "directory": "/opt/minecraft/backups",
  "command": { "command": "/opt/minecraft/backup.sh" },
  "allow_user_delete": false
}
```

- `directory` — files directly inside it are listed and can be downloaded.
  Subdirectories, hidden files and symbolic links that lead outside are
  ignored.
- `command` — optional; run when someone presses **Create backup**. It gets
  `SM_BACKUP_DIR` and `SM_SERVER_ID`, is limited by `timeouts.backup` and can
  be cancelled. Write to a temporary name and rename when done so incomplete
  files are never offered (see [examples/scripts/backup.sh](../examples/scripts/backup.sh)).
- `allow_user_delete` — let holders of BACKUP_DOWNLOAD delete files;
  otherwise only administrators can.

**Sending a command to the tmux session** (`"method": "tmux"`, tmux backend
only) is for game servers that make backups from their own console:

```json
"backup": {
  "method": "tmux",
  "directory": "/opt/minecraft/backups",
  "tmux": { "text": "/backup start", "enter": true },
  "completion": { "pattern": "Backup complete", "regex": false }
}
```

- `tmux.text` — typed literally into the server's configured session/pane
  (`send-keys -l`: tmux key names and shell syntax are not interpreted), one
  line without control characters. `enter` (default `true`) presses Enter
  afterwards. The session must exist, otherwise the request is refused with
  an explanation.
- `directory` — optional for this method; where the server writes its
  backups. New files are listed as soon as they appear.
- `completion` — optional. Delivering the command does not mean the backup
  has finished. Without a completion pattern the operation ends as
  **submitted** ("the command was sent; completion cannot be verified") and
  the file list refreshes by itself while the server writes. With a pattern,
  the operation waits (up to `timeouts.backup`) for a matching console line
  produced after the command was sent, and succeeds or fails accordingly.

Starting a backup needs BACKUP_CREATE; listing and downloading files needs
BACKUP_DOWNLOAD. One operation runs per server at a time, so a backup and a
start/stop never overlap.

### `actions` — custom actions

Custom actions are administrator-defined command modules shown as buttons on
the server page. Users run them only when an administrator grants them that
specific action (Access tab), and they can never change what is executed.

```json
"actions": [
  { "id": "save_world", "name": "Save World", "icon": "💾",
    "description": "Forces the server to save its world.",
    "method": "tmux", "tmux": { "text": "save-all flush", "enter": true } },
  { "id": "update", "name": "Update Server",
    "method": "command", "command": { "command": "/opt/minecraft/update.sh", "args": ["--channel", "stable"] },
    "working_dir": "/opt/minecraft", "timeout": "15m", "confirm": true }
]
```

| Field | Meaning |
|---|---|
| `id` | Unique within the server: 1–63 characters of `a-z`, `0-9`, `_`, `-`. Permission grants refer to it. |
| `name`, `description`, `icon` | What users see. `icon` is a short symbol such as an emoji. |
| `method` | `command` (run a program) or `tmux` (type text into the server's tmux session; tmux backend only). |
| `command` | For `command`: executable and argument list, run directly (no shell) with the server's environment plus `SM_ACTION_ID` and `SM_ACTION_USER`. |
| `working_dir` | For `command`: defaults to the server's `working_dir`. |
| `timeout` | For `command`: default `10m`; the process group is killed after it. |
| `tmux` | For `tmux`: `{ "text": ..., "enter": true }`, as for tmux backups. |
| `confirm` | Ask for confirmation before running (the API requires `{"confirm": true}`). |
| `disabled` | Hide the action from users and refuse to run it. |

Runs happen in the background. Script actions report success (exit 0) or
failure with the tail of their output; tmux actions report **submitted**,
because whether the server acted on the text cannot be verified. Each action
runs at most once at a time per server, runs are rate limited
(`rate_limits.action_run`), recorded in the operation history and the audit
log, and their results are pushed to the users who may run them.

## What can be changed while a server is running

The name, description, category, timeouts, cooldown, polling intervals, the
console readiness pattern, the backup settings and the custom actions can be
changed at any time. Anything that controls the server process (backend,
commands, paths, environment, console input) is refused until the server is
stopped, so the manager never loses track of a running server.

Deleting a definition removes the JSON file and the server's permission
assignments and action grants. It never deletes the server's own files.

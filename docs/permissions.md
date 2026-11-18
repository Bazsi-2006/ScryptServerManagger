# Operating-system permissions

Server Manager executes commands on your host, so who it runs as matters.

**Run it as a dedicated, unprivileged user. Never as root.** Everything the
manager starts runs with that user's rights, and everything an administrator
of the web UI can configure is limited by them. If something does not work
because of permissions, grant exactly the missing right as described here —
do not reach for root.

## Choosing the service user

Two setups work well:

1. **Same user as the servers** (simplest). If your game servers live under
   `/opt/minecraft` owned by `minecraft`, run Server Manager as `minecraft`.
   Scripts, tmux sessions and backup directories then work without further
   configuration.
2. **Separate `server-manager` user** with narrowly delegated rights
   (group-readable files, sudoers rules for specific commands, polkit rules
   for specific units). More isolation, more setup.

## Scripts and executables (custom backend)

The service user needs to be able to *execute* each configured program and
*enter* every directory on the way to it:

```bash
sudo -u server-manager test -x /opt/gameservers/example/start.sh && echo ok
```

Typical fixes:

```bash
chmod +x /opt/gameservers/example/start.sh
```

```bash
chmod o+x /opt /opt/gameservers /opt/gameservers/example
```

Scripts need a `#!` line (`#!/bin/sh`). `server-manager check-config` reports
configured executables that are missing or not executable.

**Protect the scripts themselves.** Whoever can modify a script that the
manager runs can run code as the service user. Scripts and their directories
should be writable only by their owner (or root), not by other accounts.

To run a server's scripts as a *different* user, delegate exactly those
scripts through sudo and make sudo the configured command:

```json
"start": { "command": "/usr/bin/sudo", "args": ["-n", "-u", "minecraft", "/opt/minecraft/start.sh"] }
```

```text
# /etc/sudoers.d/server-manager
server-manager ALL=(minecraft) NOPASSWD: /opt/minecraft/start.sh, /opt/minecraft/stop.sh
```

List full paths; never grant `ALL`. Scripts allowed this way must not be
writable by the service user, or the rule amounts to full access to that
account.

## Backups

The service user needs read access to the backup directory to list and
download, write access to delete, and whatever the backup command itself
needs. Downloads are confined to the configured directory: path traversal and
symbolic links pointing outside it are refused.

## tmux

A tmux server belongs to the user who started it; other users cannot attach
to it. Therefore:

- sessions the manager creates belong to the service user;
- to manage **existing** sessions, the manager must run as the user who owns
  them (setup 1 above).

`socket_name` / `socket_path` let you address a specific tmux server, but
tmux still verifies the connecting user, so they do not by themselves give
access to another user's sessions.

Whoever may send console commands to a pane can type into it. If the pane is
at a shell prompt, that is shell access as the session's user — grant
CONSOLE_WRITE on such servers accordingly.

## systemd

Reading state (`systemctl show`) needs no privileges. Two things do:

### Starting and stopping units

Pick one.

**polkit** (no sudo; leave `"use_sudo": false`). Install
[deploy/polkit-server-manager.rules](../deploy/polkit-server-manager.rules)
as `/etc/polkit-1/rules.d/50-server-manager.rules` and list your units in it.
It allows the service user to start, stop and restart exactly those units.

**sudo** (set `"use_sudo": true` on the server). Install rules like
[deploy/sudoers.example](../deploy/sudoers.example) with `visudo`. The
manager runs precisely these command lines, so the rules can be exact:

```text
/usr/bin/systemctl start --no-block <unit>
/usr/bin/systemctl stop --no-block <unit>
/usr/bin/systemctl restart <unit>
```

Name every unit explicitly. A rule for bare `systemctl`, or with wildcards in
the unit name, is equivalent to root.

**User services** (`"scope": "user"`) need neither: the manager controls the
service user's own `systemctl --user` units. Enable lingering so they exist
without a login session:

```bash
sudo loginctl enable-linger server-manager
```

### Reading the journal

```bash
sudo usermod -aG systemd-journal server-manager
```

Restart the service afterwards. Without it, the console of systemd servers
explains that the journal cannot be read instead of showing output.

## Docker

To control containers the service user must be able to use the Docker socket,
normally by membership of the `docker` group:

```bash
sudo usermod -aG docker server-manager
```

**Understand what this grants.** Access to the Docker socket is equivalent to
root on the host: a member of the `docker` group can start a privileged
container that mounts the whole filesystem. Server Manager itself only ever
issues inspect, start, stop, restart, kill and logs calls (and `exec` of a
fixed helper, if you configure one) against the containers you configured,
and web users can do no more than that. But the service account holds the
underlying power, so:

- treat the Server Manager host and its administrator accounts as you would
  root on that machine;
- prefer the systemd backend (with units that run `docker start -a …`) or a
  restricted socket proxy if that trade-off is not acceptable.

Rootless Docker avoids the problem: point `tools.docker_socket` at the user's
socket (`/run/user/<uid>/docker.sock`).

## Application files

Created with restrictive permissions by `init` and at runtime; keep them so.

| File | Mode | Why |
|---|---|---|
| `config.json` | 0600 | deployment details |
| `servers/*.json` | 0600 | may contain environment variables |
| `server-manager.db` (+ `-wal`, `-shm`) | 0600 | password hashes, session data, audit log |
| `secrets/jwt.key` | 0600 | signing key — the service refuses to start if it is readable by others |
| `server-manager.env` | 0600 root | environment secrets, read by systemd |
| `runtime/` | 0700 | console capture files |

The service user must be able to write `config.json` and the `servers/`
directory for changes made in the web UI to be saved. If you prefer an
immutable configuration, make them read-only: the UI then reports that
settings could not be saved, and you manage the files by hand.

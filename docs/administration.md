# Administration guide

## Users and invitations

There is no sign-up page. People get accounts through invitations.

1. **Invitations → Create invitation.** Optionally enter the recipient's
   email address and a validity in hours (default: `auth.invitation_ttl`).
2. If SMTP is configured and you gave an address, the link is emailed. If
   not, the link is shown to you once — copy it and pass it on. (In
   development mode the email text is printed to the application log.)
3. The recipient opens the link, chooses a username and password, and is
   signed in. The invitation is now used; it works exactly once.

A new account has **no access to any server** until you grant permissions.

| Action | Effect |
|---|---|
| **Resend / New link** | Issues a fresh link and invalidates the previous one (only hashes are stored, so the old link cannot be re-sent). |
| **Revoke** | The link stops working immediately. |

If sending an email fails, the invitation stays valid and you are given the
link to share by hand.

### Managing accounts (**Users**)

- **Edit** — display name, email address (set directly, without
  verification), administrator role, disabled.
- **Disabled** accounts cannot sign in and are signed out everywhere at once;
  their open pages stop receiving updates immediately.
- **Sessions** — see where an account is signed in and revoke sessions.
- **Delete** — removes the account, its sessions and permission assignments.
  Audit entries are kept.

Safeguards: you cannot remove your own administrator role, disable or delete
yourself, and the last active administrator cannot be removed. Locked out
anyway? On the host:

```bash
sudo -u server-manager server-manager user set-password --username alice
```

```bash
sudo -u server-manager server-manager admin create --username rescue
```

## Per-server permissions

Each non-administrator has one permission set per server. Edit it under
**Users → Permissions** (all servers for one user) or on a server's
**Access** tab (all users for one server). Changes are saved as you tick and
apply immediately, including to pages the user already has open.

| Permission | Value | Allows |
|---|---:|---|
| START | 1 | Start the server |
| STOP | 2 | Stop and restart it — **includes START** |
| CONSOLE_READ | 4 | View the console (snapshot and live) |
| CONSOLE_WRITE | 8 | Send console commands — **includes CONSOLE_READ** |
| BACKUPS | 16 | List, create and download backups |
| CONFIG | 32 | Edit the server's name, description, category, timeouts and readiness text |

The stored value is the sum of the ticked permissions (e.g. START +
CONSOLE_READ = 5). Implied permissions are shown ticked and greyed out.

- Everything is denied unless granted. A user with no permission on a server
  does not see it at all.
- START alone does not allow stopping. CONSOLE_READ alone does not allow
  sending commands.
- CONFIG applies to that one server. It never gives access to global
  settings, users or other servers, and it cannot change what a server
  executes ([security.md](security.md#the-config-permission)).
- Deleting backup files is reserved for administrators unless the server's
  definition sets `backup.allow_user_delete`.
- Administrators always have everything.

## Server states

| State | Meaning |
|---|---|
| STOPPED | Not running. |
| STARTING | A start is in progress, or the server is alive but its readiness condition is not met yet. |
| RUNNING | Started and ready. |
| STOPPING | A stop is in progress. |
| ERROR | The last start, stop or the server itself failed. The message says why. |
| UNKNOWN | The real state cannot be determined right now (tool missing, Docker unreachable, unit or container not found, status script failing). The message says why. |

Things worth knowing:

- State is checked continuously in the background (every
  `monitoring.status_poll_interval`), not only when someone looks. A server
  started or stopped outside the manager is noticed.
- After the manager itself restarts, nothing is assumed: every server is
  probed again. Servers that kept running are shown as RUNNING.
- One operation runs per server at a time. A second start, or a start during
  a backup, is rejected with an explanation. **Stop** is the exception: it
  cancels a start that is still in progress, so a server stuck in STARTING can
  always be stopped.
- If a start times out, the server is shown as ERROR even if its process is
  still alive — stop it, or it will switch to RUNNING by itself if a port or
  script check later succeeds.
- If a stop times out, the server is killed (unless `kill_on_timeout` is
  off, in which case the stop fails and the real state is shown).
- After each start/stop/restart there is a short cooldown
  (`rate_limits.server_cooldown`) before the next one is accepted.

The **Activity** tab of a server lists its recent operations with their
outcome and message.

## Console

The **Console** tab shows recent output and then follows it live. Several
people can watch at once. Uncheck **Live** for a static snapshot with a
refresh button. If no output can be shown (for example the journal is not
readable), the reason is displayed.

The input line appears for users with CONSOLE_WRITE when the server has an
input method configured. ↑/↓ recall your previous commands. Every command is
recorded in the audit log with the user who sent it.

## Backups

On the **Backups** tab, users with BACKUPS can list files, download them and
— if a backup command is configured — start a backup. A running backup can be
cancelled. The list refreshes by itself when a backup finishes.

## Audit log

**Audit log** records who did what, when, to which server, and with what
result: sign-ins (successful and failed), invitations, account and permission
changes, server definitions, every start/stop/restart request and its
outcome, console commands, backup runs and downloads, settings changes, and
attempts that were denied. Filter by user, server, action, result and time
range.

Results: `REQUESTED` (an operation was accepted), `SUCCESS`, `FAILURE`,
`DENIED`.

The audit log lives in the database and is separate from the application log
(stderr/journald, for troubleshooting) and from server console output.
Set `audit.retention_days` to prune old entries; by default they are kept.

## Application settings

**Settings** edits the global configuration. Changes are validated, written
to `config.json` and take effect immediately. The page also offers:

- **Send a test email** — verifies SMTP end to end and shows the error if it
  fails.
- **Reload from disk** — re-reads the server definition files after manual
  edits. Changes to a server that is currently running are deferred and
  reported.
- A list of definition files that could not be loaded, with the reason.

The listen address, file paths, tool paths and mode are shown read-only; they
are changed in the file and need a restart.

## Troubleshooting

Start with:

```bash
sudo -u server-manager server-manager check-config
```

```bash
journalctl -u server-manager -e
```

Set `logging.level` to `debug` (Settings) for request-level detail.

| Symptom | Likely cause and fix |
|---|---|
| Start fails: *executable … was not found* / *permission denied* | Path wrong, not executable, or a parent directory is not accessible to the service user. See [permissions.md](permissions.md#scripts-and-executables-custom-backend). |
| Start fails: *startup timed out … readiness text did not appear* | The readiness pattern never matched. Check the console for the real "ready" line; remember `(` must be escaped in regular expressions. |
| Server shows STOPPED right after its start script finished | It is a forking server configured as `foreground`. Set `"mode": "forking"` with a PID file or status check. |
| tmux server: UNKNOWN / session not found although it exists | The session belongs to another user. The manager must run as the session's owner. |
| systemd server: *not permitted to start …* | Missing polkit or sudoers rule. See [permissions.md](permissions.md#systemd). |
| systemd console says the journal cannot be read | Add the service user to `systemd-journal` and restart the service. |
| Docker server: UNKNOWN, *cannot reach the Docker daemon* | Docker is not running, or the service user is not in the `docker` group. |
| Live updates show "Offline" | The reverse proxy is buffering or closing long responses. Use the provided proxy configuration. |
| Signed out immediately after signing in (behind a proxy) | `public_url` is `https://…` but the site is opened over plain HTTP, so the Secure cookie is not sent back. Use HTTPS. |
| "Cross-origin request rejected" | The proxy does not pass the original `Host` header. Add `proxy_set_header Host $host;` (nginx). |
| Invitation links point to the wrong address | Set `public_url`. |
| Settings cannot be saved | The service user cannot write `config.json`. |
| All game servers stopped when the manager was restarted | The unit is missing `KillMode=process`. Use the provided unit file. |

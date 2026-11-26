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

- **Edit** — display name, email address, administrator role, disabled. An
  address you set is **unverified**: the user verifies it from their profile
  before it is used for notifications or password resets.
- **Disabled** accounts cannot sign in and are signed out everywhere at once;
  their open pages stop receiving updates immediately.
- **Sessions** — see where an account is signed in and revoke sessions.
- **Delete** — removes the account, its sessions and permission assignments.
  Audit entries are kept.

Safeguards: you cannot remove your own administrator role, disable or delete
yourself, and the last active administrator cannot be removed.

### Forgotten passwords

Regular users with a **verified** email address can use **Forgot password?**
on the sign-in page (when email delivery is configured). They receive a
single-use link that expires after `auth.password_reset_ttl` (1 hour by
default); setting a new password signs them out everywhere. The page answers
the same way whatever address is entered, so it cannot be used to find out
which addresses have accounts.

**Administrator accounts can never reset their password by email** — not
even if they have a verified address, and a link issued before someone was
made an administrator stops working. An administrator who is locked out
resets the password on the host, which requires local access:

```bash
sudo -u server-manager server-manager user set-password --username alice
```

That command works for any account, revokes its sessions and invalidates
pending reset links. If no administrator can sign in at all, create a new one:

```bash
sudo -u server-manager server-manager admin create --username rescue
```

### Email addresses

Addresses are used for invitations, notifications and (regular accounts)
password reset. Only **verified** addresses receive notifications and reset
links. A user verifies an address by opening an emailed, single-use link:

- **Profile → Email address → Send verification email** verifies the current
  address;
- changing the address there sends the link to the new address; the old
  address stays in use until the link is opened.

Accounts created from an invitation that was emailed to them start out
verified. When upgrading from a release without verification, only such
addresses are marked verified; everyone else verifies once from the profile.

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
| CONFIG | 32 | Edit the server's name, description, timeouts and readiness text |
| BACKUP_CREATE | 64 | Start (and cancel) backups — does **not** show the backup files |
| BACKUP_DOWNLOAD | 128 | List and download backup files — does **not** allow starting backups |

Value 16 was the former general BACKUPS permission. The upgrade converted
every grant of it into BACKUP_CREATE + BACKUP_DOWNLOAD, so nobody lost or
gained anything; the API still accepts 16 and stores it the same way.

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
  definition sets `backup.allow_user_delete` (then BACKUP_DOWNLOAD holders may).
- The category of a server is changed by administrators only: it decides
  which concurrency limit applies.
- Administrators always have everything.

### Custom action permissions

Custom actions (see below) are granted one by one, in the **Custom actions**
part of the same editors. A user can run an action when they have *any*
permission on the server (so they can see it) **and** a grant for that
action. Nothing else implies an action: START, STOP or CONSOLE_WRITE do not,
and an action grant gives nothing besides that action. This lets you allow,
say, "Save World" to someone who may only read the console. Grants of a
deleted action are removed with it.

## Custom actions

Administrators add actions to a server in its **Settings** tab (section
*Custom actions*; saved with the other settings) or through the API. An
action either runs a script/program with fixed arguments or types a fixed
command into the server's tmux session. Options: icon, description,
confirmation before running, timeout (scripts), enabled. The fields are
described in [configuration.md](configuration.md#actions--custom-actions).

Users see the actions they may run as small cards above the tabs of the
server page, with the result of the last run (and, for scripts, their
output). A script action reports *completed* or *failed*; a tmux action
reports *sent*: the command reached the console, but whether the server did
anything with it cannot be verified. An action runs at most once at a time
per server; every run is in the audit log (`ACTION_RUN`) and the Activity
tab.

## Concurrency limits and categories

The **Limits** page sets how many servers may be active at once — in total
and per category — and manages the categories themselves (name, description,
limit). Servers are assigned to a category in their Settings tab. Current
usage is shown on the dashboard and updates live.

- A server counts from the moment its start is accepted until it is
  confirmed stopped. A server in ERROR or UNKNOWN keeps counting if it was
  last seen running; servers started outside Server Manager count too.
- A refused start explains which limit was reached and shows the current
  usage. Administrators are limited as well, but can choose **Start anyway**;
  that override is recorded in the audit log (`LIMIT_OVERRIDE`).
- Limits only govern starts through Server Manager. Nothing is stopped
  because a limit is exceeded (for example by servers started by hand).
- Category names already used in server definitions appear under *Unmanaged
  categories*; **Manage** turns one into a managed category, keeping its
  servers.

## Email notifications

Users choose in their **Profile** whether they want emails when a server
they can access becomes ready (after the readiness check), stops normally,
stops unexpectedly or crashes, or when a start/stop fails — each type
separately, and per server. Everything is off by default, and only verified
addresses receive mail.

- "Crashed" is used only with evidence (non-zero exit, failed unit,
  container error); otherwise the email says the server stopped
  unexpectedly. A crash never also produces a "stopped" email.
- Reconciliation after Server Manager restarts, and temporary loss of
  monitoring (UNKNOWN), do not produce emails.
- Repeated events of the same kind for the same server are held back for
  `notifications.cooldown` (default 10 minutes); the next email says how many
  were suppressed. Each event is mailed at most once per person.
- Sending happens in the background with retries; failures are logged
  without addresses and never slow down server operations.
- Removing someone's access to a server stops their notifications about it
  immediately, including emails still waiting for a retry.
- `notifications.enabled` (Settings) turns the feature off for everyone.

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

On the **Backups** tab, users with BACKUP_DOWNLOAD see and download the
files, and users with BACKUP_CREATE start backups (if a backup command or a
tmux backup command is configured). A running script backup can be
cancelled. The list refreshes by itself when a backup finishes or new files
appear.

With the tmux method, **Create backup** types the configured command (for
example `/backup start`) into the server console. Unless a completion
pattern is configured, the operation is shown as *submitted* — the command
was delivered, but the manager cannot know when the server has finished —
and new backup files show up in the list as the server writes them.

## Audit log

**Audit log** records who did what, when, to which server, and with what
result: sign-ins (successful and failed), invitations, account and permission
changes, server definitions, every start/stop/restart request and its
outcome, console commands, backup runs and downloads, settings changes, and
attempts that were denied. Filter by user, server, action, result and time
range.

Results: `REQUESTED` (an operation was accepted), `SUCCESS`, `FAILURE`,
`DENIED`, `SUBMITTED` (a command was delivered to a tmux console; its effect
cannot be verified). Also recorded: custom action runs and changes, action
grants, category changes, concurrency limit refusals and overrides, email
verification requests, notification preference changes, password reset
requests (including refused ones, with the reason) and resets.

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
| "Forgot password?" is not shown | Email delivery is not configured, or `public_url` is missing (production). |
| A user gets no reset email | The account is an administrator, disabled, or its address is not verified. The audit log (`PASSWORD_RESET_REQUEST`, `DENIED`) says which. |
| No notification emails | The user has not opted in, the address is not verified, the server is muted, `notifications.enabled` is off, or SMTP is not configured. |
| Start refused: *… reached its limit …* | A concurrency limit (Limits page). Stop another server, raise the limit, or override as an administrator. |
| tmux backup or action: *tmux session … is not running* | The server's tmux session does not exist; start the server first. |
| Settings cannot be saved | The service user cannot write `config.json`. |
| All game servers stopped when the manager was restarted | The unit is missing `KillMode=process`. Use the provided unit file. |

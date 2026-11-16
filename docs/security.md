# Security model

Server Manager can, by design, run programs on your host. This document
states what it guarantees, how, and where the limits are.

## Trust levels

| Who | Can |
|---|---|
| Anyone on the network | Reach the login and invitation-registration pages. Nothing else. |
| A user | See and operate only the servers they hold permissions for, within those permissions. |
| An administrator | Everything in the application, including defining which commands run. **An administrator can execute arbitrary programs as the service user** — that is what configuring a server means. |
| The service user (OS) | Whatever the operating system lets that account do. This is the outer boundary. |

Give administrator rights only to people you would give a shell on the host,
and keep the service user unprivileged ([permissions.md](permissions.md)).

## Command execution

- Commands are stored as an executable plus an argument array and started
  with `execve`. No shell parses them, so no quoting or injection issues
  exist. The two places where another program insists on a string (tmux
  `pipe-pane`, and tmux `new-session` with a single argument) use
  application-controlled values with strict single-quote escaping.
- Only administrators can change anything that decides what is executed.
- Console commands from users are *data* delivered to the server's console
  (stdin, PTY, tmux pane, or one argument of a fixed helper). They must be a
  single line without control characters and are length-limited. For tmux
  they are sent with `send-keys -l`, so key names are not interpreted.
- Identifiers that end up on command lines are validated against strict
  patterns: server IDs, tmux session/window/pane names and key names, systemd
  unit names, Docker container names. tmux sessions are matched exactly.
- Executables must be absolute paths or bare names; relative paths are
  refused.
- Processes started by the manager get a cleaned environment: variables
  beginning with `SM_` (the manager's own secrets), systemd service variables
  and tmux client variables are removed.
- Helper commands run in their own process group and are killed as a group on
  timeout, so they cannot leave strays behind.

### The CONFIG permission

CONFIG lets a non-administrator edit a server's presentation and tuning:
name, description, category, timeouts, cooldown, polling intervals and the
console readiness pattern. It does **not** let them change the backend,
commands, arguments, paths, environment, console input method or backup
settings — the API rejects such changes with 403, because they would amount
to arbitrary command execution. Environment values are masked when a
non-administrator reads a configuration.

`security.config_permission_full_access: true` removes that restriction. It
exists for small trusted teams; turning it on makes every CONFIG holder
equivalent to an administrator of that server's commands.

## Authentication

- **No public registration, no default credentials.** The first administrator
  is created from a shell (`server-manager admin create`); everyone else needs
  a single-use invitation.
- **Passwords**: Argon2id (default 64 MiB, 3 passes, 2 lanes), random salt per
  password, PHC-encoded; concurrent hashing is bounded. Parameters can be
  raised later; hashes upgrade at the next sign-in.
- **Access tokens**: HS256 JWT, 10 minutes by default, held only in page
  memory. The header is fixed, so `alg` substitution is impossible. They
  carry no roles or permissions: every request loads the account and session
  from the database, so disabling a user, changing permissions or revoking a
  session takes effect on the next request, not at token expiry.
- **Refresh tokens**: 256-bit random, stored only as SHA-256 hashes, delivered
  in a cookie that is `HttpOnly`, `SameSite=Strict`, `Secure` over HTTPS and
  scoped to `/api/v1/auth`. Each use rotates the token. Presenting a token
  that was already rotated (beyond a 20-second grace window for concurrent
  tabs) is treated as theft: the session is revoked and the event audited.
- **Sessions** are tracked server-side; users can revoke their own,
  administrators anyone's. Changing a password revokes all other sessions.
- **Invitation and verification tokens** are 256-bit random, stored hashed,
  time-limited and single-use (enforced atomically in the database). Links
  carry the token in the URL fragment, which browsers do not send to servers
  or put in `Referer` headers. In production they never appear in logs.
- **Login throttling** per client address and per account; unknown users and
  wrong passwords are indistinguishable in response and timing.

## Authorization

- Every API route checks authentication and authorization on the server. The
  UI's hiding of controls is cosmetic.
- Permissions default to denied. Implications (STOP⇒START,
  CONSOLE_WRITE⇒CONSOLE_READ) are applied in one place and used everywhere.
- A server the caller has no permission for returns 404 on every route, the
  same as a nonexistent one, so IDs cannot be enumerated.
- Events are filtered per subscriber at delivery time. Console streams are
  per server and require CONSOLE_READ. When permissions, accounts or sessions
  change, open streams are re-validated immediately and closed if no longer
  allowed.
- Users cannot change their own role or permissions. An administrator cannot
  demote, disable or delete themselves, and the last active administrator
  cannot be removed.

## Web hardening

- **CSRF**: API calls authenticate with a bearer header, which browsers do not
  attach on their own. The two cookie-authenticated endpoints additionally
  require a custom header, and every state-changing request with an `Origin`
  that is not the site itself (or an explicitly allowed origin) is rejected.
- **CORS**: same-origin only unless origins are listed in
  `cors.allowed_origins`.
- **Headers** on every response: a strict Content-Security-Policy (no inline
  script or style, no third-party sources, `frame-ancestors 'none'`),
  `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
  `Referrer-Policy: no-referrer`, HSTS over HTTPS, `Cache-Control: no-store`
  for the API.
- **XSS**: the frontend builds the page with DOM APIs and `textContent`;
  server names, console output and other untrusted text are never parsed as
  HTML.
- **Input limits**: bounded request bodies, strict JSON decoding (unknown
  fields rejected), bounded console buffers and line lengths, slow stream
  consumers are disconnected rather than buffered.
- **Rate limits and cooldowns** on sign-in, refresh, API use, server
  start/stop, console commands and backups.
- **Downloads**: an authorized API call mints a random ticket bound to one
  file, valid for 60 seconds and one request. No reusable credential ever
  appears in a URL. Files are opened through a directory handle that refuses
  traversal and symlink escapes, and are checked again at download time.
- **Errors** returned to clients are deliberate messages; unexpected failures
  are logged in full and reported generically.

## Secrets and data at rest

- The JWT key and SMTP password come from the environment or mode-0600 files,
  never from `config.json`, and are never sent to the browser.
- The database, configuration, definitions and key file are created with
  mode 0600; the service refuses a key file readable by others.
- Passwords, tokens and environment values are never written to logs or the
  audit trail.

## Limitations you should know about

- **Administrators are fully trusted** with the service user's rights (see
  above). The application cannot protect the host from its own administrator.
- **Docker socket access is root-equivalent** for the service user
  ([permissions.md](permissions.md#docker)).
- **CONSOLE_WRITE is as powerful as the console it writes to.** A tmux pane at
  a shell prompt, or a game console with an "execute system command" feature,
  gives that power to whoever holds the permission.
- **External readiness checks cannot prove ownership.** A port or status
  script shows that something answers, not that it is the process the manager
  started. Native backend state is used wherever it exists.
- **PID files can be stale.** For forking servers tracked by PID file, a
  reused PID could be mistaken for the server until the file is corrected.
  Foreground servers are tracked by PID *and* process start time and are not
  affected.
- **No built-in TLS.** Terminate HTTPS in a reverse proxy and keep the
  listener on loopback.
- **Rate-limit state is in memory** and resets when the service restarts.
- **Single node.** State such as download tickets and event history lives in
  the process; running several instances against one database is not
  supported.

## Reporting a vulnerability

Please report security issues privately to the repository owner rather than
in a public issue.

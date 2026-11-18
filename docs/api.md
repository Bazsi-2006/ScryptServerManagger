# HTTP API

The complete, machine-readable description is the OpenAPI 3 document served
by every installation:

```text
GET /api/v1/openapi.yaml
```

(source: [internal/api/openapi.yaml](../internal/api/openapi.yaml); a test
fails if a route is missing from it). This page is a short tour.

All routes live under `/api/v1`. Requests and responses are JSON.

## Authentication

```bash
curl -s -c cookies.txt -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"..."}' \
  https://servers.example.com/api/v1/auth/login
```

The response contains `access_token` (valid `expires_in` seconds); the
refresh token is set as an `HttpOnly` cookie. Send the access token on every
call:

```bash
curl -s -H "Authorization: Bearer $TOKEN" https://servers.example.com/api/v1/servers
```

Get a new access token (the refresh cookie rotates each time):

```bash
curl -s -b cookies.txt -c cookies.txt -X POST -H 'X-Requested-With: server-manager' \
  https://servers.example.com/api/v1/auth/refresh
```

`/auth/refresh` and `/auth/logout` are the only cookie-authenticated
endpoints and require the `X-Requested-With: server-manager` header.

## Errors

```json
{ "error": { "code": "permission_denied", "message": "You do not have the STOP permission for this server." } }
```

`code` is stable and tells causes apart:

| HTTP | `code` | Cause |
|---|---|---|
| 400 | `invalid_request` | malformed request |
| 401 | `unauthenticated`, `token_expired`, `invalid_credentials` | not signed in |
| 403 | `permission_denied` | missing permission |
| 404 | `not_found` | unknown — or not visible to you |
| 409 | `already_running`, `not_running`, `operation_in_progress`, `server_active`, `unsupported_action` | conflict with the current state, or the backend does not support the action |
| 422 | `validation_failed` | invalid configuration; `details` lists `{field, message}` |
| 429 | `rate_limited`, `cooldown` | too fast; see `Retry-After` |
| 502 | `operation_failed` | the operation on the server failed |
| 503 | `backend_unavailable` | tmux, systemd or Docker cannot be reached |
| 504 | `command_timeout` | a command timed out |

A server whose state cannot be determined is reported in its status
(`"state": "UNKNOWN"` plus a message), not as an HTTP error.

## Servers and operations

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" https://servers.example.com/api/v1/servers/minecraft_survival/start
```

Start, stop, restart and backup answer `202 Accepted` immediately:

```json
{ "operation": { "id": 42, "type": "start", "user": "alice", "started_at": "…" },
  "status": { "server_id": "minecraft_survival", "state": "STARTING", "message": "", "since": "…", "operation": { … } } }
```

Then follow `GET /servers/{id}/status` or the event stream until `operation`
is `null`. `GET /servers/{id}/operations` returns the history with outcomes.

| Route | Permission |
|---|---|
| `GET /servers`, `GET /servers/{id}`, `…/status`, `…/operations` | any permission on the server |
| `POST …/start` | START |
| `POST …/stop`, `POST …/restart` | STOP |
| `GET …/console`, `GET …/console/stream` | CONSOLE_READ |
| `POST …/console/command` | CONSOLE_WRITE |
| `GET/POST …/backups`, `POST …/backups/{name}/download` | BACKUPS |
| `GET/PUT …/config` | CONFIG (execution settings: administrators) |
| `POST /servers`, `DELETE /servers/{id}`, `/admin/*` | administrator |

## Real-time streams (Server-Sent Events)

Both streams authenticate with the `Authorization` header — use `fetch` and
read the body, not `EventSource`. Tokens are never accepted in URLs.

```bash
curl -N -H "Authorization: Bearer $TOKEN" https://servers.example.com/api/v1/events
```

```text
id: 3f9a1c2e77aa-17
event: hello
data: {"resync":true,"time":"…"}

id: 3f9a1c2e77aa-18
event: server.status.changed
data: {"server_id":"minecraft_survival","state":"RUNNING","message":"","since":"…","operation":null}
```

- You only receive events you are authorized for (administrators: all; users:
  servers they hold permissions on).
- Events carry minimal data; re-fetch details through the API.
- Reconnect with `Last-Event-ID`. If the gap can be replayed, `hello` has
  `"resync": false` and the missed events follow. Otherwise `"resync": true`
  tells you to reload your state.
- The server sends a comment line every 20 seconds as a heartbeat, and a
  `bye` event before it closes a stream (for example when your access was
  revoked).

The console stream of one server:

```bash
curl -N -H "Authorization: Bearer $TOKEN" https://servers.example.com/api/v1/servers/minecraft_survival/console/stream
```

It sends `reset` (discard what you show), `lines` (`{"lines":[{"seq":12,"text":"…"}]}`)
and `ready` (history delivered; live from here). Event IDs are
`<epoch>:<seq>`; reconnecting with `Last-Event-ID` continues after the last
line you saw, without duplicates.

## Backup downloads

Browsers cannot attach a bearer token to a file download, so downloads use a
ticket:

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  https://servers.example.com/api/v1/servers/minecraft_survival/backups/world.tar.gz/download
```

```json
{ "url": "/api/v1/downloads/8Jc…", "expires_in": 60, "size": 73400320, "name": "world.tar.gz" }
```

`GET` that URL within 60 seconds. It works once and only for that file.

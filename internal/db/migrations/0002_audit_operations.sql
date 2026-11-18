-- Audit trail, operation history and persisted runtime state.

CREATE TABLE audit_log (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    ts        INTEGER NOT NULL,            -- unix milliseconds
    user_id   INTEGER,                     -- no FK: entries outlive accounts
    username  TEXT    NOT NULL DEFAULT '',
    server_id TEXT    NOT NULL DEFAULT '',
    action    TEXT    NOT NULL,
    result    TEXT    NOT NULL,
    ip        TEXT    NOT NULL DEFAULT '',
    details   TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_ts     ON audit_log(ts);
CREATE INDEX audit_user   ON audit_log(user_id, ts);
CREATE INDEX audit_server ON audit_log(server_id, ts);
CREATE INDEX audit_action ON audit_log(action, ts);

CREATE TABLE operations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    server_id   TEXT    NOT NULL,
    type        TEXT    NOT NULL,          -- start | stop | restart | backup
    user_id     INTEGER,
    username    TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL,          -- running | succeeded | failed | cancelled | interrupted
    message     TEXT    NOT NULL DEFAULT '',
    started_at  INTEGER NOT NULL,
    finished_at INTEGER
);
CREATE INDEX operations_server ON operations(server_id, id);

-- Small JSON blobs that let the manager re-adopt processes it started
-- before a restart (PID, process start time, ...).
CREATE TABLE server_runtime (
    server_id  TEXT    PRIMARY KEY,
    data       TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);

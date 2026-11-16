-- Core identity, session and authorization tables.

CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL COLLATE NOCASE UNIQUE,
    display_name  TEXT    NOT NULL DEFAULT '',
    email         TEXT    COLLATE NOCASE,
    password_hash TEXT    NOT NULL,
    is_admin      INTEGER NOT NULL DEFAULT 0,
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    last_login_at INTEGER
);
CREATE UNIQUE INDEX users_email_unique ON users(email) WHERE email IS NOT NULL;

-- One row per refresh-token session. Only SHA-256 hashes of tokens are stored.
CREATE TABLE sessions (
    id              TEXT    PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash      TEXT    NOT NULL UNIQUE,
    prev_token_hash TEXT,
    rotated_at      INTEGER,
    created_at      INTEGER NOT NULL,
    last_used_at    INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL,
    revoked_at      INTEGER,
    ip              TEXT    NOT NULL DEFAULT '',
    user_agent      TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX sessions_prev ON sessions(prev_token_hash);

CREATE TABLE invitations (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash   TEXT    NOT NULL UNIQUE,
    email        TEXT,
    created_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    used_at      INTEGER,
    used_by      INTEGER REFERENCES users(id) ON DELETE SET NULL,
    revoked_at   INTEGER,
    last_sent_at INTEGER
);

-- Per-server permission bitmask (see internal/perm).
CREATE TABLE server_permissions (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    server_id  TEXT    NOT NULL,
    mask       INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, server_id)
);
CREATE INDEX server_permissions_server ON server_permissions(server_id);

CREATE TABLE email_changes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    new_email  TEXT    NOT NULL,
    token_hash TEXT    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

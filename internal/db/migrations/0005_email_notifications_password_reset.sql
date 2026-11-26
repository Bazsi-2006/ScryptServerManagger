-- Email ownership, password reset and server activity notifications.

-- An address counts as verified once its owner has proven they receive mail
-- there. NULL means unverified.
ALTER TABLE users ADD COLUMN email_verified_at INTEGER;

-- Before this version only one proof was recorded: an account created from
-- an invitation that was emailed to the same address. Those addresses are
-- verified; every other existing address must be verified from the profile
-- page before it can be used for notifications or password resets.
UPDATE users SET email_verified_at = (
    SELECT i.used_at FROM invitations i
     WHERE i.used_by = users.id
       AND i.last_sent_at IS NOT NULL
       AND i.email IS NOT NULL
       AND lower(i.email) = lower(users.email)
     ORDER BY i.used_at DESC LIMIT 1)
 WHERE users.email IS NOT NULL;

-- Single-use password reset links. Only SHA-256 hashes of tokens are stored.
CREATE TABLE password_resets (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT    NOT NULL UNIQUE,
    email      TEXT    NOT NULL,   -- the address the link was sent to
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER
);
CREATE INDEX password_resets_user ON password_resets(user_id);

-- Per-user notification preferences (opt-in: no row means nothing is sent).
CREATE TABLE notification_prefs (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    enabled    INTEGER NOT NULL DEFAULT 0,
    on_ready   INTEGER NOT NULL DEFAULT 0,
    on_stopped INTEGER NOT NULL DEFAULT 0,
    on_crash   INTEGER NOT NULL DEFAULT 0,
    on_failure INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);

-- Servers a user does not want notifications about.
CREATE TABLE notification_mutes (
    user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    server_id TEXT    NOT NULL,
    PRIMARY KEY (user_id, server_id)
);

-- Delivery tracking. event_key identifies one state transition, so the
-- same event can never be mailed twice to the same person.
CREATE TABLE notification_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    event_key  TEXT    NOT NULL,
    user_id    INTEGER NOT NULL,   -- no FK: entries outlive accounts
    server_id  TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    status     TEXT    NOT NULL,   -- pending | sent | logged | failed | abandoned
    attempts   INTEGER NOT NULL DEFAULT 0,
    error      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (event_key, user_id)
);
CREATE INDEX notification_log_created ON notification_log(created_at);

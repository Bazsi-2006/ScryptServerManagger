// Package store keeps the only state that needs a database: password hashes
// and login sessions. Which users exist and what they may do is decided by
// the JSON configuration, never by this database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo
)

// ErrNotFound is returned when a credential or session does not exist.
var ErrNotFound = errors.New("store: not found")

// schemaVersion is stored in PRAGMA user_version.
const schemaVersion = 1

const schemaV1 = `
CREATE TABLE credentials (
	username      TEXT PRIMARY KEY NOT NULL,
	password_hash TEXT NOT NULL,
	updated_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE sessions (
	token_hash   TEXT PRIMARY KEY NOT NULL,
	username     TEXT NOT NULL REFERENCES credentials(username) ON DELETE CASCADE,
	csrf_token   TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX sessions_by_user ON sessions(username);
CREATE INDEX sessions_by_expiry ON sessions(expires_at);
`

// Store wraps the SQLite database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (creating if needed) the database at path. A new file and its
// directory are created accessible only by the current user.
func Open(path string) (*Store, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("store: database path %q must not contain '?' or '#'", path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: creating %s: %w", dir, err)
	}
	// Create the file ourselves so it never exists with a permissive mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	st, err := f.Stat()
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("store: %s is not a regular file", path)
	}
	if st.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("store: restricting permissions of %s: %w", path, err)
		}
	}

	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	s := &Store{db: db, path: path}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: reading schema version of %s: %w", s.path, err)
	}
	switch {
	case version == schemaVersion:
		return nil
	case version > schemaVersion:
		return fmt.Errorf("store: %s was created by a newer version (schema %d > %d)", s.path, version, schemaVersion)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer tx.Rollback()
	if version == 0 {
		if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
			return fmt.Errorf("store: creating schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return tx.Commit()
}

// ---- credentials -----------------------------------------------------------

// Credential describes a stored password (never the hash itself).
type Credential struct {
	Username  string
	UpdatedAt time.Time
	Sessions  int
}

// SetPassword stores a password hash for username and revokes all of the
// user's existing sessions.
func (s *Store) SetPassword(ctx context.Context, username, hash string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE username = ?`, username); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO credentials (username, password_hash, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(username) DO UPDATE SET password_hash = excluded.password_hash, updated_at = excluded.updated_at`,
		username, hash, now.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// PasswordHash returns the stored hash for username, or ErrNotFound.
func (s *Store) PasswordHash(ctx context.Context, username string) (string, error) {
	var h string
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM credentials WHERE username = ?`, username).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return h, err
}

// RemoveCredentials deletes the stored password and every session of username.
func (s *Store) RemoveCredentials(ctx context.Context, username string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM credentials WHERE username = ?`, username)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListCredentials lists stored passwords with their session counts.
func (s *Store) ListCredentials(ctx context.Context) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.username, c.updated_at, (SELECT COUNT(*) FROM sessions s WHERE s.username = c.username)
		FROM credentials c ORDER BY c.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		var c Credential
		var updated int64
		if err := rows.Scan(&c.Username, &updated, &c.Sessions); err != nil {
			return nil, err
		}
		c.UpdatedAt = time.Unix(updated, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- sessions --------------------------------------------------------------

// Session is a server-side login session. The browser holds the token; only
// its hash is stored.
type Session struct {
	TokenHash  string
	Username   string
	CSRFToken  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// CreateSession stores a new session and keeps at most maxPerUser sessions
// for the user by deleting the oldest ones.
func (s *Store) CreateSession(ctx context.Context, sess Session, maxPerUser int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, username, csrf_token, created_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		sess.TokenHash, sess.Username, sess.CSRFToken, sess.CreatedAt.Unix(), sess.LastSeenAt.Unix(), sess.ExpiresAt.Unix()); err != nil {
		return err
	}
	if maxPerUser > 0 {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM sessions WHERE username = ? AND token_hash NOT IN (
				SELECT token_hash FROM sessions WHERE username = ? ORDER BY created_at DESC, rowid DESC LIMIT ?)`,
			sess.Username, sess.Username, maxPerUser); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Session returns the session with the given token hash, or ErrNotFound.
func (s *Store) Session(ctx context.Context, tokenHash string) (Session, error) {
	var sess Session
	var created, seen, expires int64
	err := s.db.QueryRowContext(ctx, `
		SELECT token_hash, username, csrf_token, created_at, last_seen_at, expires_at
		FROM sessions WHERE token_hash = ?`, tokenHash).
		Scan(&sess.TokenHash, &sess.Username, &sess.CSRFToken, &created, &seen, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = time.Unix(created, 0), time.Unix(seen, 0), time.Unix(expires, 0)
	return sess, nil
}

// TouchSession records activity on a session.
func (s *Store) TouchSession(ctx context.Context, tokenHash string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, now.Unix(), tokenHash)
	return err
}

// DeleteSession removes one session.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteUserSessions removes every session of username.
func (s *Store) DeleteUserSessions(ctx context.Context, username string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE username = ?`, username)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteSessionsExcept removes the sessions of every user not in keep. It is
// used at startup so that users removed from or disabled in the
// configuration lose their sessions immediately.
func (s *Store) DeleteSessionsExcept(ctx context.Context, keep []string) (int64, error) {
	if len(keep) == 0 {
		res, err := s.db.ExecContext(ctx, `DELETE FROM sessions`)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	args := make([]any, len(keep))
	for i, u := range keep {
		args[i] = u
	}
	q := `DELETE FROM sessions WHERE username NOT IN (?` + strings.Repeat(",?", len(keep)-1) + `)`
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteExpiredSessions removes sessions past their absolute expiry or idle
// for longer than idle.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time, idle time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?`,
		now.Unix(), now.Add(-idle).Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

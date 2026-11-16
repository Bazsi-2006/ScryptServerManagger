package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Session struct {
	ID         string     `json:"id"`
	UserID     int64      `json:"user_id"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt time.Time  `json:"last_used_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	IP         string     `json:"ip"`
	UserAgent  string     `json:"user_agent"`
}

var (
	ErrSessionInvalid = errors.New("session is revoked or expired")
	ErrTokenReuse     = errors.New("refresh token reuse detected")
)

const sessionCols = `id, user_id, created_at, last_used_at, expires_at, revoked_at, ip, user_agent`

func scanSession(r scanner) (*Session, error) {
	var s Session
	var created, used, expires int64
	var revoked sql.NullInt64
	if err := r.Scan(&s.ID, &s.UserID, &created, &used, &expires, &revoked, &s.IP, &s.UserAgent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.CreatedAt, s.LastUsedAt, s.ExpiresAt, s.RevokedAt = fromUnix(created), fromUnix(used), fromUnix(expires), fromNullUnix(revoked)
	return &s, nil
}

func (s *Store) CreateSession(ctx context.Context, sess *Session, tokenHash string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sessions(id, user_id, token_hash, created_at, last_used_at, expires_at, ip, user_agent)
		VALUES (?,?,?,?,?,?,?,?)`, sess.ID, sess.UserID, tokenHash, unix(sess.CreatedAt), unix(sess.LastUsedAt), unix(sess.ExpiresAt), sess.IP, sess.UserAgent)
	return err
}

// RotateSession exchanges the presented refresh-token hash for newHash.
//
// The previous token stays acceptable for the grace period to tolerate
// concurrent refreshes from several tabs. Presenting an already-rotated
// token after the grace period is treated as theft: the session is revoked
// and ErrTokenReuse is returned.
func (s *Store) RotateSession(ctx context.Context, presentedHash, newHash string, now time.Time, grace time.Duration, newExpiry time.Time, ip string) (*Session, error) {
	var out *Session
	var reuse bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sess, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE token_hash = ?`, presentedHash))
		viaPrev := false
		if errors.Is(err, ErrNotFound) {
			sess, err = scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE prev_token_hash = ?`, presentedHash))
			viaPrev = true
		}
		if err != nil {
			return err
		}
		if sess.RevokedAt != nil || !now.Before(sess.ExpiresAt) {
			return ErrSessionInvalid
		}
		if viaPrev {
			var rotated sql.NullInt64
			if err := tx.QueryRowContext(ctx, `SELECT rotated_at FROM sessions WHERE id = ?`, sess.ID).Scan(&rotated); err != nil {
				return err
			}
			if !rotated.Valid || now.Sub(time.Unix(rotated.Int64, 0)) > grace {
				if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at = ? WHERE id = ?`, unix(now), sess.ID); err != nil {
					return err
				}
				reuse = true
				return nil // commit the revocation
			}
			// Within the grace window: issue another token but keep the
			// original rotation time so the window does not slide.
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET token_hash = ?, last_used_at = ?, expires_at = ?, ip = ? WHERE id = ?`,
				newHash, unix(now), unix(newExpiry), ip, sess.ID); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET token_hash = ?, prev_token_hash = ?, rotated_at = ?, last_used_at = ?, expires_at = ?, ip = ? WHERE id = ?`,
				newHash, presentedHash, unix(now), unix(now), unix(newExpiry), ip, sess.ID); err != nil {
				return err
			}
		}
		sess.LastUsedAt, sess.ExpiresAt, sess.IP = now.UTC().Truncate(time.Second), newExpiry.UTC().Truncate(time.Second), ip
		out = sess
		return nil
	})
	if err != nil {
		return nil, err
	}
	if reuse {
		return nil, ErrTokenReuse
	}
	return out, nil
}

func (s *Store) SessionByID(ctx context.Context, id string) (*Session, error) {
	return scanSession(s.DB.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id))
}

// SessionByTokenHash finds the session whose current token matches.
func (s *Store) SessionByTokenHash(ctx context.Context, hash string) (*Session, error) {
	return scanSession(s.DB.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE token_hash = ?`, hash))
}

// SessionActive reports whether the session exists, is not revoked and has
// not expired.
func (s *Store) SessionActive(ctx context.Context, id string, now time.Time) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ? AND revoked_at IS NULL AND expires_at > ?`, id, unix(now)).Scan(&n)
	return n > 0, err
}

// ListSessions returns the active sessions of a user, newest first.
func (s *Store) ListSessions(ctx context.Context, userID int64, now time.Time) ([]*Session, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions
		WHERE user_id = ? AND revoked_at IS NULL AND expires_at > ? ORDER BY last_used_at DESC`, userID, unix(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// RevokeSession revokes one session; it returns ErrNotFound if no active
// session with that ID exists.
func (s *Store) RevokeSession(ctx context.Context, id string, now time.Time) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, unix(now), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeUserSessions revokes every active session of the user except the one
// with ID keep (pass "" to revoke all). It returns the revoked session IDs.
func (s *Store) RevokeUserSessions(ctx context.Context, userID int64, keep string, now time.Time) ([]string, error) {
	var ids []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE user_id = ? AND revoked_at IS NULL AND id != ?`, userID, keep)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL AND id != ?`, unix(now), userID, keep)
		return err
	})
	return ids, err
}

// PurgeSessions deletes sessions that expired or were revoked before cutoff.
func (s *Store) PurgeSessions(ctx context.Context, cutoff time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ? OR (revoked_at IS NOT NULL AND revoked_at < ?)`, unix(cutoff), unix(cutoff))
	return err
}

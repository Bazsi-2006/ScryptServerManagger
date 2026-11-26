package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrResetInvalid covers unknown, used, expired and no-longer-eligible
// password reset tokens alike, so callers cannot tell them apart.
var ErrResetInvalid = errors.New("password reset link is invalid or has expired")

func deleteUnusedResets(ctx context.Context, e execer, userID int64) error {
	_, err := e.ExecContext(ctx, `DELETE FROM password_resets WHERE user_id = ? AND used_at IS NULL`, userID)
	return err
}

// CreatePasswordReset stores a new reset token for the user. Earlier unused
// tokens of the same user stop working: only the newest link is valid.
func (s *Store) CreatePasswordReset(ctx context.Context, userID int64, email, tokenHash string, now, expires time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := deleteUnusedResets(ctx, tx, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO password_resets(user_id, token_hash, email, created_at, expires_at) VALUES (?,?,?,?,?)`,
			userID, tokenHash, email, unix(now), unix(expires))
		return err
	})
}

// PasswordReset is a pending reset as seen when validating a link.
type PasswordReset struct {
	UserID    int64
	Email     string
	ExpiresAt time.Time
}

// ResetEligible decides whether an account may use an email reset link that
// was sent to email. It is evaluated again when the link is redeemed, so
// changes since it was issued (promotion to administrator, a disabled
// account, a different or unverified address) take effect.
func ResetEligible(u *User, email string) bool {
	return u != nil && !u.IsAdmin && !u.Disabled && u.EmailVerified && u.Email != "" && strings.EqualFold(u.Email, email)
}

// PasswordResetByHash returns a pending, unexpired reset whose account is
// still eligible, or ErrResetInvalid.
func (s *Store) PasswordResetByHash(ctx context.Context, tokenHash string, now time.Time) (*PasswordReset, error) {
	var r PasswordReset
	var expires int64
	err := s.DB.QueryRowContext(ctx, `SELECT user_id, email, expires_at FROM password_resets
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`, tokenHash, unix(now)).Scan(&r.UserID, &r.Email, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrResetInvalid
	}
	if err != nil {
		return nil, err
	}
	u, err := s.UserByID(ctx, r.UserID)
	if err != nil || !ResetEligible(u, r.Email) {
		return nil, ErrResetInvalid
	}
	r.ExpiresAt = fromUnix(expires)
	return &r, nil
}

// ConsumePasswordReset redeems a reset token: within one transaction it
// re-checks the token (unused, unexpired, issued for userID) and the
// account's eligibility, marks the token used, stores the new password hash
// and revokes every session of the account. The caller hashes the password
// beforehand so the slow key derivation does not hold the write lock.
func (s *Store) ConsumePasswordReset(ctx context.Context, tokenHash string, userID int64, hash string, now time.Time) (*User, []string, error) {
	var out *User
	var revoked []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var id, owner int64
		var email string
		err := tx.QueryRowContext(ctx, `SELECT id, user_id, email FROM password_resets
			WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`, tokenHash, unix(now)).Scan(&id, &owner, &email)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != userID) {
			return ErrResetInvalid
		}
		if err != nil {
			return err
		}
		u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, userID))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrResetInvalid
			}
			return err
		}
		if !ResetEligible(u, email) {
			return ErrResetInvalid
		}
		// The conditional update is what makes the token single-use even
		// under concurrent redemption attempts.
		res, err := tx.ExecContext(ctx, `UPDATE password_resets SET used_at = ? WHERE id = ? AND used_at IS NULL`, unix(now), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrResetInvalid
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, unix(now), userID); err != nil {
			return err
		}
		if err := deleteUnusedResets(ctx, tx, userID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE user_id = ? AND revoked_at IS NULL`, userID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var sid string
			if err := rows.Scan(&sid); err != nil {
				rows.Close()
				return err
			}
			revoked = append(revoked, sid)
		}
		rows.Close()
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, unix(now), userID); err != nil {
			return err
		}
		u.PasswordHash = hash
		out = u
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, revoked, nil
}

// PurgePasswordResets deletes reset tokens that expired before cutoff.
func (s *Store) PurgePasswordResets(ctx context.Context, cutoff time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM password_resets WHERE expires_at < ?`, unix(cutoff))
	return err
}

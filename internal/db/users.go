package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type User struct {
	ID           int64      `json:"id"`
	Username     string     `json:"username"`
	DisplayName  string     `json:"display_name"`
	Email        string     `json:"email"`
	PasswordHash string     `json:"-"`
	IsAdmin      bool       `json:"is_admin"`
	Disabled     bool       `json:"disabled"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	LastLoginAt  *time.Time `json:"last_login_at"`
}

const userCols = `id, username, display_name, COALESCE(email,''), password_hash, is_admin, disabled, created_at, updated_at, last_login_at`

type scanner interface{ Scan(dest ...any) error }

func scanUser(r scanner) (*User, error) {
	var u User
	var created, updated int64
	var last sql.NullInt64
	if err := r.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.PasswordHash, &u.IsAdmin, &u.Disabled, &created, &updated, &last); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt, u.UpdatedAt, u.LastLoginAt = fromUnix(created), fromUnix(updated), fromNullUnix(last)
	return &u, nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertUser(ctx context.Context, e execer, u *User, now time.Time) error {
	res, err := e.ExecContext(ctx, `INSERT INTO users(username, display_name, email, password_hash, is_admin, disabled, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`, u.Username, u.DisplayName, nullStr(u.Email), u.PasswordHash, u.IsAdmin, u.Disabled, unix(now), unix(now))
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	}
	u.ID, _ = res.LastInsertId()
	u.CreatedAt, u.UpdatedAt = now.UTC().Truncate(time.Second), now.UTC().Truncate(time.Second)
	return nil
}

// CreateUser inserts a user. ErrConflict means the username or email is taken.
func (s *Store) CreateUser(ctx context.Context, u *User) error {
	return insertUser(ctx, s.DB, u, time.Now())
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, username))
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY username COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// UserUpdate lists optional changes to a user row; nil fields are untouched.
type UserUpdate struct {
	DisplayName  *string
	Email        *string // empty string clears the address
	PasswordHash *string
	IsAdmin      *bool
	Disabled     *bool
}

var ErrLastAdmin = errors.New("cannot remove the last active administrator")

// UpdateUser applies the update atomically. It refuses changes that would
// leave the system without an enabled administrator.
func (s *Store) UpdateUser(ctx context.Context, id int64, up UserUpdate) (*User, error) {
	var out *User
	err := s.tx(ctx, func(tx *sql.Tx) error {
		u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
		if err != nil {
			return err
		}
		wasActiveAdmin := u.IsAdmin && !u.Disabled
		if up.DisplayName != nil {
			u.DisplayName = *up.DisplayName
		}
		if up.Email != nil {
			u.Email = *up.Email
		}
		if up.PasswordHash != nil {
			u.PasswordHash = *up.PasswordHash
		}
		if up.IsAdmin != nil {
			u.IsAdmin = *up.IsAdmin
		}
		if up.Disabled != nil {
			u.Disabled = *up.Disabled
		}
		if wasActiveAdmin && !(u.IsAdmin && !u.Disabled) {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE is_admin = 1 AND disabled = 0 AND id != ?`, id).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return ErrLastAdmin
			}
		}
		now := time.Now()
		if _, err := tx.ExecContext(ctx, `UPDATE users SET display_name=?, email=?, password_hash=?, is_admin=?, disabled=?, updated_at=? WHERE id=?`,
			u.DisplayName, nullStr(u.Email), u.PasswordHash, u.IsAdmin, u.Disabled, unix(now), id); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		u.UpdatedAt = now.UTC().Truncate(time.Second)
		out = u
		return nil
	})
	return out, err
}

func (s *Store) TouchLogin(ctx context.Context, id int64, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, unix(now), id)
	return err
}

// DeleteUser removes the account and (by cascade) its sessions and
// permissions. Audit entries are kept.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
		if err != nil {
			return err
		}
		if u.IsAdmin && !u.Disabled {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE is_admin = 1 AND disabled = 0 AND id != ?`, id).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return ErrLastAdmin
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
		return err
	})
}

// ---- email change verification ----

func (s *Store) CreateEmailChange(ctx context.Context, userID int64, newEmail, tokenHash string, expires time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM email_changes WHERE user_id = ?`, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO email_changes(user_id, new_email, token_hash, created_at, expires_at) VALUES (?,?,?,?,?)`,
			userID, newEmail, tokenHash, unix(time.Now()), unix(expires))
		return err
	})
}

// ConsumeEmailChange applies a pending email change identified by its token
// hash and returns the affected user.
func (s *Store) ConsumeEmailChange(ctx context.Context, tokenHash string, now time.Time) (*User, error) {
	var out *User
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var id, userID, expires int64
		var email string
		err := tx.QueryRowContext(ctx, `SELECT id, user_id, new_email, expires_at FROM email_changes WHERE token_hash = ?`, tokenHash).
			Scan(&id, &userID, &email, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM email_changes WHERE id = ?`, id); err != nil {
			return err
		}
		if now.Unix() >= expires {
			// Commit the deletion but report the token as unusable.
			out = nil
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET email = ?, updated_at = ? WHERE id = ?`, email, unix(now), userID); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, userID))
		out = u
		return err
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, ErrNotFound
	}
	return out, nil
}

// ---- per-server permissions ----

type ServerPermission struct {
	UserID   int64  `json:"user_id"`
	ServerID string `json:"server_id"`
	Mask     int    `json:"mask"`
}

// SetPermission stores the mask for (user, server); a zero mask removes it.
func (s *Store) SetPermission(ctx context.Context, userID int64, serverID string, mask int) error {
	if mask == 0 {
		_, err := s.DB.ExecContext(ctx, `DELETE FROM server_permissions WHERE user_id = ? AND server_id = ?`, userID, serverID)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO server_permissions(user_id, server_id, mask, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(user_id, server_id) DO UPDATE SET mask = excluded.mask, updated_at = excluded.updated_at`,
		userID, serverID, mask, unix(time.Now()))
	return err
}

func (s *Store) PermissionsForUser(ctx context.Context, userID int64) (map[string]int, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT server_id, mask FROM server_permissions WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var m int
		if err := rows.Scan(&id, &m); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}

func (s *Store) PermissionsForServer(ctx context.Context, serverID string) ([]ServerPermission, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT user_id, server_id, mask FROM server_permissions WHERE server_id = ?`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServerPermission
	for rows.Next() {
		var p ServerPermission
		if err := rows.Scan(&p.UserID, &p.ServerID, &p.Mask); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeleteServerPermissions(ctx context.Context, serverID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM server_permissions WHERE server_id = ?`, serverID)
	return err
}

package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Invitation struct {
	ID            int64      `json:"id"`
	Email         string     `json:"email"`
	CreatedBy     *int64     `json:"created_by"`
	CreatedByName string     `json:"created_by_name"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	UsedAt        *time.Time `json:"used_at"`
	UsedBy        *int64     `json:"used_by"`
	UsedByName    string     `json:"used_by_name"`
	RevokedAt     *time.Time `json:"revoked_at"`
	LastSentAt    *time.Time `json:"last_sent_at"`
}

// Status is one of pending, used, revoked or expired.
func (i *Invitation) Status(now time.Time) string {
	switch {
	case i.UsedAt != nil:
		return "used"
	case i.RevokedAt != nil:
		return "revoked"
	case !now.Before(i.ExpiresAt):
		return "expired"
	}
	return "pending"
}

// ErrInvitationInvalid covers unknown, used, revoked and expired invitations
// alike so callers cannot distinguish them.
var ErrInvitationInvalid = errors.New("invitation is invalid or has expired")

const invitationSelect = `SELECT i.id, COALESCE(i.email,''), i.created_by, COALESCE(c.username,''), i.created_at, i.expires_at,
	i.used_at, i.used_by, COALESCE(u.username,''), i.revoked_at, i.last_sent_at
	FROM invitations i LEFT JOIN users c ON c.id = i.created_by LEFT JOIN users u ON u.id = i.used_by`

func scanInvitation(r scanner) (*Invitation, error) {
	var i Invitation
	var created, expires int64
	var createdBy, usedBy, usedAt, revokedAt, sentAt sql.NullInt64
	if err := r.Scan(&i.ID, &i.Email, &createdBy, &i.CreatedByName, &created, &expires, &usedAt, &usedBy, &i.UsedByName, &revokedAt, &sentAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if createdBy.Valid {
		i.CreatedBy = &createdBy.Int64
	}
	if usedBy.Valid {
		i.UsedBy = &usedBy.Int64
	}
	i.CreatedAt, i.ExpiresAt = fromUnix(created), fromUnix(expires)
	i.UsedAt, i.RevokedAt, i.LastSentAt = fromNullUnix(usedAt), fromNullUnix(revokedAt), fromNullUnix(sentAt)
	return &i, nil
}

func (s *Store) CreateInvitation(ctx context.Context, tokenHash, email string, createdBy int64, expires time.Time) (*Invitation, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO invitations(token_hash, email, created_by, created_at, expires_at) VALUES (?,?,?,?,?)`,
		tokenHash, nullStr(email), createdBy, unix(time.Now()), unix(expires))
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.InvitationByID(ctx, id)
}

func (s *Store) InvitationByID(ctx context.Context, id int64) (*Invitation, error) {
	return scanInvitation(s.DB.QueryRowContext(ctx, invitationSelect+` WHERE i.id = ?`, id))
}

func (s *Store) InvitationByTokenHash(ctx context.Context, hash string) (*Invitation, error) {
	return scanInvitation(s.DB.QueryRowContext(ctx, invitationSelect+` WHERE i.token_hash = ?`, hash))
}

func (s *Store) ListInvitations(ctx context.Context) ([]*Invitation, error) {
	rows, err := s.DB.QueryContext(ctx, invitationSelect+` ORDER BY i.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Invitation
	for rows.Next() {
		i, err := scanInvitation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// RevokeInvitation revokes a pending invitation.
func (s *Store) RevokeInvitation(ctx context.Context, id int64, now time.Time) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE invitations SET revoked_at = ? WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL`, unix(now), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReissueInvitation replaces the token of a not-yet-used invitation (the old
// link stops working) and sets a new expiry.
func (s *Store) ReissueInvitation(ctx context.Context, id int64, newHash string, expires time.Time) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE invitations SET token_hash = ?, expires_at = ? WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL`,
		newHash, unix(expires), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) MarkInvitationSent(ctx context.Context, id int64, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE invitations SET last_sent_at = ? WHERE id = ?`, unix(now), id)
	return err
}

// RegisterWithInvitation atomically consumes the invitation and creates the
// account, so an invitation can never yield two users.
func (s *Store) RegisterWithInvitation(ctx context.Context, tokenHash string, u *User, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM invitations WHERE token_hash = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
			tokenHash, unix(now)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvitationInvalid
		}
		if err != nil {
			return err
		}
		if err := insertUser(ctx, tx, u, now); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE invitations SET used_at = ?, used_by = ? WHERE id = ? AND used_at IS NULL`, unix(now), u.ID, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrInvitationInvalid
		}
		return nil
	})
}

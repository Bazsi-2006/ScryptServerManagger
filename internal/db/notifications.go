package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Notification kinds. Each maps to one opt-in preference.
const (
	NotifyReady   = "ready"   // the server started and passed its readiness check
	NotifyStopped = "stopped" // the server stopped normally
	NotifyCrash   = "crash"   // the server stopped unexpectedly or failed
	NotifyFailure = "failure" // a start or stop operation failed
)

// NotificationKinds lists every kind.
var NotificationKinds = []string{NotifyReady, NotifyStopped, NotifyCrash, NotifyFailure}

func kindColumn(kind string) (string, error) {
	switch kind {
	case NotifyReady:
		return "on_ready", nil
	case NotifyStopped:
		return "on_stopped", nil
	case NotifyCrash:
		return "on_crash", nil
	case NotifyFailure:
		return "on_failure", nil
	}
	return "", fmt.Errorf("unknown notification kind %q", kind)
}

// NotificationPrefs are a user's email notification choices. Everything is
// off by default.
type NotificationPrefs struct {
	Enabled   bool `json:"enabled"`
	OnReady   bool `json:"on_ready"`
	OnStopped bool `json:"on_stopped"`
	OnCrash   bool `json:"on_crash"`
	OnFailure bool `json:"on_failure"`
}

// Wants reports whether the preferences ask for this kind.
func (p NotificationPrefs) Wants(kind string) bool {
	if !p.Enabled {
		return false
	}
	switch kind {
	case NotifyReady:
		return p.OnReady
	case NotifyStopped:
		return p.OnStopped
	case NotifyCrash:
		return p.OnCrash
	case NotifyFailure:
		return p.OnFailure
	}
	return false
}

func (s *Store) NotificationPrefs(ctx context.Context, userID int64) (NotificationPrefs, error) {
	var p NotificationPrefs
	err := s.DB.QueryRowContext(ctx, `SELECT enabled, on_ready, on_stopped, on_crash, on_failure FROM notification_prefs WHERE user_id = ?`, userID).
		Scan(&p.Enabled, &p.OnReady, &p.OnStopped, &p.OnCrash, &p.OnFailure)
	if errors.Is(err, sql.ErrNoRows) {
		return NotificationPrefs{}, nil
	}
	return p, err
}

func (s *Store) SetNotificationPrefs(ctx context.Context, userID int64, p NotificationPrefs) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO notification_prefs(user_id, enabled, on_ready, on_stopped, on_crash, on_failure, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET enabled = excluded.enabled, on_ready = excluded.on_ready, on_stopped = excluded.on_stopped,
			on_crash = excluded.on_crash, on_failure = excluded.on_failure, updated_at = excluded.updated_at`,
		userID, p.Enabled, p.OnReady, p.OnStopped, p.OnCrash, p.OnFailure, unix(time.Now()))
	return err
}

// NotificationMutes lists the servers a user muted.
func (s *Store) NotificationMutes(ctx context.Context, userID int64) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT server_id FROM notification_mutes WHERE user_id = ? ORDER BY server_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetNotificationMutes replaces the user's muted servers.
func (s *Store) SetNotificationMutes(ctx context.Context, userID int64, serverIDs []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM notification_mutes WHERE user_id = ?`, userID); err != nil {
			return err
		}
		for _, id := range serverIDs {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notification_mutes(user_id, server_id) VALUES (?,?)`, userID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// NotificationCandidate is an account that wants a kind of notification
// and has a verified address. Whether it may see the server is decided by
// the caller from IsAdmin and Mask.
type NotificationCandidate struct {
	User  *User
	Mask  int  // raw permission mask on the server (0 = none)
	Muted bool // the user muted this server
}

// NotificationCandidates returns the enabled accounts with a verified email
// address that opted in to kind, with their permission on serverID.
func (s *Store) NotificationCandidates(ctx context.Context, serverID, kind string) ([]NotificationCandidate, error) {
	col, err := kindColumn(kind)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+userColsU+`,
			COALESCE((SELECT mask FROM server_permissions sp WHERE sp.user_id = u.id AND sp.server_id = ?), 0),
			EXISTS(SELECT 1 FROM notification_mutes m WHERE m.user_id = u.id AND m.server_id = ?)
		FROM users u JOIN notification_prefs p ON p.user_id = u.id
		WHERE p.enabled = 1 AND p.`+col+` = 1 AND u.disabled = 0 AND u.email IS NOT NULL AND u.email_verified_at IS NOT NULL
		ORDER BY u.id`, serverID, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationCandidate
	for rows.Next() {
		var c NotificationCandidate
		var mask int
		var muted bool
		u, err := scanUser(scanTail{rows, []any{&mask, &muted}})
		if err != nil {
			return nil, err
		}
		c.User, c.Mask, c.Muted = u, mask, muted
		out = append(out, c)
	}
	return out, rows.Err()
}

// scanTail lets scanUser read rows that carry extra trailing columns.
type scanTail struct {
	r     scanner
	extra []any
}

func (t scanTail) Scan(dest ...any) error { return t.r.Scan(append(dest, t.extra...)...) }

// ---- delivery tracking ----

// Delivery statuses.
const (
	DeliveryPending   = "pending"
	DeliverySent      = "sent"
	DeliveryLogged    = "logged"
	DeliveryFailed    = "failed"
	DeliveryAbandoned = "abandoned"
	DeliverySkipped   = "skipped"
)

// RecordNotification registers one (event, recipient) delivery. It
// returns false when that pair was already recorded, which is how
// duplicates are prevented.
func (s *Store) RecordNotification(ctx context.Context, eventKey string, userID int64, serverID, kind string, now time.Time) (int64, bool, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO notification_log(event_key, user_id, server_id, kind, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?) ON CONFLICT(event_key, user_id) DO NOTHING`,
		eventKey, userID, serverID, kind, DeliveryPending, unix(now), unix(now))
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, false, nil
	}
	id, _ := res.LastInsertId()
	return id, true, nil
}

// UpdateNotification stores the result of a delivery attempt.
func (s *Store) UpdateNotification(ctx context.Context, id int64, status string, attempts int, errText string, now time.Time) error {
	if len(errText) > 500 {
		errText = errText[:500]
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE notification_log SET status = ?, attempts = ?, error = ?, updated_at = ? WHERE id = ?`,
		status, attempts, errText, unix(now), id)
	return err
}

// NotificationDelivery is one row of the delivery log.
type NotificationDelivery struct {
	ID        int64     `json:"id"`
	EventKey  string    `json:"event_key"`
	UserID    int64     `json:"user_id"`
	ServerID  string    `json:"server_id"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	Error     string    `json:"error"`
	CreatedAt time.Time `json:"created_at"`
}

// ListNotifications returns recent deliveries, newest first (tests and
// diagnostics).
func (s *Store) ListNotifications(ctx context.Context, limit int) ([]NotificationDelivery, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, event_key, user_id, server_id, kind, status, attempts, error, created_at
		FROM notification_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationDelivery
	for rows.Next() {
		var d NotificationDelivery
		var created int64
		if err := rows.Scan(&d.ID, &d.EventKey, &d.UserID, &d.ServerID, &d.Kind, &d.Status, &d.Attempts, &d.Error, &created); err != nil {
			return nil, err
		}
		d.CreatedAt = fromUnix(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

// AbandonPendingNotifications marks deliveries left pending by a previous
// process (its in-memory retry queue is gone).
func (s *Store) AbandonPendingNotifications(ctx context.Context, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE notification_log SET status = ?, error = 'server-manager restarted before delivery', updated_at = ? WHERE status = ?`,
		DeliveryAbandoned, unix(now), DeliveryPending)
	return err
}

// PurgeNotifications deletes delivery records older than cutoff.
func (s *Store) PurgeNotifications(ctx context.Context, cutoff time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM notification_log WHERE created_at < ?`, unix(cutoff))
	return err
}

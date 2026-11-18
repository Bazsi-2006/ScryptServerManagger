package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type AuditEntry struct {
	ID       int64          `json:"id"`
	Time     time.Time      `json:"time"`
	UserID   *int64         `json:"user_id"`
	Username string         `json:"username"`
	ServerID string         `json:"server_id"`
	Action   string         `json:"action"`
	Result   string         `json:"result"`
	IP       string         `json:"ip"`
	Details  map[string]any `json:"details"`
}

func (s *Store) InsertAudit(ctx context.Context, e *AuditEntry) error {
	details := "{}"
	if len(e.Details) > 0 {
		if b, err := json.Marshal(e.Details); err == nil {
			details = string(b)
		}
	}
	var uid sql.NullInt64
	if e.UserID != nil {
		uid = sql.NullInt64{Int64: *e.UserID, Valid: true}
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO audit_log(ts, user_id, username, server_id, action, result, ip, details) VALUES (?,?,?,?,?,?,?,?)`,
		e.Time.UnixMilli(), uid, e.Username, e.ServerID, e.Action, e.Result, e.IP, details)
	return err
}

type AuditFilter struct {
	UserID   *int64
	Username string
	ServerID string
	Action   string
	Result   string
	From     *time.Time
	To       *time.Time
	BeforeID int64
	Limit    int
}

// QueryAudit returns entries matching the filter, newest first.
func (s *Store) QueryAudit(ctx context.Context, f AuditFilter) ([]*AuditEntry, error) {
	var where []string
	var args []any
	add := func(cond string, v any) { where = append(where, cond); args = append(args, v) }
	if f.UserID != nil {
		add("user_id = ?", *f.UserID)
	}
	if f.Username != "" {
		add("username = ? COLLATE NOCASE", f.Username)
	}
	if f.ServerID != "" {
		add("server_id = ?", f.ServerID)
	}
	if f.Action != "" {
		add("action = ?", f.Action)
	}
	if f.Result != "" {
		add("result = ?", f.Result)
	}
	if f.From != nil {
		add("ts >= ?", f.From.UnixMilli())
	}
	if f.To != nil {
		add("ts <= ?", f.To.UnixMilli())
	}
	if f.BeforeID > 0 {
		add("id < ?", f.BeforeID)
	}
	q := `SELECT id, ts, user_id, username, server_id, action, result, ip, details FROM audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var ts int64
		var uid sql.NullInt64
		var details string
		if err := rows.Scan(&e.ID, &ts, &uid, &e.Username, &e.ServerID, &e.Action, &e.Result, &e.IP, &details); err != nil {
			return nil, err
		}
		e.Time = time.UnixMilli(ts).UTC()
		if uid.Valid {
			e.UserID = &uid.Int64
		}
		e.Details = map[string]any{}
		_ = json.Unmarshal([]byte(details), &e.Details)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// AuditActions lists the distinct action names present in the log.
func (s *Store) AuditActions(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT action FROM audit_log ORDER BY action`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) PurgeAudit(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM audit_log WHERE ts < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- operation history ----

type Operation struct {
	ID         int64      `json:"id"`
	ServerID   string     `json:"server_id"`
	Type       string     `json:"type"`
	UserID     *int64     `json:"user_id"`
	Username   string     `json:"username"`
	Status     string     `json:"status"`
	Message    string     `json:"message"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

func (s *Store) InsertOperation(ctx context.Context, op *Operation) error {
	var uid sql.NullInt64
	if op.UserID != nil {
		uid = sql.NullInt64{Int64: *op.UserID, Valid: true}
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO operations(server_id, type, user_id, username, status, message, started_at) VALUES (?,?,?,?,?,?,?)`,
		op.ServerID, op.Type, uid, op.Username, op.Status, op.Message, op.StartedAt.UnixMilli())
	if err != nil {
		return err
	}
	op.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) FinishOperation(ctx context.Context, id int64, status, message string, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE operations SET status = ?, message = ?, finished_at = ? WHERE id = ?`, status, message, now.UnixMilli(), id)
	return err
}

// InterruptRunningOperations marks operations left "running" by a previous
// process as interrupted. Called once at startup.
func (s *Store) InterruptRunningOperations(ctx context.Context, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE operations SET status = 'interrupted', message = 'server-manager was restarted while this operation was in progress', finished_at = ?
		WHERE status = 'running'`, now.UnixMilli())
	return err
}

func (s *Store) ListOperations(ctx context.Context, serverID string, limit int) ([]*Operation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, server_id, type, user_id, username, status, message, started_at, finished_at
		FROM operations WHERE server_id = ? ORDER BY id DESC LIMIT ?`, serverID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Operation{}
	for rows.Next() {
		var op Operation
		var uid, fin sql.NullInt64
		var started int64
		if err := rows.Scan(&op.ID, &op.ServerID, &op.Type, &uid, &op.Username, &op.Status, &op.Message, &started, &fin); err != nil {
			return nil, err
		}
		if uid.Valid {
			op.UserID = &uid.Int64
		}
		op.StartedAt = time.UnixMilli(started).UTC()
		if fin.Valid {
			t := time.UnixMilli(fin.Int64).UTC()
			op.FinishedAt = &t
		}
		out = append(out, &op)
	}
	return out, rows.Err()
}

func (s *Store) PurgeOperations(ctx context.Context, before time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM operations WHERE started_at < ? AND status != 'running'`, before.UnixMilli())
	return err
}

// ---- persisted runtime state ----

func (s *Store) GetRuntime(ctx context.Context, serverID string) (string, error) {
	var data string
	err := s.DB.QueryRowContext(ctx, `SELECT data FROM server_runtime WHERE server_id = ?`, serverID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return data, err
}

func (s *Store) SetRuntime(ctx context.Context, serverID, data string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO server_runtime(server_id, data, updated_at) VALUES (?,?,?)
		ON CONFLICT(server_id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`, serverID, data, unix(time.Now()))
	return err
}

func (s *Store) DeleteRuntime(ctx context.Context, serverID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM server_runtime WHERE server_id = ?`, serverID)
	return err
}

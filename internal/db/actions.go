package db

import (
	"context"
	"strings"
	"time"
)

// ActionGrant allows one user to run one custom action of one server.
type ActionGrant struct {
	UserID   int64  `json:"user_id"`
	ServerID string `json:"server_id"`
	ActionID string `json:"action_id"`
}

// SetActionPermission grants (allowed=true) or revokes a custom action.
func (s *Store) SetActionPermission(ctx context.Context, userID int64, serverID, actionID string, allowed bool) error {
	if !allowed {
		_, err := s.DB.ExecContext(ctx, `DELETE FROM action_permissions WHERE user_id = ? AND server_id = ? AND action_id = ?`, userID, serverID, actionID)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO action_permissions(user_id, server_id, action_id, granted_at) VALUES (?,?,?,?)
		ON CONFLICT(user_id, server_id, action_id) DO NOTHING`, userID, serverID, actionID, unix(time.Now()))
	return err
}

// ActionPermissionsForUser returns the user's grants as server -> action set.
func (s *Store) ActionPermissionsForUser(ctx context.Context, userID int64) (map[string]map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT server_id, action_id FROM action_permissions WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var srv, act string
		if err := rows.Scan(&srv, &act); err != nil {
			return nil, err
		}
		if out[srv] == nil {
			out[srv] = map[string]bool{}
		}
		out[srv][act] = true
	}
	return out, rows.Err()
}

// ActionPermissionsForServer lists every grant on a server's actions.
func (s *Store) ActionPermissionsForServer(ctx context.Context, serverID string) ([]ActionGrant, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT user_id, server_id, action_id FROM action_permissions WHERE server_id = ? ORDER BY action_id, user_id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ActionGrant{}
	for rows.Next() {
		var g ActionGrant
		if err := rows.Scan(&g.UserID, &g.ServerID, &g.ActionID); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// PruneActionPermissions deletes grants for actions of serverID that no
// longer exist (keep lists the current action IDs), so a later action that
// reuses an old ID does not inherit stale grants. It returns how many
// grants were removed.
func (s *Store) PruneActionPermissions(ctx context.Context, serverID string, keep []string) (int64, error) {
	q := `DELETE FROM action_permissions WHERE server_id = ?`
	args := []any{serverID}
	if len(keep) > 0 {
		q += ` AND action_id NOT IN (?` + strings.Repeat(",?", len(keep)-1) + `)`
		for _, k := range keep {
			args = append(args, k)
		}
	}
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FinishActionOperation records the outcome of an action run, including the
// tail of its output.
func (s *Store) FinishActionOperation(ctx context.Context, id int64, status, message, output string, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE operations SET status = ?, message = ?, output = ?, finished_at = ? WHERE id = ?`,
		status, message, output, now.UnixMilli(), id)
	return err
}

// LastActionRuns returns the most recent run of each action of a server,
// keyed by action ID, output included.
func (s *Store) LastActionRuns(ctx context.Context, serverID string) (map[string]*Operation, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+operationCols+`, o.output FROM operations o
		WHERE o.server_id = ? AND o.type = 'action' AND o.id = (
			SELECT MAX(id) FROM operations WHERE server_id = o.server_id AND type = 'action' AND action_id = o.action_id)`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*Operation{}
	for rows.Next() {
		var output string
		op, err := scanOperation(rows, &output)
		if err != nil {
			return nil, err
		}
		op.Output = output
		out[op.ActionID] = op
	}
	return out, rows.Err()
}

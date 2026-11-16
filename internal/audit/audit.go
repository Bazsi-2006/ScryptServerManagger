// Package audit records security-relevant actions in the persistent audit
// trail (SQLite) and mirrors them to the application log.
package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
)

// Action names. They are stable identifiers used for filtering.
const (
	LoginSuccess       = "LOGIN"
	Logout             = "LOGOUT"
	TokenReuse         = "REFRESH_TOKEN_REUSE"
	Register           = "USER_REGISTER"
	PasswordChange     = "PASSWORD_CHANGE"
	ProfileUpdate      = "PROFILE_UPDATE"
	EmailChangeRequest = "EMAIL_CHANGE_REQUEST"
	EmailChange        = "EMAIL_CHANGE"
	SessionRevoke      = "SESSION_REVOKE"

	InvitationCreate = "INVITATION_CREATE"
	InvitationRevoke = "INVITATION_REVOKE"
	InvitationResend = "INVITATION_RESEND"

	UserUpdate       = "USER_UPDATE"
	UserDisable      = "USER_DISABLE"
	UserEnable       = "USER_ENABLE"
	UserDelete       = "USER_DELETE"
	UserAdminGrant   = "USER_ADMIN_GRANT"
	UserAdminRevoke  = "USER_ADMIN_REVOKE"
	PermissionChange = "PERMISSION_CHANGE"

	ServerCreate   = "SERVER_CREATE"
	ServerUpdate   = "SERVER_UPDATE"
	ServerDelete   = "SERVER_DELETE"
	ServerStart    = "SERVER_START"
	ServerStop     = "SERVER_STOP"
	ServerRestart  = "SERVER_RESTART"
	ConsoleCommand = "CONSOLE_COMMAND"
	BackupCreate   = "BACKUP_CREATE"
	BackupDownload = "BACKUP_DOWNLOAD"
	BackupDelete   = "BACKUP_DELETE"
	BackupCancel   = "BACKUP_CANCEL"

	SettingsUpdate = "SETTINGS_UPDATE"
	ServersReload  = "SERVERS_RELOAD"
)

// Results.
const (
	Success   = "SUCCESS"
	Failure   = "FAILURE"
	Denied    = "DENIED"
	Requested = "REQUESTED"
)

// Actor identifies who performed an action. The zero value means "system".
type Actor struct {
	UserID   int64
	Username string
	IP       string
}

// Recorder writes audit entries. Callers must never put secrets (passwords,
// tokens, environment values) into details.
type Recorder struct {
	store *db.Store
	log   *slog.Logger
}

func NewRecorder(store *db.Store, log *slog.Logger) *Recorder {
	return &Recorder{store: store, log: log}
}

// Record stores one entry. Failures are logged, never returned: auditing
// must not make the audited operation fail halfway.
func (r *Recorder) Record(actor Actor, serverID, action, result string, details map[string]any) {
	e := &db.AuditEntry{Time: time.Now(), Username: actor.Username, ServerID: serverID, Action: action, Result: result, IP: actor.IP, Details: details}
	if actor.UserID != 0 {
		id := actor.UserID
		e.UserID = &id
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.store.InsertAudit(ctx, e); err != nil {
		r.log.Error("failed to write audit entry", "action", action, "error", err)
	}
	attrs := []any{"action", action, "result", result}
	if actor.Username != "" {
		attrs = append(attrs, "user", actor.Username)
	}
	if serverID != "" {
		attrs = append(attrs, "server", serverID)
	}
	r.log.Info("audit", attrs...)
}

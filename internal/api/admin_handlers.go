package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	mailer "github.com/Bazsi-2006/ScryptServerManagger/internal/mail"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/perm"
)

func pathID(r *http.Request, name string) (int64, *apiError) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || v <= 0 {
		return 0, errNotFound
	}
	return v, nil
}

// ---- users ----

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if users == nil {
		users = []*db.User{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	uid, aerr := pathID(r, "uid")
	if aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	u, err := s.Store.UserByID(r.Context(), uid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	perms, err := s.Store.PermissionsForUser(r.Context(), uid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sessions, err := s.sessionViews(r.Context(), uid, s.ident(r).SessionID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "permissions": perms, "sessions": sessions})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	uid, aerr := pathID(r, "uid")
	if aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	var req struct {
		DisplayName *string `json:"display_name"`
		Email       *string `json:"email"`
		IsAdmin     *bool   `json:"is_admin"`
		Disabled    *bool   `json:"disabled"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	me := s.ident(r).User
	before, err := s.Store.UserByID(r.Context(), uid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Administrators cannot change their own role or disable themselves;
	// another administrator has to do it.
	if uid == me.ID && (req.IsAdmin != nil && !*req.IsAdmin || req.Disabled != nil && *req.Disabled) {
		s.fail(w, r, errf(http.StatusConflict, "self_modification", "You cannot remove your own administrator role or disable your own account."))
		return
	}
	up := db.UserUpdate{IsAdmin: req.IsAdmin, Disabled: req.Disabled}
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if name == "" || !validDisplayName(name) {
			s.fail(w, r, badRequest("The display name must be 1-80 characters without control characters."))
			return
		}
		up.DisplayName = &name
	}
	if req.Email != nil {
		email := strings.TrimSpace(*req.Email)
		if email != "" {
			norm, ok := normalizeEmail(email)
			if !ok {
				s.fail(w, r, badRequest("Please enter a valid email address."))
				return
			}
			email = norm
		}
		up.Email = &email
	}
	after, err := s.Store.UpdateUser(r.Context(), uid, up)
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			s.fail(w, r, errf(http.StatusConflict, "already_exists", "That email address is already in use."))
			return
		}
		s.fail(w, r, err)
		return
	}
	actor := s.actor(r)
	target := map[string]any{"target_user": after.Username, "target_user_id": after.ID}
	if before.Disabled != after.Disabled {
		if after.Disabled {
			// A disabled account must lose access at once.
			_, _ = s.Store.RevokeUserSessions(r.Context(), uid, "", time.Now())
			s.Audit.Record(actor, "", audit.UserDisable, audit.Success, target)
		} else {
			s.Audit.Record(actor, "", audit.UserEnable, audit.Success, target)
		}
	}
	if before.IsAdmin != after.IsAdmin {
		if after.IsAdmin {
			s.Audit.Record(actor, "", audit.UserAdminGrant, audit.Success, target)
		} else {
			s.Audit.Record(actor, "", audit.UserAdminRevoke, audit.Success, target)
		}
	}
	if before.DisplayName != after.DisplayName || before.Email != after.Email {
		s.Audit.Record(actor, "", audit.UserUpdate, audit.Success, target)
	}
	s.Hub.Revalidate()
	s.notifyAdmins("users.changed")
	s.notifyUser(uid, "permissions.changed")
	writeJSON(w, http.StatusOK, map[string]any{"user": after})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	uid, aerr := pathID(r, "uid")
	if aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	if uid == s.ident(r).User.ID {
		s.fail(w, r, errf(http.StatusConflict, "self_modification", "You cannot delete your own account."))
		return
	}
	u, err := s.Store.UserByID(r.Context(), uid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.DeleteUser(r.Context(), uid); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), "", audit.UserDelete, audit.Success, map[string]any{"target_user": u.Username, "target_user_id": u.ID})
	s.Hub.Revalidate()
	s.notifyAdmins("users.changed")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetPermission(w http.ResponseWriter, r *http.Request) {
	uid, aerr := pathID(r, "uid")
	if aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	serverID := r.PathValue("id")
	var req struct {
		Mask *int `json:"mask"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Mask == nil || !perm.Mask(*req.Mask).Valid() {
		s.fail(w, r, badRequest("mask must be a combination of the defined permission bits (0-%d).", int(perm.All)))
		return
	}
	u, err := s.Store.UserByID(r.Context(), uid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.Manager.Definition(serverID); err != nil {
		s.fail(w, r, err)
		return
	}
	old, err := s.Store.PermissionsForUser(r.Context(), uid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetPermission(r.Context(), uid, serverID, *req.Mask); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), serverID, audit.PermissionChange, audit.Success, map[string]any{
		"target_user": u.Username, "target_user_id": u.ID,
		"old": perm.Mask(old[serverID]).String(), "new": perm.Mask(*req.Mask).String(),
	})
	// Reload identities first so the affected user's streams reflect the
	// new permissions before they are told to refresh.
	s.Hub.Revalidate()
	s.notifyUser(uid, "permissions.changed")
	s.notifyAdmins("users.changed")
	writeJSON(w, http.StatusOK, map[string]any{"user_id": uid, "server_id": serverID, "mask": *req.Mask, "effective": int(perm.Mask(*req.Mask).Effective())})
}

// handleServerPermissions lists every non-administrator with their mask for
// one server (administrators implicitly hold everything).
func (s *Server) handleServerPermissions(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, err := s.Manager.Definition(serverID); err != nil {
		s.fail(w, r, err)
		return
	}
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	perms, err := s.Store.PermissionsForServer(r.Context(), serverID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	masks := map[int64]int{}
	for _, p := range perms {
		masks[p.UserID] = p.Mask
	}
	type row struct {
		UserID      int64  `json:"user_id"`
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Disabled    bool   `json:"disabled"`
		IsAdmin     bool   `json:"is_admin"`
		Mask        int    `json:"mask"`
	}
	out := []row{}
	for _, u := range users {
		out = append(out, row{UserID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Disabled: u.Disabled, IsAdmin: u.IsAdmin, Mask: masks[u.ID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"permissions": out})
}

func (s *Server) handleRevokeUserSessions(w http.ResponseWriter, r *http.Request) {
	uid, aerr := pathID(r, "uid")
	if aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	u, err := s.Store.UserByID(r.Context(), uid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	keep := ""
	if uid == s.ident(r).User.ID {
		keep = s.ident(r).SessionID
	}
	var revoked int
	if sid := r.PathValue("sid"); sid != "" {
		sess, err := s.Store.SessionByID(r.Context(), sid)
		if err != nil || sess.UserID != uid {
			s.fail(w, r, errNotFound)
			return
		}
		if err := s.Store.RevokeSession(r.Context(), sid, time.Now()); err != nil {
			s.fail(w, r, err)
			return
		}
		revoked = 1
	} else {
		ids, err := s.Store.RevokeUserSessions(r.Context(), uid, keep, time.Now())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		revoked = len(ids)
	}
	s.Audit.Record(s.actor(r), "", audit.SessionRevoke, audit.Success, map[string]any{"target_user": u.Username, "target_user_id": u.ID, "count": revoked})
	s.Hub.Revalidate()
	writeJSON(w, http.StatusOK, map[string]any{"revoked": revoked})
}

// ---- invitations ----

type invitationView struct {
	*db.Invitation
	Status string `json:"status"`
}

func (s *Server) handleListInvitations(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListInvitations(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := time.Now()
	out := make([]invitationView, 0, len(list))
	for _, inv := range list {
		out = append(out, invitationView{Invitation: inv, Status: inv.Status(now)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitations": out})
}

// deliverInvitation emails the invitation when it has a recipient. The
// invitation itself is already stored; a delivery problem is reported in
// the response and never invalidates it. The link is returned to the
// administrator only when it did not reach the recipient by email.
func (s *Server) deliverInvitation(r *http.Request, inv *db.Invitation, token string) map[string]any {
	out := map[string]any{"invitation": invitationView{Invitation: inv, Status: inv.Status(time.Now())}}
	base, err := s.publicBase(r)
	if err != nil {
		out["email"] = map[string]any{"attempted": false, "sent": false, "error": err.Error()}
		out["token"] = token
		return out
	}
	url := link(base, "register", token)
	if inv.Email == "" {
		out["link"] = url
		out["email"] = map[string]any{"attempted": false, "sent": false}
		return out
	}
	body := fmt.Sprintf("Hello,\n\nyou have been invited to create an account on Server Manager.\n\nOpen this link to register:\n\n%s\n\nThe invitation can be used once and expires on %s.\n\nIf you were not expecting this invitation, you can ignore this message.\n",
		url, inv.ExpiresAt.Format("2 January 2006 at 15:04 MST"))
	outcome, err := s.Mailer.Send(r.Context(), mailer.Message{To: inv.Email, Subject: "You have been invited to Server Manager", Body: body})
	switch {
	case err != nil:
		reason := "The invitation email could not be sent."
		if errors.Is(err, mailer.ErrNotConfigured) {
			reason = "Email delivery is not configured."
		} else {
			s.Log.Warn("invitation email could not be sent", "invitation", inv.ID, "error", err)
		}
		out["email"] = map[string]any{"attempted": true, "sent": false, "error": reason + " Share the link manually."}
		out["link"] = url
	case outcome == mailer.Logged:
		out["email"] = map[string]any{"attempted": true, "sent": false, "logged": true}
		out["link"] = url
	default:
		_ = s.Store.MarkInvitationSent(r.Context(), inv.ID, time.Now())
		now := time.Now().UTC()
		inv.LastSentAt = &now
		out["email"] = map[string]any{"attempted": true, "sent": true}
	}
	return out
}

func (s *Server) inviteExpiry(hours *int) (time.Time, *apiError) {
	ttl := s.Config.Get().Auth.InvitationTTL.D()
	if hours != nil {
		if *hours < 1 || *hours > 90*24 {
			return time.Time{}, badRequest("expires_in_hours must be between 1 and 2160.")
		}
		ttl = time.Duration(*hours) * time.Hour
	}
	return time.Now().Add(ttl), nil
}

func (s *Server) handleCreateInvitation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email          string `json:"email"`
		ExpiresInHours *int   `json:"expires_in_hours"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	email := ""
	if strings.TrimSpace(req.Email) != "" {
		norm, ok := normalizeEmail(req.Email)
		if !ok {
			s.fail(w, r, badRequest("Please enter a valid email address."))
			return
		}
		email = norm
	}
	expires, aerr := s.inviteExpiry(req.ExpiresInHours)
	if aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	token := auth.MustToken(32)
	inv, err := s.Store.CreateInvitation(r.Context(), auth.HashToken(token), email, s.ident(r).User.ID, expires)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), "", audit.InvitationCreate, audit.Success, map[string]any{"invitation_id": inv.ID, "has_email": email != ""})
	out := s.deliverInvitation(r, inv, token)
	delete(out, "token")
	s.notifyAdmins("invitations.changed")
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleRevokeInvitation(w http.ResponseWriter, r *http.Request) {
	id, aerr := pathID(r, "iid")
	if aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	if err := s.Store.RevokeInvitation(r.Context(), id, time.Now()); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			s.fail(w, r, errf(http.StatusConflict, "not_pending", "This invitation has already been used or revoked."))
			return
		}
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), "", audit.InvitationRevoke, audit.Success, map[string]any{"invitation_id": id})
	s.notifyAdmins("invitations.changed")
	w.WriteHeader(http.StatusNoContent)
}

// handleResendInvitation issues a fresh token (only hashes are stored, so
// the old link cannot be re-sent and stops working) and delivers it again.
func (s *Server) handleResendInvitation(w http.ResponseWriter, r *http.Request) {
	id, aerr := pathID(r, "iid")
	if aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	var req struct {
		ExpiresInHours *int `json:"expires_in_hours"`
	}
	if r.ContentLength != 0 {
		if err := s.decode(w, r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	expires, aerr := s.inviteExpiry(req.ExpiresInHours)
	if aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	token := auth.MustToken(32)
	if err := s.Store.ReissueInvitation(r.Context(), id, auth.HashToken(token), expires); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			s.fail(w, r, errf(http.StatusConflict, "not_pending", "This invitation has already been used or revoked."))
			return
		}
		s.fail(w, r, err)
		return
	}
	inv, err := s.Store.InvitationByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), "", audit.InvitationResend, audit.Success, map[string]any{"invitation_id": id})
	out := s.deliverInvitation(r, inv, token)
	delete(out, "token")
	s.notifyAdmins("invitations.changed")
	writeJSON(w, http.StatusOK, out)
}

// ---- audit log ----

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := db.AuditFilter{Username: q.Get("username"), ServerID: q.Get("server_id"), Action: q.Get("action"), Result: q.Get("result")}
	if v := q.Get("user_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			s.fail(w, r, badRequest("user_id must be a number."))
			return
		}
		f.UserID = &id
	}
	parseTime := func(name string) (*time.Time, *apiError) {
		v := q.Get(name)
		if v == "" {
			return nil, nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, badRequest("%s must be an RFC 3339 timestamp.", name)
		}
		return &t, nil
	}
	var aerr *apiError
	if f.From, aerr = parseTime("from"); aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	if f.To, aerr = parseTime("to"); aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	if v := q.Get("before_id"); v != "" {
		f.BeforeID, _ = strconv.ParseInt(v, 10, 64)
	}
	f.Limit = 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			s.fail(w, r, badRequest("limit must be between 1 and 500."))
			return
		}
		f.Limit = n
	}
	entries, err := s.Store.QueryAudit(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	actions, err := s.Store.AuditActions(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]any{"entries": entries, "actions": actions, "next_before_id": nil}
	if len(entries) == f.Limit {
		out["next_before_id"] = entries[len(entries)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- application settings ----

// settingsView is the editable subset of the configuration. It contains no
// secrets: passwords and signing keys live in the environment or secret
// files and are never sent to clients.
type settingsView struct {
	PublicURL      string            `json:"public_url"`
	TrustedProxies []string          `json:"trusted_proxies"`
	Auth           settingsAuth      `json:"auth"`
	SMTP           settingsSMTP      `json:"smtp"`
	CORS           config.CORS       `json:"cors"`
	RateLimits     config.RateLimits `json:"rate_limits"`
	Monitoring     config.Monitoring `json:"monitoring"`
	Console        config.Console    `json:"console"`
	Backups        config.Backups    `json:"backups"`
	Logging        settingsLogging   `json:"logging"`
	Audit          config.Audit      `json:"audit"`
	Security       config.Security   `json:"security"`
}

type settingsAuth struct {
	AccessTokenTTL    config.Duration `json:"access_token_ttl"`
	RefreshTokenTTL   config.Duration `json:"refresh_token_ttl"`
	InvitationTTL     config.Duration `json:"invitation_ttl"`
	EmailChangeTTL    config.Duration `json:"email_change_ttl"`
	MinPasswordLength int             `json:"min_password_length"`
	CookieSecure      string          `json:"cookie_secure"`
}

type settingsSMTP struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	From     string `json:"from"`
	TLS      string `json:"tls"`
}

type settingsLogging struct {
	Level string `json:"level"`
}

func viewSettings(c *config.Config) settingsView {
	return settingsView{
		PublicURL: c.PublicURL, TrustedProxies: append([]string{}, c.TrustedProxies...),
		Auth: settingsAuth{AccessTokenTTL: c.Auth.AccessTokenTTL, RefreshTokenTTL: c.Auth.RefreshTokenTTL, InvitationTTL: c.Auth.InvitationTTL,
			EmailChangeTTL: c.Auth.EmailChangeTTL, MinPasswordLength: c.Auth.MinPasswordLength, CookieSecure: c.Auth.CookieSecure},
		SMTP: settingsSMTP{Host: c.SMTP.Host, Port: c.SMTP.Port, Username: c.SMTP.Username, From: c.SMTP.From, TLS: c.SMTP.TLS},
		CORS: config.CORS{AllowedOrigins: append([]string{}, c.CORS.AllowedOrigins...)}, RateLimits: c.RateLimits,
		Monitoring: c.Monitoring, Console: c.Console, Backups: c.Backups, Logging: settingsLogging{Level: c.Logging.Level},
		Audit: c.Audit, Security: c.Security,
	}
}

func (s *Server) settingsPayload() map[string]any {
	c := s.Config.Get()
	pw, pwErr := c.SMTPPassword()
	return map[string]any{
		"settings": viewSettings(c),
		// Read-only context: where things live and what needs a restart or
		// a config-file edit. Paths are shown to administrators only.
		"info": map[string]any{
			"mode": c.Mode, "listen": c.Listen, "version": s.Version, "config_file": s.Config.Path(),
			"paths": c.Paths, "tools": c.Tools,
			"smtp_password_set": pw != "" && pwErr == nil, "smtp_configured": c.SMTP.Configured(),
			"invalid_server_files": s.Manager.InvalidFiles(),
		},
	}
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsPayload())
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsView
	// Start from the current values so omitted sections stay unchanged.
	req = viewSettings(s.Config.Get())
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	err := s.Config.Update(func(c *config.Config) error {
		c.PublicURL = strings.TrimSpace(req.PublicURL)
		c.TrustedProxies = req.TrustedProxies
		c.Auth.AccessTokenTTL, c.Auth.RefreshTokenTTL = req.Auth.AccessTokenTTL, req.Auth.RefreshTokenTTL
		c.Auth.InvitationTTL, c.Auth.EmailChangeTTL = req.Auth.InvitationTTL, req.Auth.EmailChangeTTL
		c.Auth.MinPasswordLength, c.Auth.CookieSecure = req.Auth.MinPasswordLength, req.Auth.CookieSecure
		c.SMTP.Host, c.SMTP.Port, c.SMTP.Username = strings.TrimSpace(req.SMTP.Host), req.SMTP.Port, req.SMTP.Username
		c.SMTP.From, c.SMTP.TLS = strings.TrimSpace(req.SMTP.From), req.SMTP.TLS
		c.CORS = req.CORS
		c.RateLimits, c.Monitoring, c.Console, c.Backups = req.RateLimits, req.Monitoring, req.Console, req.Backups
		c.Logging.Level = req.Logging.Level
		c.Audit, c.Security = req.Audit, req.Security
		return nil
	})
	if err != nil {
		var ve *config.ValidationError
		if !errors.As(err, &ve) {
			s.Log.Error("could not save settings", "error", err)
			s.Audit.Record(s.actor(r), "", audit.SettingsUpdate, audit.Failure, nil)
			s.fail(w, r, errf(http.StatusInternalServerError, "settings_not_saved", "The settings could not be written to the configuration file. Check that the service user may write to it."))
			return
		}
		s.fail(w, r, err)
		return
	}
	if s.SetLevel != nil {
		s.SetLevel(s.Config.Get().Logging.Level)
	}
	s.Audit.Record(s.actor(r), "", audit.SettingsUpdate, audit.Success, nil)
	s.notifyAdmins("settings.changed")
	writeJSON(w, http.StatusOK, s.settingsPayload())
}

func (s *Server) handleTestEmail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To string `json:"to"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	to, ok := normalizeEmail(req.To)
	if !ok {
		s.fail(w, r, badRequest("Please enter a valid email address."))
		return
	}
	outcome, err := s.Mailer.Send(r.Context(), mailer.Message{To: to, Subject: "Server Manager test message",
		Body: "This is a test message from Server Manager. If you can read it, email delivery works.\n"})
	if err != nil {
		if errors.Is(err, mailer.ErrNotConfigured) {
			s.fail(w, r, errf(http.StatusConflict, "email_unavailable", "SMTP is not configured (host and sender address are required)."))
			return
		}
		// Administrators need the SMTP error to fix their configuration.
		s.fail(w, r, errf(http.StatusBadGateway, "email_failed", "%s.", capitalize(err.Error())))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": outcome == mailer.Sent, "logged": outcome == mailer.Logged})
}

func (s *Server) handleReloadServers(w http.ResponseWriter, r *http.Request) {
	res, err := s.Manager.Reload()
	if err != nil {
		s.Log.Error("reload of server definitions failed", "error", err)
		s.fail(w, r, errf(http.StatusInternalServerError, "reload_failed", "The server definitions directory could not be read."))
		return
	}
	s.Audit.Record(s.actor(r), "", audit.ServersReload, audit.Success, map[string]any{
		"added": len(res.Added), "removed": len(res.Removed), "updated": len(res.Updated), "invalid": len(res.Invalid)})
	s.Hub.Revalidate()
	writeJSON(w, http.StatusOK, res)
}

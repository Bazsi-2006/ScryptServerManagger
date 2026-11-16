package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/events"
	mailer "github.com/Bazsi-2006/ScryptServerManagger/internal/mail"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/perm"
)

const (
	refreshCookie = "sm_refresh"
	cookiePath    = "/api/v1/auth"
	// refreshGrace tolerates concurrent refreshes from several tabs.
	refreshGrace = 20 * time.Second
)

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,31}$`)

func (s *Server) argonParams() auth.Params {
	p := s.Config.Get().Auth.Argon2
	return auth.Params{MemoryKiB: p.MemoryKiB, Iterations: p.Iterations, Parallelism: p.Parallelism}
}

func (s *Server) cookieSecure(r *http.Request) bool {
	c := s.Config.Get()
	switch c.Auth.CookieSecure {
	case "always":
		return true
	case "never":
		return false
	}
	return strings.HasPrefix(c.PublicURL, "https://") || s.requestSecure(r)
}

func (s *Server) setRefreshCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookie, Value: token, Path: cookiePath, Expires: expires,
		MaxAge: int(time.Until(expires).Seconds()), HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearRefreshCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookie, Value: "", Path: cookiePath, MaxAge: -1, Expires: time.Unix(1, 0),
		HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteStrictMode,
	})
}

// checkPassword enforces the password policy.
func (s *Server) checkPassword(password, username string) *apiError {
	min := s.Config.Get().Auth.MinPasswordLength
	switch {
	case len([]rune(password)) < min:
		return badRequest("The password must be at least %d characters long.", min)
	case len(password) > 1024:
		return badRequest("The password is too long.")
	case username != "" && strings.EqualFold(password, username):
		return badRequest("The password must not be the same as the username.")
	}
	return nil
}

func validDisplayName(name string) bool {
	if len(name) > 80 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func normalizeEmail(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 254 {
		return "", false
	}
	a, err := mail.ParseAddress(v)
	if err != nil || a.Address != v || !strings.Contains(v, "@") {
		return "", false
	}
	return strings.ToLower(v), true
}

// permissionsView returns the caller's effective masks for existing servers.
func (s *Server) permissionsView(ctx context.Context, u *db.User) (map[string]int, error) {
	out := map[string]int{}
	if u.IsAdmin {
		for _, d := range s.Manager.Definitions() {
			out[d.ID] = int(perm.All)
		}
		return out, nil
	}
	raw, err := s.Store.PermissionsForUser(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	for id, m := range raw {
		if eff := perm.Mask(m).Effective(); eff.Any() {
			out[id] = int(eff)
		}
	}
	return out, nil
}

func (s *Server) features(u *db.User) map[string]any {
	c := s.Config.Get()
	return map[string]any{
		"host_metrics":        u.IsAdmin || c.Monitoring.HostMetricsForUsers,
		"metrics_interval_ms": c.Monitoring.HostMetricsInterval.D().Milliseconds(),
		"email_available":     s.Mailer.Available(),
		"min_password_length": c.Auth.MinPasswordLength,
		"max_command_length":  c.Console.MaxCommandLength,
		"config_full_access":  u.IsAdmin || c.Security.ConfigPermissionFullAccess,
		"development":         c.Development(),
		"version":             s.Version,
	}
}

func (s *Server) mePayload(ctx context.Context, u *db.User) (map[string]any, error) {
	perms, err := s.permissionsView(ctx, u)
	if err != nil {
		return nil, err
	}
	return map[string]any{"user": u, "permissions": perms, "features": s.features(u)}, nil
}

// issue creates the access token response for an established session.
func (s *Server) issue(w http.ResponseWriter, r *http.Request, u *db.User, sessionID string, status int) {
	ttl := s.Config.Get().Auth.AccessTokenTTL.D()
	token, _ := s.Signer.Issue(u.ID, sessionID, time.Now(), ttl)
	payload, err := s.mePayload(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	payload["access_token"] = token
	payload["token_type"] = "Bearer"
	payload["expires_in"] = int(ttl.Seconds())
	payload["session_id"] = sessionID
	writeJSON(w, status, payload)
}

// newSession creates a refresh session and sets its cookie.
func (s *Server) newSession(w http.ResponseWriter, r *http.Request, u *db.User) (string, error) {
	now := time.Now()
	refresh := auth.MustToken(32)
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	sess := &db.Session{ID: auth.MustToken(16), UserID: u.ID, CreatedAt: now, LastUsedAt: now,
		ExpiresAt: now.Add(s.Config.Get().Auth.RefreshTokenTTL.D()), IP: clientIP(r), UserAgent: ua}
	if err := s.Store.CreateSession(r.Context(), sess, auth.HashToken(refresh)); err != nil {
		return "", err
	}
	s.setRefreshCookie(w, r, refresh, sess.ExpiresAt)
	return sess.ID, nil
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	ip := clientIP(r)
	rl := s.Config.Get().RateLimits.Login
	// Limit by address and by account, so neither a single host hammering
	// many accounts nor many hosts hammering one account gets far.
	if !s.rateLimit(w, r, "login:ip:"+ip, rl) {
		return
	}
	if req.Username == "" || req.Password == "" || len(req.Username) > 64 || len(req.Password) > 1024 {
		s.fail(w, r, errf(http.StatusUnauthorized, "invalid_credentials", "Invalid username or password."))
		return
	}
	userKey := "login:user:" + strings.ToLower(req.Username)
	if !s.rateLimit(w, r, userKey, rl) {
		return
	}
	actor := audit.Actor{Username: req.Username, IP: ip}
	u, err := s.Store.UserByUsername(r.Context(), req.Username)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	if u == nil {
		// Spend the same effort as for a real account.
		auth.VerifyPassword(req.Password, s.dummyHash, s.argonParams())
		s.Audit.Record(actor, "", audit.LoginSuccess, audit.Failure, map[string]any{"reason": "unknown user"})
		s.fail(w, r, errf(http.StatusUnauthorized, "invalid_credentials", "Invalid username or password."))
		return
	}
	actor.UserID, actor.Username = u.ID, u.Username
	ok, rehash := auth.VerifyPassword(req.Password, u.PasswordHash, s.argonParams())
	if !ok {
		s.Audit.Record(actor, "", audit.LoginSuccess, audit.Failure, map[string]any{"reason": "wrong password"})
		s.fail(w, r, errf(http.StatusUnauthorized, "invalid_credentials", "Invalid username or password."))
		return
	}
	if u.Disabled {
		s.Audit.Record(actor, "", audit.LoginSuccess, audit.Failure, map[string]any{"reason": "account disabled"})
		s.fail(w, r, errf(http.StatusForbidden, "account_disabled", "This account has been disabled."))
		return
	}
	if rehash {
		if h, err := auth.HashPassword(req.Password, s.argonParams()); err == nil {
			_, _ = s.Store.UpdateUser(r.Context(), u.ID, db.UserUpdate{PasswordHash: &h})
		}
	}
	sid, err := s.newSession(w, r, u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := time.Now()
	_ = s.Store.TouchLogin(r.Context(), u.ID, now)
	u.LastLoginAt = &now
	s.limiter.Reset(userKey)
	s.Audit.Record(actor, "", audit.LoginSuccess, audit.Success, nil)
	s.issue(w, r, u, sid, http.StatusOK)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if aerr := requireXHR(r); aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	ip := clientIP(r)
	if !s.rateLimit(w, r, "refresh:ip:"+ip, s.Config.Get().RateLimits.Refresh) {
		return
	}
	c, err := r.Cookie(refreshCookie)
	if err != nil || c.Value == "" {
		s.fail(w, r, errUnauthenticated)
		return
	}
	now := time.Now()
	next := auth.MustToken(32)
	expires := now.Add(s.Config.Get().Auth.RefreshTokenTTL.D())
	sess, err := s.Store.RotateSession(r.Context(), auth.HashToken(c.Value), auth.HashToken(next), now, refreshGrace, expires, ip)
	if err != nil {
		s.clearRefreshCookie(w, r)
		switch {
		case errors.Is(err, db.ErrTokenReuse):
			// An already-rotated token was replayed: assume theft, kill the
			// session and cut its live streams.
			s.Audit.Record(audit.Actor{IP: ip}, "", audit.TokenReuse, audit.Denied, nil)
			s.Hub.Revalidate()
			s.fail(w, r, errUnauthenticated)
		case errors.Is(err, db.ErrNotFound), errors.Is(err, db.ErrSessionInvalid):
			s.fail(w, r, errUnauthenticated)
		default:
			s.fail(w, r, err)
		}
		return
	}
	u, err := s.Store.UserByID(r.Context(), sess.UserID)
	if err != nil || u.Disabled {
		_ = s.Store.RevokeSession(r.Context(), sess.ID, now)
		s.clearRefreshCookie(w, r)
		s.fail(w, r, errUnauthenticated)
		return
	}
	s.setRefreshCookie(w, r, next, expires)
	s.issue(w, r, u, sess.ID, http.StatusOK)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if aerr := requireXHR(r); aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	now := time.Now()
	var sess *db.Session
	if c, err := r.Cookie(refreshCookie); err == nil && c.Value != "" {
		sess, _ = s.Store.SessionByTokenHash(r.Context(), auth.HashToken(c.Value))
	}
	if sess == nil {
		// Fall back to the access token's session.
		if id, aerr := s.authenticate(r); aerr == nil {
			sess, _ = s.Store.SessionByID(r.Context(), id.SessionID)
		}
	}
	if sess != nil {
		if err := s.Store.RevokeSession(r.Context(), sess.ID, now); err == nil {
			actor := audit.Actor{UserID: sess.UserID, IP: clientIP(r)}
			if u, err := s.Store.UserByID(r.Context(), sess.UserID); err == nil {
				actor.Username = u.Username
			}
			s.Audit.Record(actor, "", audit.Logout, audit.Success, nil)
			s.Hub.Revalidate()
		}
	}
	s.clearRefreshCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// handleInvitationCheck lets the registration page verify a token before
// showing the form. The token travels in the body, never in the URL.
func (s *Server) handleInvitationCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !s.rateLimit(w, r, "invite:ip:"+clientIP(r), s.Config.Get().RateLimits.Login) {
		return
	}
	inv, err := s.Store.InvitationByTokenHash(r.Context(), auth.HashToken(req.Token))
	if err != nil || inv.Status(time.Now()) != "pending" {
		s.fail(w, r, errf(http.StatusNotFound, "invitation_invalid", "This invitation is invalid or has expired."))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid": true, "email": inv.Email, "expires_at": inv.ExpiresAt,
		"min_password_length": s.Config.Get().Auth.MinPasswordLength,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token       string `json:"token"`
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	ip := clientIP(r)
	if !s.rateLimit(w, r, "register:ip:"+ip, s.Config.Get().RateLimits.Login) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if !usernameRe.MatchString(req.Username) {
		s.fail(w, r, badRequest("The username must be 3-32 characters: letters, digits, '.', '_' or '-', starting with a letter or digit."))
		return
	}
	if !validDisplayName(req.DisplayName) {
		s.fail(w, r, badRequest("The display name is invalid."))
		return
	}
	if aerr := s.checkPassword(req.Password, req.Username); aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	invalid := errf(http.StatusNotFound, "invitation_invalid", "This invitation is invalid or has expired.")
	hash := auth.HashToken(req.Token)
	inv, err := s.Store.InvitationByTokenHash(r.Context(), hash)
	if err != nil || inv.Status(time.Now()) != "pending" {
		s.fail(w, r, invalid)
		return
	}
	pw, err := auth.HashPassword(req.Password, s.argonParams())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if req.DisplayName == "" {
		req.DisplayName = req.Username
	}
	// The invitation's address (if any) was proven by receiving the link.
	u := &db.User{Username: req.Username, DisplayName: req.DisplayName, Email: strings.ToLower(inv.Email), PasswordHash: pw}
	if err := s.Store.RegisterWithInvitation(r.Context(), hash, u, time.Now()); err != nil {
		switch {
		case errors.Is(err, db.ErrInvitationInvalid):
			s.fail(w, r, invalid)
		case errors.Is(err, db.ErrConflict):
			s.fail(w, r, errf(http.StatusConflict, "already_exists", "That username or email address is already in use."))
		default:
			s.fail(w, r, err)
		}
		return
	}
	s.Audit.Record(audit.Actor{UserID: u.ID, Username: u.Username, IP: ip}, "", audit.Register, audit.Success, map[string]any{"invitation_id": inv.ID})
	s.notifyAdmins("users.changed")
	s.notifyAdmins("invitations.changed")
	sid, err := s.newSession(w, r, u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.issue(w, r, u, sid, http.StatusCreated)
}

func (s *Server) notifyAdmins(typ string) {
	s.Hub.Publish(events.Event{Type: typ, Audience: events.AudienceAdmins, Data: map[string]any{}})
}

func (s *Server) notifyUser(userID int64, typ string) {
	s.Hub.Publish(events.Event{Type: typ, Audience: events.AudienceUser, UserID: userID, Data: map[string]any{}})
}

// publicBase returns the base URL used in emailed links. In production it
// must be configured: deriving it from the Host header would let a forged
// request poison links sent to other people.
func (s *Server) publicBase(r *http.Request) (string, error) {
	c := s.Config.Get()
	if c.PublicURL != "" {
		return strings.TrimRight(c.PublicURL, "/"), nil
	}
	if !c.Development() {
		return "", errf(http.StatusConflict, "public_url_missing", "public_url must be configured before links can be generated.")
	}
	scheme := "http"
	if s.requestSecure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host, nil
}

// link builds a frontend link. The token sits in the URL fragment, which
// browsers do not send to servers or include in Referer headers.
func link(base, route, token string) string {
	return base + "/#/" + route + "?token=" + url.QueryEscape(token)
}

func (s *Server) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !s.rateLimit(w, r, "verify:ip:"+clientIP(r), s.Config.Get().RateLimits.Login) {
		return
	}
	u, err := s.Store.ConsumeEmailChange(r.Context(), auth.HashToken(req.Token), time.Now())
	if err != nil {
		switch {
		case errors.Is(err, db.ErrNotFound):
			s.fail(w, r, errf(http.StatusNotFound, "token_invalid", "This verification link is invalid or has expired."))
		case errors.Is(err, db.ErrConflict):
			s.fail(w, r, errf(http.StatusConflict, "already_exists", "That email address is already in use by another account."))
		default:
			s.fail(w, r, err)
		}
		return
	}
	s.Audit.Record(audit.Actor{UserID: u.ID, Username: u.Username, IP: clientIP(r)}, "", audit.EmailChange, audit.Success, nil)
	s.notifyUser(u.ID, "profile.changed")
	s.notifyAdmins("users.changed")
	writeJSON(w, http.StatusOK, map[string]any{"email": u.Email})
}

// ---- profile ----

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	payload, err := s.mePayload(r.Context(), s.ident(r).User)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	payload["session_id"] = s.ident(r).SessionID
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DisplayName *string `json:"display_name"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	id := s.ident(r)
	if req.DisplayName == nil {
		s.fail(w, r, badRequest("Nothing to update."))
		return
	}
	name := strings.TrimSpace(*req.DisplayName)
	if name == "" || !validDisplayName(name) {
		s.fail(w, r, badRequest("The display name must be 1-80 characters without control characters."))
		return
	}
	u, err := s.Store.UpdateUser(r.Context(), id.User.ID, db.UserUpdate{DisplayName: &name})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), "", audit.ProfileUpdate, audit.Success, nil)
	s.notifyAdmins("users.changed")
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

// verifyCurrentPassword re-authenticates sensitive profile changes.
func (s *Server) verifyCurrentPassword(w http.ResponseWriter, r *http.Request, password string) bool {
	id := s.ident(r)
	if !s.rateLimit(w, r, "reauth:user:"+itoa(id.User.ID), s.Config.Get().RateLimits.Login) {
		return false
	}
	if ok, _ := auth.VerifyPassword(password, id.User.PasswordHash, s.argonParams()); !ok {
		s.fail(w, r, errf(http.StatusForbidden, "invalid_credentials", "The current password is incorrect."))
		return false
	}
	return true
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	id := s.ident(r)
	if !s.verifyCurrentPassword(w, r, req.Current) {
		s.Audit.Record(s.actor(r), "", audit.PasswordChange, audit.Denied, nil)
		return
	}
	if aerr := s.checkPassword(req.New, id.User.Username); aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	if req.New == req.Current {
		s.fail(w, r, badRequest("The new password must differ from the current one."))
		return
	}
	h, err := auth.HashPassword(req.New, s.argonParams())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.Store.UpdateUser(r.Context(), id.User.ID, db.UserUpdate{PasswordHash: &h}); err != nil {
		s.fail(w, r, err)
		return
	}
	// Whoever knew the old password loses access everywhere else.
	revoked, _ := s.Store.RevokeUserSessions(r.Context(), id.User.ID, id.SessionID, time.Now())
	s.Audit.Record(s.actor(r), "", audit.PasswordChange, audit.Success, map[string]any{"sessions_revoked": len(revoked)})
	s.Hub.Revalidate()
	writeJSON(w, http.StatusOK, map[string]any{"sessions_revoked": len(revoked)})
}

func (s *Server) handleRequestEmailChange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"current_password"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	id := s.ident(r)
	email, ok := normalizeEmail(req.Email)
	if !ok {
		s.fail(w, r, badRequest("Please enter a valid email address."))
		return
	}
	if strings.EqualFold(email, id.User.Email) {
		s.fail(w, r, badRequest("That is already your email address."))
		return
	}
	if !s.verifyCurrentPassword(w, r, req.Password) {
		return
	}
	if !s.Mailer.Available() {
		s.fail(w, r, errf(http.StatusConflict, "email_unavailable", "Email delivery is not configured, so the address cannot be verified. Ask an administrator to update it."))
		return
	}
	base, err := s.publicBase(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	token := auth.MustToken(32)
	ttl := s.Config.Get().Auth.EmailChangeTTL.D()
	if err := s.Store.CreateEmailChange(r.Context(), id.User.ID, email, auth.HashToken(token), time.Now().Add(ttl)); err != nil {
		s.fail(w, r, err)
		return
	}
	body := fmt.Sprintf("Hello %s,\n\nplease confirm this email address for your Server Manager account by opening the link below:\n\n%s\n\nThe link expires in %s. If you did not request this change, you can ignore this message.\n",
		id.User.DisplayName, link(base, "verify-email", token), humanDuration(ttl))
	if _, err := s.Mailer.Send(r.Context(), mailer.Message{To: email, Subject: "Confirm your email address", Body: body}); err != nil {
		s.Log.Warn("email change verification could not be sent", "error", err)
		s.Audit.Record(s.actor(r), "", audit.EmailChangeRequest, audit.Failure, map[string]any{"reason": "delivery failed"})
		s.fail(w, r, errf(http.StatusBadGateway, "email_failed", "The verification email could not be sent. Your address was not changed."))
		return
	}
	s.Audit.Record(s.actor(r), "", audit.EmailChangeRequest, audit.Success, nil)
	writeJSON(w, http.StatusAccepted, map[string]any{"pending_email": email})
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
	return d.String()
}

type sessionView struct {
	*db.Session
	Current bool `json:"current"`
}

func (s *Server) sessionViews(ctx context.Context, userID int64, current string) ([]sessionView, error) {
	list, err := s.Store.ListSessions(ctx, userID, time.Now())
	if err != nil {
		return nil, err
	}
	out := make([]sessionView, 0, len(list))
	for _, x := range list {
		out = append(out, sessionView{Session: x, Current: x.ID == current})
	}
	return out, nil
}

func (s *Server) handleMySessions(w http.ResponseWriter, r *http.Request) {
	id := s.ident(r)
	out, err := s.sessionViews(r.Context(), id.User.ID, id.SessionID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) handleRevokeMySession(w http.ResponseWriter, r *http.Request) {
	id := s.ident(r)
	sess, err := s.Store.SessionByID(r.Context(), r.PathValue("sid"))
	if err != nil || sess.UserID != id.User.ID {
		s.fail(w, r, errNotFound)
		return
	}
	if err := s.Store.RevokeSession(r.Context(), sess.ID, time.Now()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), "", audit.SessionRevoke, audit.Success, map[string]any{"scope": "own"})
	s.Hub.Revalidate()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	id := s.ident(r)
	revoked, err := s.Store.RevokeUserSessions(r.Context(), id.User.ID, id.SessionID, time.Now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), "", audit.SessionRevoke, audit.Success, map[string]any{"scope": "own-others", "count": len(revoked)})
	s.Hub.Revalidate()
	writeJSON(w, http.StatusOK, map[string]any{"revoked": len(revoked)})
}

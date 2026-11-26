package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	mailer "github.com/Bazsi-2006/ScryptServerManagger/internal/mail"
)

// ---- email verification ----

// handleSendVerification emails a verification link for the caller's
// current, not yet verified address. (Changing the address goes through
// POST /me/email, which verifies the new address the same way.)
func (s *Server) handleSendVerification(w http.ResponseWriter, r *http.Request) {
	id := s.ident(r)
	u := id.User
	if u.Email == "" {
		s.fail(w, r, badRequest("Your account has no email address yet. Add one first."))
		return
	}
	if u.EmailVerified {
		s.fail(w, r, errf(http.StatusConflict, "already_verified", "Your email address is already verified."))
		return
	}
	if !s.rateLimit(w, r, "verify-send:user:"+itoa(u.ID), s.Config.Get().RateLimits.PasswordReset) {
		return
	}
	if !s.Mailer.Available() {
		s.fail(w, r, errf(http.StatusConflict, "email_unavailable", "Email delivery is not configured, so the address cannot be verified."))
		return
	}
	base, err := s.publicBase(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	token := auth.MustToken(32)
	ttl := s.Config.Get().Auth.EmailChangeTTL.D()
	if err := s.Store.CreateEmailChange(r.Context(), u.ID, u.Email, auth.HashToken(token), time.Now().Add(ttl)); err != nil {
		s.fail(w, r, err)
		return
	}
	body := fmt.Sprintf("Hello %s,\n\nplease confirm that this email address belongs to your Server Manager account by opening the link below:\n\n%s\n\nThe link expires in %s. If you did not ask for this, you can ignore this message.\n",
		u.DisplayName, link(base, "verify-email", token), humanDuration(ttl))
	if _, err := s.Mailer.Send(r.Context(), mailer.Message{To: u.Email, Subject: "Confirm your email address", Body: body}); err != nil {
		s.Log.Warn("verification email could not be sent", "user_id", u.ID, "error", err)
		s.Audit.Record(s.actor(r), "", audit.EmailVerifyRequest, audit.Failure, map[string]any{"reason": "delivery failed"})
		s.fail(w, r, errf(http.StatusBadGateway, "email_failed", "The verification email could not be sent."))
		return
	}
	s.Audit.Record(s.actor(r), "", audit.EmailVerifyRequest, audit.Success, nil)
	writeJSON(w, http.StatusAccepted, map[string]any{"sent_to": u.Email})
}

// ---- notification preferences ----

type notificationSettings struct {
	db.NotificationPrefs
	// Muted lists servers the user does not want emails about.
	Muted []string `json:"muted"`
}

func (s *Server) notificationPayload(ctx context.Context, u *db.User) (map[string]any, error) {
	prefs, err := s.Store.NotificationPrefs(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	muted, err := s.Store.NotificationMutes(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	c := s.Config.Get()
	return map[string]any{
		"preferences": notificationSettings{NotificationPrefs: prefs, Muted: muted},
		// Context the profile page explains to the user.
		"email":            u.Email,
		"email_verified":   u.EmailVerified,
		"email_available":  s.Mailer.Available(),
		"globally_enabled": c.Notifications.Enabled,
	}, nil
}

func (s *Server) handleGetNotifications(w http.ResponseWriter, r *http.Request) {
	out, err := s.notificationPayload(r.Context(), s.ident(r).User)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePutNotifications(w http.ResponseWriter, r *http.Request) {
	var req notificationSettings
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	id := s.ident(r)
	// Only servers the caller can see can be muted; anything else is
	// dropped rather than revealing which IDs exist.
	muted := []string{}
	seen := map[string]bool{}
	for _, sid := range req.Muted {
		if seen[sid] || len(muted) >= 1000 {
			continue
		}
		seen[sid] = true
		if def, err := s.Manager.Definition(sid); err == nil {
			if mask, err := s.effective(r, id, def.ID); err == nil && mask.Any() {
				muted = append(muted, def.ID)
			}
		}
	}
	sort.Strings(muted)
	if err := s.Store.SetNotificationPrefs(r.Context(), id.User.ID, req.NotificationPrefs); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetNotificationMutes(r.Context(), id.User.ID, muted); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), "", audit.NotificationPrefsUpdate, audit.Success, map[string]any{
		"enabled": req.Enabled, "on_ready": req.OnReady, "on_stopped": req.OnStopped, "on_crash": req.OnCrash, "on_failure": req.OnFailure, "muted": len(muted)})
	out, err := s.notificationPayload(r.Context(), id.User)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- password reset (regular accounts only) ----

// resetRequested is the one response to every reset request, whatever the
// outcome, so the endpoint reveals nothing about accounts.
const resetRequested = "If an eligible account exists for this email address, password reset instructions will be sent."

// handleForgotPassword starts an email password reset. The answer is the
// same for unknown addresses, administrators, unverified or disabled
// accounts and real resets; the lookup and the email happen in the
// background so response timing does not differ either.
func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	ip := clientIP(r)
	rl := s.Config.Get().RateLimits.PasswordReset
	if !s.rateLimit(w, r, "pwreset:ip:"+ip, rl) {
		return
	}
	email, ok := normalizeEmail(req.Email)
	if !ok {
		s.fail(w, r, badRequest("Please enter a valid email address."))
		return
	}
	if !s.rateLimit(w, r, "pwreset:email:"+email, rl) {
		return
	}
	base, baseErr := s.publicBase(r)
	if baseErr == nil && s.Mailer.Available() {
		go s.sendPasswordReset(email, base, ip)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"message": resetRequested})
}

func (s *Server) sendPasswordReset(email, base, ip string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	u, err := s.Store.UserByEmail(ctx, email)
	if err != nil {
		return // unknown address: nothing to do, nothing to record
	}
	actor := audit.Actor{UserID: u.ID, Username: u.Username, IP: ip}
	if !db.ResetEligible(u, email) {
		reason := "not eligible"
		switch {
		case u.IsAdmin:
			reason = "administrator accounts cannot reset their password by email"
		case u.Disabled:
			reason = "account disabled"
		case !u.EmailVerified:
			reason = "email address not verified"
		}
		s.Audit.Record(actor, "", audit.PasswordResetRequest, audit.Denied, map[string]any{"reason": reason})
		return
	}
	token := auth.MustToken(32)
	now := time.Now()
	ttl := s.Config.Get().Auth.PasswordResetTTL.D()
	if err := s.Store.CreatePasswordReset(ctx, u.ID, u.Email, auth.HashToken(token), now, now.Add(ttl)); err != nil {
		s.Log.Error("could not store password reset", "user_id", u.ID, "error", err)
		return
	}
	body := fmt.Sprintf("Hello %s,\n\nsomeone (hopefully you) asked to reset the password of the Server Manager account %q.\n\nOpen this link to choose a new password:\n\n%s\n\nThe link can be used once and expires in %s. When the password has been reset, you are signed out everywhere.\n\nIf you did not request this, ignore this message; your password stays unchanged.\n",
		u.DisplayName, u.Username, link(base, "reset-password", token), humanDuration(ttl))
	if _, err := s.Mailer.Send(ctx, mailer.Message{To: u.Email, Subject: "Reset your Server Manager password", Body: body}); err != nil {
		s.Log.Warn("password reset email could not be sent", "user_id", u.ID, "error", err)
		s.Audit.Record(actor, "", audit.PasswordResetRequest, audit.Failure, map[string]any{"reason": "delivery failed"})
		return
	}
	s.Audit.Record(actor, "", audit.PasswordResetRequest, audit.Success, nil)
}

var errResetInvalid = &apiError{Status: http.StatusNotFound, Code: "token_invalid", Message: "This password reset link is invalid or has expired."}

// handleCheckReset lets the reset page validate a link before showing the
// form. The token travels in the body, never in a URL.
func (s *Server) handleCheckReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !s.rateLimit(w, r, "pwreset-check:ip:"+clientIP(r), s.Config.Get().RateLimits.Login) {
		return
	}
	reset, err := s.Store.PasswordResetByHash(r.Context(), auth.HashToken(req.Token), time.Now())
	if err != nil {
		if errors.Is(err, db.ErrResetInvalid) {
			s.fail(w, r, errResetInvalid)
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "expires_at": reset.ExpiresAt, "min_password_length": s.Config.Get().Auth.MinPasswordLength})
}

// handleResetPassword redeems a reset link: the token, its expiry and the
// account's eligibility (not an administrator, not disabled, same verified
// address) are all checked again at this moment.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	ip := clientIP(r)
	if !s.rateLimit(w, r, "pwreset-redeem:ip:"+ip, s.Config.Get().RateLimits.Login) {
		return
	}
	hash := auth.HashToken(req.Token)
	reset, err := s.Store.PasswordResetByHash(r.Context(), hash, time.Now())
	if err != nil {
		if errors.Is(err, db.ErrResetInvalid) {
			s.fail(w, r, errResetInvalid)
			return
		}
		s.fail(w, r, err)
		return
	}
	u, err := s.Store.UserByID(r.Context(), reset.UserID)
	if err != nil {
		s.fail(w, r, errResetInvalid)
		return
	}
	if aerr := s.checkPassword(req.Password, u.Username); aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	pw, err := auth.HashPassword(req.Password, s.argonParams())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u, revoked, err := s.Store.ConsumePasswordReset(r.Context(), hash, reset.UserID, pw, time.Now())
	if err != nil {
		if errors.Is(err, db.ErrResetInvalid) {
			s.fail(w, r, errResetInvalid)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(audit.Actor{UserID: u.ID, Username: u.Username, IP: ip}, "", audit.PasswordReset, audit.Success,
		map[string]any{"sessions_revoked": len(revoked)})
	s.Hub.Revalidate()
	writeJSON(w, http.StatusOK, map[string]any{"reset": true, "sessions_revoked": len(revoked)})
}

// passwordResetAvailable reports whether the forgot-password flow can work.
func (s *Server) passwordResetAvailable() bool {
	c := s.Config.Get()
	return s.Mailer.Available() && (c.PublicURL != "" || c.Development())
}

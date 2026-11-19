package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/store"
)

type ctxKey int

const sessionKey ctxKey = 1

// session is the authenticated session attached to a request.
type session struct {
	username  string
	csrf      string
	tokenHash string
}

func sessionFrom(ctx context.Context) *session {
	s, _ := ctx.Value(sessionKey).(*session)
	return s
}

// ---- cookies ---------------------------------------------------------------

func (s *Server) secureCookies(r *http.Request) bool {
	switch s.cfg.CookieSecure {
	case config.CookieSecureAlways:
		return true
	case config.CookieSecureNever:
		return false
	default:
		return clientInfoFor(r, s.cfg.TrustedProxies).HTTPS
	}
}

// cookieName uses the __Host- prefix when possible (Secure, Path=/, no
// Domain), which stops sibling subdomains from planting a session cookie,
// and __Secure- when the app lives under a path prefix.
func (s *Server) cookieName(secure bool) string {
	switch {
	case secure && s.cfg.BasePath == "":
		return "__Host-vmctl_session"
	case secure:
		return "__Secure-vmctl_session"
	default:
		return "vmctl_session"
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAge time.Duration) {
	secure := s.secureCookies(r)
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(secure),
		Value:    token,
		Path:     basePathOr(s.cfg.BasePath),
		MaxAge:   int(maxAge / time.Second),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	secure := s.secureCookies(r)
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(secure),
		Value:    "",
		Path:     basePathOr(s.cfg.BasePath),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) sessionToken(r *http.Request) string {
	c, err := r.Cookie(s.cookieName(s.secureCookies(r)))
	if err != nil || !auth.WellFormedToken(c.Value) {
		return ""
	}
	return c.Value
}

// ---- session validation ----------------------------------------------------

var errNoSession = errors.New("no valid session")

// lookupSession validates the session cookie against the database and the
// current configuration. Expired sessions and sessions of users that are
// removed or disabled are deleted.
func (s *Server) lookupSession(r *http.Request) (*session, error) {
	token := s.sessionToken(r)
	if token == "" {
		return nil, errNoSession
	}
	hash := auth.HashToken(token)
	ctx := r.Context()
	sess, err := s.store.Session(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errNoSession
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	switch {
	case !now.Before(sess.ExpiresAt), now.Sub(sess.LastSeenAt) >= s.cfg.IdleTimeout:
		_ = s.store.DeleteSession(ctx, hash)
		return nil, errNoSession
	case !s.cfg.UserActive(sess.Username):
		_ = s.store.DeleteSession(ctx, hash)
		s.log.Warn("session rejected: user is not active in the configuration", "user", sess.Username)
		return nil, errNoSession
	}
	if now.Sub(sess.LastSeenAt) >= time.Minute {
		if err := s.store.TouchSession(ctx, hash, now); err != nil {
			s.log.Warn("updating session activity failed", "error", err)
		}
	}
	return &session{username: sess.Username, csrf: sess.CSRFToken, tokenHash: hash}, nil
}

// requireSession rejects requests without a valid session with 401.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.lookupSession(r)
		if err != nil {
			if !errors.Is(err, errNoSession) {
				s.log.Error("session lookup failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error.")
				return
			}
			if s.sessionToken(r) != "" {
				s.clearSessionCookie(w, r)
			}
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Please sign in.")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	})
}

// requireCSRF checks the X-CSRF-Token header against the session's token.
func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		if sess == nil || !auth.EqualTokens(r.Header.Get("X-CSRF-Token"), sess.csrf) {
			s.log.Warn("request rejected: missing or invalid CSRF token", "path", r.URL.Path,
				"client", clientInfoFor(r, s.cfg.TrustedProxies).IP)
			writeError(w, http.StatusForbidden, "csrf_invalid", "Security token missing or invalid. Reload the page and try again.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- handlers --------------------------------------------------------------

type sessionResponse struct {
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username,omitempty"`
	CSRFToken     string `json:"csrf_token,omitempty"`
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.lookupSession(r)
	if err != nil {
		if !errors.Is(err, errNoSession) {
			s.log.Error("session lookup failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error.")
			return
		}
		if s.sessionToken(r) != "" {
			s.clearSessionCookie(w, r)
		}
		writeJSON(w, http.StatusOK, sessionResponse{})
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{Authenticated: true, Username: sess.username, CSRFToken: sess.csrf})
}

const invalidCredentials = "Invalid username or password."

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Expected application/json.")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, loginBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request.")
		return
	}

	client := clientInfoFor(r, s.cfg.TrustedProxies)
	ipKey := limiterKey(client.IP)
	username := strings.ToLower(strings.TrimSpace(req.Username))
	defined, _ := s.cfg.UserDefined(username)
	now := s.now()

	// Lockouts are checked before any password hashing.
	if wait, blocked := s.loginIP.blocked(ipKey, now); blocked {
		s.tooManyAttempts(w, wait)
		s.log.Warn("login blocked: too many failures from this address", "client", client.IP)
		return
	}
	if defined {
		if wait, blocked := s.loginUser.blocked(username, now); blocked {
			s.tooManyAttempts(w, wait)
			s.log.Warn("login blocked: too many failures for this account", "user", username, "client", client.IP)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ok, err := s.checkPassword(ctx, username, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrBusy) {
			setRetryAfter(w, 5*time.Second)
			writeError(w, http.StatusServiceUnavailable, "busy", "The server is busy. Try again in a few seconds.")
			return
		}
		s.log.Error("login failed with an internal error", "user", safeLogValue(username), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error.")
		return
	}
	if !ok {
		s.loginIP.fail(ipKey, now)
		if defined {
			s.loginUser.fail(username, now)
		}
		s.log.Warn("login failed", "user", safeLogValue(username), "client", client.IP)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", invalidCredentials)
		return
	}
	s.loginUser.reset(username)

	// Never reuse a session presented by the browser (session fixation):
	// drop it and issue a new token.
	if old := s.sessionToken(r); old != "" {
		_ = s.store.DeleteSession(r.Context(), auth.HashToken(old))
	}
	token, err1 := auth.NewToken()
	csrf, err2 := auth.NewToken()
	if err := errors.Join(err1, err2); err != nil {
		s.log.Error("generating session token failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error.")
		return
	}
	sess := store.Session{
		TokenHash:  auth.HashToken(token),
		Username:   username,
		CSRFToken:  csrf,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(s.cfg.SessionLifetime),
	}
	if err := s.store.CreateSession(r.Context(), sess, maxSessionsPerUser); err != nil {
		s.log.Error("creating session failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error.")
		return
	}
	s.setSessionCookie(w, r, token, s.cfg.SessionLifetime)
	s.log.Info("login succeeded", "user", username, "client", client.IP)
	writeJSON(w, http.StatusOK, sessionResponse{Authenticated: true, Username: username, CSRFToken: csrf})
}

// checkPassword verifies credentials. Unknown, disabled and password-less
// users take the same time as a wrong password and get the same answer.
func (s *Server) checkPassword(ctx context.Context, username, password string) (bool, error) {
	if password == "" || len(password) > auth.MaxPasswordLen || !s.cfg.UserActive(username) {
		auth.VerifyDummy(ctx, password)
		return false, nil
	}
	hash, err := s.store.PasswordHash(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		auth.VerifyDummy(ctx, password)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ok, err := auth.VerifyPassword(ctx, password, hash)
	if errors.Is(err, auth.ErrMalformedHash) {
		return false, fmt.Errorf("stored password hash for %q is malformed; reset it with 'user set-password'", username)
	}
	return ok, err
}

func (s *Server) tooManyAttempts(w http.ResponseWriter, wait time.Duration) {
	setRetryAfter(w, wait)
	minutes := int((wait + time.Minute - 1) / time.Minute)
	unit := "minutes"
	if minutes == 1 {
		unit = "minute"
	}
	writeError(w, http.StatusTooManyRequests, "too_many_attempts",
		fmt.Sprintf("Too many failed sign-in attempts. Try again in %d %s.", minutes, unit))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	if err := s.store.DeleteSession(r.Context(), sess.tokenHash); err != nil {
		s.log.Error("deleting session failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error.")
		return
	}
	s.clearSessionCookie(w, r)
	s.log.Info("logout", "user", sess.username)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

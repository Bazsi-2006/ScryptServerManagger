package api

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/perm"
)

type ctxKey int

const (
	ctxAuth ctxKey = iota
	ctxIP
)

// identity is the authenticated caller of a request.
type identity struct {
	User      *db.User
	SessionID string
	perms     map[string]perm.Mask
	actions   map[string]map[string]bool
}

func (s *Server) ident(r *http.Request) *identity {
	id, _ := r.Context().Value(ctxAuth).(*identity)
	return id
}

func (s *Server) actor(r *http.Request) audit.Actor {
	a := audit.Actor{IP: clientIP(r)}
	if id := s.ident(r); id != nil {
		a.UserID, a.Username = id.User.ID, id.User.Username
	}
	return a
}

// loadPerms returns the caller's raw permission masks (cached per request).
func (s *Server) loadPerms(r *http.Request, id *identity) (map[string]perm.Mask, error) {
	if id.perms != nil {
		return id.perms, nil
	}
	raw, err := s.Store.PermissionsForUser(r.Context(), id.User.ID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]perm.Mask, len(raw))
	for k, v := range raw {
		out[k] = perm.Mask(v)
	}
	id.perms = out
	return out, nil
}

// loadActionGrants returns the caller's custom action grants (cached per
// request).
func (s *Server) loadActionGrants(r *http.Request, id *identity) (map[string]map[string]bool, error) {
	if id.actions != nil {
		return id.actions, nil
	}
	grants, err := s.Store.ActionPermissionsForUser(r.Context(), id.User.ID)
	if err != nil {
		return nil, err
	}
	id.actions = grants
	return grants, nil
}

// actionAllowed reports whether the caller may run a custom action:
// administrators always; others need access to the server and an explicit
// grant for that action. Lifecycle or console permissions never imply it.
func (s *Server) actionAllowed(r *http.Request, id *identity, serverID, actionID string) (bool, error) {
	if id.User.IsAdmin {
		return true, nil
	}
	mask, err := s.effective(r, id, serverID)
	if err != nil || !mask.Any() {
		return false, err
	}
	grants, err := s.loadActionGrants(r, id)
	if err != nil {
		return false, err
	}
	return grants[serverID][actionID], nil
}

// effective returns the caller's effective mask for a server.
// Administrators hold every permission.
func (s *Server) effective(r *http.Request, id *identity, serverID string) (perm.Mask, error) {
	if id.User.IsAdmin {
		return perm.All, nil
	}
	perms, err := s.loadPerms(r, id)
	if err != nil {
		return 0, err
	}
	return perms[serverID].Effective(), nil
}

func clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(ctxIP).(string); ok {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) trusted(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, c := range s.Config.Get().TrustedProxies {
		if _, n, err := net.ParseCIDR(c); err == nil {
			if n.Contains(ip) {
				return true
			}
		} else if p := net.ParseIP(c); p != nil && p.Equal(ip) {
			return true
		}
	}
	return false
}

// resolveIP determines the client address. X-Forwarded-For is honoured only
// when the direct peer is a configured trusted proxy, and is walked from the
// right so client-supplied entries cannot spoof the result.
func (s *Server) resolveIP(r *http.Request) (ip string, viaProxy bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if !s.trusted(peer) {
		return host, false
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		cand := net.ParseIP(strings.TrimSpace(parts[i]))
		if cand == nil {
			continue
		}
		if !s.trusted(cand) {
			return cand.String(), true
		}
	}
	return host, true
}

// requestSecure reports whether the client reached us over HTTPS, directly
// or through a trusted reverse proxy.
func (s *Server) requestSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if s.trusted(net.ParseIP(host)) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return false
}

// originAllowed validates the Origin of a state-changing request. Requests
// without an Origin header come from non-browser clients, which are not
// subject to CSRF.
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	c := s.Config.Get()
	if c.PublicURL != "" {
		if pu, err := url.Parse(c.PublicURL); err == nil && strings.EqualFold(pu.Scheme, u.Scheme) && strings.EqualFold(pu.Host, u.Host) {
			return true
		}
	}
	return s.corsOrigin(origin)
}

func (s *Server) corsOrigin(origin string) bool {
	for _, o := range s.Config.Get().CORS.AllowedOrigins {
		if strings.EqualFold(strings.TrimSuffix(o, "/"), origin) {
			return true
		}
	}
	return false
}

const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// base wraps every request: panic recovery, client IP, security headers,
// CORS, origin validation and access logging.
func (s *Server) base(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ip, _ := s.resolveIP(r)
		r = r.WithContext(context.WithValue(r.Context(), ctxIP, ip))
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.Log.Error("panic in request handler", "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				if !sw.wrote {
					s.fail(sw, r, &apiError{Status: http.StatusInternalServerError, Code: "internal_error", Message: "An internal error occurred."})
				}
			}
			// Paths never contain secrets: tokens travel in headers, cookies
			// or request bodies (download tickets are single-use).
			s.Log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ip", ip, "duration_ms", time.Since(start).Milliseconds())
		}()

		h := sw.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", csp)
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		if s.requestSecure(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		isAPI := strings.HasPrefix(r.URL.Path, "/api/")
		if isAPI {
			h.Set("Cache-Control", "no-store")
		}

		// CORS: same-origin by default; only explicitly configured origins
		// get cross-origin access.
		if origin := r.Header.Get("Origin"); origin != "" && isAPI && s.corsOrigin(origin) {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "Retry-After")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Requested-With, Last-Event-ID")
				h.Set("Access-Control-Max-Age", "600")
				sw.WriteHeader(http.StatusNoContent)
				return
			}
		}

		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !s.originAllowed(r) {
				s.fail(sw, r, errf(http.StatusForbidden, "csrf_rejected", "Cross-origin request rejected."))
				return
			}
		}
		next.ServeHTTP(sw, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach Flush and deadlines.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// authenticate validates the access token and loads the caller. The user
// row and session are checked on every request, so disabling an account or
// revoking a session takes effect immediately rather than at token expiry.
func (s *Server) authenticate(r *http.Request) (*identity, *apiError) {
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || token == "" {
		return nil, errUnauthenticated
	}
	claims, err := s.Signer.Verify(token, time.Now())
	if err != nil {
		if err == auth.ErrTokenExpired {
			return nil, &apiError{Status: http.StatusUnauthorized, Code: "token_expired", Message: "The access token has expired."}
		}
		return nil, errUnauthenticated
	}
	user, err := s.Store.UserByID(r.Context(), claims.UserID)
	if err != nil || user.Disabled {
		return nil, errUnauthenticated
	}
	active, err := s.Store.SessionActive(r.Context(), claims.SessionID, time.Now())
	if err != nil || !active {
		return nil, errUnauthenticated
	}
	return &identity{User: user, SessionID: claims.SessionID}, nil
}

type handler func(w http.ResponseWriter, r *http.Request)

// deadline bounds ordinary (non-streaming) requests.
func deadline(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = rc.SetWriteDeadline(time.Now().Add(60 * time.Second))
}

func noDeadline(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
}

// authed requires a valid session and applies the per-user API rate limit.
func (s *Server) authed(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deadline(w)
		id, aerr := s.authenticate(r)
		if aerr != nil {
			s.fail(w, r, aerr)
			return
		}
		if !s.rateLimit(w, r, "api:user:"+itoa(id.User.ID), s.Config.Get().RateLimits.API) {
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxAuth, id)))
	}
}

// admin requires an administrator.
func (s *Server) admin(next handler) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request) {
		if !s.ident(r).User.IsAdmin {
			s.fail(w, r, errAdminOnly)
			return
		}
		next(w, r)
	})
}

// public wraps unauthenticated endpoints with an IP-based rate limit.
func (s *Server) public(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deadline(w)
		if !s.rateLimit(w, r, "api:ip:"+clientIP(r), s.Config.Get().RateLimits.API) {
			return
		}
		next(w, r)
	}
}

// requireXHR is the CSRF defence for the cookie-authenticated endpoints: a
// custom header cannot be sent cross-site without a CORS preflight, which
// only configured origins pass. (The cookie is also SameSite=Strict.)
func requireXHR(r *http.Request) *apiError {
	if r.Header.Get("X-Requested-With") != "server-manager" {
		return errf(http.StatusForbidden, "csrf_rejected", "Missing X-Requested-With header.")
	}
	return nil
}

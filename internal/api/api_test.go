package api_test

// End-to-end tests of the HTTP API against a fully wired application
// (real SQLite database, real lifecycle manager, real test processes).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/app"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

const adminPassword = "admin-password-123"

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	t    *testing.T
	app  *app.App
	srv  *httptest.Server
	cfg  *config.Holder
	logs *syncBuffer
	dir  string
}

func newHarness(t *testing.T, tweak func(*config.Config)) *harness {
	t.Helper()
	h := &harness{t: t, cfg: testutil.Config(t), logs: &syncBuffer{}, dir: t.TempDir()}
	if tweak != nil {
		if err := h.cfg.Update(func(c *config.Config) error { tweak(c); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a, err := app.Build(h.cfg, log, app.Options{Version: "test", NoStatic: true})
	if err != nil {
		t.Fatal(err)
	}
	h.app = a
	h.srv = httptest.NewServer(a.Handler)
	t.Cleanup(func() {
		h.srv.CloseClientConnections()
		h.srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	h.createUser("admin", adminPassword, true)
	return h
}

func (h *harness) createUser(name, password string, admin bool) *db.User {
	h.t.Helper()
	p := h.cfg.Get().Auth.Argon2
	hash, err := auth.HashPassword(password, auth.Params{MemoryKiB: p.MemoryKiB, Iterations: p.Iterations, Parallelism: p.Parallelism})
	if err != nil {
		h.t.Fatal(err)
	}
	u := &db.User{Username: name, DisplayName: name, PasswordHash: hash, IsAdmin: admin}
	if err := h.app.Store.CreateUser(context.Background(), u); err != nil {
		h.t.Fatal(err)
	}
	return u
}

// client is one browser-like API client (own cookie jar and access token).
type client struct {
	h     *harness
	http  *http.Client
	token string
}

func (h *harness) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{h: h, http: &http.Client{Jar: jar, Timeout: 20 * time.Second}}
}

type resp struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

func (r resp) code() string {
	if e, ok := r.Body["error"].(map[string]any); ok {
		return fmt.Sprint(e["code"])
	}
	return ""
}

func (c *client) do(method, path string, body any, headers ...string) resp {
	c.h.t.Helper()
	var rd io.Reader
	if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.h.srv.URL+"/api/v1"+path, rd)
	if err != nil {
		c.h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode, Header: res.Header, Raw: raw, Body: map[string]any{}}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func (c *client) want(status int, method, path string, body any, headers ...string) resp {
	c.h.t.Helper()
	r := c.do(method, path, body, headers...)
	if r.Status != status {
		c.h.t.Fatalf("%s %s = %d, want %d: %s", method, path, r.Status, status, r.Raw)
	}
	return r
}

func (c *client) login(user, password string) resp {
	c.h.t.Helper()
	r := c.do("POST", "/auth/login", map[string]string{"username": user, "password": password})
	if r.Status == 200 {
		c.token = r.Body["access_token"].(string)
	}
	return r
}

func (h *harness) as(user, password string) *client {
	h.t.Helper()
	c := h.client()
	if r := c.login(user, password); r.Status != 200 {
		h.t.Fatalf("login as %s failed: %d %s", user, r.Status, r.Raw)
	}
	return c
}

func (h *harness) admin() *client { return h.as("admin", adminPassword) }

const testServer = `echo "booting"
sleep 0.3
echo 'Done (0.3s)!'
while read line; do echo "got: $line"; [ "$line" = "stop" ] && exit 0; done`

func (h *harness) serverDef(id string) map[string]any {
	script := testutil.Script(h.t, h.dir, id+".sh", testServer)
	return map[string]any{
		"id": id, "name": "Server " + id, "backend": "custom", "category": "Test",
		"custom":    map[string]any{"start": map[string]any{"command": script}, "stop_input": "stop"},
		"console":   map[string]any{"input": "stdin"},
		"env":       map[string]string{"API_TOKEN": "env-secret-value"},
		"readiness": map[string]any{"method": "console", "console": map[string]any{"pattern": "Done (", "regex": false}, "interval": "200ms"},
		"timeouts":  map[string]any{"start": "10s", "stop": "5s"},
	}
}

func (h *harness) grant(adminC *client, userID int64, server string, mask int) {
	h.t.Helper()
	adminC.want(200, "PUT", fmt.Sprintf("/admin/users/%d/permissions/%s", userID, server), map[string]int{"mask": mask})
}

func (h *harness) waitState(c *client, server, state string) {
	h.t.Helper()
	testutil.Eventually(h.t, 10*time.Second, server+" to become "+state, func() bool {
		r := c.do("GET", "/servers/"+server+"/status", nil)
		st, _ := r.Body["status"].(map[string]any)
		return st != nil && st["state"] == state && st["operation"] == nil
	})
}

// ---- authentication ----

func TestLoginRefreshRotationAndLogout(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()

	if r := c.login("admin", "wrong-password"); r.Status != 401 || r.code() != "invalid_credentials" {
		t.Fatalf("wrong password: %d %s", r.Status, r.Raw)
	}
	if r := c.login("nobody", "whatever-password"); r.Status != 401 || r.code() != "invalid_credentials" {
		t.Fatalf("unknown user must look the same as a wrong password: %d %s", r.Status, r.Raw)
	}
	r := c.login("admin", adminPassword)
	if r.Status != 200 || r.Body["expires_in"].(float64) != 600 {
		t.Fatalf("login: %d %s", r.Status, r.Raw)
	}
	if bytes.Contains(r.Raw, []byte("password_hash")) || bytes.Contains(r.Raw, []byte("argon2")) {
		t.Fatal("the login response must not contain the password hash")
	}
	cookie := r.Header.Get("Set-Cookie")
	for _, attr := range []string{"sm_refresh=", "HttpOnly", "SameSite=Strict", "Path=/api/v1/auth"} {
		if !strings.Contains(cookie, attr) {
			t.Errorf("refresh cookie lacks %q: %s", attr, cookie)
		}
	}
	if strings.Contains(string(r.Raw), strings.Split(strings.SplitN(cookie, "=", 2)[1], ";")[0]) {
		t.Error("the refresh token must only travel in the cookie")
	}
	c.want(200, "GET", "/me", nil)

	// Refresh needs the anti-CSRF header and rotates the cookie.
	if r := c.do("POST", "/auth/refresh", nil); r.Status != 403 || r.code() != "csrf_rejected" {
		t.Fatalf("refresh without X-Requested-With: %d %s", r.Status, r.Raw)
	}
	oldToken := c.token
	r = c.want(200, "POST", "/auth/refresh", nil, "X-Requested-With", "server-manager")
	if r.Header.Get("Set-Cookie") == "" || r.Header.Get("Set-Cookie") == cookie {
		t.Fatal("refresh must issue a new refresh token")
	}
	c.token = r.Body["access_token"].(string)
	if c.token == "" {
		t.Fatal("no access token after refresh")
	}
	_ = oldToken

	// A client without the cookie cannot refresh.
	if r := h.client().do("POST", "/auth/refresh", nil, "X-Requested-With", "server-manager"); r.Status != 401 {
		t.Fatalf("refresh without cookie: %d", r.Status)
	}

	// Logout revokes the session server-side: the still-unexpired access
	// token stops working immediately and the cookie is useless.
	token := c.token
	c.want(204, "POST", "/auth/logout", nil, "X-Requested-With", "server-manager")
	if r := c.do("GET", "/me", nil); r.Status != 401 {
		t.Fatalf("access token after logout: %d", r.Status)
	}
	c.token = ""
	if r := c.do("POST", "/auth/refresh", nil, "X-Requested-With", "server-manager"); r.Status != 401 {
		t.Fatalf("refresh after logout: %d", r.Status)
	}
	other := h.client()
	other.token = token
	if r := other.do("GET", "/servers", nil); r.Status != 401 {
		t.Fatal("a revoked session's token must be rejected for every client")
	}
}

func TestReplayedRefreshTokenKillsTheSession(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()
	r := c.login("admin", adminPassword)
	stolen := strings.Split(strings.SplitN(r.Header.Get("Set-Cookie"), "=", 2)[1], ";")[0]
	c.want(200, "POST", "/auth/refresh", nil, "X-Requested-With", "server-manager")

	// Make the first rotation older than the grace window.
	if _, err := h.app.Store.DB.Exec(`UPDATE sessions SET rotated_at = rotated_at - 3600`); err != nil {
		t.Fatal(err)
	}
	thief := h.client()
	rr := thief.do("POST", "/auth/refresh", nil, "X-Requested-With", "server-manager", "Cookie", "sm_refresh="+stolen)
	if rr.Status != 401 {
		t.Fatalf("replayed refresh token: %d %s", rr.Status, rr.Raw)
	}
	// The legitimate client is logged out too: the token family is burned.
	if r := c.do("POST", "/auth/refresh", nil, "X-Requested-With", "server-manager"); r.Status != 401 {
		t.Fatalf("session should be revoked after reuse detection: %d", r.Status)
	}
	if r := c.do("GET", "/me", nil); r.Status != 401 {
		t.Fatalf("access token of a revoked session: %d", r.Status)
	}
	audit := h.admin().want(200, "GET", "/admin/audit?action=REFRESH_TOKEN_REUSE", nil)
	if len(audit.Body["entries"].([]any)) != 1 {
		t.Fatal("token reuse must be audited")
	}
}

func TestCookieIsSecureBehindHTTPSProxy(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()
	// httptest connects from 127.0.0.1, which is a trusted proxy by default.
	r := c.do("POST", "/auth/login", map[string]string{"username": "admin", "password": adminPassword}, "X-Forwarded-Proto", "https")
	if !strings.Contains(r.Header.Get("Set-Cookie"), "Secure") {
		t.Fatalf("cookie must be Secure when the client used HTTPS: %s", r.Header.Get("Set-Cookie"))
	}
	if r.Header.Get("Strict-Transport-Security") == "" {
		t.Error("HSTS expected for HTTPS requests")
	}
	h2 := newHarness(t, func(c *config.Config) { c.PublicURL = "https://sm.example.com" })
	r = h2.client().do("POST", "/auth/login", map[string]string{"username": "admin", "password": adminPassword})
	if !strings.Contains(r.Header.Get("Set-Cookie"), "Secure") {
		t.Fatal("cookie must be Secure when public_url is https")
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.RateLimits.Login = config.Rate{Requests: 3, Window: config.Duration(time.Minute)}
	})
	c := h.client()
	for i := 0; i < 3; i++ {
		if r := c.login("admin", "bad-password-000"); r.Status != 401 {
			t.Fatalf("attempt %d: %d", i, r.Status)
		}
	}
	r := c.login("admin", adminPassword)
	if r.Status != 429 || r.code() != "rate_limited" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("4th attempt should be rate limited even with the right password: %d %s", r.Status, r.Raw)
	}
}

func TestPasswordsAndTokensNeverReachTheLogs(t *testing.T) {
	h := newHarness(t, nil)
	c := h.admin()
	c.login("admin", "a-wrong-password-attempt")
	c.want(200, "POST", "/auth/refresh", nil, "X-Requested-With", "server-manager")
	c.want(200, "POST", "/me/password", map[string]string{"current_password": adminPassword, "new_password": "brand-new-password-9"})
	c.want(201, "POST", "/servers", h.serverDef("logsrv"))
	logs := h.logs.String()
	for _, secret := range []string{adminPassword, "a-wrong-password-attempt", "brand-new-password-9", c.token, "env-secret-value"} {
		if strings.Contains(logs, secret) {
			t.Errorf("a secret (%.12s…) was written to the application log", secret)
		}
	}
	if !strings.Contains(logs, "action=LOGIN") {
		t.Error("expected audit lines in the application log")
	}
	entries := c.want(200, "GET", "/admin/audit", nil)
	if strings.Contains(string(entries.Raw), adminPassword) || strings.Contains(string(entries.Raw), "env-secret-value") {
		t.Error("the audit log must not contain passwords or environment values")
	}
}

// ---- HTTP hardening ----

func TestSecurityHeadersCORSAndOriginChecks(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.CORS.AllowedOrigins = []string{"https://trusted.example"} })
	c := h.client()
	r := c.do("GET", "/health", nil)
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store",
	} {
		if r.Header.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, r.Header.Get(k), want)
		}
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("weak CSP: %s", csp)
	}
	// Error responses carry the headers too.
	if r := c.do("GET", "/servers", nil); r.Status != 401 || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security headers missing on error responses")
	}

	// Same-origin by default: an unknown origin gets no CORS grant ...
	r = c.do("GET", "/health", nil, "Origin", "https://evil.example")
	if r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("untrusted origin must not receive CORS headers")
	}
	// ... and cannot perform state-changing requests.
	r = c.do("POST", "/auth/login", map[string]string{"username": "admin", "password": adminPassword}, "Origin", "https://evil.example")
	if r.Status != 403 || r.code() != "csrf_rejected" {
		t.Fatalf("cross-origin POST: %d %s", r.Status, r.Raw)
	}
	// The configured origin is allowed, including preflight.
	r = c.do("OPTIONS", "/servers", nil, "Origin", "https://trusted.example", "Access-Control-Request-Method", "POST")
	if r.Status != 204 || r.Header.Get("Access-Control-Allow-Origin") != "https://trusted.example" || !strings.Contains(r.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight for trusted origin: %d %v", r.Status, r.Header)
	}
	r = c.do("POST", "/auth/login", map[string]string{"username": "admin", "password": adminPassword}, "Origin", "https://trusted.example")
	if r.Status != 200 {
		t.Fatalf("trusted origin login: %d", r.Status)
	}
}

func TestRequestValidation(t *testing.T) {
	h := newHarness(t, nil)
	c := h.admin()
	if r := c.do("POST", "/servers", `{"id":"x","name":"X","backend":"custom","custom":{"start":{"command":"/bin/true"}},"surprise":1}`); r.Status != 400 || r.code() != "invalid_request" {
		t.Fatalf("unknown field: %d %s", r.Status, r.Raw)
	}
	if r := c.do("POST", "/servers", `{"id":"x"} trailing`); r.Status != 400 {
		t.Fatalf("trailing data: %d", r.Status)
	}
	big := `{"id":"x","name":"` + strings.Repeat("A", 2<<20) + `"}`
	if r := c.do("POST", "/servers", big); r.Status != 413 || r.code() != "request_too_large" {
		t.Fatalf("oversized body: %d %s", r.Status, r.code())
	}
	r := c.do("POST", "/servers", map[string]any{"id": "Bad ID", "name": "", "backend": "custom", "custom": map[string]any{"start": map[string]any{"command": "relative/path"}}})
	if r.Status != 422 || r.code() != "validation_failed" {
		t.Fatalf("invalid definition: %d %s", r.Status, r.Raw)
	}
	fields := map[string]bool{}
	for _, d := range r.Body["error"].(map[string]any)["details"].([]any) {
		fields[d.(map[string]any)["field"].(string)] = true
	}
	for _, f := range []string{"id", "name", "custom.start.command"} {
		if !fields[f] {
			t.Errorf("validation error should name field %q: %s", f, r.Raw)
		}
	}
	if r := c.do("GET", "/nope", nil); r.Status != 404 {
		t.Fatalf("unknown API route: %d", r.Status)
	}
	// Internal details never leak: a 500 is generic. (None should occur here.)
	if strings.Contains(h.logs.String(), "panic") {
		t.Fatal("handler panicked")
	}
}

// ---- invitations and registration ----

func TestInvitationRegistrationFlow(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()

	// No public registration.
	if r := h.client().do("POST", "/auth/register", map[string]string{"token": "made-up", "username": "eve", "password": "long-enough-password"}); r.Status != 404 || r.code() != "invitation_invalid" {
		t.Fatalf("registration without invitation: %d %s", r.Status, r.Raw)
	}

	r := admin.want(201, "POST", "/admin/invitations", map[string]any{"expires_in_hours": 1})
	link, _ := r.Body["link"].(string)
	m := regexp.MustCompile(`#/register\?token=([A-Za-z0-9_-]{40,})`).FindStringSubmatch(link)
	if m == nil {
		t.Fatalf("no usable link in response: %s", r.Raw)
	}
	token := m[1]
	if strings.Contains(h.logs.String(), token) {
		t.Fatal("invitation tokens must not be written to the log when no email is involved")
	}
	var stored string
	h.app.Store.DB.QueryRow(`SELECT token_hash FROM invitations`).Scan(&stored)
	if stored == token || stored == "" {
		t.Fatal("only a hash of the invitation token may be stored")
	}

	guest := h.client()
	guest.want(200, "POST", "/auth/invitation", map[string]string{"token": token})
	if r := guest.do("POST", "/auth/register", map[string]string{"token": token, "username": "al", "password": "long-enough-password"}); r.Status != 400 {
		t.Fatalf("short username: %d", r.Status)
	}
	if r := guest.do("POST", "/auth/register", map[string]string{"token": token, "username": "alice", "password": "short"}); r.Status != 400 {
		t.Fatalf("short password: %d", r.Status)
	}
	if r := guest.do("POST", "/auth/register", map[string]string{"token": token, "username": "admin", "password": "long-enough-password"}); r.Status != 409 {
		t.Fatalf("taken username: %d", r.Status)
	}
	reg := guest.want(201, "POST", "/auth/register", map[string]string{"token": token, "username": "alice", "display_name": "Alice", "password": "long-enough-password"})
	if reg.Body["user"].(map[string]any)["is_admin"] != false {
		t.Fatal("invited users must not be administrators")
	}
	guest.token = reg.Body["access_token"].(string)
	if perms := guest.want(200, "GET", "/me", nil).Body["permissions"].(map[string]any); len(perms) != 0 {
		t.Fatalf("a new account must start with no permissions: %v", perms)
	}

	// Single use.
	if r := h.client().do("POST", "/auth/register", map[string]string{"token": token, "username": "mallory", "password": "long-enough-password"}); r.Status != 404 {
		t.Fatalf("second use of an invitation: %d", r.Status)
	}
	if r := h.client().do("POST", "/auth/invitation", map[string]string{"token": token}); r.Status != 404 {
		t.Fatalf("used invitation should check as invalid: %d", r.Status)
	}

	// Expired and revoked invitations.
	r = admin.want(201, "POST", "/admin/invitations", map[string]any{})
	tok2 := regexp.MustCompile(`token=([A-Za-z0-9_-]+)`).FindStringSubmatch(r.Body["link"].(string))[1]
	id2 := int64(r.Body["invitation"].(map[string]any)["id"].(float64))
	h.app.Store.DB.Exec(`UPDATE invitations SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).Unix(), id2)
	if r := h.client().do("POST", "/auth/register", map[string]string{"token": tok2, "username": "late", "password": "long-enough-password"}); r.Status != 404 {
		t.Fatalf("expired invitation: %d", r.Status)
	}
	// Resending issues a fresh link; the old one is dead.
	r = admin.want(200, "POST", fmt.Sprintf("/admin/invitations/%d/resend", id2), nil)
	tok3 := regexp.MustCompile(`token=([A-Za-z0-9_-]+)`).FindStringSubmatch(r.Body["link"].(string))[1]
	if tok3 == tok2 {
		t.Fatal("resend must rotate the token")
	}
	h.client().want(200, "POST", "/auth/invitation", map[string]string{"token": tok3})
	admin.want(204, "DELETE", fmt.Sprintf("/admin/invitations/%d", id2), nil)
	if r := h.client().do("POST", "/auth/invitation", map[string]string{"token": tok3}); r.Status != 404 {
		t.Fatalf("revoked invitation: %d", r.Status)
	}
	list := admin.want(200, "GET", "/admin/invitations", nil).Body["invitations"].([]any)
	statuses := map[string]bool{}
	for _, inv := range list {
		statuses[inv.(map[string]any)["status"].(string)] = true
		if _, leaked := inv.(map[string]any)["token_hash"]; leaked {
			t.Fatal("invitation listings must not expose token hashes")
		}
	}
	if !statuses["used"] || !statuses["revoked"] {
		t.Fatalf("statuses %v", statuses)
	}

	// Only administrators manage invitations.
	if r := guest.do("POST", "/admin/invitations", map[string]any{}); r.Status != 403 {
		t.Fatalf("non-admin creating an invitation: %d", r.Status)
	}
	if r := guest.do("GET", "/admin/invitations", nil); r.Status != 403 {
		t.Fatalf("non-admin listing invitations: %d", r.Status)
	}
}

func TestInvitationEmailInDevelopmentAndProduction(t *testing.T) {
	// Development without SMTP: the message is printed to the log.
	h := newHarness(t, nil)
	r := h.admin().want(201, "POST", "/admin/invitations", map[string]any{"email": "New.User@Example.com"})
	email := r.Body["email"].(map[string]any)
	if email["logged"] != true || email["sent"] != false {
		t.Fatalf("development delivery: %v", email)
	}
	if !strings.Contains(h.logs.String(), "#/register?token=") || !strings.Contains(h.logs.String(), "new.user@example.com") {
		t.Fatal("development mode should print the invitation email")
	}

	// Production without SMTP: nothing is logged, the failure is reported
	// and the invitation stays valid.
	p := newHarness(t, func(c *config.Config) { c.Mode = config.ModeProduction; c.PublicURL = "https://sm.example.com" })
	r = p.admin().want(201, "POST", "/admin/invitations", map[string]any{"email": "someone@example.com"})
	email = r.Body["email"].(map[string]any)
	if email["sent"] != false || email["error"] == nil {
		t.Fatalf("production without SMTP: %v", email)
	}
	link := r.Body["link"].(string)
	if !strings.HasPrefix(link, "https://sm.example.com/#/register?token=") {
		t.Fatalf("link should use public_url: %s", link)
	}
	tok := strings.SplitN(link, "token=", 2)[1]
	if strings.Contains(p.logs.String(), tok) {
		t.Fatal("production must never log invitation tokens")
	}
	p.client().want(200, "POST", "/auth/invitation", map[string]string{"token": tok})

	// SMTP configured but unreachable: reported, invitation intact.
	u := newHarness(t, func(c *config.Config) {
		c.Mode = config.ModeProduction
		c.PublicURL = "https://sm.example.com"
		c.SMTP = config.SMTP{Host: "127.0.0.1", Port: 1, From: "sm@example.com", TLS: "none"}
	})
	r = u.admin().want(201, "POST", "/admin/invitations", map[string]any{"email": "someone@example.com"})
	if e := r.Body["email"].(map[string]any); e["sent"] != false || e["error"] == nil || r.Body["link"] == nil {
		t.Fatalf("unreachable SMTP: %s", r.Raw)
	}
	if strings.Contains(string(r.Raw), "connection refused") {
		t.Error("SMTP internals should not be echoed in the invitation response")
	}
}

// ---- per-server authorization ----

func TestPerServerPermissionsAreEnforced(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	admin.want(201, "POST", "/servers", h.serverDef("alpha"))
	admin.want(201, "POST", "/servers", h.serverDef("beta"))
	bob := h.createUser("bob", "bob-password-123", false)
	h.grant(admin, bob.ID, "alpha", 1) // START only
	c := h.as("bob", "bob-password-123")

	// Visibility: only alpha, and no configuration details in the listing.
	list := c.want(200, "GET", "/servers", nil)
	servers := list.Body["servers"].([]any)
	if len(servers) != 1 || servers[0].(map[string]any)["id"] != "alpha" {
		t.Fatalf("visible servers: %s", list.Raw)
	}
	if strings.Contains(string(list.Raw), ".sh") || strings.Contains(string(list.Raw), "env-secret-value") || strings.Contains(string(list.Raw), "command") {
		t.Fatalf("server listing leaks execution details: %s", list.Raw)
	}
	if got := servers[0].(map[string]any)["permissions"].(float64); got != 1 {
		t.Fatalf("effective permissions = %v, want 1", got)
	}

	// beta is indistinguishable from a server that does not exist.
	for _, probe := range [][2]string{
		{"GET", "/servers/beta"}, {"GET", "/servers/beta/status"}, {"POST", "/servers/beta/start"}, {"POST", "/servers/beta/stop"},
		{"GET", "/servers/beta/console"}, {"GET", "/servers/beta/console/stream"}, {"GET", "/servers/beta/backups"},
		{"GET", "/servers/beta/config"}, {"GET", "/servers/beta/operations"},
	} {
		a, b := c.do(probe[0], probe[1], nil), c.do(probe[0], strings.Replace(probe[1], "beta", "ghost", 1), nil)
		if a.Status != 404 || b.Status != 404 || a.code() != b.code() {
			t.Errorf("%s %s: %d vs nonexistent %d - existence must not be observable", probe[0], probe[1], a.Status, b.Status)
		}
	}

	// START works, STOP is denied.
	c.want(202, "POST", "/servers/alpha/start", nil)
	h.waitState(admin, "alpha", "RUNNING")
	for _, denied := range [][2]string{
		{"POST", "/servers/alpha/stop"}, {"POST", "/servers/alpha/restart"}, {"GET", "/servers/alpha/console"},
		{"GET", "/servers/alpha/console/stream"}, {"GET", "/servers/alpha/backups"}, {"POST", "/servers/alpha/backups"},
		{"GET", "/servers/alpha/config"}, {"GET", "/servers/alpha/console/history"},
	} {
		if r := c.do(denied[0], denied[1], nil); r.Status != 403 || r.code() != "permission_denied" {
			t.Errorf("%s %s with START only: %d %s", denied[0], denied[1], r.Status, r.code())
		}
	}
	if r := c.do("POST", "/servers/alpha/console/command", map[string]string{"command": "stop"}); r.Status != 403 {
		t.Errorf("console command with START only: %d", r.Status)
	}
	if r := c.do("PUT", "/servers/alpha/config", h.serverDef("alpha")); r.Status != 403 {
		t.Errorf("config change with START only: %d", r.Status)
	}
	h.waitState(admin, "alpha", "RUNNING") // nothing above stopped it

	// Administration is off limits, including granting oneself permissions.
	for _, adminOnly := range [][2]string{
		{"GET", "/admin/users"}, {"GET", "/admin/audit"}, {"GET", "/admin/settings"}, {"POST", "/servers"}, {"DELETE", "/servers/alpha"},
		{"GET", "/servers/alpha/permissions"}, {"POST", "/admin/servers/reload"},
	} {
		if r := c.do(adminOnly[0], adminOnly[1], map[string]any{}); r.Status != 403 {
			t.Errorf("%s %s as regular user: %d", adminOnly[0], adminOnly[1], r.Status)
		}
	}
	if r := c.do("PUT", fmt.Sprintf("/admin/users/%d/permissions/alpha", bob.ID), map[string]int{"mask": 63}); r.Status != 403 {
		t.Fatalf("self-granting permissions: %d", r.Status)
	}
	if r := c.do("PATCH", fmt.Sprintf("/admin/users/%d", bob.ID), map[string]bool{"is_admin": true}); r.Status != 403 {
		t.Fatalf("self-promotion: %d", r.Status)
	}

	// STOP implies START; CONSOLE_WRITE implies CONSOLE_READ.
	h.grant(admin, bob.ID, "alpha", 2|8)
	me := c.want(200, "GET", "/me", nil)
	if got := me.Body["permissions"].(map[string]any)["alpha"].(float64); got != 15 {
		t.Fatalf("effective mask for STOP|CONSOLE_WRITE = %v, want 15", got)
	}
	c.want(200, "GET", "/servers/alpha/console", nil)
	c.want(204, "POST", "/servers/alpha/console/command", map[string]string{"command": "hello from bob"})
	c.want(202, "POST", "/servers/alpha/stop", nil)
	h.waitState(admin, "alpha", "STOPPED")
	c.want(202, "POST", "/servers/alpha/start", nil) // START via STOP
	h.waitState(admin, "alpha", "RUNNING")

	// Read-only console access cannot write.
	h.grant(admin, bob.ID, "alpha", 4)
	c.want(200, "GET", "/servers/alpha/console", nil)
	if r := c.do("POST", "/servers/alpha/console/command", map[string]string{"command": "x"}); r.Status != 403 {
		t.Fatalf("command with CONSOLE_READ only: %d", r.Status)
	}
	if r := c.do("POST", "/servers/alpha/start", nil); r.Status != 403 {
		t.Fatalf("start with CONSOLE_READ only: %d", r.Status)
	}

	// Revoking everything hides the server again.
	h.grant(admin, bob.ID, "alpha", 0)
	if r := c.do("GET", "/servers/alpha", nil); r.Status != 404 {
		t.Fatalf("after revocation: %d", r.Status)
	}

	// Invalid masks and unknown targets are rejected.
	if r := admin.do("PUT", fmt.Sprintf("/admin/users/%d/permissions/alpha", bob.ID), map[string]int{"mask": 64}); r.Status != 400 {
		t.Fatalf("undefined permission bit: %d", r.Status)
	}
	if r := admin.do("PUT", fmt.Sprintf("/admin/users/%d/permissions/ghost", bob.ID), map[string]int{"mask": 1}); r.Status != 404 {
		t.Fatalf("permission for unknown server: %d", r.Status)
	}

	// Denied attempts and commands are in the audit trail.
	denied := admin.want(200, "GET", "/admin/audit?username=bob&result=DENIED", nil).Body["entries"].([]any)
	if len(denied) == 0 {
		t.Fatal("denied actions should be audited")
	}
	cmds := admin.want(200, "GET", "/admin/audit?action=CONSOLE_COMMAND&server_id=alpha&result=SUCCESS", nil).Body["entries"].([]any)
	if len(cmds) != 1 || cmds[0].(map[string]any)["details"].(map[string]any)["command"] != "hello from bob" {
		t.Fatalf("console commands must be audited: %v", cmds)
	}
	admin.want(202, "POST", "/servers/alpha/stop", nil)
	h.waitState(admin, "alpha", "STOPPED")
}

func TestConfigPermissionCannotChangeWhatIsExecuted(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	admin.want(201, "POST", "/servers", h.serverDef("cfg"))
	carol := h.createUser("carol", "carol-password-1", false)
	h.grant(admin, carol.ID, "cfg", 32) // CONFIG only
	c := h.as("carol", "carol-password-1")

	got := c.want(200, "GET", "/servers/cfg/config", nil)
	if got.Body["edit_scope"] != "safe" {
		t.Fatalf("edit scope: %v", got.Body["edit_scope"])
	}
	if strings.Contains(string(got.Raw), "env-secret-value") {
		t.Fatal("environment values must be hidden from non-administrators")
	}
	def := got.Body["config"].(map[string]any)

	// Safe fields can be edited (the masked environment round-trips).
	def["name"] = "Renamed by Carol"
	def["timeouts"].(map[string]any)["start"] = "30s"
	c.want(200, "PUT", "/servers/cfg/config", def)
	adminView := admin.want(200, "GET", "/servers/cfg/config", nil)
	cfg := adminView.Body["config"].(map[string]any)
	if cfg["name"] != "Renamed by Carol" || cfg["env"].(map[string]any)["API_TOKEN"] != "env-secret-value" {
		t.Fatalf("safe edit result: %s", adminView.Raw)
	}

	// Anything that decides what runs on the host is refused.
	attacks := map[string]func(map[string]any){
		"start command": func(d map[string]any) {
			d["custom"].(map[string]any)["start"] = map[string]any{"command": "/bin/sh", "args": []string{"-c", "id > /tmp/pwned"}}
		},
		"environment":   func(d map[string]any) { d["env"] = map[string]string{"LD_PRELOAD": "/tmp/evil.so"} },
		"backup":        func(d map[string]any) { d["backup"] = map[string]any{"directory": "/etc"} },
		"console input": func(d map[string]any) { d["console"] = map[string]any{"input": "pty"} },
		"stop command": func(d map[string]any) {
			d["custom"].(map[string]any)["stop"] = map[string]any{"command": "/bin/sh", "args": []string{"-c", "id"}}
		},
		"backend": func(d map[string]any) {
			d["backend"] = "tmux"
			d["tmux"] = map[string]any{"session": "x"}
			delete(d, "custom")
			d["console"] = map[string]any{}
		},
	}
	for name, mutate := range attacks {
		fresh := c.want(200, "GET", "/servers/cfg/config", nil).Body["config"].(map[string]any)
		mutate(fresh)
		if r := c.do("PUT", "/servers/cfg/config", fresh); r.Status != 403 || r.code() != "permission_denied" {
			t.Errorf("CONFIG holder changing the %s: %d %s", name, r.Status, r.Raw)
		}
	}
	after := admin.want(200, "GET", "/servers/cfg/config", nil)
	if strings.Contains(string(after.Raw), "/bin/sh") || strings.Contains(string(after.Raw), "LD_PRELOAD") {
		t.Fatal("a rejected change was persisted")
	}
	// CONFIG does not extend to application settings or other servers.
	if r := c.do("GET", "/admin/settings", nil); r.Status != 403 {
		t.Fatalf("CONFIG must not grant global settings: %d", r.Status)
	}
	if r := c.do("POST", "/servers/cfg/start", nil); r.Status != 403 {
		t.Fatalf("CONFIG must not grant START: %d", r.Status)
	}
	// The ID is immutable.
	fresh := admin.want(200, "GET", "/servers/cfg/config", nil).Body["config"].(map[string]any)
	fresh["id"] = "other"
	if r := admin.do("PUT", "/servers/cfg/config", fresh); r.Status != 400 {
		t.Fatalf("changing the id: %d", r.Status)
	}
}

func TestOperationConflictsAndRateLimits(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.RateLimits.ServerStart = config.Rate{Requests: 3, Window: config.Duration(time.Minute)}
	})
	admin := h.admin()
	admin.want(201, "POST", "/servers", h.serverDef("ops"))
	if r := admin.do("POST", "/servers/ops/stop", nil); r.Status != 409 || r.code() != "not_running" {
		t.Fatalf("stop of a stopped server: %d %s", r.Status, r.code())
	}
	admin.want(202, "POST", "/servers/ops/start", nil)
	r := admin.do("POST", "/servers/ops/start", nil)
	if r.Status != 409 || (r.code() != "operation_in_progress" && r.code() != "already_running") {
		t.Fatalf("second start: %d %s", r.Status, r.code())
	}
	h.waitState(admin, "ops", "RUNNING")
	if r := admin.do("POST", "/servers/ops/start", nil); r.Status != 409 || r.code() != "already_running" {
		t.Fatalf("start while running: %d %s", r.Status, r.code())
	}
	// The per-user start limit (3/min) is now exhausted.
	if r := admin.do("POST", "/servers/ops/start", nil); r.Status != 429 || r.code() != "rate_limited" {
		t.Fatalf("start rate limit: %d %s", r.Status, r.code())
	}
	// Unsupported actions say so.
	if r := admin.do("POST", "/servers/ops/backups", nil); r.Status != 409 || r.code() != "unsupported_action" {
		t.Fatalf("backup without configuration: %d %s", r.Status, r.code())
	}
	if r := admin.do("POST", "/servers/ops/console/command", map[string]string{"command": "two\nlines"}); r.Status != 400 {
		t.Fatalf("multi-line command: %d", r.Status)
	}
	// Deleting a running server needs force; its files are never touched.
	if r := admin.do("DELETE", "/servers/ops", nil); r.Status != 409 || r.code() != "server_active" {
		t.Fatalf("delete while running: %d %s", r.Status, r.code())
	}
	admin.want(202, "POST", "/servers/ops/stop", nil)
	h.waitState(admin, "ops", "STOPPED")
	admin.want(204, "DELETE", "/servers/ops", nil)
	if _, err := os.Stat(filepath.Join(h.dir, "ops.sh")); err != nil {
		t.Fatal("deleting a server definition must not delete the server's files")
	}
	if _, err := os.Stat(filepath.Join(h.cfg.Get().Paths.ServersDir, "ops.json")); !os.IsNotExist(err) {
		t.Fatal("the definition file should be gone")
	}

	// A global cooldown is reported as such.
	admin.want(201, "POST", "/servers", h.serverDef("cool"))
	h.cfg.Update(func(c *config.Config) error { c.RateLimits.ServerCooldown = config.Duration(time.Hour); return nil })
	admin.want(202, "POST", "/servers/cool/start", nil)
	h.waitState(admin, "cool", "RUNNING")
	if r := admin.do("POST", "/servers/cool/stop", nil); r.Status != 429 || r.code() != "cooldown" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("cooldown: %d %s", r.Status, r.code())
	}
	h.cfg.Update(func(c *config.Config) error { c.RateLimits.ServerCooldown = 0; return nil })
	admin.want(202, "POST", "/servers/cool/stop", nil)
	h.waitState(admin, "cool", "STOPPED")
}

// ---- backups ----

func TestBackupListingDownloadAndPathSecurity(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	bdir := filepath.Join(h.dir, "backups")
	os.MkdirAll(bdir, 0o755)
	secret := filepath.Join(h.dir, "secret.txt")
	os.WriteFile(secret, []byte("TOP SECRET"), 0o600)
	os.WriteFile(filepath.Join(bdir, "world.tar.gz"), []byte("backup-bytes-1234567890"), 0o600)
	os.Symlink(secret, filepath.Join(bdir, "evil-link"))
	def := h.serverDef("bk")
	def["backup"] = map[string]any{"directory": bdir, "command": map[string]any{"command": testutil.Script(t, h.dir, "backup.sh", `echo new > "$SM_BACKUP_DIR/created-by-script.tar"`)}}
	admin.want(201, "POST", "/servers", def)

	dave := h.createUser("dave", "dave-password-12", false)
	h.grant(admin, dave.ID, "bk", 16) // BACKUPS
	erin := h.createUser("erin", "erin-password-12", false)
	h.grant(admin, erin.ID, "bk", 1) // START only
	d, e := h.as("dave", "dave-password-12"), h.as("erin", "erin-password-12")

	list := d.want(200, "GET", "/servers/bk/backups", nil)
	files := list.Body["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["name"] != "world.tar.gz" || files[0].(map[string]any)["size"].(float64) != 23 {
		t.Fatalf("listing (the escaping symlink must not appear): %s", list.Raw)
	}
	if strings.Contains(string(list.Raw), bdir) {
		t.Fatal("the backup listing must not reveal filesystem paths")
	}
	if list.Body["can_delete"] != false {
		t.Fatal("users cannot delete backups by default")
	}
	if r := e.do("GET", "/servers/bk/backups", nil); r.Status != 403 {
		t.Fatalf("listing without BACKUPS: %d", r.Status)
	}
	if r := e.do("POST", "/servers/bk/backups/world.tar.gz/download", nil); r.Status != 403 {
		t.Fatalf("download without BACKUPS: %d", r.Status)
	}

	// Traversal and symlink escapes.
	for _, name := range []string{"..%2Fsecret.txt", "..%2F..%2Fetc%2Fpasswd", "evil-link", "%2Fetc%2Fpasswd", ".hidden", "missing.tar"} {
		r := d.do("POST", "/servers/bk/backups/"+name+"/download", nil)
		if r.Status != 400 && r.Status != 404 {
			t.Errorf("download ticket for %q: %d %s", name, r.Status, r.Raw)
		}
	}

	// Legitimate download through a single-use ticket.
	tk := d.want(200, "POST", "/servers/bk/backups/world.tar.gz/download", nil)
	url := tk.Body["url"].(string)
	plain := &http.Client{} // no token, no cookies: the ticket alone authorizes
	res, err := plain.Get(h.srv.URL + url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "backup-bytes-1234567890" {
		t.Fatalf("download: %d %q", res.StatusCode, body)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "world.tar.gz") {
		t.Errorf("Content-Disposition: %s", cd)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("download headers: %v", res.Header)
	}
	res, _ = plain.Get(h.srv.URL + url)
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("a download ticket must be single-use, second use = %d", res.StatusCode)
	}
	res, _ = plain.Get(h.srv.URL + "/api/v1/downloads/not-a-ticket")
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("forged ticket: %d", res.StatusCode)
	}

	// Size limit.
	h.cfg.Update(func(c *config.Config) error { c.Backups.MaxDownloadBytes = 10; return nil })
	if r := d.do("POST", "/servers/bk/backups/world.tar.gz/download", nil); r.Status != 413 {
		t.Fatalf("download size limit: %d", r.Status)
	}
	h.cfg.Update(func(c *config.Config) error { c.Backups.MaxDownloadBytes = 0; return nil })

	// Creating a backup runs the configured script as a tracked operation.
	d.want(202, "POST", "/servers/bk/backups", nil)
	testutil.Eventually(t, 10*time.Second, "new backup file", func() bool {
		return strings.Contains(string(d.do("GET", "/servers/bk/backups", nil).Raw), "created-by-script.tar")
	})

	// Deletion: administrators only (unless enabled per server).
	if r := d.do("DELETE", "/servers/bk/backups/world.tar.gz", nil); r.Status != 403 {
		t.Fatalf("user deleting a backup: %d", r.Status)
	}
	if r := admin.do("DELETE", "/servers/bk/backups/..%2Fsecret.txt", nil); r.Status != 400 {
		t.Fatalf("traversal delete: %d", r.Status)
	}
	admin.want(204, "DELETE", "/servers/bk/backups/world.tar.gz", nil)
	admin.want(204, "DELETE", "/servers/bk/backups/evil-link", nil) // removes the link only
	if data, err := os.ReadFile(secret); err != nil || string(data) != "TOP SECRET" {
		t.Fatal("a file outside the backup directory was touched")
	}
	downloads := admin.want(200, "GET", "/admin/audit?action=BACKUP_DOWNLOAD&result=SUCCESS", nil).Body["entries"].([]any)
	if len(downloads) != 1 || downloads[0].(map[string]any)["username"] != "dave" {
		t.Fatalf("downloads must be audited: %v", downloads)
	}
}

// ---- server-sent events ----

type sseEvent struct {
	Type string
	Data map[string]any
	ID   string
}

type sseStream struct {
	C      chan sseEvent
	Status int
	closed chan struct{}
	cancel context.CancelFunc
}

func (c *client) stream(path string, lastID string) *sseStream {
	c.h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", c.h.srv.URL+"/api/v1"+path, nil)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		c.h.t.Fatal(err)
	}
	s := &sseStream{C: make(chan sseEvent, 1024), Status: res.StatusCode, closed: make(chan struct{}), cancel: cancel}
	c.h.t.Cleanup(cancel)
	if res.StatusCode != 200 {
		res.Body.Close()
		close(s.closed)
		return s
	}
	go func() {
		defer close(s.closed)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		ev := sseEvent{}
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.Type != "" {
					s.C <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.Type = line[7:]
			case strings.HasPrefix(line, "id: "):
				ev.ID = line[4:]
			case strings.HasPrefix(line, "data: "):
				json.Unmarshal([]byte(line[6:]), &ev.Data)
			}
		}
	}()
	return s
}

// expect waits for an event satisfying match.
func (s *sseStream) expect(t *testing.T, what string, match func(sseEvent) bool) sseEvent {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-s.C:
			if match(ev) {
				return ev
			}
		case <-s.closed:
			// Drain whatever was received before the stream ended.
			for {
				select {
				case ev := <-s.C:
					if match(ev) {
						return ev
					}
				default:
					t.Fatalf("stream closed while waiting for %s", what)
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// never asserts that no event matching bad arrives for a while.
func (s *sseStream) never(t *testing.T, what string, wait time.Duration, bad func(sseEvent) bool) {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case ev := <-s.C:
			if bad(ev) {
				t.Fatalf("%s: received %s %v", what, ev.Type, ev.Data)
			}
		case <-s.closed:
			return
		case <-deadline:
			return
		}
	}
}

func isType(typ string) func(sseEvent) bool { return func(e sseEvent) bool { return e.Type == typ } }

func statusOf(server, state string) func(sseEvent) bool {
	return func(e sseEvent) bool {
		return e.Type == "server.status.changed" && e.Data["server_id"] == server && e.Data["state"] == state
	}
}

func TestEventStreamAuthorizationAndDelivery(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Monitoring.HostMetricsForUsers = false })
	admin := h.admin()
	admin.want(201, "POST", "/servers", h.serverDef("one"))
	admin.want(201, "POST", "/servers", h.serverDef("two"))
	frank := h.createUser("frank", "frank-password-1", false)
	h.grant(admin, frank.ID, "one", 1)
	fc := h.as("frank", "frank-password-1")

	// Authentication is required, and tokens in the URL are not accepted.
	if s := h.client().stream("/events", ""); s.Status != 401 {
		t.Fatalf("anonymous event stream: %d", s.Status)
	}
	if s := h.client().stream("/events?access_token="+fc.token, ""); s.Status != 401 {
		t.Fatalf("token in query string must not authenticate: %d", s.Status)
	}

	// Two browsers of the administrator plus one of the restricted user.
	a1, a2, fs := admin.stream("/events", ""), h.admin().stream("/events", ""), fc.stream("/events", "")
	for _, s := range []*sseStream{a1, a2, fs} {
		hello := s.expect(t, "hello", isType("hello"))
		if hello.Data["resync"] != true {
			t.Fatal("a first connection must be told to load state")
		}
	}

	admin.want(202, "POST", "/servers/one/start", nil)
	admin.want(202, "POST", "/servers/two/start", nil)
	// Both administrator browsers see both servers change, without polling.
	for _, s := range []*sseStream{a1, a2} {
		s.expect(t, "one STARTING", statusOf("one", "STARTING"))
	}
	a1.expect(t, "two RUNNING (browser 1)", statusOf("two", "RUNNING"))
	a2.expect(t, "two RUNNING (browser 2)", statusOf("two", "RUNNING"))
	// The restricted user sees server one and nothing about server two.
	fs.expect(t, "one RUNNING", statusOf("one", "RUNNING"))
	fs.never(t, "events about a server without permission, admin events or host metrics", 1500*time.Millisecond, func(e sseEvent) bool {
		return e.Data["server_id"] == "two" || e.Type == "users.changed" || e.Type == "host.metrics" || e.Type == "invitations.changed"
	})

	// Granting access takes effect on the live stream.
	h.grant(admin, frank.ID, "two", 2)
	fs.expect(t, "permissions.changed", isType("permissions.changed"))
	admin.want(202, "POST", "/servers/two/stop", nil)
	fs.expect(t, "two STOPPED after grant", statusOf("two", "STOPPED"))

	// Reconnecting with Last-Event-ID replays only what was missed.
	last := fs.expect(t, "any event with id", func(e sseEvent) bool { return e.ID != "" })
	_ = last
	var lastID string
	for {
		select {
		case ev := <-fs.C:
			if ev.ID != "" {
				lastID = ev.ID
			}
			continue
		case <-time.After(300 * time.Millisecond):
		}
		break
	}
	if lastID == "" {
		lastID = last.ID
	}
	fs.cancel()
	admin.want(202, "POST", "/servers/one/stop", nil)
	h.waitState(admin, "one", "STOPPED")
	fs2 := fc.stream("/events", lastID)
	hello := fs2.expect(t, "hello on reconnect", isType("hello"))
	if hello.Data["resync"] != false {
		t.Fatal("a replayable reconnect must not force a full reload")
	}
	fs2.expect(t, "missed event replayed", statusOf("one", "STOPPED"))

	// Disabling the account cuts the stream at once.
	admin.want(200, "PATCH", fmt.Sprintf("/admin/users/%d", frank.ID), map[string]bool{"disabled": true})
	bye := fs2.expect(t, "bye", isType("bye"))
	if bye.Data["reason"] != "unauthorized" {
		t.Fatalf("bye reason %v", bye.Data)
	}
	select {
	case <-fs2.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream of a disabled account must be closed")
	}
	if r := fc.do("GET", "/servers", nil); r.Status != 401 {
		t.Fatalf("disabled account's token: %d", r.Status)
	}
	if r := fc.login("frank", "frank-password-1"); r.Status != 403 || r.code() != "account_disabled" {
		t.Fatalf("disabled account login: %d %s", r.Status, r.code())
	}
}

func TestConsoleStreamLiveOutputAndRevocation(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	admin.want(201, "POST", "/servers", h.serverDef("con"))
	gina := h.createUser("gina", "gina-password-12", false)
	h.grant(admin, gina.ID, "con", 4) // CONSOLE_READ
	gc := h.as("gina", "gina-password-12")

	lines := func(e sseEvent) []string {
		var out []string
		if raw, ok := e.Data["lines"].([]any); ok {
			for _, l := range raw {
				out = append(out, l.(map[string]any)["text"].(string))
			}
		}
		return out
	}
	hasLine := func(text string) func(sseEvent) bool {
		return func(e sseEvent) bool {
			for _, l := range lines(e) {
				if l == text {
					return true
				}
			}
			return false
		}
	}

	// Two viewers watch the same console.
	as, gs := admin.stream("/servers/con/console/stream", ""), gc.stream("/servers/con/console/stream", "")
	as.expect(t, "ready", isType("ready"))
	gs.expect(t, "ready", isType("ready"))
	admin.want(202, "POST", "/servers/con/start", nil)
	as.expect(t, "live startup line (admin)", hasLine("Done (0.3s)!"))
	gs.expect(t, "live startup line (reader)", hasLine("Done (0.3s)!"))
	h.waitState(admin, "con", "RUNNING")
	admin.want(204, "POST", "/servers/con/console/command", map[string]string{"command": "status"})
	ev := gs.expect(t, "command output", hasLine("got: status"))

	// Resume after the last seen line: nothing is duplicated.
	gs.cancel()
	admin.want(204, "POST", "/servers/con/console/command", map[string]string{"command": "while-away"})
	testutil.Eventually(t, 5*time.Second, "output while disconnected", func() bool {
		return strings.Contains(string(admin.do("GET", "/servers/con/console", nil).Raw), "got: while-away")
	})
	gs2 := gc.stream("/servers/con/console/stream", ev.ID)
	var replay []string
	for {
		e := gs2.expect(t, "replay", func(e sseEvent) bool { return e.Type == "lines" || e.Type == "ready" || e.Type == "reset" })
		if e.Type == "reset" {
			t.Fatal("a resumable reconnect must not reset the console")
		}
		if e.Type == "ready" {
			break
		}
		replay = append(replay, lines(e)...)
	}
	joined := strings.Join(replay, "|")
	if !strings.Contains(joined, "got: while-away") || strings.Contains(joined, "got: status") || strings.Contains(joined, "booting") {
		t.Fatalf("replay should contain exactly the missed lines, got %q", replay)
	}

	// Revoking CONSOLE_READ ends the live stream immediately.
	h.grant(admin, gina.ID, "con", 1)
	bye := gs2.expect(t, "bye after revocation", isType("bye"))
	if bye.Data["reason"] != "unauthorized" {
		t.Fatalf("reason %v", bye.Data)
	}
	if s := gc.stream("/servers/con/console/stream", ""); s.Status != 403 {
		t.Fatalf("console stream without CONSOLE_READ: %d", s.Status)
	}
	admin.want(202, "POST", "/servers/con/stop", nil)
	h.waitState(admin, "con", "STOPPED")
}

// ---- administration ----

func TestUserAdministrationSafeguards(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	me := int64(admin.want(200, "GET", "/me", nil).Body["user"].(map[string]any)["id"].(float64))

	if r := admin.do("PATCH", fmt.Sprintf("/admin/users/%d", me), map[string]bool{"is_admin": false}); r.Status != 409 {
		t.Fatalf("removing own admin role: %d", r.Status)
	}
	if r := admin.do("PATCH", fmt.Sprintf("/admin/users/%d", me), map[string]bool{"disabled": true}); r.Status != 409 {
		t.Fatalf("disabling oneself: %d", r.Status)
	}
	if r := admin.do("DELETE", fmt.Sprintf("/admin/users/%d", me), nil); r.Status != 409 {
		t.Fatalf("deleting oneself: %d", r.Status)
	}

	henry := h.createUser("henry", "henry-password-1", false)
	hc := h.as("henry", "henry-password-1")
	// Promote, verify, demote.
	admin.want(200, "PATCH", fmt.Sprintf("/admin/users/%d", henry.ID), map[string]bool{"is_admin": true})
	hc.want(200, "GET", "/admin/users", nil)
	admin.want(200, "PATCH", fmt.Sprintf("/admin/users/%d", henry.ID), map[string]bool{"is_admin": false})
	if r := hc.do("GET", "/admin/users", nil); r.Status != 403 {
		t.Fatalf("demotion must take effect immediately: %d", r.Status)
	}

	// Sessions: list and revoke.
	second := h.as("henry", "henry-password-1")
	detail := admin.want(200, "GET", fmt.Sprintf("/admin/users/%d", henry.ID), nil)
	if n := len(detail.Body["sessions"].([]any)); n != 2 {
		t.Fatalf("sessions listed: %d", n)
	}
	if strings.Contains(string(detail.Raw), "token") {
		t.Fatal("session listings must not contain token material")
	}
	admin.want(200, "DELETE", fmt.Sprintf("/admin/users/%d/sessions", henry.ID), nil)
	if hc.do("GET", "/me", nil).Status != 401 || second.do("GET", "/me", nil).Status != 401 {
		t.Fatal("revoked sessions must stop working immediately")
	}

	// Users manage their own sessions.
	hc = h.as("henry", "henry-password-1")
	other := h.as("henry", "henry-password-1")
	if r := hc.want(200, "POST", "/me/sessions/revoke-others", nil); r.Body["revoked"].(float64) != 1 {
		t.Fatalf("revoke others: %s", r.Raw)
	}
	if other.do("GET", "/me", nil).Status != 401 || hc.do("GET", "/me", nil).Status != 200 {
		t.Fatal("only the other session should be revoked")
	}

	// Password change: needs the current password, signs out other devices.
	other = h.as("henry", "henry-password-1")
	if r := hc.do("POST", "/me/password", map[string]string{"current_password": "wrong", "new_password": "new-henry-password"}); r.Status != 403 {
		t.Fatalf("password change with wrong current password: %d", r.Status)
	}
	hc.want(200, "POST", "/me/password", map[string]string{"current_password": "henry-password-1", "new_password": "new-henry-password"})
	if other.do("GET", "/me", nil).Status != 401 {
		t.Fatal("other sessions must be revoked after a password change")
	}
	if r := h.client().login("henry", "henry-password-1"); r.Status != 401 {
		t.Fatal("the old password must stop working")
	}
	h.as("henry", "new-henry-password")

	// Deleting an account removes access but keeps the audit trail.
	admin.want(204, "DELETE", fmt.Sprintf("/admin/users/%d", henry.ID), nil)
	if hc.do("GET", "/me", nil).Status != 401 {
		t.Fatal("deleted account must lose access")
	}
	if n := len(admin.want(200, "GET", "/admin/audit?username=henry", nil).Body["entries"].([]any)); n == 0 {
		t.Fatal("audit entries must survive account deletion")
	}
}

func TestEmailChangeRequiresVerification(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	if r := admin.do("POST", "/me/email", map[string]string{"email": "new@example.com", "current_password": "wrong"}); r.Status != 403 {
		t.Fatalf("email change with wrong password: %d", r.Status)
	}
	if r := admin.do("POST", "/me/email", map[string]string{"email": "not-an-address", "current_password": adminPassword}); r.Status != 400 {
		t.Fatalf("invalid address: %d", r.Status)
	}
	admin.want(202, "POST", "/me/email", map[string]string{"email": "New@Example.com", "current_password": adminPassword})
	if email := admin.want(200, "GET", "/me", nil).Body["user"].(map[string]any)["email"]; email != "" {
		t.Fatalf("the address must not change before verification, got %v", email)
	}
	// Development mode prints the message; pull the link out of the log.
	m := regexp.MustCompile(`#/verify-email\?token=([A-Za-z0-9_-]{40,})`).FindStringSubmatch(h.logs.String())
	if m == nil {
		t.Fatal("verification email not found in development log")
	}
	if r := h.client().do("POST", "/auth/email/verify", map[string]string{"token": "bogus"}); r.Status != 404 {
		t.Fatalf("bogus verification token: %d", r.Status)
	}
	h.client().want(200, "POST", "/auth/email/verify", map[string]string{"token": m[1]})
	if email := admin.want(200, "GET", "/me", nil).Body["user"].(map[string]any)["email"]; email != "new@example.com" {
		t.Fatalf("email after verification: %v", email)
	}
	if r := h.client().do("POST", "/auth/email/verify", map[string]string{"token": m[1]}); r.Status != 404 {
		t.Fatalf("verification links are single-use: %d", r.Status)
	}

	// Production without SMTP cannot verify, and says so.
	p := newHarness(t, func(c *config.Config) { c.Mode = config.ModeProduction })
	if r := p.admin().do("POST", "/me/email", map[string]string{"email": "x@example.com", "current_password": adminPassword}); r.Status != 409 || r.code() != "email_unavailable" {
		t.Fatalf("email change without delivery: %d %s", r.Status, r.code())
	}
}

func TestSettingsEndpoint(t *testing.T) {
	t.Setenv("SM_SMTP_PASSWORD", "smtp-super-secret")
	t.Setenv("SM_JWT_SECRET", strings.Repeat("j", 48))
	h := newHarness(t, nil)
	admin := h.admin()
	got := admin.want(200, "GET", "/admin/settings", nil)
	for _, secret := range []string{"smtp-super-secret", strings.Repeat("j", 48)} {
		if strings.Contains(string(got.Raw), secret) {
			t.Fatal("settings must never include secret values")
		}
	}
	if got.Body["info"].(map[string]any)["smtp_password_set"] != true {
		t.Fatal("presence of the SMTP password should be reported as a boolean")
	}
	settings := got.Body["settings"].(map[string]any)
	settings["rate_limits"].(map[string]any)["login"] = map[string]any{"requests": 9, "window": "2m"}
	settings["monitoring"].(map[string]any)["host_metrics_for_users"] = false
	admin.want(200, "PUT", "/admin/settings", settings)
	if c := h.cfg.Get(); c.RateLimits.Login.Requests != 9 || c.RateLimits.Login.Window.D() != 2*time.Minute || c.Monitoring.HostMetricsForUsers {
		t.Fatalf("settings not applied: %+v", c.RateLimits.Login)
	}
	settings["auth"].(map[string]any)["access_token_ttl"] = "1s"
	if r := admin.do("PUT", "/admin/settings", settings); r.Status != 422 || r.code() != "validation_failed" {
		t.Fatalf("invalid settings: %d %s", r.Status, r.Raw)
	}
	// Fields that are not editable through the API are rejected outright.
	if r := admin.do("PUT", "/admin/settings", map[string]any{"listen": "0.0.0.0:80"}); r.Status != 400 {
		t.Fatalf("non-editable field: %d", r.Status)
	}
	if r := admin.do("PUT", "/admin/settings", map[string]any{"paths": map[string]string{"database": "/tmp/x.db"}}); r.Status != 400 {
		t.Fatalf("paths must not be editable: %d", r.Status)
	}

	// Host metrics respect the visibility setting.
	u := h.createUser("ivan", "ivan-password-12", false)
	_ = u
	ic := h.as("ivan", "ivan-password-12")
	if r := ic.do("GET", "/system/metrics", nil); r.Status != 403 {
		t.Fatalf("metrics hidden from users: %d", r.Status)
	}
	testutil.Eventually(t, 10*time.Second, "first host sample", func() bool {
		return admin.do("GET", "/system/metrics", nil).Body["latest"] != nil
	})
	m := admin.want(200, "GET", "/system/metrics", nil).Body["latest"].(map[string]any)
	if m["mem_total_bytes"].(float64) <= 0 || m["mem_available_bytes"].(float64) > m["mem_total_bytes"].(float64) {
		t.Fatalf("implausible metrics: %v", m)
	}
	for _, k := range []string{"cpu_percent", "mem_used_bytes", "mem_used_percent", "mem_available_bytes", "mem_total_bytes"} {
		if _, ok := m[k]; !ok {
			t.Errorf("metrics lack %s", k)
		}
	}
}

func TestStateSurvivesApplicationRestart(t *testing.T) {
	cfg := testutil.Config(t)
	dir := t.TempDir()
	start := func() (*app.App, *httptest.Server) {
		a, err := app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{NoStatic: true})
		if err != nil {
			t.Fatal(err)
		}
		return a, httptest.NewServer(a.Handler)
	}
	stop := func(a *app.App, s *httptest.Server) {
		s.CloseClientConnections()
		s.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		a.Close(ctx)
	}
	a, srv := start()
	h := &harness{t: t, app: a, srv: srv, cfg: cfg, logs: &syncBuffer{}, dir: dir}
	h.createUser("admin", adminPassword, true)
	kate := h.createUser("kate", "kate-password-12", false)
	admin := h.admin()
	admin.want(201, "POST", "/servers", h.serverDef("persist"))
	h.grant(admin, kate.ID, "persist", 5)
	admin.want(202, "POST", "/servers/persist/start", nil)
	h.waitState(admin, "persist", "RUNNING")
	stop(a, srv)

	a, srv = start()
	defer stop(a, srv)
	h.app, h.srv = a, srv
	// Accounts, permissions, definitions and history are all still there,
	// and the server that kept running is recognised as RUNNING.
	admin = h.admin()
	kc := h.as("kate", "kate-password-12")
	if got := kc.want(200, "GET", "/me", nil).Body["permissions"].(map[string]any)["persist"].(float64); got != 5 {
		t.Fatalf("permissions after restart: %v", got)
	}
	h.waitState(admin, "persist", "RUNNING")
	ops := admin.want(200, "GET", "/servers/persist/operations", nil).Body["operations"].([]any)
	if len(ops) != 1 || ops[0].(map[string]any)["status"] != "succeeded" {
		t.Fatalf("operation history after restart: %v", ops)
	}
	if n := len(admin.want(200, "GET", "/admin/audit?action=SERVER_START", nil).Body["entries"].([]any)); n != 2 {
		t.Fatalf("audit entries after restart: %d", n)
	}
	admin.want(202, "POST", "/servers/persist/stop", nil)
	h.waitState(admin, "persist", "STOPPED")
}

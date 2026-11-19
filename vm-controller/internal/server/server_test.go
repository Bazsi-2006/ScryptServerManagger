package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/control"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/store"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/vexa"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/vexatest"
)

const (
	apiKey   = "vxa_server_test_secret"
	password = "correct horse battery"
	vm1      = "aaaaaaaa-1111-4111-8111-11111111111a" // Minecraft 1, stop disabled
	vm2      = "bbbbbbbb-2222-4222-8222-22222222222b" // Minecraft 2, stop allowed
	vm3      = "cccccccc-3333-4333-8333-33333333333c" // nobody assigned
	vexaOnly = "dddddddd-4444-4444-8444-44444444444d" // exists in Vexa, not in config
)

var fastParams = auth.Params{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1}

const multiConfig = `{
  "mode": "multiuser",
  "base_path": "/vmctl",
  "vexa": {"base_url": "VEXA_URL"},
  "vms": [
    {"id": "` + vm1 + `", "name": "Minecraft VM 1", "allow_stop": false, "url": "/"},
    {"id": "` + vm2 + `", "name": "Minecraft VM 2", "allow_stop": true},
    {"id": "` + vm3 + `", "name": "Private VM", "allow_stop": true}
  ],
  "users": [
    {"username": "alice", "permissions": {"` + vm1 + `": "start"}},
    {"username": "bob", "permissions": {"` + vm2 + `": "start-stop"}},
    {"username": "charlie", "permissions": {"` + vm1 + `": "start", "` + vm2 + `": "start"}},
    {"username": "dave", "disabled": true, "permissions": {"` + vm2 + `": "start-stop"}},
    {"username": "nopass", "permissions": {"` + vm1 + `": "start"}}
  ]
}`

const singleConfig = `{
  "mode": "single-user",
  "vexa": {"base_url": "VEXA_URL"},
  "vms": [
    {"id": "` + vm1 + `", "name": "Minecraft Game Servers", "allow_stop": false},
    {"id": "` + vm2 + `", "name": "Development VM", "allow_stop": true},
    {"id": "` + vm3 + `", "name": "Testing VM", "allow_stop": false}
  ],
  "single_user": {"username": "owner"}
}`

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t     *testing.T
	mock  *vexatest.Server
	cfg   *config.Config
	store *store.Store
	srv   *Server
	ts    *httptest.Server
	clk   *clock
	logs  *bytes.Buffer
	base  string
	// bodies collects every response body so tests can assert that no
	// secret ever reached a browser.
	mu     sync.Mutex
	bodies []string
}

func newHarness(t *testing.T, cfgJSON string, users ...string) *harness {
	t.Helper()
	mock := vexatest.New(apiKey)
	t.Cleanup(mock.Close)
	mock.AddVM(vm1, "minecraft-1", "stopped")
	mock.AddVM(vm2, "minecraft-2", "running")
	mock.AddVM(vm3, "private", "stopped")
	mock.AddVM(vexaOnly, "not-configured", "stopped")

	dir := t.TempDir()
	js := strings.Replace(cfgJSON, "VEXA_URL", mock.URL(), 1)
	js = strings.Replace(js, `"mode"`, `"database": "`+filepath.Join(dir, "vm.db")+`", "mode"`, 1)
	cfg, err := config.Parse([]byte(js), dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	for _, u := range users {
		h, err := auth.HashPasswordWithParams(password, fastParams)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetPassword(context.Background(), u, h, clk.Now()); err != nil {
			t.Fatal(err)
		}
	}
	h := &harness{t: t, mock: mock, cfg: cfg, store: st, clk: clk, logs: &bytes.Buffer{}, base: cfg.BasePath}
	h.start()
	return h
}

// start (re)creates the server on top of the same database, e.g. after a
// configuration change.
func (h *harness) start() {
	h.t.Helper()
	logger := slog.New(slog.NewTextHandler(syncWriter{h}, nil))
	client, err := vexa.New(vexa.Options{BaseURL: h.cfg.Vexa.BaseURL, APIKey: apiKey, Timeout: 2 * time.Second})
	if err != nil {
		h.t.Fatal(err)
	}
	ctl := control.New(client, h.cfg.VMs, control.Options{
		CacheTTL: h.cfg.Vexa.StatusCacheTTL, StartTimeout: h.cfg.Vexa.StartTimeout, StopTimeout: h.cfg.Vexa.StopTimeout,
		Logger: logger, Now: h.clk.Now,
	})
	srv, err := New(h.cfg, h.store, ctl, Options{Logger: logger, Now: h.clk.Now})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := srv.Prepare(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	if h.ts != nil {
		h.ts.Close()
	}
	h.srv = srv
	h.ts = httptest.NewServer(srv.Handler())
	h.t.Cleanup(h.ts.Close)
}

type syncWriter struct{ h *harness }

func (w syncWriter) Write(p []byte) (int, error) {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	return w.h.logs.Write(p)
}

func (h *harness) reconfigure(cfgJSON string) {
	h.t.Helper()
	js := strings.Replace(cfgJSON, "VEXA_URL", h.mock.URL(), 1)
	js = strings.Replace(js, `"mode"`, `"database": "`+h.cfg.Database+`", "mode"`, 1)
	cfg, err := config.Parse([]byte(js), "")
	if err != nil {
		h.t.Fatal(err)
	}
	h.cfg = cfg
	h.start()
}

type browser struct {
	h    *harness
	c    *http.Client
	csrf string
}

func (h *harness) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{h: h, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

type result struct {
	status int
	header http.Header
	body   string
	json   map[string]any
}

func (r result) code() string {
	if e, ok := r.json["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func (r result) message() string {
	if e, ok := r.json["error"].(map[string]any); ok {
		s, _ := e["message"].(string)
		return s
	}
	return ""
}

// do sends a request to path below the base path. headers may override the
// default CSRF header; pass "X-CSRF-Token": "" to omit it.
func (b *browser) do(method, path string, body any, headers ...string) result {
	b.h.t.Helper()
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, b.h.ts.URL+b.h.base+path, rd)
	if err != nil {
		b.h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && b.csrf != "" {
		req.Header.Set("X-CSRF-Token", b.csrf)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	b.h.mu.Lock()
	b.h.bodies = append(b.h.bodies, string(data)+fmtHeaders(resp.Header))
	b.h.mu.Unlock()
	r := result{status: resp.StatusCode, header: resp.Header, body: string(data)}
	_ = json.Unmarshal(data, &r.json)
	return r
}

func fmtHeaders(h http.Header) string {
	var sb strings.Builder
	for k, v := range h {
		sb.WriteString(k + ": " + strings.Join(v, ",") + "\n")
	}
	return sb.String()
}

func (b *browser) login(user string) {
	b.h.t.Helper()
	r := b.do("POST", "/api/login", map[string]string{"username": user, "password": password})
	if r.status != http.StatusOK {
		b.h.t.Fatalf("login %s: %d %s", user, r.status, r.body)
	}
	b.csrf, _ = r.json["csrf_token"].(string)
	if b.csrf == "" {
		b.h.t.Fatal("login returned no CSRF token")
	}
}

func (h *harness) assertPowerCalls(want ...string) {
	h.t.Helper()
	got := h.mock.PowerCalls()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		h.t.Fatalf("Vexa power calls = %v, want %v", got, want)
	}
}

func (h *harness) assertNoSecretsLeaked() {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range h.bodies {
		if strings.Contains(b, apiKey) || strings.Contains(b, "argon2id") || strings.Contains(b, h.mock.URL()) {
			h.t.Fatalf("response leaked a secret or internal address: %s", b)
		}
	}
	if strings.Contains(h.logs.String(), apiKey) {
		h.t.Fatal("the API key appeared in the log")
	}
}

func vmIDs(r result) []string {
	var ids []string
	list, _ := r.json["vms"].([]any)
	for _, v := range list {
		m, _ := v.(map[string]any)
		id, _ := m["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func vmByID(r result, id string) map[string]any {
	list, _ := r.json["vms"].([]any)
	for _, v := range list {
		if m, _ := v.(map[string]any); m["id"] == id {
			return m
		}
	}
	return nil
}

// ---- tests -----------------------------------------------------------------

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	h := newHarness(t, multiConfig, "alice")
	b := h.browser()
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/vms"},
		{"GET", "/api/vms/" + vm1},
		{"POST", "/api/vms/" + vm1 + "/start"},
		{"POST", "/api/vms/" + vm2 + "/stop"},
		{"POST", "/api/logout"},
	} {
		if r := b.do(c.method, c.path, nil); r.status != http.StatusUnauthorized || r.code() != "unauthenticated" {
			t.Errorf("%s %s: %d %s", c.method, c.path, r.status, r.body)
		}
	}
	// A forged cookie is no better than none.
	forged := &http.Cookie{Name: "vmctl_session", Value: strings.Repeat("A", auth.TokenLen)}
	req, _ := http.NewRequest("POST", h.ts.URL+"/vmctl/api/vms/"+vm1+"/start", nil)
	req.AddCookie(forged)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged cookie: %d", resp.StatusCode)
	}
	if n := len(h.mock.Requests()); n != 0 {
		t.Fatalf("unauthenticated requests reached Vexa %d times", n)
	}
	if r := b.do("GET", "/api/session", nil); r.status != 200 || r.json["authenticated"] != false {
		t.Fatalf("session: %s", r.body)
	}
}

func TestLoginOutcomes(t *testing.T) {
	h := newHarness(t, multiConfig, "alice", "bob", "dave")
	b := h.browser()
	cases := []struct {
		name, user, pass string
	}{
		{"wrong password", "alice", "wrong password!"},
		{"unknown user", "mallory", password},
		{"disabled user", "dave", password},
		{"user without password", "nopass", password},
		{"empty password", "alice", ""},
	}
	for _, c := range cases {
		r := b.do("POST", "/api/login", map[string]string{"username": c.user, "password": c.pass})
		if r.status != http.StatusUnauthorized || r.message() != invalidCredentials {
			t.Errorf("%s: %d %s", c.name, r.status, r.body)
		}
		if r.header.Get("Set-Cookie") != "" {
			t.Errorf("%s: a cookie was set", c.name)
		}
	}

	if r := b.do("POST", "/api/login", "not an object"); r.status != http.StatusBadRequest {
		t.Errorf("malformed body: %d", r.status)
	}
	r := b.do("POST", "/api/login", map[string]string{"username": "alice", "password": password}, "Content-Type", "text/plain")
	if r.status != http.StatusUnsupportedMediaType {
		t.Errorf("form-style login must be rejected: %d", r.status)
	}
	r = b.do("POST", "/api/login", map[string]any{"username": "alice", "password": password, "admin": true})
	if r.status != http.StatusBadRequest {
		t.Errorf("unknown fields must be rejected: %d", r.status)
	}

	// Usernames are case-insensitive and trimmed.
	r = b.do("POST", "/api/login", map[string]string{"username": "  Alice ", "password": password})
	if r.status != 200 || r.json["username"] != "alice" {
		t.Fatalf("login: %d %s", r.status, r.body)
	}
	sc := r.header.Get("Set-Cookie")
	for _, want := range []string{"vmctl_session=", "Path=/vmctl", "HttpOnly", "SameSite=Strict", "Max-Age=43200"} {
		if !strings.Contains(sc, want) {
			t.Errorf("Set-Cookie %q lacks %q", sc, want)
		}
	}
	if strings.Contains(sc, "Secure") {
		t.Error("plain-HTTP request without a trusted proxy header must not get a Secure cookie")
	}
	b.csrf, _ = r.json["csrf_token"].(string)
	if r := b.do("GET", "/api/session", nil); r.json["authenticated"] != true || r.json["csrf_token"] != b.csrf {
		t.Fatalf("session after login: %s", r.body)
	}
	if strings.Contains(h.logs.String(), password) {
		t.Fatal("password written to the log")
	}
	h.assertNoSecretsLeaked()
}

func TestSecureCookieBehindTrustedProxy(t *testing.T) {
	h := newHarness(t, multiConfig, "alice")
	b := h.browser()
	r := b.do("POST", "/api/login", map[string]string{"username": "alice", "password": password},
		"X-Forwarded-Proto", "https", "X-Forwarded-For", "203.0.113.7")
	sc := r.header.Get("Set-Cookie")
	if r.status != 200 || !strings.Contains(sc, "__Secure-vmctl_session=") || !strings.Contains(sc, "; Secure") {
		t.Fatalf("Set-Cookie behind HTTPS proxy: %q", sc)
	}
	if !strings.Contains(h.logs.String(), "client=203.0.113.7") {
		t.Errorf("client address from trusted proxy not used: %s", h.logs.String())
	}

	// Root base path: __Host- prefix.
	h2 := newHarness(t, singleConfig, "owner")
	r = h2.browser().do("POST", "/api/login", map[string]string{"username": "owner", "password": password}, "X-Forwarded-Proto", "https")
	if sc := r.header.Get("Set-Cookie"); !strings.Contains(sc, "__Host-vmctl_session=") || !strings.Contains(sc, "Path=/;") {
		t.Fatalf("root path cookie: %q", sc)
	}
}

func TestUntrustedForwardedHeadersAreIgnored(t *testing.T) {
	h := newHarness(t, strings.Replace(multiConfig, `"mode"`, `"trusted_proxies": [], "mode"`, 1), "alice", "bob")
	b := h.browser()
	r := b.do("POST", "/api/login", map[string]string{"username": "alice", "password": password}, "X-Forwarded-Proto", "https")
	if strings.Contains(r.header.Get("Set-Cookie"), "Secure") {
		t.Fatal("X-Forwarded-Proto from an untrusted peer was believed")
	}
	// Rotating X-Forwarded-For must not escape the per-address limit.
	for i := 0; i < 10; i++ {
		b.do("POST", "/api/login", map[string]string{"username": "nobody", "password": "x"}, "X-Forwarded-For", "198.51.100."+string(rune('0'+i)))
	}
	r = b.do("POST", "/api/login", map[string]string{"username": "bob", "password": password}, "X-Forwarded-For", "198.51.100.99")
	if r.status != http.StatusTooManyRequests {
		t.Fatalf("spoofed X-Forwarded-For bypassed the limiter: %d", r.status)
	}
}

func TestLoginRateLimiting(t *testing.T) {
	h := newHarness(t, multiConfig, "alice", "bob")
	b := h.browser()
	for i := 0; i < 8; i++ {
		if r := b.do("POST", "/api/login", map[string]string{"username": "alice", "password": "nope"}); r.status != 401 {
			t.Fatalf("attempt %d: %d", i, r.status)
		}
	}
	r := b.do("POST", "/api/login", map[string]string{"username": "alice", "password": password})
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" || !strings.Contains(r.message(), "Try again in") {
		t.Fatalf("account lockout: %d %s", r.status, r.body)
	}
	// Other accounts from the same address still work until the address
	// limit (10) is reached.
	if r := b.do("POST", "/api/login", map[string]string{"username": "bob", "password": password}); r.status != 200 {
		t.Fatalf("bob: %d", r.status)
	}
	b.do("POST", "/api/login", map[string]string{"username": "bob", "password": "nope"})
	b.do("POST", "/api/login", map[string]string{"username": "bob", "password": "nope"})
	if r := b.do("POST", "/api/login", map[string]string{"username": "bob", "password": password}); r.status != http.StatusTooManyRequests {
		t.Fatalf("address lockout: %d", r.status)
	}
	h.clk.Advance(16 * time.Minute)
	if r := b.do("POST", "/api/login", map[string]string{"username": "alice", "password": password}); r.status != 200 {
		t.Fatalf("after the window: %d %s", r.status, r.body)
	}
}

func TestMultiuserAuthorization(t *testing.T) {
	h := newHarness(t, multiConfig, "alice", "bob", "charlie")
	alice, bob, charlie := h.browser(), h.browser(), h.browser()
	alice.login("alice")
	bob.login("bob")
	charlie.login("charlie")

	// Visibility.
	r := alice.do("GET", "/api/vms", nil)
	if ids := vmIDs(r); len(ids) != 1 || ids[0] != vm1 {
		t.Fatalf("alice sees %v", ids)
	}
	if v := vmByID(r, vm1); v["permission"] != "start" || len(v["actions"].([]any)) != 1 || v["state"] != "stopped" {
		t.Fatalf("alice vm1: %v", v)
	}
	if ids := vmIDs(bob.do("GET", "/api/vms", nil)); len(ids) != 1 || ids[0] != vm2 {
		t.Fatalf("bob sees %v", ids)
	}
	r = charlie.do("GET", "/api/vms", nil)
	if ids := vmIDs(r); len(ids) != 2 {
		t.Fatalf("charlie sees %v", ids)
	}
	if v := vmByID(r, vm2); v["permission"] != "start" || len(v["actions"].([]any)) != 0 {
		t.Fatalf("charlie must not be offered stop on running vm2: %v", v)
	}
	for _, id := range []string{vm2, vm3, vexaOnly, "nonexistent"} {
		if r := alice.do("GET", "/api/vms/"+id, nil); r.status != http.StatusNotFound {
			t.Errorf("alice GET %s: %d", id, r.status)
		}
	}
	if r := alice.do("GET", "/api/vms/"+vm1, nil); r.status != 200 {
		t.Errorf("alice GET own vm: %d", r.status)
	}

	// Actions outside the user's assignment never reach Vexa.
	denied := []struct {
		b      *browser
		path   string
		status int
	}{
		{alice, "/api/vms/" + vm2 + "/start", 404},
		{alice, "/api/vms/" + vm3 + "/start", 404},
		{alice, "/api/vms/" + vexaOnly + "/start", 404},
		{alice, "/api/vms/" + vm1 + "/stop", 403},
		{bob, "/api/vms/" + vm1 + "/start", 404},
		{charlie, "/api/vms/" + vm1 + "/stop", 403},
		{charlie, "/api/vms/" + vm2 + "/stop", 403}, // VM allows stop, charlie does not
		{charlie, "/api/vms/" + vm3 + "/stop", 404},
		{alice, "/api/vms/" + strings.ToUpper(vm1) + "/start", 404},
		{alice, "/api/vms/" + vm1 + "/force-off", 404},
		{alice, "/api/vms/" + vm1 + "/reboot", 404},
		{alice, "/api/vms/" + vm1 + "/actions/start", 404},
		{alice, "/api/vms/..%2F" + vm2 + "/start", 404},
		{alice, "/api/vms/" + vm1 + "%2F..%2F" + vm2 + "/start", 404},
		{alice, "/api/vms/" + vm2 + "%00/start", 404},
		{alice, "/api/vms/%2e%2e/start", 404},
		{alice, "/api/v1/vms/" + vm1 + "/actions/start", 404},
		{alice, "/api/proxy?path=/api/v1/vms", 404},
	}
	for _, d := range denied {
		if r := d.b.do("POST", d.path, nil); r.status != d.status {
			t.Errorf("POST %s: %d %s, want %d", d.path, r.status, r.body, d.status)
		}
	}
	// GET on a power route falls through to the JSON 404 of /api/.
	if r := alice.do("GET", "/api/vms/"+vm1+"/start", nil); r.status != http.StatusNotFound {
		t.Errorf("GET on a power route: %d", r.status)
	}
	h.assertPowerCalls()

	// A body cannot change the action.
	r = alice.do("POST", "/api/vms/"+vm1+"/start", map[string]string{"action": "force-off", "id": vm2})
	if r.status != http.StatusAccepted {
		t.Fatalf("alice start vm1: %d %s", r.status, r.body)
	}
	vm, _ := r.json["vm"].(map[string]any)
	if vm["id"] != vm1 || (vm["state"] != "starting" && vm["state"] != "running") {
		t.Fatalf("start response: %s", r.body)
	}
	if r := bob.do("POST", "/api/vms/"+vm2+"/stop", nil); r.status != http.StatusAccepted {
		t.Fatalf("bob stop vm2: %d %s", r.status, r.body)
	}
	h.assertPowerCalls(vm1+" start", vm2+" shutdown")

	// A second, conflicting request is refused while the shutdown is pending.
	if r := bob.do("POST", "/api/vms/"+vm2+"/stop", nil); r.status != http.StatusConflict {
		t.Fatalf("repeated stop: %d %s", r.status, r.body)
	}
	if r := bob.do("POST", "/api/vms/"+vm2+"/start", nil); r.status != http.StatusConflict {
		t.Fatalf("start during stop: %d %s", r.status, r.body)
	}
	r = bob.do("GET", "/api/vms", nil)
	if v := vmByID(r, vm2); v["state"] != "stopping" || v["pending"] != "stop" || len(v["actions"].([]any)) != 0 {
		t.Fatalf("pending stop not shown: %v", v)
	}
	if r.json["poll_seconds"] != float64(3) {
		t.Errorf("poll_seconds while pending = %v", r.json["poll_seconds"])
	}
	h.assertPowerCalls(vm1+" start", vm2+" shutdown")
	h.assertNoSecretsLeaked()
	if !strings.Contains(h.logs.String(), "power request denied") {
		t.Error("denied requests should be logged")
	}
}

func TestSingleUserMode(t *testing.T) {
	h := newHarness(t, singleConfig, "owner")
	b := h.browser()
	b.login("owner")
	r := b.do("GET", "/api/vms", nil)
	if ids := vmIDs(r); len(ids) != 3 {
		t.Fatalf("owner sees %v", ids)
	}
	if v := vmByID(r, vm1); v["permission"] != "start" {
		t.Fatalf("vm1 permission %v", v["permission"])
	}
	if v := vmByID(r, vm2); v["permission"] != "start-stop" || v["actions"].([]any)[0] != "stop" {
		t.Fatalf("vm2: %v", v)
	}
	h.mock.SetState(vm1, "running")
	h.clk.Advance(10 * time.Second)
	if r := b.do("POST", "/api/vms/"+vm1+"/stop", nil); r.status != http.StatusForbidden {
		t.Fatalf("stop on allow_stop=false VM: %d %s", r.status, r.body)
	}
	if r := b.do("POST", "/api/vms/"+vm3+"/start", nil); r.status != http.StatusAccepted {
		t.Fatalf("start vm3: %d %s", r.status, r.body)
	}
	if r := b.do("POST", "/api/vms/"+vm2+"/stop", nil); r.status != http.StatusAccepted {
		t.Fatalf("stop vm2: %d %s", r.status, r.body)
	}
	if r := b.do("POST", "/api/vms/"+vm2+"/start", nil); r.status != http.StatusConflict {
		t.Fatalf("start while stopping: %d", r.status)
	}
	h.assertPowerCalls(vm3+" start", vm2+" shutdown")
}

func TestCSRFAndCrossOriginProtection(t *testing.T) {
	h := newHarness(t, multiConfig, "alice")
	b := h.browser()
	b.login("alice")
	cases := []struct {
		name    string
		headers []string
		code    string
	}{
		{"missing token", []string{"X-CSRF-Token", ""}, "csrf_invalid"},
		{"wrong token", []string{"X-CSRF-Token", strings.Repeat("x", auth.TokenLen)}, "csrf_invalid"},
		{"cross-site fetch", []string{"Sec-Fetch-Site", "cross-site"}, "cross_origin"},
		{"same-site sibling", []string{"Sec-Fetch-Site", "same-site"}, "cross_origin"},
		{"foreign origin", []string{"Origin", "https://evil.example"}, "cross_origin"},
	}
	for _, c := range cases {
		r := b.do("POST", "/api/vms/"+vm1+"/start", nil, c.headers...)
		if r.status != http.StatusForbidden || r.code() != c.code {
			t.Errorf("%s: %d %s", c.name, r.status, r.body)
		}
	}
	// Logout is protected too.
	if r := b.do("POST", "/api/logout", nil, "X-CSRF-Token", ""); r.status != http.StatusForbidden {
		t.Errorf("logout without token: %d", r.status)
	}
	// Login CSRF.
	r := h.browser().do("POST", "/api/login", map[string]string{"username": "alice", "password": password}, "Sec-Fetch-Site", "cross-site")
	if r.status != http.StatusForbidden {
		t.Errorf("cross-site login: %d", r.status)
	}
	h.assertPowerCalls()
	// The same request from the page itself works.
	if r := b.do("POST", "/api/vms/"+vm1+"/start", nil, "Sec-Fetch-Site", "same-origin", "Origin", h.ts.URL); r.status != http.StatusAccepted {
		t.Fatalf("same-origin request: %d %s", r.status, r.body)
	}
}

func TestSessionLifecycle(t *testing.T) {
	h := newHarness(t, multiConfig, "alice", "bob")
	b := h.browser()
	b.login("alice")

	// Logout invalidates the session server-side.
	old := b.csrf
	if r := b.do("POST", "/api/logout", nil); r.status != 200 || !strings.Contains(r.header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout: %d %v", r.status, r.header)
	}
	if r := b.do("GET", "/api/vms", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", r.status)
	}
	_ = old

	// Session fixation: logging in again issues a new token and the old one
	// stops working.
	b.login("alice")
	first := cookieValue(b, h)
	b.login("alice")
	second := cookieValue(b, h)
	if first == "" || first == second {
		t.Fatal("login did not rotate the session token")
	}
	if _, err := h.store.Session(context.Background(), auth.HashToken(first)); err != store.ErrNotFound {
		t.Fatal("the previous session survived a new login")
	}

	// Idle timeout (2h by default).
	h.clk.Advance(2*time.Hour + time.Minute)
	if r := b.do("GET", "/api/vms", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("idle session still valid: %d", r.status)
	}

	// Absolute lifetime (12h), even with continuous activity.
	b.login("alice")
	for i := 0; i < 13; i++ {
		h.clk.Advance(59 * time.Minute)
		b.do("GET", "/api/session", nil)
	}
	if r := b.do("GET", "/api/vms", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("session outlived its absolute lifetime: %d", r.status)
	}

	// Changing a password (as the CLI does) revokes existing sessions.
	b.login("alice")
	hsh, _ := auth.HashPasswordWithParams("another password", fastParams)
	if err := h.store.SetPassword(context.Background(), "alice", hsh, h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if r := b.do("GET", "/api/vms", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("session survived a password change: %d", r.status)
	}
}

func cookieValue(b *browser, h *harness) string {
	req, _ := http.NewRequest("GET", h.ts.URL+"/vmctl/", nil)
	for _, c := range b.c.Jar.Cookies(req.URL) {
		if c.Name == "vmctl_session" {
			return c.Value
		}
	}
	return ""
}

func TestRemovedOrDisabledUserLosesAccess(t *testing.T) {
	h := newHarness(t, multiConfig, "alice", "bob", "charlie")
	alice, bob, charlie := h.browser(), h.browser(), h.browser()
	alice.login("alice")
	bob.login("bob")
	charlie.login("charlie")

	// Remove alice, disable bob, restart.
	cfg := strings.Replace(multiConfig, `{"username": "alice", "permissions": {"`+vm1+`": "start"}},`, ``, 1)
	cfg = strings.Replace(cfg, `{"username": "bob", "permissions"`, `{"username": "bob", "disabled": true, "permissions"`, 1)
	h.reconfigure(cfg)

	if r := alice.do("GET", "/api/vms", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("removed user still has access: %d", r.status)
	}
	if r := bob.do("POST", "/api/vms/"+vm2+"/stop", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("disabled user still has access: %d", r.status)
	}
	if r := charlie.do("GET", "/api/vms", nil); r.status != 200 {
		t.Fatalf("unaffected user lost access: %d", r.status)
	}
	if r := alice.do("POST", "/api/login", map[string]string{"username": "alice", "password": password}); r.status != 401 {
		t.Fatalf("removed user can log in: %d", r.status)
	}
	h.assertPowerCalls()

	// Narrowing a permission takes effect for existing sessions.
	cfg2 := strings.Replace(cfg, `"`+vm2+`": "start"}}`, `"`+vm2+`": "none"}}`, 1)
	h.reconfigure(cfg2)
	if ids := vmIDs(charlie.do("GET", "/api/vms", nil)); len(ids) != 1 || ids[0] != vm1 {
		t.Fatalf("charlie after narrowing: %v", ids)
	}
	if r := charlie.do("POST", "/api/vms/"+vm2+"/start", nil); r.status != http.StatusNotFound {
		t.Fatalf("revoked VM still usable: %d", r.status)
	}
	h.assertPowerCalls()
}

func TestVexaFailuresAreHandled(t *testing.T) {
	h := newHarness(t, singleConfig, "owner")
	b := h.browser()
	b.login("owner")

	h.mock.Fail("GET /api/v1/vms/", vexatest.Failure{Status: 500, Code: "database_error", Message: "secret internal detail at /var/lib/vexa"})
	r := b.do("GET", "/api/vms", nil)
	if r.status != 200 {
		t.Fatalf("list must still work when Vexa fails: %d", r.status)
	}
	if v := vmByID(r, vm1); v["state"] != "unavailable" || len(v["actions"].([]any)) != 0 {
		t.Fatalf("vm1 during outage: %v", v)
	}
	if r := b.do("POST", "/api/vms/"+vm1+"/start", nil); r.status != http.StatusServiceUnavailable {
		t.Fatalf("start during outage: %d %s", r.status, r.body)
	}
	h.mock.ClearFailures()
	h.clk.Advance(10 * time.Second)

	h.mock.Fail("POST /api/v1/vms/", vexatest.Failure{Status: 403, Code: "forbidden", Message: "secret internal detail: missing scope"})
	r = b.do("POST", "/api/vms/"+vm1+"/start", nil)
	if r.status != http.StatusBadGateway || r.code() != "vexa_error" {
		t.Fatalf("Vexa 403 on power: %d %s", r.status, r.body)
	}

	h.mock.Close() // Vexa is gone entirely
	h.clk.Advance(10 * time.Second)
	r = b.do("GET", "/api/vms", nil)
	if v := vmByID(r, vm2); r.status != 200 || v["state"] != "unavailable" {
		t.Fatalf("Vexa down: %d %v", r.status, v)
	}
	for _, body := range h.bodies {
		if strings.Contains(body, "secret internal detail") {
			t.Fatalf("Vexa internals leaked to the browser: %s", body)
		}
	}
	h.assertNoSecretsLeaked()
}

func TestPowerRateLimit(t *testing.T) {
	h := newHarness(t, singleConfig, "owner")
	b := h.browser()
	b.login("owner")
	h.mock.SetState(vm1, "running") // start attempts are refused (wrong state) but still count
	var last result
	for i := 0; i < 7; i++ {
		last = b.do("POST", "/api/vms/"+vm1+"/start", nil)
	}
	if last.status != http.StatusTooManyRequests || last.header.Get("Retry-After") == "" {
		t.Fatalf("7th request: %d", last.status)
	}
	h.assertPowerCalls()
}

func TestFrontendAndHeaders(t *testing.T) {
	h := newHarness(t, multiConfig, "alice")
	b := h.browser()
	r := b.do("GET", "/", nil)
	if r.status != 200 || !strings.Contains(r.body, `<meta name="app-base" content="/vmctl/">`) {
		t.Fatalf("index: %d", r.status)
	}
	if !strings.Contains(r.body, `src="/vmctl/static/app.js?v=`) {
		t.Fatal("asset URLs must include the base path and version")
	}
	csp := r.header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP: %q", csp)
	}
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
		if r.header.Get(k) != v {
			t.Errorf("%s = %q", k, r.header.Get(k))
		}
	}
	if r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS must not be enabled")
	}

	js := b.do("GET", "/static/app.js", nil)
	if js.status != 200 || !strings.HasPrefix(js.header.Get("Content-Type"), "text/javascript") || js.header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("app.js: %d %v", js.status, js.header)
	}
	for _, p := range []string{"/static/../embed.go", "/static/..%2fembed.go", "/static/%2e%2e/index.html", "/static/index.html"} {
		if r := b.do("GET", p, nil); r.status == 200 || strings.Contains(r.body, "package web") || strings.Contains(r.body, "{{") {
			t.Errorf("GET %s: %d (only embedded assets may be served)", p, r.status)
		} else {
			t.Logf("GET %s -> %d", p, r.status)
		}
	}
	if r := b.do("GET", "/static/missing.js", nil); r.status != 404 {
		t.Errorf("missing asset: %d", r.status)
	}
	api := b.do("GET", "/api/session", nil)
	if api.header.Get("Cache-Control") != "no-store" {
		t.Errorf("API Cache-Control = %q", api.header.Get("Cache-Control"))
	}
	if r := b.do("GET", "/api/unknown", nil); r.status != 404 || r.code() != "not_found" {
		t.Errorf("unknown API route: %d %s", r.status, r.body)
	}
	if r := b.do("GET", "/healthz", nil); r.status != 200 || r.body != "ok\n" {
		t.Errorf("healthz: %d %q", r.status, r.body)
	}

	// Outside the base path.
	h.base = ""
	if r := b.do("GET", "/vmctl", nil); r.status != http.StatusMovedPermanently || r.header.Get("Location") != "/vmctl/" {
		t.Errorf("/vmctl: %d %v", r.status, r.header.Get("Location"))
	}
	if r := b.do("GET", "/api/vms", nil); r.status != 404 {
		t.Errorf("API outside the base path: %d", r.status)
	}
}

func TestClientInfo(t *testing.T) {
	h := newHarness(t, singleConfig)
	tp := h.cfg.TrustedProxies
	mk := func(remote string, hdr ...string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Add(hdr[i], hdr[i+1])
		}
		return r
	}
	cases := []struct {
		r     *http.Request
		ip    string
		https bool
	}{
		{mk("203.0.113.5:1234"), "203.0.113.5", false},
		{mk("203.0.113.5:1234", "X-Forwarded-For", "10.0.0.1", "X-Forwarded-Proto", "https"), "203.0.113.5", false},
		{mk("127.0.0.1:1234", "X-Forwarded-For", "198.51.100.1"), "198.51.100.1", false},
		{mk("127.0.0.1:1234", "X-Forwarded-For", "6.6.6.6, 198.51.100.1", "X-Forwarded-Proto", "https"), "198.51.100.1", true},
		{mk("127.0.0.1:1234", "X-Forwarded-For", "198.51.100.1, 127.0.0.1"), "198.51.100.1", false},
		{mk("127.0.0.1:1234", "X-Forwarded-For", "garbage"), "127.0.0.1", false},
		{mk("[::1]:1234", "X-Forwarded-For", "2001:db8::1"), "2001:db8::1", false},
	}
	for i, c := range cases {
		got := clientInfoFor(c.r, tp)
		if got.IP.String() != c.ip || got.HTTPS != c.https {
			t.Errorf("case %d: %v https=%v, want %s https=%v", i, got.IP, got.HTTPS, c.ip, c.https)
		}
	}
	if k := limiterKey(mustAddr("2001:db8:1:2:3:4:5:6")); k != "2001:db8:1:2::/64" {
		t.Errorf("IPv6 limiter key = %s", k)
	}
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	vm1 = "aaaaaaaa-1111-4111-8111-11111111111a"
	vm2 = "bbbbbbbb-2222-4222-8222-22222222222b"
	vm3 = "cccccccc-3333-4333-8333-33333333333c"
)

// multiJSON is the scenario from the requirements: Alice can start VM 1,
// Bob can start and stop VM 2, Charlie can start both but stop neither.
const multiJSON = `{
  "mode": "multiuser",
  "vexa": {"base_url": "http://127.0.0.1:8080"},
  "vms": [
    {"id": "` + vm1 + `", "name": "Minecraft VM 1", "allow_stop": false},
    {"id": "` + vm2 + `", "name": "Minecraft VM 2", "allow_stop": true},
    {"id": "` + vm3 + `", "name": "Hidden VM", "allow_stop": true}
  ],
  "users": [
    {"username": "alice", "permissions": {"` + vm1 + `": "start"}},
    {"username": "bob", "permissions": {"` + vm2 + `": "start-stop"}},
    {"username": "charlie", "permissions": {"` + vm1 + `": "start", "` + vm2 + `": "start"}},
    {"username": "dave", "disabled": true, "permissions": {"` + vm2 + `": "start-stop"}},
    {"username": "erin", "permissions": {"` + vm3 + `": "none"}}
  ]
}`

const singleJSON = `{
  "mode": "single-user",
  "vexa": {"base_url": "http://127.0.0.1:8080"},
  "vms": [
    {"id": "` + vm1 + `", "name": "Minecraft Game Servers", "allow_stop": false},
    {"id": "` + vm2 + `", "name": "Development VM", "allow_stop": true},
    {"id": "` + vm3 + `", "name": "Testing VM"}
  ],
  "single_user": {"username": "owner"}
}`

func mustParse(t *testing.T, js string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(js), "/etc/vm-controller")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func TestMultiuserPermissions(t *testing.T) {
	cfg := mustParse(t, multiJSON)
	cases := []struct {
		user, vm string
		want     Permission
	}{
		{"alice", vm1, PermStart},
		{"alice", vm2, PermNone},
		{"alice", vm3, PermNone},
		{"bob", vm1, PermNone},
		{"bob", vm2, PermStartStop},
		{"charlie", vm1, PermStart},
		{"charlie", vm2, PermStart},
		{"charlie", vm3, PermNone},
		{"dave", vm2, PermNone}, // disabled
		{"erin", vm3, PermNone}, // explicit none
		{"mallory", vm1, PermNone},
		{"alice", "not-a-vm", PermNone},
		{"alice", strings.ToUpper(vm1), PermNone}, // IDs are matched exactly
	}
	for _, c := range cases {
		if got := cfg.Permission(c.user, c.vm); got != c.want {
			t.Errorf("Permission(%s, %s) = %v, want %v", c.user, c.vm, got, c.want)
		}
	}
	if !cfg.UserActive("alice") || cfg.UserActive("dave") || cfg.UserActive("mallory") {
		t.Error("UserActive is wrong")
	}
	if got := cfg.VisibleVMs("charlie"); len(got) != 2 || got[0].VM.ID != vm1 || got[1].VM.ID != vm2 {
		t.Errorf("VisibleVMs(charlie) = %+v", got)
	}
	if got := cfg.VisibleVMs("erin"); len(got) != 0 {
		t.Errorf("VisibleVMs(erin) = %+v, want none", got)
	}
	if got := cfg.VisibleVMs("dave"); len(got) != 0 {
		t.Errorf("disabled user sees %+v", got)
	}
}

func TestSingleUserPermissions(t *testing.T) {
	cfg := mustParse(t, singleJSON)
	if cfg.Permission("owner", vm1) != PermStart {
		t.Error("owner should only start vm1")
	}
	if cfg.Permission("owner", vm2) != PermStartStop {
		t.Error("owner should start and stop vm2")
	}
	if cfg.Permission("owner", vm3) != PermStart {
		t.Error("allow_stop defaults to false")
	}
	if len(cfg.VisibleVMs("owner")) != 3 {
		t.Error("owner should see all VMs")
	}
	if cfg.Permission("alice", vm1) != PermNone {
		t.Error("only the single user exists")
	}
	if got := cfg.Usernames(); len(got) != 1 || got[0] != "owner" {
		t.Errorf("Usernames = %v", got)
	}
}

func TestPermissionAllows(t *testing.T) {
	if PermNone.Allows(ActionStart) || PermNone.Allows(ActionStop) {
		t.Error("none allows nothing")
	}
	if !PermStart.Allows(ActionStart) || PermStart.Allows(ActionStop) {
		t.Error("start allows only start")
	}
	if !PermStartStop.Allows(ActionStart) || !PermStartStop.Allows(ActionStop) {
		t.Error("start-stop allows both")
	}
	if PermStartStop.Allows(Action("force-off")) {
		t.Error("unknown actions must never be allowed")
	}
}

func TestDefaults(t *testing.T) {
	cfg := mustParse(t, singleJSON)
	if cfg.Listen != "127.0.0.1:8090" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.BasePath != "" || cfg.CookieSecure != CookieSecureAuto {
		t.Errorf("BasePath=%q CookieSecure=%q", cfg.BasePath, cfg.CookieSecure)
	}
	if cfg.Vexa.APIKeyEnv != "VEXA_API_KEY" || cfg.Vexa.Timeout != 10*time.Second {
		t.Errorf("vexa defaults: %+v", cfg.Vexa)
	}
	if cfg.Vexa.StopTimeout != 420*time.Second || cfg.Vexa.StartTimeout != 180*time.Second {
		t.Errorf("operation timeouts: %+v", cfg.Vexa)
	}
	if cfg.SessionLifetime != 12*time.Hour || cfg.IdleTimeout != 2*time.Hour {
		t.Errorf("session: %v %v", cfg.SessionLifetime, cfg.IdleTimeout)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Errorf("default trusted proxies: %v", cfg.TrustedProxies)
	}
}

func TestPathsAndBasePath(t *testing.T) {
	js := strings.Replace(singleJSON, `"mode"`, `"base_path": "/vmctl/", "database": "data/vm.db", "mode"`, 1)
	cfg := mustParse(t, js)
	if cfg.BasePath != "/vmctl" {
		t.Errorf("BasePath = %q", cfg.BasePath)
	}
	if cfg.Database != "/etc/vm-controller/data/vm.db" {
		t.Errorf("relative database path not resolved against config dir: %q", cfg.Database)
	}
	explicitNone := strings.Replace(singleJSON, `"mode"`, `"trusted_proxies": [], "mode"`, 1)
	if cfg := mustParse(t, explicitNone); len(cfg.TrustedProxies) != 0 {
		t.Errorf("explicit empty trusted_proxies must trust nobody: %v", cfg.TrustedProxies)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name, json, want string
	}{
		{"unknown permission",
			strings.Replace(multiJSON, `"`+vm1+`": "start"}},
    {"username": "bob"`, `"`+vm1+`": "admin"}},
    {"username": "bob"`, 1),
			`unknown permission "admin"`},
		{"duplicate vm id",
			strings.Replace(multiJSON, `"id": "`+vm3+`"`, `"id": "`+vm1+`"`, 1),
			"duplicate VM id"},
		{"duplicate vm id differing in case",
			strings.Replace(multiJSON, `"id": "`+vm3+`"`, `"id": "`+strings.ToUpper(vm1)+`"`, 1),
			"duplicate VM id"},
		{"undefined vm",
			strings.Replace(multiJSON, `{"`+vm1+`": "start"}},
    {"username": "bob"`, `{"no-such-vm": "start"}},
    {"username": "bob"`, 1),
			`VM "no-such-vm" is not defined`},
		{"stop on vm with stopping disabled",
			strings.Replace(multiJSON, `{"`+vm1+`": "start"}},
    {"username": "bob"`, `{"`+vm1+`": "start-stop"}},
    {"username": "bob"`, 1),
			`contradicts allow_stop=false`},
		{"duplicate username",
			strings.Replace(multiJSON, `"username": "bob"`, `"username": "alice"`, 1),
			`duplicate user "alice"`},
		{"uppercase username",
			strings.Replace(multiJSON, `"username": "bob"`, `"username": "Bob"`, 1),
			`users[1].username`},
		{"empty username",
			strings.Replace(singleJSON, `"username": "owner"`, `"username": ""`, 1),
			`single_user.username`},
		{"unknown field",
			strings.Replace(singleJSON, `"allow_stop": true`, `"allow_stopp": true`, 1),
			`unknown field "allow_stopp"`},
		{"duplicate key",
			strings.Replace(multiJSON, `{"`+vm1+`": "start"}},
    {"username": "bob"`, `{"`+vm1+`": "start", "`+vm1+`": "start-stop"}},
    {"username": "bob"`, 1),
			`duplicate key "` + vm1 + `" in $.users[0].permissions`},
		{"missing mode", strings.Replace(singleJSON, `"mode": "single-user",`, ``, 1), "mode: required"},
		{"unknown mode", strings.Replace(singleJSON, `"single-user"`, `"single"`, 1), `unknown mode "single"`},
		{"single user missing", strings.Replace(singleJSON, `"single_user": {"username": "owner"}`, `"users": []`, 1), "single_user: required"},
		{"multiuser without users", strings.Replace(singleJSON, `"single-user"`, `"multiuser"`, 1), "at least one user"},
		{"no vms", `{"mode":"single-user","vexa":{"base_url":"http://127.0.0.1:8080"},"vms":[],"single_user":{"username":"owner"}}`, "at least one VM"},
		{"bad vm id", strings.Replace(singleJSON, vm3, "../../admin", 1), `vms[2].id`},
		{"missing vm name", strings.Replace(singleJSON, `"name": "Testing VM"`, `"name": " "`, 1), `vms[2].name: required`},
		{"javascript url", strings.Replace(singleJSON, `"name": "Testing VM"`, `"name": "Testing VM", "url": "javascript:alert(1)"`, 1), `vms[2].url`},
		{"protocol-relative url", strings.Replace(singleJSON, `"name": "Testing VM"`, `"name": "Testing VM", "url": "//evil.example"`, 1), `vms[2].url`},
		{"base url with credentials", strings.Replace(singleJSON, `http://127.0.0.1:8080`, `http://user:pw@127.0.0.1:8080`, 1), "must not contain credentials"},
		{"base url with query", strings.Replace(singleJSON, `http://127.0.0.1:8080`, `http://127.0.0.1:8080/?x=1`, 1), "query or fragment"},
		{"base url scheme", strings.Replace(singleJSON, `http://127.0.0.1:8080`, `file:///etc/passwd`, 1), "scheme must be http or https"},
		{"plain http to remote host", strings.Replace(singleJSON, `http://127.0.0.1:8080`, `http://vexa.example.com`, 1), "allow_insecure_http"},
		{"both key sources", strings.Replace(singleJSON, `"base_url": "http://127.0.0.1:8080"`, `"base_url": "http://127.0.0.1:8080", "api_key_env": "K", "api_key_file": "/k"`, 1), "only one of api_key_env"},
		{"bad env name", strings.Replace(singleJSON, `"base_url": "http://127.0.0.1:8080"`, `"base_url": "http://127.0.0.1:8080", "api_key_env": "A-B"`, 1), "api_key_env"},
		{"bad base path", strings.Replace(singleJSON, `"mode"`, `"base_path": "/a/../b", "mode"`, 1), "base_path"},
		{"relative base path", strings.Replace(singleJSON, `"mode"`, `"base_path": "vmctl", "mode"`, 1), "base_path"},
		{"trust everyone", strings.Replace(singleJSON, `"mode"`, `"trusted_proxies": ["0.0.0.0/0"], "mode"`, 1), "would trust every client"},
		{"bad proxy", strings.Replace(singleJSON, `"mode"`, `"trusted_proxies": ["caddy"], "mode"`, 1), "not an IP address"},
		{"bad listen", strings.Replace(singleJSON, `"mode"`, `"listen": "8090", "mode"`, 1), "listen"},
		{"listen hostname", strings.Replace(singleJSON, `"mode"`, `"listen": "example.com:8090", "mode"`, 1), "must be an IP address"},
		{"bad cookie mode", strings.Replace(singleJSON, `"mode"`, `"cookie_secure": "yes", "mode"`, 1), "cookie_secure"},
		{"timeout range", strings.Replace(singleJSON, `"base_url": "http://127.0.0.1:8080"`, `"base_url": "http://127.0.0.1:8080", "timeout_seconds": 0`, 1), "vexa.timeout_seconds"},
		{"session range", strings.Replace(singleJSON, `"mode"`, `"session": {"lifetime_hours": 0}, "mode"`, 1), "session.lifetime_hours"},
		{"wrong type", strings.Replace(singleJSON, `"allow_stop": true`, `"allow_stop": "yes"`, 1), "must be bool"},
		{"syntax error", singleJSON[:len(singleJSON)-2], "invalid JSON"},
		{"trailing data", singleJSON + ` {}`, "unexpected data"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.json), "/etc")
			if err == nil {
				t.Fatalf("expected an error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

func TestMultipleErrorsReportedTogether(t *testing.T) {
	js := strings.Replace(strings.Replace(multiJSON, `"start-stop"`, `"owner"`, 1), `"username": "charlie"`, `"username": "Charlie"`, 1)
	_, err := Parse([]byte(js), "")
	if err == nil || !strings.Contains(err.Error(), "2 problems") {
		t.Fatalf("want both problems reported, got %v", err)
	}
}

func TestWarnings(t *testing.T) {
	js := strings.Replace(singleJSON, `"single_user"`, `"users": [{"username":"x","permissions":{}}], "single_user"`, 1)
	cfg := mustParse(t, js)
	if !containsWarning(cfg.Warnings, `"users" section is ignored`) {
		t.Errorf("warnings: %v", cfg.Warnings)
	}
	cfg = mustParse(t, multiJSON)
	if !containsWarning(cfg.Warnings, `"Hidden VM"`) {
		t.Errorf("expected unassigned VM warning: %v", cfg.Warnings)
	}
	byName := strings.Replace(singleJSON, `"id": "`+vm3+`"`, `"id": "testing-vm"`, 1)
	if cfg := mustParse(t, byName); !containsWarning(cfg.Warnings, "not a UUID") {
		t.Errorf("expected non-UUID warning: %v", cfg.Warnings)
	}
	remote := strings.Replace(singleJSON, `"base_url": "http://127.0.0.1:8080"`, `"base_url": "http://10.0.0.2:8080", "allow_insecure_http": true`, 1)
	if cfg := mustParse(t, remote); !containsWarning(cfg.Warnings, "unencrypted") {
		t.Errorf("expected plain-http warning: %v", cfg.Warnings)
	}
}

func containsWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestLoadAPIKey(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")

	cfg := mustParse(t, singleJSON)
	cfg.Vexa.APIKeyEnv = "VMCTL_TEST_KEY"
	t.Setenv("VMCTL_TEST_KEY", "  vxa_secret\n")
	if key, err := cfg.LoadAPIKey(); err != nil || key != "vxa_secret" {
		t.Fatalf("env key = %q, %v", key, err)
	}
	t.Setenv("VMCTL_TEST_KEY", "")
	if _, err := cfg.LoadAPIKey(); err == nil {
		t.Fatal("empty key must be rejected")
	}
	t.Setenv("VMCTL_TEST_KEY", "vxa bad")
	if _, err := cfg.LoadAPIKey(); err == nil {
		t.Fatal("key with whitespace must be rejected")
	}

	cfg.Vexa.APIKeyEnv = ""
	cfg.Vexa.APIKeyFile = keyFile
	if err := os.WriteFile(keyFile, []byte("vxa_fromfile\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if key, err := cfg.LoadAPIKey(); err != nil || key != "vxa_fromfile" {
		t.Fatalf("file key = %q, %v", key, err)
	}
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.LoadAPIKey(); err == nil || !strings.Contains(err.Error(), "accessible by other users") {
		t.Fatalf("world-readable key file must be rejected, got %v", err)
	}
	cfg.Vexa.APIKeyFile = filepath.Join(dir, "missing")
	if _, err := cfg.LoadAPIKey(); err == nil {
		t.Fatal("missing key file must be rejected")
	}
}

func TestExamplesAreValid(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.json")
	if err != nil || len(files) < 2 {
		t.Fatalf("example configs not found: %v %v", files, err)
	}
	for _, f := range files {
		cfg, err := Load(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		t.Logf("%s: mode=%s vms=%d users=%v warnings=%v", filepath.Base(f), cfg.Mode, len(cfg.VMs), cfg.Usernames(), cfg.Warnings)
	}
}

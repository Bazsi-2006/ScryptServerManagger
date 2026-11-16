package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
)

// withStdin feeds text to commands that read the password from stdin.
func withStdin(t *testing.T, text string, fn func() error) error {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(text)
	w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old; r.Close() }()
	return fn()
}

func TestFirstRunBootstrap(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "etc", "config.json")
	data := filepath.Join(dir, "data")
	if err := cmdInit([]string{"--config", cfgPath, "--data-dir", data}); err != nil {
		t.Fatal(err)
	}
	h, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	c := h.Get()
	if c.Mode != config.ModeProduction || c.Listen != "127.0.0.1:8080" {
		t.Fatalf("defaults should be production on loopback: %s %s", c.Mode, c.Listen)
	}
	for _, f := range []string{cfgPath, c.Paths.Database, c.Auth.JWTSecretFile} {
		st, err := os.Stat(f)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v mode %v, want 0600", f, err, st.Mode().Perm())
		}
	}
	raw, _ := os.ReadFile(cfgPath)
	if strings.Contains(strings.ToLower(string(raw)), "secret\":") || strings.Contains(string(raw), "password\":") {
		t.Error("the configuration file must not hold secrets")
	}
	// Running init again is harmless.
	if err := cmdInit([]string{"--config", cfgPath}); err != nil {
		t.Fatal(err)
	}

	// No accounts exist by default: there are no built-in credentials.
	store, _ := db.Open(c.Paths.Database)
	if n, _ := store.CountUsers(context.Background()); n != 0 {
		t.Fatalf("fresh install has %d users", n)
	}
	store.Close()

	short := func() error { return cmdAdmin([]string{"create", "--config", cfgPath, "--username", "root", "--password-stdin"}) }
	if err := withStdin(t, "short\n", short); err == nil {
		t.Fatal("a too-short password must be rejected")
	}
	if err := withStdin(t, "a-strong-password\n", func() error {
		return cmdAdmin([]string{"create", "--config", cfgPath, "--username", "bad name", "--password-stdin"})
	}); err == nil {
		t.Fatal("an invalid username must be rejected")
	}
	if err := withStdin(t, "a-strong-password\n", short); err != nil {
		t.Fatal(err)
	}
	if err := withStdin(t, "a-strong-password\n", short); err == nil {
		t.Fatal("creating the same account twice must fail")
	}

	store, _ = db.Open(c.Paths.Database)
	defer store.Close()
	u, err := store.UserByUsername(context.Background(), "root")
	if err != nil || !u.IsAdmin {
		t.Fatalf("admin not created: %v", err)
	}
	if !strings.HasPrefix(u.PasswordHash, "$argon2id$") || strings.Contains(u.PasswordHash, "a-strong-password") {
		t.Fatal("the password must be stored as an Argon2id hash")
	}
	if ok, _ := auth.VerifyPassword("a-strong-password", u.PasswordHash, params(c)); !ok {
		t.Fatal("stored hash does not verify")
	}

	// Password reset revokes sessions.
	if err := withStdin(t, "another-password-1\n", func() error {
		return cmdUser([]string{"set-password", "--config", cfgPath, "--username", "root", "--password-stdin"})
	}); err != nil {
		t.Fatal(err)
	}
	u, _ = store.UserByUsername(context.Background(), "root")
	if ok, _ := auth.VerifyPassword("another-password-1", u.PasswordHash, params(c)); !ok {
		t.Fatal("password was not updated")
	}
	if err := cmdUser([]string{"list", "--config", cfgPath}); err != nil {
		t.Fatal(err)
	}
	if err := cmdMigrate([]string{"--config", cfgPath}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := cmdInit([]string{"--config", cfgPath, "--data-dir", filepath.Join(dir, "data")}); err != nil {
		t.Fatal(err)
	}
	if err := cmdCheck([]string{"--config", cfgPath}); err != nil {
		t.Fatalf("a fresh configuration should pass: %v", err)
	}
	servers := filepath.Join(dir, "servers")
	os.WriteFile(filepath.Join(servers, "ok.json"), []byte(`{"id":"ok","name":"OK","backend":"custom","custom":{"start":{"command":"/bin/true"}}}`), 0o600)
	if err := cmdCheck([]string{"--config", cfgPath}); err != nil {
		t.Fatalf("valid definition: %v", err)
	}
	os.WriteFile(filepath.Join(servers, "broken.json"), []byte(`{"id":"broken","name":"B","backend":"warp-drive"}`), 0o600)
	if err := cmdCheck([]string{"--config", cfgPath}); err == nil {
		t.Fatal("an invalid server definition must fail the check")
	}
	os.Remove(filepath.Join(servers, "broken.json"))
	os.WriteFile(cfgPath, []byte(`{"listen":"not-an-address","mode":"production"}`), 0o600)
	if err := cmdCheck([]string{"--config", cfgPath}); err == nil {
		t.Fatal("an invalid configuration must fail the check")
	}
	if err := cmdCheck([]string{"--config", filepath.Join(dir, "missing.json")}); err == nil {
		t.Fatal("a missing configuration file must fail the check")
	}
}

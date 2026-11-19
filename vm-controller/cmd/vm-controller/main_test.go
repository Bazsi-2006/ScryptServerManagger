package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := `{
  "mode": "multiuser",
  "database": "data/vm.db",
  "vexa": {"base_url": "http://127.0.0.1:1", "api_key_env": "VMCTL_CLI_TEST_KEY"},
  "vms": [{"id": "vm-1", "name": "Minecraft", "allow_stop": true}],
  "users": [
    {"username": "alice", "permissions": {"vm-1": "start-stop"}},
    {"username": "bob", "disabled": true, "permissions": {"vm-1": "start"}}
  ]
}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCLI(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := run(args, strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), err
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"bogus"}, {"user"}, {"user", "bogus"}, {"user", "set-password"}, {"user", "set-password", "a", "b"}, {"serve", "extra"}} {
		if _, _, err := runCLI(t, "", args...); !errors.Is(err, errUsage) {
			t.Errorf("%v: err = %v, want usage error", args, err)
		}
	}
	if out, _, err := runCLI(t, "", "version"); err != nil || !strings.HasPrefix(out, "vm-controller ") {
		t.Errorf("version: %q %v", out, err)
	}
}

func TestPasswordAndUserCommands(t *testing.T) {
	cfg := writeConfig(t)

	if _, _, err := runCLI(t, "correct horse battery\n", "user", "set-password", "--config", cfg, "--stdin", "mallory"); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("unknown user: %v", err)
	}
	if _, _, err := runCLI(t, "short\n", "user", "set-password", "--config", cfg, "--stdin", "alice"); err == nil || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("weak password: %v", err)
	}
	if _, _, err := runCLI(t, "", "user", "set-password", "--config", cfg, "alice"); err == nil || !strings.Contains(err.Error(), "not a terminal") {
		t.Fatalf("non-interactive without --stdin: %v", err)
	}
	// Flags may also follow the username.
	out, _, err := runCLI(t, "correct horse battery\n", "user", "set-password", "Alice", "--config", cfg, "--stdin")
	if err != nil || !strings.Contains(out, "Password for alice updated") {
		t.Fatalf("set-password: %q %v", out, err)
	}
	if st, err := os.Stat(filepath.Join(filepath.Dir(cfg), "data", "vm.db")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("database not created privately next to the config: %v", err)
	}
	out, _, err = runCLI(t, "correct horse battery\n", "user", "set-password", "--config", cfg, "--stdin", "bob")
	if err != nil || !strings.Contains(out, "disabled") {
		t.Fatalf("disabled user note: %q %v", out, err)
	}

	out, _, err = runCLI(t, "", "user", "list", "--config", cfg)
	if err != nil || !strings.Contains(out, "alice") || !strings.Contains(out, "active") || !strings.Contains(out, "disabled") {
		t.Fatalf("user list: %q %v", out, err)
	}
	if strings.Contains(out, "argon2") {
		t.Fatal("user list must not print hashes")
	}

	out, _, err = runCLI(t, "", "user", "revoke-sessions", "--config", cfg, "alice")
	if err != nil || !strings.Contains(out, "Signed out 0 session(s)") {
		t.Fatalf("revoke-sessions: %q %v", out, err)
	}
	out, _, err = runCLI(t, "", "user", "remove", "--config", cfg, "alice")
	if err != nil || !strings.Contains(out, "Removed the stored password") {
		t.Fatalf("remove: %q %v", out, err)
	}
	out, _, _ = runCLI(t, "", "user", "list", "--config", cfg)
	if !strings.Contains(out, "not set") {
		t.Fatalf("after remove: %q", out)
	}
}

func TestCheckConfig(t *testing.T) {
	cfg := writeConfig(t)
	out, _, err := runCLI(t, "", "check-config", "--config", cfg)
	if err != nil || !strings.Contains(out, "is valid") || !strings.Contains(out, "Minecraft=start-stop") {
		t.Fatalf("check-config: %q %v", out, err)
	}
	t.Setenv("VMCTL_CLI_TEST_KEY", "vxa_test")
	out, _, err = runCLI(t, "", "check-config", "--config", cfg, "--online")
	if err == nil || !strings.Contains(out, "UNREACHABLE") {
		t.Fatalf("check-config --online against nothing: %q %v", out, err)
	}
	if strings.Contains(out, "vxa_test") {
		t.Fatal("API key printed")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"mode":"multiuser"}`), 0o600)
	if _, _, err := runCLI(t, "", "check-config", "--config", bad); err == nil {
		t.Fatal("invalid config accepted")
	}
	for _, ex := range []string{"../../examples/config.multiuser.json", "../../examples/config.single-user.json"} {
		if _, _, err := runCLI(t, "", "check-config", "--config", ex); err != nil {
			t.Errorf("%s: %v", ex, err)
		}
	}
}

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAreValid(t *testing.T) {
	c := Default()
	c.applyPathDefaults("/etc/server-manager")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8080" {
		t.Errorf("default listen should be loopback, got %s", c.Listen)
	}
	if c.Paths.ServersDir != "/etc/server-manager/servers" || c.Paths.Database != "/var/lib/server-manager/server-manager.db" {
		t.Errorf("unexpected default paths: %+v", c.Paths)
	}
}

func TestParseRejectsUnknownFieldsAndBadValues(t *testing.T) {
	if _, err := Parse([]byte(`{"listne":"127.0.0.1:1"}`), "/x"); err == nil {
		t.Error("unknown field (typo) must be rejected")
	}
	bad := map[string]string{
		"mode":        `{"mode":"prod"}`,
		"listen":      `{"listen":"nonsense"}`,
		"public_url":  `{"public_url":"ftp://x"}`,
		"proxies":     `{"trusted_proxies":["not-an-ip"]}`,
		"cors":        `{"cors":{"allowed_origins":["https://a.example/path"]}}`,
		"token ttl":   `{"auth":{"access_token_ttl":"1s"}}`,
		"pw length":   `{"auth":{"min_password_length":3}}`,
		"argon":       `{"auth":{"argon2":{"memory_kib":16,"iterations":1,"parallelism":1}}}`,
		"smtp tls":    `{"smtp":{"tls":"ssl3"}}`,
		"smtp from":   `{"smtp":{"host":"mail","from":"not an address"}}`,
		"rate":        `{"rate_limits":{"login":{"requests":0,"window":"1m"}}}`,
		"metrics":     `{"monitoring":{"host_metrics_interval":"10ms"}}`,
		"duration":    `{"auth":{"access_token_ttl":"soon"}}`,
		"docker sock": `{"tools":{"docker_socket":"relative.sock"}}`,
		"log level":   `{"logging":{"level":"chatty"}}`,
	}
	for name, doc := range bad {
		if _, err := Parse([]byte(doc), "/x"); err == nil {
			t.Errorf("%s: expected a validation error for %s", name, doc)
		}
	}
}

func TestValidationReportsAllProblems(t *testing.T) {
	_, err := Parse([]byte(`{"mode":"x","listen":"y","logging":{"level":"z"}}`), "/x")
	ve, ok := err.(*ValidationError)
	if !ok || len(ve.Problems) != 3 {
		t.Fatalf("want 3 problems, got %v", err)
	}
}

func TestDurationJSON(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"90s"`), &d); err != nil || d.D() != 90*time.Second {
		t.Fatalf("string form: %v %v", d, err)
	}
	if err := json.Unmarshal([]byte(`5`), &d); err != nil || d.D() != 5*time.Second {
		t.Fatalf("numeric seconds: %v %v", d, err)
	}
	b, _ := json.Marshal(Duration(2 * time.Minute))
	if string(b) != `"2m0s"` {
		t.Fatalf("marshal: %s", b)
	}
}

func TestRelativePathsResolveAgainstConfigDir(t *testing.T) {
	c, err := Parse([]byte(`{"paths":{"data_dir":"data","servers_dir":"defs"}}`), "/srv/sm")
	if err != nil {
		t.Fatal(err)
	}
	if c.Paths.DataDir != "/srv/sm/data" || c.Paths.ServersDir != "/srv/sm/defs" || c.Paths.RuntimeDir != "/srv/sm/data/runtime" {
		t.Fatalf("%+v", c.Paths)
	}
}

func TestHolderUpdatePersistsAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"paths":{"data_dir":"data"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	notified := 0
	h.OnChange(func(*Config) { notified++ })
	if err := h.Update(func(c *Config) error { c.PublicURL = "https://sm.example.com"; return nil }); err != nil {
		t.Fatal(err)
	}
	if h.Get().PublicURL != "https://sm.example.com" || notified != 1 {
		t.Fatal("update not applied")
	}
	again, err := Load(path)
	if err != nil || again.Get().PublicURL != "https://sm.example.com" {
		t.Fatalf("not persisted: %v", err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("config file mode %v, want 0600", st.Mode().Perm())
	}
	// An invalid update changes neither memory nor disk.
	before, _ := os.ReadFile(path)
	if err := h.Update(func(c *Config) error { c.Listen = "bogus"; return nil }); err == nil {
		t.Fatal("invalid update accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || h.Get().Listen == "bogus" {
		t.Fatal("invalid update leaked")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestJWTSecretGeneratedOnceWithTightPermissions(t *testing.T) {
	t.Setenv("SM_JWT_SECRET", "")
	c := Default()
	c.Auth.JWTSecretFile = filepath.Join(t.TempDir(), "secrets", "jwt.key")
	a, err := c.JWTSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.JWTSecret()
	if err != nil || string(a) != string(b) || len(a) < 32 {
		t.Fatal("secret must be stable and long")
	}
	st, _ := os.Stat(c.Auth.JWTSecretFile)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode %v", st.Mode().Perm())
	}
	if err := os.Chmod(c.Auth.JWTSecretFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.JWTSecret(); err == nil {
		t.Fatal("a world-readable secret file must be refused")
	}
	t.Setenv("SM_JWT_SECRET", "short")
	if _, err := c.JWTSecret(); err == nil {
		t.Fatal("a short SM_JWT_SECRET must be refused")
	}
	t.Setenv("SM_JWT_SECRET", strings.Repeat("k", 40))
	if k, err := c.JWTSecret(); err != nil || string(k) != strings.Repeat("k", 40) {
		t.Fatal("environment secret should take precedence")
	}
}

func TestSMTPPasswordSources(t *testing.T) {
	c := Default()
	t.Setenv("SM_SMTP_PASSWORD", "")
	if pw, err := c.SMTPPassword(); err != nil || pw != "" {
		t.Fatal("no password configured should be empty")
	}
	f := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(f, []byte("from-file\n"), 0o600)
	c.SMTP.PasswordFile = f
	if pw, _ := c.SMTPPassword(); pw != "from-file" {
		t.Fatalf("file password: %q", pw)
	}
	t.Setenv("SM_SMTP_PASSWORD", "from-env")
	if pw, _ := c.SMTPPassword(); pw != "from-env" {
		t.Fatalf("env password: %q", pw)
	}
	// The password must never be part of the serialised configuration.
	data, _ := json.Marshal(c)
	if strings.Contains(string(data), "from-env") || strings.Contains(string(data), "from-file\"") {
		t.Fatal("secret leaked into JSON")
	}
}

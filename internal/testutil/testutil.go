// Package testutil holds helpers shared by the test suites.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
)

// Config returns an in-memory development configuration rooted in a
// temporary directory, with cheap password hashing and fast polling.
func Config(t testing.TB) *config.Holder {
	t.Helper()
	dir := t.TempDir()
	c := config.Default()
	c.Mode = config.ModeDevelopment
	c.Listen = "127.0.0.1:0"
	c.Paths = config.Paths{
		DataDir: filepath.Join(dir, "data"), Database: filepath.Join(dir, "data", "test.db"),
		ServersDir: filepath.Join(dir, "servers"), LogDir: filepath.Join(dir, "log"),
		SecretsDir: filepath.Join(dir, "data", "secrets"), RuntimeDir: filepath.Join(dir, "data", "runtime"),
	}
	c.Auth.JWTSecretFile = filepath.Join(c.Paths.SecretsDir, "jwt.key")
	c.Auth.Argon2 = config.Argon2{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1}
	c.Monitoring.StatusPollInterval = config.Duration(time.Second)
	c.RateLimits.ServerCooldown = 0
	// Tests sign in far more often than a person would; individual tests
	// lower these again when they exercise the limits.
	c.RateLimits.Login = config.Rate{Requests: 10000, Window: config.Duration(time.Minute)}
	c.RateLimits.Refresh = config.Rate{Requests: 10000, Window: config.Duration(time.Minute)}
	c.RateLimits.API = config.Rate{Requests: 100000, Window: config.Duration(time.Minute)}
	if err := c.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	if err := c.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return config.NewHolder(c, "")
}

// Script writes an executable shell script and returns its path.
func Script(t testing.TB, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Eventually polls cond until it holds or the timeout expires.
func Eventually(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, what)
}

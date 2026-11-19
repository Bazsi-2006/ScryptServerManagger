package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "vm.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func session(user, hash string, created time.Time) Session {
	return Session{TokenHash: hash, Username: user, CSRFToken: "csrf-" + hash, CreatedAt: created, LastSeenAt: created, ExpiresAt: created.Add(time.Hour)}
}

func TestFilePermissionsAndReopen(t *testing.T) {
	s, path := open(t)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("database mode = %v, want 0600", st.Mode().Perm())
	}
	dst, _ := os.Stat(filepath.Dir(path))
	if dst.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, want 0700", dst.Mode().Perm())
	}
	ctx := context.Background()
	if err := s.SetPassword(ctx, "alice", "hash1", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if h, err := s2.PasswordHash(ctx, "alice"); err != nil || h != "hash1" {
		t.Fatalf("after reopen: %q %v", h, err)
	}
}

func TestRejectsNewerSchemaAndBadPaths(t *testing.T) {
	s, path := open(t)
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("newer schema must be rejected")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "x?mode=ro")); err == nil {
		t.Fatal("path with '?' must be rejected")
	}
}

func TestCredentialsAndSessions(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)

	if _, err := s.PasswordHash(ctx, "alice"); err != ErrNotFound {
		t.Fatalf("missing user: %v", err)
	}
	if err := s.CreateSession(ctx, session("alice", "h0", now), 10); err == nil {
		t.Fatal("a session needs stored credentials (foreign key)")
	}
	if err := s.SetPassword(ctx, "alice", "hash1", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, "bob", "hashb", now); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, session("alice", "h1", now), 10); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, session("bob", "hb", now), 10); err != nil {
		t.Fatal(err)
	}
	got, err := s.Session(ctx, "h1")
	if err != nil || got.Username != "alice" || got.CSRFToken != "csrf-h1" || !got.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("Session = %+v, %v", got, err)
	}

	// Changing the password revokes the user's sessions, not others'.
	if err := s.SetPassword(ctx, "alice", "hash2", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, "h1"); err != ErrNotFound {
		t.Fatalf("session survived password change: %v", err)
	}
	if _, err := s.Session(ctx, "hb"); err != nil {
		t.Fatalf("other user's session was revoked: %v", err)
	}
	if h, _ := s.PasswordHash(ctx, "alice"); h != "hash2" {
		t.Fatalf("hash = %q", h)
	}

	list, err := s.ListCredentials(ctx)
	if err != nil || len(list) != 2 || list[0].Username != "alice" || list[1].Sessions != 1 {
		t.Fatalf("ListCredentials = %+v, %v", list, err)
	}

	// Removing credentials cascades to sessions.
	if ok, err := s.RemoveCredentials(ctx, "bob"); !ok || err != nil {
		t.Fatalf("RemoveCredentials: %v %v", ok, err)
	}
	if _, err := s.Session(ctx, "hb"); err != ErrNotFound {
		t.Fatal("session survived credential removal")
	}
	if ok, _ := s.RemoveCredentials(ctx, "bob"); ok {
		t.Fatal("second removal reported success")
	}
}

func TestSessionLimitsAndCleanup(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	for _, u := range []string{"alice", "bob", "carol"} {
		if err := s.SetPassword(ctx, u, "h", now); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := s.CreateSession(ctx, session("alice", string(rune('a'+i)), now.Add(time.Duration(i)*time.Second)), 3); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{"a", "b"} {
		if _, err := s.Session(ctx, h); err != ErrNotFound {
			t.Errorf("oldest session %s should have been evicted", h)
		}
	}
	for _, h := range []string{"c", "d", "e"} {
		if _, err := s.Session(ctx, h); err != nil {
			t.Errorf("session %s missing: %v", h, err)
		}
	}

	if err := s.CreateSession(ctx, session("bob", "bob1", now), 10); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, session("carol", "carol1", now), 10); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DeleteSessionsExcept(ctx, []string{"alice", "carol"}); err != nil || n != 1 {
		t.Fatalf("DeleteSessionsExcept = %d, %v", n, err)
	}
	if _, err := s.Session(ctx, "bob1"); err != ErrNotFound {
		t.Fatal("bob's session should be gone")
	}

	// Idle expiry: touch "e" so it stays, the others go idle.
	later := now.Add(50 * time.Minute)
	if err := s.TouchSession(ctx, "e", later); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DeleteExpiredSessions(ctx, later.Add(time.Minute), 30*time.Minute); err != nil || n != 3 {
		t.Fatalf("DeleteExpiredSessions = %d, %v", n, err)
	}
	if _, err := s.Session(ctx, "e"); err != nil {
		t.Fatal("recently used session was deleted")
	}
	// Absolute expiry.
	if n, _ := s.DeleteExpiredSessions(ctx, now.Add(2*time.Hour), 24*time.Hour); n != 1 {
		t.Fatalf("absolute expiry removed %d sessions", n)
	}
	if n, err := s.DeleteUserSessions(ctx, "alice"); err != nil || n != 0 {
		t.Fatalf("DeleteUserSessions = %d, %v", n, err)
	}
	if n, err := s.DeleteSessionsExcept(ctx, nil); err != nil || n != 0 {
		t.Fatalf("DeleteSessionsExcept(nil) = %d, %v", n, err)
	}
}

package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var ctx = context.Background()

func mkUser(t *testing.T, s *Store, name string, admin bool) *User {
	t.Helper()
	u := &User{Username: name, DisplayName: name, PasswordHash: "x", IsAdmin: admin}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestOpenSetsWALForeignKeysAndFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mode string
	s.DB.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %s, want wal", mode)
	}
	var fk int
	s.DB.QueryRow("PRAGMA foreign_keys").Scan(&fk)
	if fk != 1 {
		t.Error("foreign keys must be enforced")
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("database mode %v, want 0600", st.Mode().Perm())
	}
}

func TestMigrationsApplyOnceAndUpgradeInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	embedded, _ := loadMigrations(migrationFS, "migrations")
	v, _ := s.SchemaVersion(ctx)
	if v != embedded[len(embedded)-1].version || v < 2 {
		t.Fatalf("schema version %d", v)
	}
	u := mkUser(t, s, "keeper", true)
	// Re-running is a no-op.
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// A newer binary ships one more migration: it applies on top, data stays.
	next := append(append([]migration{}, embedded...), migration{version: v + 1, name: "9999_test.sql", sql: `ALTER TABLE users ADD COLUMN note TEXT NOT NULL DEFAULT ''; CREATE TABLE extra (id INTEGER PRIMARY KEY);`})
	if err := s.applyMigrations(ctx, next); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SchemaVersion(ctx); got != v+1 {
		t.Fatalf("version after upgrade %d", got)
	}
	if _, err := s.UserByID(ctx, u.ID); err != nil {
		t.Fatal("existing data lost by migration")
	}
	// A failing migration rolls back completely.
	broken := append(next, migration{version: v + 2, name: "broken.sql", sql: `CREATE TABLE half (id INTEGER); THIS IS NOT SQL;`})
	if err := s.applyMigrations(ctx, broken); err == nil {
		t.Fatal("broken migration must fail")
	}
	if got, _ := s.SchemaVersion(ctx); got != v+1 {
		t.Fatal("failed migration must not be recorded")
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'half'`).Scan(&n)
	if n != 0 {
		t.Fatal("failed migration left partial schema behind")
	}
	s.Close()
	// An older binary must refuse a database from the future.
	if _, err := Open(path); err == nil {
		t.Fatal("opening a newer schema with an older binary must fail")
	}
}

func TestUsersUniqueAndCaseInsensitive(t *testing.T) {
	s := open(t)
	mkUser(t, s, "Alice", false)
	if err := s.CreateUser(ctx, &User{Username: "alice", PasswordHash: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate username (different case) should conflict, got %v", err)
	}
	if u, err := s.UserByUsername(ctx, "ALICE"); err != nil || u.Username != "Alice" {
		t.Fatal("lookup must be case-insensitive")
	}
	a := &User{Username: "bob", Email: "x@example.com", PasswordHash: "x"}
	if err := s.CreateUser(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, &User{Username: "carol", Email: "X@Example.com", PasswordHash: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate email should conflict")
	}
	// Several users without an email are fine.
	mkUser(t, s, "dave", false)
	mkUser(t, s, "erin", false)
}

func TestLastAdminIsProtected(t *testing.T) {
	s := open(t)
	admin := mkUser(t, s, "root", true)
	no, yes := false, true
	if _, err := s.UpdateUser(ctx, admin.ID, UserUpdate{IsAdmin: &no}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting the last admin: %v", err)
	}
	if _, err := s.UpdateUser(ctx, admin.ID, UserUpdate{Disabled: &yes}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disabling the last admin: %v", err)
	}
	if err := s.DeleteUser(ctx, admin.ID); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("deleting the last admin: %v", err)
	}
	second := mkUser(t, s, "root2", true)
	if _, err := s.UpdateUser(ctx, admin.ID, UserUpdate{IsAdmin: &no}); err != nil {
		t.Fatalf("with another admin present demotion must work: %v", err)
	}
	if err := s.DeleteUser(ctx, second.ID); !errors.Is(err, ErrLastAdmin) {
		t.Fatal("second is now the last admin")
	}
}

func TestSessionRotation(t *testing.T) {
	s := open(t)
	u := mkUser(t, s, "u", false)
	now := time.Now()
	sess := &Session{ID: "s1", UserID: u.ID, CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateSession(ctx, sess, "h0"); err != nil {
		t.Fatal(err)
	}
	grace := 10 * time.Second
	exp := now.Add(2 * time.Hour)

	// Normal rotation.
	if _, err := s.RotateSession(ctx, "h0", "h1", now, grace, exp, "ip"); err != nil {
		t.Fatal(err)
	}
	// The old token is still accepted inside the grace window (two tabs).
	if _, err := s.RotateSession(ctx, "h0", "h2", now.Add(5*time.Second), grace, exp, "ip"); err != nil {
		t.Fatalf("grace-window rotation: %v", err)
	}
	// h1 was superseded by h2 and is neither current nor previous.
	if _, err := s.RotateSession(ctx, "h1", "h3", now.Add(6*time.Second), grace, exp, "ip"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("superseded token: %v", err)
	}
	// Current token keeps working.
	if _, err := s.RotateSession(ctx, "h2", "h4", now.Add(7*time.Second), grace, exp, "ip"); err != nil {
		t.Fatal(err)
	}
	// Replaying an already-rotated token after the grace window is theft:
	// the whole session dies.
	if _, err := s.RotateSession(ctx, "h2", "h5", now.Add(time.Minute), grace, exp, "ip"); !errors.Is(err, ErrTokenReuse) {
		t.Fatalf("reuse detection: %v", err)
	}
	if _, err := s.RotateSession(ctx, "h4", "h6", now.Add(time.Minute), grace, exp, "ip"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("session must be revoked after reuse: %v", err)
	}
	if active, _ := s.SessionActive(ctx, "s1", now.Add(time.Minute)); active {
		t.Fatal("session should be inactive")
	}
	if _, err := s.RotateSession(ctx, "never-issued", "h7", now, grace, exp, "ip"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token: %v", err)
	}
}

func TestSessionExpiryAndRevocation(t *testing.T) {
	s := open(t)
	u := mkUser(t, s, "u", false)
	now := time.Now()
	for _, id := range []string{"a", "b", "c"} {
		s.CreateSession(ctx, &Session{ID: id, UserID: u.ID, CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour)}, "hash-"+id)
	}
	if _, err := s.RotateSession(ctx, "hash-a", "n", now.Add(2*time.Hour), time.Second, now.Add(3*time.Hour), ""); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expired session: %v", err)
	}
	ids, err := s.RevokeUserSessions(ctx, u.ID, "b", now)
	if err != nil || len(ids) != 2 {
		t.Fatalf("revoke others: %v %v", ids, err)
	}
	list, _ := s.ListSessions(ctx, u.ID, now)
	if len(list) != 1 || list[0].ID != "b" {
		t.Fatalf("remaining sessions: %+v", list)
	}
	if err := s.RevokeSession(ctx, "b", now); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, "b", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("double revoke should report not found")
	}
	// Deleting the user cascades.
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 0 {
		t.Fatal("sessions must be deleted with the user")
	}
}

func TestInvitationLifecycle(t *testing.T) {
	s := open(t)
	admin := mkUser(t, s, "admin", true)
	now := time.Now()

	inv, err := s.CreateInvitation(ctx, "tok1", "new@example.com", admin.ID, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if inv.Status(now) != "pending" || inv.CreatedByName != "admin" || inv.CreatedBy == nil {
		t.Fatalf("%+v", inv)
	}
	if inv.Status(now.Add(2*time.Hour)) != "expired" {
		t.Fatal("status should be expired after expiry")
	}

	// Expired invitations cannot be redeemed.
	if err := s.RegisterWithInvitation(ctx, "tok1", &User{Username: "late", PasswordHash: "x"}, now.Add(2*time.Hour)); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("expired: %v", err)
	}
	// Unknown token.
	if err := s.RegisterWithInvitation(ctx, "nope", &User{Username: "ghost", PasswordHash: "x"}, now); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("unknown: %v", err)
	}
	// A username conflict must not burn the invitation.
	if err := s.RegisterWithInvitation(ctx, "tok1", &User{Username: "ADMIN", PasswordHash: "x"}, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	u := &User{Username: "newbie", PasswordHash: "x"}
	if err := s.RegisterWithInvitation(ctx, "tok1", u, now); err != nil {
		t.Fatalf("redeem after failed attempt: %v", err)
	}
	// Single use.
	if err := s.RegisterWithInvitation(ctx, "tok1", &User{Username: "second", PasswordHash: "x"}, now); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("second use: %v", err)
	}
	got, _ := s.InvitationByID(ctx, inv.ID)
	if got.Status(now) != "used" || got.UsedByName != "newbie" {
		t.Fatalf("%+v", got)
	}
	if err := s.RevokeInvitation(ctx, inv.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a used invitation cannot be revoked")
	}

	// Revocation and reissue.
	inv2, _ := s.CreateInvitation(ctx, "tok2", "", admin.ID, now.Add(time.Hour))
	if err := s.ReissueInvitation(ctx, inv2.ID, "tok2b", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterWithInvitation(ctx, "tok2", &User{Username: "old-link", PasswordHash: "x"}, now); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatal("the old token must stop working after a reissue")
	}
	if err := s.RevokeInvitation(ctx, inv2.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterWithInvitation(ctx, "tok2b", &User{Username: "revoked", PasswordHash: "x"}, now); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatal("revoked invitation must not be redeemable")
	}
}

func TestInvitationRedeemedExactlyOnceConcurrently(t *testing.T) {
	s := open(t)
	admin := mkUser(t, s, "admin", true)
	now := time.Now()
	s.CreateInvitation(ctx, "race", "", admin.ID, now.Add(time.Hour))
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u := &User{Username: "racer" + string(rune('a'+i)), PasswordHash: "x"}
			if err := s.RegisterWithInvitation(ctx, "race", u, now); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d registrations succeeded, want exactly 1", wins)
	}
	if n, _ := s.CountUsers(ctx); n != 2 {
		t.Fatalf("user count %d, want 2", n)
	}
}

func TestPermissionsStoredAsBitmask(t *testing.T) {
	s := open(t)
	u := mkUser(t, s, "u", false)
	if err := s.SetPermission(ctx, u.ID, "mc", 5); err != nil {
		t.Fatal(err)
	}
	s.SetPermission(ctx, u.ID, "mc", 9) // update in place
	s.SetPermission(ctx, u.ID, "web", 32)
	var typ string
	var raw sql.NullInt64
	s.DB.QueryRow(`SELECT typeof(mask), mask FROM server_permissions WHERE server_id = 'mc'`).Scan(&typ, &raw)
	if typ != "integer" || raw.Int64 != 9 {
		t.Fatalf("stored %s %d", typ, raw.Int64)
	}
	perms, _ := s.PermissionsForUser(ctx, u.ID)
	if len(perms) != 2 || perms["mc"] != 9 || perms["web"] != 32 {
		t.Fatalf("%v", perms)
	}
	s.SetPermission(ctx, u.ID, "web", 0) // zero removes
	perms, _ = s.PermissionsForUser(ctx, u.ID)
	if _, ok := perms["web"]; ok {
		t.Fatal("zero mask should delete the row")
	}
	s.DeleteServerPermissions(ctx, "mc")
	if perms, _ = s.PermissionsForUser(ctx, u.ID); len(perms) != 0 {
		t.Fatal("server permissions should be gone")
	}
}

func TestEmailChangeFlow(t *testing.T) {
	s := open(t)
	u := mkUser(t, s, "u", false)
	other := &User{Username: "o", Email: "taken@example.com", PasswordHash: "x"}
	s.CreateUser(ctx, other)
	now := time.Now()

	s.CreateEmailChange(ctx, u.ID, "first@example.com", "t1", now.Add(time.Hour))
	s.CreateEmailChange(ctx, u.ID, "second@example.com", "t2", now.Add(time.Hour))
	if _, err := s.ConsumeEmailChange(ctx, "t1", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a newer request must invalidate the older token")
	}
	got, err := s.ConsumeEmailChange(ctx, "t2", now)
	if err != nil || got.Email != "second@example.com" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := s.ConsumeEmailChange(ctx, "t2", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("token must be single-use")
	}
	s.CreateEmailChange(ctx, u.ID, "late@example.com", "t3", now.Add(time.Minute))
	if _, err := s.ConsumeEmailChange(ctx, "t3", now.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired token must be rejected")
	}
	s.CreateEmailChange(ctx, u.ID, "taken@example.com", "t4", now.Add(time.Hour))
	if _, err := s.ConsumeEmailChange(ctx, "t4", now); !errors.Is(err, ErrConflict) {
		t.Fatalf("address in use: %v", err)
	}
	if cur, _ := s.UserByID(ctx, u.ID); cur.Email != "second@example.com" {
		t.Fatal("email must be unchanged after failures")
	}
}

func TestAuditFilters(t *testing.T) {
	s := open(t)
	base := time.Date(2026, 11, 16, 18, 0, 0, 0, time.UTC)
	uid := int64(7)
	add := func(min int, user string, id *int64, server, action, result string) {
		s.InsertAudit(ctx, &AuditEntry{Time: base.Add(time.Duration(min) * time.Minute), UserID: id, Username: user, ServerID: server, Action: action, Result: result, Details: map[string]any{"n": min}})
	}
	add(0, "alice", &uid, "mc", "SERVER_START", "SUCCESS")
	add(1, "alice", &uid, "mc", "SERVER_STOP", "FAILURE")
	add(2, "bob", nil, "web", "SERVER_START", "SUCCESS")
	add(3, "bob", nil, "", "LOGIN", "FAILURE")

	count := func(f AuditFilter) int {
		got, err := s.QueryAudit(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	from, to := base.Add(time.Minute), base.Add(2*time.Minute)
	checks := []struct {
		name string
		f    AuditFilter
		want int
	}{
		{"all", AuditFilter{}, 4},
		{"user id", AuditFilter{UserID: &uid}, 2},
		{"username", AuditFilter{Username: "BOB"}, 2},
		{"server", AuditFilter{ServerID: "mc"}, 2},
		{"action", AuditFilter{Action: "SERVER_START"}, 2},
		{"result", AuditFilter{Result: "FAILURE"}, 2},
		{"range", AuditFilter{From: &from, To: &to}, 2},
		{"combined", AuditFilter{ServerID: "mc", Result: "SUCCESS"}, 1},
		{"limit", AuditFilter{Limit: 3}, 3},
	}
	for _, c := range checks {
		if got := count(c.f); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
	all, _ := s.QueryAudit(ctx, AuditFilter{})
	if all[0].Action != "LOGIN" || all[0].Details["n"].(float64) != 3 {
		t.Fatalf("newest first with details: %+v", all[0])
	}
	page, _ := s.QueryAudit(ctx, AuditFilter{BeforeID: all[1].ID})
	if len(page) != 2 {
		t.Fatalf("pagination returned %d", len(page))
	}
	if n, _ := s.PurgeAudit(ctx, base.Add(90*time.Second)); n != 2 {
		t.Fatalf("purged %d", n)
	}
}

func TestOperationsHistoryAndInterruption(t *testing.T) {
	s := open(t)
	now := time.Now()
	a := &Operation{ServerID: "mc", Type: "start", Username: "u", Status: "running", StartedAt: now}
	b := &Operation{ServerID: "mc", Type: "backup", Username: "u", Status: "running", StartedAt: now}
	s.InsertOperation(ctx, a)
	s.InsertOperation(ctx, b)
	s.FinishOperation(ctx, a.ID, "succeeded", "", now)
	if err := s.InterruptRunningOperations(ctx, now); err != nil {
		t.Fatal(err)
	}
	ops, _ := s.ListOperations(ctx, "mc", 10)
	if len(ops) != 2 || ops[0].Status != "interrupted" || ops[1].Status != "succeeded" || ops[0].FinishedAt == nil {
		t.Fatalf("%+v %+v", ops[0], ops[1])
	}
}

func TestRuntimeBlob(t *testing.T) {
	s := open(t)
	if _, err := s.GetRuntime(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing blob should be ErrNotFound")
	}
	s.SetRuntime(ctx, "x", `{"pid":1}`)
	s.SetRuntime(ctx, "x", `{"pid":2}`)
	if v, _ := s.GetRuntime(ctx, "x"); v != `{"pid":2}` {
		t.Fatalf("got %s", v)
	}
	s.DeleteRuntime(ctx, "x")
	if _, err := s.GetRuntime(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("blob should be deleted")
	}
}

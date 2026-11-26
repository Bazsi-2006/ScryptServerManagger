package db

import (
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// openAtVersion creates a database that only has the migrations up to and
// including version, as an installation of an older release would.
func openAtVersion(t *testing.T, version int) (*Store, []migration) {
	t.Helper()
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_txlock", "immediate")
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "old.db")+"?"+q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{DB: raw}
	t.Cleanup(func() { s.Close() })
	all, err := loadMigrations(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	var old []migration
	for _, m := range all {
		if m.version <= version {
			old = append(old, m)
		}
	}
	if err := s.applyMigrations(ctx, old); err != nil {
		t.Fatal(err)
	}
	return s, all
}

func TestUpgradeFromVersion2KeepsUsersPermissionsAndBackupRights(t *testing.T) {
	s, all := openAtVersion(t, 2)
	now := time.Now().Unix()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Users as the previous release stored them.
	exec(`INSERT INTO users(id, username, display_name, email, password_hash, is_admin, created_at, updated_at) VALUES
		(1, 'admin', 'Admin', 'admin@example.com', 'h', 1, ?, ?),
		(2, 'invited', 'Invited', 'invited@example.com', 'h', 0, ?, ?),
		(3, 'handset', 'Hand Set', 'handset@example.com', 'h', 0, ?, ?),
		(4, 'linkonly', 'Link Only', 'link@example.com', 'h', 0, ?, ?),
		(5, 'noemail', 'No Email', NULL, 'h', 0, ?, ?)`, now, now, now, now, now, now, now, now, now, now)
	// 2 registered from an invitation that was emailed to the same address;
	// 4 from an invitation whose link the administrator passed on by hand.
	exec(`INSERT INTO invitations(token_hash, email, created_by, created_at, expires_at, used_at, used_by, last_sent_at) VALUES
		('a', 'Invited@Example.com', 1, ?, ?, ?, 2, ?),
		('b', 'link@example.com', 1, ?, ?, ?, 4, NULL)`, now, now+3600, now, now, now, now+3600, now)
	exec(`INSERT INTO server_permissions(user_id, server_id, mask, updated_at) VALUES
		(2, 'mc', 16, ?), (2, 'web', 1|2|4|16|32, ?), (3, 'mc', 1|8, ?), (4, 'mc', 32, ?)`, now, now, now, now)
	exec(`INSERT INTO operations(server_id, type, username, status, started_at) VALUES ('mc', 'start', 'invited', 'succeeded', ?)`, now*1000)

	if err := s.applyMigrations(ctx, all); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if v, _ := s.SchemaVersion(ctx); v != all[len(all)-1].version {
		t.Fatalf("schema version %d", v)
	}

	// BACKUPS (16) became BACKUP_CREATE|BACKUP_DOWNLOAD (64|128); every
	// other bit kept its meaning.
	want := map[int64]map[string]int{
		2: {"mc": 64 | 128, "web": 1 | 2 | 4 | 32 | 64 | 128},
		3: {"mc": 1 | 8},
		4: {"mc": 32},
	}
	for uid, servers := range want {
		got, err := s.PermissionsForUser(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		for srv, mask := range servers {
			if got[srv] != mask {
				t.Errorf("user %d server %s: mask %d, want %d", uid, srv, got[srv], mask)
			}
		}
	}
	// Running the migrations again changes nothing.
	if err := s.applyMigrations(ctx, all); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.PermissionsForUser(ctx, 2); got["mc"] != 64|128 {
		t.Errorf("second run changed masks: %v", got)
	}

	// Only an address proven by an emailed invitation counts as verified.
	verified := map[int64]bool{1: false, 2: true, 3: false, 4: false, 5: false}
	for id, v := range verified {
		u, err := s.UserByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if u.EmailVerified != v {
			t.Errorf("user %s verified=%v, want %v", u.Username, u.EmailVerified, v)
		}
	}
	// Existing rows of other tables are untouched.
	ops, _ := s.ListOperations(ctx, "mc", 10)
	if len(ops) != 1 || ops[0].Username != "invited" || ops[0].ActionID != "" {
		t.Fatalf("operation history after upgrade: %+v", ops)
	}
}

func TestEmailVerificationFollowsAddressChanges(t *testing.T) {
	s := open(t)
	verified := time.Now().UTC().Truncate(time.Second)
	u := &User{Username: "vera", DisplayName: "Vera", Email: "vera@example.com", PasswordHash: "x", EmailVerifiedAt: &verified}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserByID(ctx, u.ID); !got.EmailVerified {
		t.Fatal("verified flag not stored")
	}
	// Unrelated changes keep it; a new address (set by an administrator)
	// is unverified.
	name := "V."
	if got, _ := s.UpdateUser(ctx, u.ID, UserUpdate{DisplayName: &name}); !got.EmailVerified {
		t.Fatal("changing the name must not unverify the address")
	}
	same := "VERA@example.com"
	if got, _ := s.UpdateUser(ctx, u.ID, UserUpdate{Email: &same}); !got.EmailVerified {
		t.Fatal("a case-only change is the same address")
	}
	other := "new@example.com"
	got, _ := s.UpdateUser(ctx, u.ID, UserUpdate{Email: &other})
	if got.EmailVerified {
		t.Fatal("a new address must start unverified")
	}
	// The verification link proves it.
	if err := s.CreateEmailChange(ctx, u.ID, other, "tok", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if pending, _, _ := s.PendingEmailChange(ctx, u.ID, time.Now()); pending != other {
		t.Fatalf("pending change: %q", pending)
	}
	got, err := s.ConsumeEmailChange(ctx, "tok", time.Now())
	if err != nil || !got.EmailVerified || got.Email != other {
		t.Fatalf("after verification: %+v %v", got, err)
	}
	if byMail, err := s.UserByEmail(ctx, "NEW@example.com"); err != nil || byMail.ID != u.ID {
		t.Fatal("lookup by email must be case-insensitive")
	}
	empty := ""
	if got, _ := s.UpdateUser(ctx, u.ID, UserUpdate{Email: &empty}); got.EmailVerified || got.EmailVerifiedAt != nil {
		t.Fatal("removing the address clears verification")
	}
}

func verifiedUser(t *testing.T, s *Store, name string, admin bool) *User {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	u := &User{Username: name, DisplayName: name, Email: name + "@example.com", PasswordHash: "old", IsAdmin: admin, EmailVerifiedAt: &now}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPasswordResetIsSingleUseAndRechecksEligibility(t *testing.T) {
	s := open(t)
	mkUser(t, s, "root", true) // keeps an administrator around
	u := verifiedUser(t, s, "rita", false)
	now := time.Now()
	sess := &Session{ID: "s1", UserID: u.ID, CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateSession(ctx, sess, "rt"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePasswordReset(ctx, u.ID, u.Email, "h1", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PasswordResetByHash(ctx, "h1", now); err != nil {
		t.Fatalf("fresh token: %v", err)
	}
	// A newer link replaces the old one.
	if err := s.CreatePasswordReset(ctx, u.ID, u.Email, "h2", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PasswordResetByHash(ctx, "h1", now); !errors.Is(err, ErrResetInvalid) {
		t.Fatal("an older link must stop working once a new one is issued")
	}
	// Redeeming changes the password and revokes every session.
	got, revoked, err := s.ConsumePasswordReset(ctx, "h2", u.ID, "new-hash", now)
	if err != nil || got.PasswordHash != "new-hash" || len(revoked) != 1 {
		t.Fatalf("consume: %+v %v %v", got, revoked, err)
	}
	if active, _ := s.SessionActive(ctx, "s1", now); active {
		t.Fatal("sessions must be revoked after a reset")
	}
	if _, _, err := s.ConsumePasswordReset(ctx, "h2", u.ID, "again", now); !errors.Is(err, ErrResetInvalid) {
		t.Fatal("a used token must not work twice")
	}
	// Expired.
	s.CreatePasswordReset(ctx, u.ID, u.Email, "h3", now, now.Add(time.Minute))
	if _, _, err := s.ConsumePasswordReset(ctx, "h3", u.ID, "x", now.Add(2*time.Minute)); !errors.Is(err, ErrResetInvalid) {
		t.Fatal("an expired token must be rejected")
	}
	// A token for another account is no good.
	s.CreatePasswordReset(ctx, u.ID, u.Email, "h4", now, now.Add(time.Hour))
	if _, _, err := s.ConsumePasswordReset(ctx, "h4", u.ID+100, "x", now); !errors.Is(err, ErrResetInvalid) {
		t.Fatal("a token is bound to its account")
	}
	// Promotion to administrator kills pending links, and even a link
	// that survived would be refused at redemption.
	yes := true
	if _, err := s.UpdateUser(ctx, u.ID, UserUpdate{IsAdmin: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PasswordResetByHash(ctx, "h4", now); !errors.Is(err, ErrResetInvalid) {
		t.Fatal("promotion must invalidate pending reset links")
	}
	if _, err := s.DB.Exec(`INSERT INTO password_resets(user_id, token_hash, email, created_at, expires_at) VALUES (?,?,?,?,?)`,
		u.ID, "h5", u.Email, now.Unix(), now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConsumePasswordReset(ctx, "h5", u.ID, "x", now); !errors.Is(err, ErrResetInvalid) {
		t.Fatal("an administrator must never redeem an email reset")
	}
	// A different (or unverified) address invalidates links sent to the old one.
	v := verifiedUser(t, s, "vic", false)
	s.CreatePasswordReset(ctx, v.ID, v.Email, "h6", now, now.Add(time.Hour))
	other := "other@example.com"
	s.UpdateUser(ctx, v.ID, UserUpdate{Email: &other})
	if _, err := s.PasswordResetByHash(ctx, "h6", now); !errors.Is(err, ErrResetInvalid) {
		t.Fatal("changing the address must invalidate pending links")
	}
}

func TestPasswordResetRedeemedExactlyOnceConcurrently(t *testing.T) {
	s := open(t)
	u := verifiedUser(t, s, "conc", false)
	now := time.Now()
	s.CreatePasswordReset(ctx, u.ID, u.Email, "race", now, now.Add(time.Hour))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.ConsumePasswordReset(ctx, "race", u.ID, "h", now); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("token redeemed %d times", ok)
	}
}

func TestActionPermissionsAndPruning(t *testing.T) {
	s := open(t)
	a, b := mkUser(t, s, "alice", false), mkUser(t, s, "bob", false)
	for _, g := range []struct {
		u        int64
		srv, act string
	}{{a.ID, "mc", "save"}, {a.ID, "mc", "update"}, {b.ID, "mc", "save"}, {a.ID, "web", "save"}} {
		if err := s.SetActionPermission(ctx, g.u, g.srv, g.act, true); err != nil {
			t.Fatal(err)
		}
	}
	// Granting twice is harmless.
	if err := s.SetActionPermission(ctx, a.ID, "mc", "save", true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ActionPermissionsForUser(ctx, a.ID)
	if !got["mc"]["save"] || !got["mc"]["update"] || !got["web"]["save"] || got["mc"]["other"] {
		t.Fatalf("grants: %v", got)
	}
	s.SetActionPermission(ctx, a.ID, "mc", "update", false)
	if got, _ := s.ActionPermissionsForUser(ctx, a.ID); got["mc"]["update"] {
		t.Fatal("revoke failed")
	}
	// Pruning keeps only actions that still exist on that server.
	if n, err := s.PruneActionPermissions(ctx, "mc", []string{"update"}); err != nil || n != 2 {
		t.Fatalf("prune removed %d (%v), want 2", n, err)
	}
	if list, _ := s.ActionPermissionsForServer(ctx, "mc"); len(list) != 0 {
		t.Fatalf("after prune: %v", list)
	}
	if list, _ := s.ActionPermissionsForServer(ctx, "web"); len(list) != 1 {
		t.Fatal("pruning one server must not touch another")
	}
	// Deleting a server's permissions also removes its action grants.
	if err := s.DeleteServerPermissions(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ActionPermissionsForServer(ctx, "web"); len(list) != 0 {
		t.Fatal("server deletion left action grants behind")
	}
	// Deleting a user cascades.
	s.SetActionPermission(ctx, b.ID, "mc", "save", true)
	s.DeleteUser(ctx, b.ID)
	if list, _ := s.ActionPermissionsForServer(ctx, "mc"); len(list) != 0 {
		t.Fatal("user deletion left action grants behind")
	}
}

func TestActionRunsAreRecordedWithOutput(t *testing.T) {
	s := open(t)
	start := time.Now()
	for i, act := range []string{"save", "save", "update"} {
		op := &Operation{ServerID: "mc", Type: "action", ActionID: act, Username: "u", Status: "running", StartedAt: start.Add(time.Duration(i) * time.Second)}
		if err := s.InsertOperation(ctx, op); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishActionOperation(ctx, op.ID, "succeeded", "ok", "output "+act+string(rune('0'+i)), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	last, err := s.LastActionRuns(ctx, "mc")
	if err != nil || len(last) != 2 || last["save"].Output != "output save1" || last["update"].ActionID != "update" {
		t.Fatalf("last runs: %+v %v", last, err)
	}
	// The general history carries the action ID but not the output.
	ops, _ := s.ListOperations(ctx, "mc", 10)
	if len(ops) != 3 || ops[0].ActionID != "update" || ops[0].Output != "" {
		t.Fatalf("history: %+v", ops[0])
	}
}

func TestNotificationPreferencesCandidatesAndDeliveryLog(t *testing.T) {
	s := open(t)
	admin := verifiedUser(t, s, "boss", true)
	fan := verifiedUser(t, s, "fan", false)
	quiet := verifiedUser(t, s, "quiet", false)
	unverified := mkUser(t, s, "unv", false)
	addr := "unv@example.com"
	s.UpdateUser(ctx, unverified.ID, UserUpdate{Email: &addr})
	for _, u := range []*User{admin, fan, unverified} {
		if err := s.SetNotificationPrefs(ctx, u.ID, NotificationPrefs{Enabled: true, OnReady: true, OnCrash: true}); err != nil {
			t.Fatal(err)
		}
	}
	// quiet opted in but switched the master toggle off.
	s.SetNotificationPrefs(ctx, quiet.ID, NotificationPrefs{Enabled: false, OnReady: true})
	if p, _ := s.NotificationPrefs(ctx, quiet.ID); p.Wants(NotifyReady) {
		t.Fatal("the master switch must win")
	}
	if p, _ := s.NotificationPrefs(ctx, mkUser(t, s, "fresh", false).ID); p.Enabled || p.Wants(NotifyReady) {
		t.Fatal("notifications must be opt-in")
	}
	s.SetPermission(ctx, fan.ID, "mc", 4)
	s.SetNotificationMutes(ctx, fan.ID, []string{"web"})

	cands, err := s.NotificationCandidates(ctx, "mc", NotifyReady)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]NotificationCandidate{}
	for _, c := range cands {
		got[c.User.Username] = c
	}
	if len(got) != 2 || got["boss"].User == nil || got["fan"].Mask != 4 || got["fan"].Muted {
		t.Fatalf("candidates for mc: %+v", got)
	}
	if _, ok := got["unv"]; ok {
		t.Fatal("unverified addresses must never be candidates")
	}
	webCands, _ := s.NotificationCandidates(ctx, "web", NotifyReady)
	for _, c := range webCands {
		if c.User.Username == "fan" && !c.Muted {
			t.Fatal("mute not reported")
		}
	}
	if stopped, _ := s.NotificationCandidates(ctx, "mc", NotifyStopped); len(stopped) != 0 {
		t.Fatal("nobody asked for stop notifications")
	}

	// The (event, recipient) pair is recorded once.
	id, fresh, err := s.RecordNotification(ctx, "mc|ready|1", fan.ID, "mc", NotifyReady, time.Now())
	if err != nil || !fresh {
		t.Fatal(err)
	}
	if _, again, _ := s.RecordNotification(ctx, "mc|ready|1", fan.ID, "mc", NotifyReady, time.Now()); again {
		t.Fatal("duplicate delivery recorded")
	}
	s.UpdateNotification(ctx, id, DeliverySent, 1, "", time.Now())
	s.RecordNotification(ctx, "mc|ready|2", fan.ID, "mc", NotifyReady, time.Now())
	if err := s.AbandonPendingNotifications(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	log, _ := s.ListNotifications(ctx, 10)
	if len(log) != 2 || log[0].Status != DeliveryAbandoned || log[1].Status != DeliverySent {
		t.Fatalf("delivery log: %+v", log)
	}
}

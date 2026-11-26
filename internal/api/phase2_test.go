package api_test

// End-to-end tests of the phase 2 features: split backup permissions, tmux
// backups, custom actions, concurrency limits and categories, email
// verification, notifications and password reset.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

// verified gives an account a verified email address.
func (h *harness) verified(u *db.User, email string) {
	h.t.Helper()
	yes := true
	if _, err := h.app.Store.UpdateUser(context.Background(), u.ID, db.UserUpdate{Email: &email, EmailVerified: &yes}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) grantAction(adminC *client, userID int64, server, action string, allowed bool) {
	h.t.Helper()
	adminC.want(200, "PUT", fmt.Sprintf("/admin/users/%d/actions/%s/%s", userID, server, action), map[string]bool{"allowed": allowed})
}

func ids(list []any) []string {
	var out []string
	for _, x := range list {
		out = append(out, x.(map[string]any)["id"].(string))
	}
	return out
}

func TestBackupCreateAndDownloadAreSeparatePermissions(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	bdir := filepath.Join(h.dir, "backups")
	os.MkdirAll(bdir, 0o755)
	os.WriteFile(filepath.Join(bdir, "old.tar"), []byte("old backup"), 0o600)
	def := h.serverDef("bk")
	def["backup"] = map[string]any{"directory": bdir, "command": map[string]any{"command": testutil.Script(t, h.dir, "b.sh", `echo new > "$SM_BACKUP_DIR/new-$(date +%s%N).tar"`)}}
	admin.want(201, "POST", "/servers", def)

	creator := h.createUser("cara", "cara-password-1", false)
	downloader := h.createUser("dora", "dora-password-1", false)
	legacy := h.createUser("lee", "lee-password-12", false)
	neither := h.createUser("nick", "nick-password-1", false)
	h.grant(admin, creator.ID, "bk", 64)
	h.grant(admin, downloader.ID, "bk", 128)
	h.grant(admin, legacy.ID, "bk", 16) // the former BACKUPS bit
	h.grant(admin, neither.ID, "bk", 1)
	c, d := h.as("cara", "cara-password-1"), h.as("dora", "dora-password-1")

	// The old bit is stored as both new permissions.
	if got := admin.want(200, "GET", fmt.Sprintf("/admin/users/%d", legacy.ID), nil).Body["permissions"].(map[string]any)["bk"]; got != float64(192) {
		t.Fatalf("legacy BACKUPS grant stored as %v, want 192", got)
	}

	// BACKUP_CREATE: can start backups, cannot see or download files.
	list := c.want(200, "GET", "/servers/bk/backups", nil)
	if list.Body["can_list"] != false || list.Body["can_create"] != true || len(list.Body["files"].([]any)) != 0 || strings.Contains(string(list.Raw), "old.tar") {
		t.Fatalf("creator's view: %s", list.Raw)
	}
	c.want(202, "POST", "/servers/bk/backups", nil)
	h.waitState(admin, "bk", "STOPPED")
	for _, denied := range [][2]string{{"POST", "/servers/bk/backups/old.tar/download"}, {"DELETE", "/servers/bk/backups/old.tar"}} {
		if r := c.do(denied[0], denied[1], nil); r.Status != 403 || r.code() != "permission_denied" {
			t.Errorf("creator %s %s: %d", denied[0], denied[1], r.Status)
		}
	}
	if strings.Contains(string(c.want(200, "GET", "/servers/bk/backups", nil).Raw), ".tar") {
		t.Fatal("the creator must not see backup files, not even the new one")
	}

	// BACKUP_DOWNLOAD: sees and downloads files, cannot start or cancel.
	files := d.want(200, "GET", "/servers/bk/backups", nil)
	if files.Body["can_create"] != false || files.Body["can_list"] != true || len(files.Body["files"].([]any)) != 2 {
		t.Fatalf("downloader's view: %s", files.Raw)
	}
	d.want(200, "POST", "/servers/bk/backups/old.tar/download", nil)
	for _, denied := range []string{"/servers/bk/backups", "/servers/bk/backups/cancel"} {
		if r := d.do("POST", denied, nil); r.Status != 403 {
			t.Errorf("downloader POST %s: %d", denied, r.Status)
		}
	}
	// Neither: no backup access at all.
	n := h.as("nick", "nick-password-1")
	if r := n.do("GET", "/servers/bk/backups", nil); r.Status != 403 {
		t.Fatalf("without backup permissions: %d", r.Status)
	}
	// Both (via the legacy grant): everything.
	l := h.as("lee", "lee-password-12")
	both := l.want(200, "GET", "/servers/bk/backups", nil)
	if both.Body["can_create"] != true || both.Body["can_list"] != true {
		t.Fatalf("both permissions: %s", both.Raw)
	}
}

func TestCustomActionsEndToEnd(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.RateLimits.ActionRun = config.Rate{Requests: 4, Window: config.Duration(time.Minute)}
	})
	admin := h.admin()
	admin.want(201, "POST", "/servers", h.serverDef("mc"))
	script := testutil.Script(t, h.dir, "save.sh", `echo "saving as $SM_ACTION_USER with $1"`)
	action := map[string]any{"id": "save", "name": "Save World", "description": "Forces a save", "icon": "💾",
		"method": "command", "command": map[string]any{"command": script, "args": []string{"flush; rm -rf /"}}}
	alice := h.createUser("alice", "alice-password-1", false)
	bob := h.createUser("bob", "bob-password-12", false)
	carol := h.createUser("carol", "carol-password-1", false)
	h.grant(admin, alice.ID, "mc", 4)  // CONSOLE_READ: can see the server, cannot type
	h.grant(admin, bob.ID, "mc", 4|8)  // even CONSOLE_WRITE does not imply actions
	h.grant(admin, carol.ID, "mc", 32) // CONFIG
	a, b, c := h.as("alice", "alice-password-1"), h.as("bob", "bob-password-12"), h.as("carol", "carol-password-1")

	// Only administrators manage actions.
	if r := a.do("POST", "/servers/mc/actions", action); r.Status != 403 {
		t.Fatalf("user creating an action: %d", r.Status)
	}
	if r := admin.do("POST", "/servers/mc/actions", map[string]any{"id": "Bad ID", "name": "x", "method": "command"}); r.Status != 422 {
		t.Fatalf("invalid action: %d %s", r.Status, r.Raw)
	}
	admin.want(201, "POST", "/servers/mc/actions", action)
	if r := admin.do("POST", "/servers/mc/actions", action); r.Status != 409 {
		t.Fatalf("duplicate action id: %d", r.Status)
	}
	h.grantAction(admin, alice.ID, "mc", "save", true)

	// Visibility: name and description only, never the command.
	av := a.want(200, "GET", "/servers/mc", nil)
	if got := ids(av.Body["server"].(map[string]any)["actions"].([]any)); len(got) != 1 || got[0] != "save" {
		t.Fatalf("alice's actions: %s", av.Raw)
	}
	// ("method":"command" is fine; a "command" key would carry the executable.)
	for _, leak := range []string{script, "rm -rf", `"command":`, `"args"`} {
		if strings.Contains(string(av.Raw), leak) || strings.Contains(string(a.want(200, "GET", "/servers/mc/actions", nil).Raw), leak) {
			t.Fatalf("action configuration leaked to a user (%s)", leak)
		}
	}
	if got := b.want(200, "GET", "/servers/mc", nil).Body["server"].(map[string]any)["actions"].([]any); len(got) != 0 {
		t.Fatalf("bob must not see actions he was not granted: %v", got)
	}

	// Alice runs it without being able to type into the console.
	ev := a.stream("/events", "")
	evB := b.stream("/events", "")
	ev.expect(t, "hello", isType("hello"))
	evB.expect(t, "hello", isType("hello"))
	if r := a.do("POST", "/servers/mc/console/command", map[string]string{"command": "say hi"}); r.Status != 403 {
		t.Fatalf("alice has no console write: %d", r.Status)
	}
	// The request can only name the action.
	if r := a.do("POST", "/servers/mc/actions/save/run", map[string]any{"command": "/bin/sh"}); r.Status != 400 {
		t.Fatalf("injecting a command at run time: %d", r.Status)
	}
	run := a.want(202, "POST", "/servers/mc/actions/save/run", nil)
	if run.Body["run"].(map[string]any)["action_id"] != "save" {
		t.Fatalf("run: %s", run.Raw)
	}
	done := ev.expect(t, "action completed", isType("server.action.completed"))
	if done.Data["status"] != "succeeded" || done.Data["output"] != "saving as alice with flush; rm -rf /" {
		t.Fatalf("result: %v", done.Data)
	}
	evB.never(t, "action events must not reach users without the grant", 500*time.Millisecond, func(e sseEvent) bool {
		return strings.HasPrefix(e.Type, "server.action.")
	})
	last := a.want(200, "GET", "/servers/mc/actions", nil).Body["actions"].([]any)[0].(map[string]any)["last_run"].(map[string]any)
	if last["status"] != "succeeded" || last["username"] != "alice" {
		t.Fatalf("last run: %v", last)
	}

	// Unauthorized users are refused (and audited), CONFIG included.
	for _, u := range []*client{b, c} {
		if r := u.do("POST", "/servers/mc/actions/save/run", nil); r.Status != 403 || r.code() != "permission_denied" {
			t.Errorf("unauthorized run: %d %s", r.Status, r.Raw)
		}
	}
	if r := b.do("POST", "/servers/mc/actions/nonexistent/run", nil); r.Status != 403 {
		t.Fatalf("unknown actions must look like ungranted ones: %d", r.Status)
	}
	stranger := h.createUser("stranger", "stranger-password", false)
	_ = stranger
	if r := h.as("stranger", "stranger-password").do("POST", "/servers/mc/actions/save/run", nil); r.Status != 404 {
		t.Fatalf("a user without server access: %d", r.Status)
	}

	// CONFIG holders neither see nor change actions.
	cfg := c.want(200, "GET", "/servers/mc/config", nil)
	if strings.Contains(string(cfg.Raw), "save.sh") || cfg.Body["config"].(map[string]any)["actions"] != nil {
		t.Fatalf("actions shown to a CONFIG holder: %s", cfg.Raw)
	}
	body := cfg.Body["config"].(map[string]any)
	body["actions"] = []any{map[string]any{"id": "evil", "name": "Evil", "method": "command", "command": map[string]any{"command": "/bin/sh"}}}
	body["name"] = "Renamed"
	c.want(200, "PUT", "/servers/mc/config", body)
	full := admin.want(200, "GET", "/servers/mc/config", nil).Body["config"].(map[string]any)
	if got := ids(full["actions"].([]any)); len(got) != 1 || got[0] != "save" || full["name"] != "Renamed" {
		t.Fatalf("a CONFIG holder changed the actions: %v", full["actions"])
	}

	// Confirmation and disabling.
	action["confirm"] = true
	admin.want(200, "PUT", "/servers/mc/actions/save", action)
	if r := a.do("POST", "/servers/mc/actions/save/run", nil); r.Status != 409 || r.code() != "confirmation_required" {
		t.Fatalf("unconfirmed run: %d %s", r.Status, r.Raw)
	}
	a.want(202, "POST", "/servers/mc/actions/save/run", map[string]bool{"confirm": true})
	ev.expect(t, "confirmed run", isType("server.action.completed"))
	// Rate limit (4 per minute per user and server; three runs so far).
	a.want(202, "POST", "/servers/mc/actions/save/run", map[string]bool{"confirm": true})
	ev.expect(t, "fourth run", isType("server.action.completed"))
	if r := a.do("POST", "/servers/mc/actions/save/run", map[string]bool{"confirm": true}); r.Status != 429 {
		t.Fatalf("action rate limit: %d", r.Status)
	}
	action["disabled"] = true
	admin.want(200, "PUT", "/servers/mc/actions/save", action)
	if got := a.want(200, "GET", "/servers/mc", nil).Body["server"].(map[string]any)["actions"].([]any); len(got) != 0 {
		t.Fatal("disabled actions are hidden from users")
	}

	// Revoking and deleting.
	h.grantAction(admin, alice.ID, "mc", "save", false)
	admin.want(204, "DELETE", "/servers/mc/actions/save", nil)
	if r := admin.do("POST", "/servers/mc/actions/save/run", nil); r.Status != 404 {
		t.Fatalf("deleted action: %d", r.Status)
	}
	if got := admin.want(200, "GET", "/servers/mc/permissions", nil).Body["actions"].([]any); len(got) != 0 {
		t.Fatalf("actions after delete: %v", got)
	}
	runs := admin.want(200, "GET", "/admin/audit?action=ACTION_RUN&username=alice&result=SUCCESS", nil).Body["entries"].([]any)
	if len(runs) != 3 || runs[0].(map[string]any)["details"].(map[string]any)["action_id"] != "save" {
		t.Fatalf("action runs must be audited: %v", runs)
	}
	if denied := admin.want(200, "GET", "/admin/audit?action=ACTION_RUN&result=DENIED", nil).Body["entries"].([]any); len(denied) < 2 {
		t.Fatalf("denied runs must be audited: %v", denied)
	}
}

func TestConcurrencyLimitsAndCategories(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	for _, id := range []string{"a", "b", "c"} {
		def := h.serverDef(id)
		def["category"] = "Minecraft"
		if id == "c" {
			def["category"] = ""
		}
		admin.want(201, "POST", "/servers", def)
	}
	// "Minecraft" is used by definitions but not managed yet.
	cats := admin.want(200, "GET", "/admin/categories", nil)
	if um := cats.Body["unmanaged"].([]any); len(um) != 1 || um[0].(map[string]any)["value"] != "Minecraft" {
		t.Fatalf("unmanaged categories: %s", cats.Raw)
	}
	if r := admin.do("POST", "/admin/categories", map[string]any{"id": "Bad Id", "name": "x"}); r.Status != 422 {
		t.Fatalf("invalid category: %d", r.Status)
	}
	admin.want(201, "POST", "/admin/categories", map[string]any{"id": "minecraft", "name": "Minecraft", "max_concurrent": 1})
	if r := admin.do("POST", "/admin/categories", map[string]any{"id": "minecraft", "name": "Other"}); r.Status != 409 {
		t.Fatalf("duplicate category: %d", r.Status)
	}
	// Managing the category bound the servers to its ID.
	if got := admin.want(200, "GET", "/servers/a/config", nil).Body["config"].(map[string]any)["category"]; got != "minecraft" {
		t.Fatalf("server not bound to the category ID: %v", got)
	}
	sv := admin.want(200, "GET", "/servers/b", nil).Body["server"].(map[string]any)
	if sv["category_id"] != "minecraft" || sv["category_name"] != "Minecraft" {
		t.Fatalf("server view: %v", sv)
	}

	u := h.createUser("una", "una-password-12", false)
	h.grant(admin, u.ID, "a", 2)
	h.grant(admin, u.ID, "b", 2)
	uc := h.as("una", "una-password-12")
	events := uc.stream("/events", "")
	events.expect(t, "hello", isType("hello"))

	uc.want(202, "POST", "/servers/a/start", nil)
	h.waitState(admin, "a", "RUNNING")
	events.expect(t, "usage change", isType("capacity.changed"))
	r := uc.do("POST", "/servers/b/start", nil)
	if r.Status != 409 || r.code() != "concurrency_limit" {
		t.Fatalf("category limit: %d %s", r.Status, r.Raw)
	}
	msg := r.Body["error"].(map[string]any)["message"].(string)
	if msg != "Cannot start the server: the Minecraft category has reached its limit of 1 concurrently running server." {
		t.Fatalf("limit message: %q", msg)
	}
	if det := r.Body["error"].(map[string]any)["details"].(map[string]any); det["scope"] != "category" || det["in_use"] != float64(1) {
		t.Fatalf("limit details: %v", det)
	}
	// Only administrators may override, and it is audited.
	if r := uc.do("POST", "/servers/b/start", map[string]bool{"override_limits": true}); r.Status != 403 {
		t.Fatalf("user override: %d", r.Status)
	}
	admin.want(202, "POST", "/servers/b/start", map[string]bool{"override_limits": true})
	h.waitState(admin, "b", "RUNNING")
	if got := admin.want(200, "GET", "/admin/audit?action=LIMIT_OVERRIDE", nil).Body["entries"].([]any); len(got) != 1 {
		t.Fatalf("override audit: %v", got)
	}
	if got := admin.want(200, "GET", "/admin/audit?action=SERVER_START&result=DENIED", nil).Body["entries"].([]any); len(got) < 2 {
		t.Fatalf("refused starts must be audited: %v", got)
	}

	// Usage, as a user sees it: global plus the categories they can see.
	capv := uc.want(200, "GET", "/capacity", nil)
	cs := capv.Body["categories"].([]any)
	if len(cs) != 1 || cs[0].(map[string]any)["in_use"] != float64(2) || capv.Body["global"].(map[string]any)["in_use"] != float64(2) {
		t.Fatalf("capacity: %s", capv.Raw)
	}

	// The global limit applies to everyone, administrators included.
	admin.want(200, "PUT", "/admin/settings", map[string]any{"limits": map[string]int{"max_concurrent_servers": 2}})
	if r := admin.do("POST", "/servers/c/start", nil); r.Status != 409 || !strings.Contains(r.Raw2(), "global limit of 2") {
		t.Fatalf("global limit for an administrator: %d %s", r.Status, r.Raw)
	}
	uc.want(202, "POST", "/servers/a/stop", nil)
	h.waitState(admin, "a", "STOPPED")
	admin.want(202, "POST", "/servers/c/start", nil)
	h.waitState(admin, "c", "RUNNING")

	// Category changes are reserved for administrators.
	cfgUser := h.createUser("cfg", "cfg-password-12", false)
	h.grant(admin, cfgUser.ID, "c", 32)
	cc := h.as("cfg", "cfg-password-12")
	cdef := cc.want(200, "GET", "/servers/c/config", nil).Body["config"].(map[string]any)
	cdef["category"] = "minecraft"
	if r := cc.do("PUT", "/servers/c/config", cdef); r.Status != 403 {
		t.Fatalf("CONFIG holder changing the category: %d", r.Status)
	}

	// Renaming keeps assignments; deleting turns them into plain names.
	admin.want(200, "PUT", "/admin/categories/minecraft", map[string]any{"name": "Minecraft Java", "max_concurrent": 3})
	if sv := admin.want(200, "GET", "/servers/b", nil).Body["server"].(map[string]any); sv["category_name"] != "Minecraft Java" {
		t.Fatalf("after rename: %v", sv["category_name"])
	}
	admin.want(204, "DELETE", "/admin/categories/minecraft", nil)
	if got := admin.want(200, "GET", "/servers/b/config", nil).Body["config"].(map[string]any)["category"]; got != "Minecraft Java" {
		t.Fatalf("after delete: %v", got)
	}
	for _, id := range []string{"b", "c"} {
		admin.want(202, "POST", "/servers/"+id+"/stop", nil)
		h.waitState(admin, id, "STOPPED")
	}
}

func (r resp) Raw2() string { return string(r.Raw) }

var resetLinkRe = regexp.MustCompile(`#/reset-password\?token=([A-Za-z0-9_-]{40,})`)

func TestPasswordResetForRegularUsersOnly(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.RateLimits.PasswordReset = config.Rate{Requests: 100, Window: config.Duration(time.Minute)}
	})
	admin := h.admin()
	adminUser, _ := h.app.Store.UserByUsername(context.Background(), "admin")
	h.verified(adminUser, "admin@example.com")
	rita := h.createUser("rita", "rita-password-1", false)
	h.verified(rita, "rita@example.com")
	unv := h.createUser("unv", "unv-password-12", false)
	email := "unv@example.com"
	h.app.Store.UpdateUser(context.Background(), unv.ID, db.UserUpdate{Email: &email})

	pub := h.client()
	if meta := pub.want(200, "GET", "/meta", nil); meta.Body["password_reset"] != true {
		t.Fatalf("meta: %s", meta.Raw)
	}
	if f := admin.want(200, "GET", "/me", nil).Body["features"].(map[string]any); f["password_reset"] != false {
		t.Fatal("administrators are never offered email password reset")
	}
	ritaSession := h.as("rita", "rita-password-1")

	// Identical answers whatever the address.
	var first []byte
	for _, addr := range []string{"rita@example.com", "admin@example.com", "nobody@example.com", "unv@example.com"} {
		r := pub.want(202, "POST", "/auth/password/forgot", map[string]string{"email": addr})
		if first == nil {
			first = r.Raw
		} else if string(r.Raw) != string(first) {
			t.Fatalf("response for %s differs: %s vs %s", addr, r.Raw, first)
		}
	}
	if !strings.Contains(string(first), "If an eligible account exists for this email address") {
		t.Fatalf("neutral message: %s", first)
	}
	testutil.Eventually(t, 5*time.Second, "reset email", func() bool { return resetLinkRe.MatchString(h.logs.String()) })
	time.Sleep(300 * time.Millisecond)
	links := resetLinkRe.FindAllStringSubmatch(h.logs.String(), -1)
	if len(links) != 1 {
		t.Fatalf("exactly one link (for rita) expected, found %d", len(links))
	}
	if !strings.Contains(h.logs.String(), "to=rita@example.com") {
		t.Fatal("the link must go to rita")
	}
	token := links[0][1]
	denied := admin.want(200, "GET", "/admin/audit?action=PASSWORD_RESET_REQUEST&result=DENIED", nil).Body["entries"].([]any)
	if len(denied) != 2 {
		t.Fatalf("refused requests (administrator, unverified) are audited for administrators: %v", denied)
	}

	if r := pub.do("POST", "/auth/password/reset/check", map[string]string{"token": "bogus"}); r.Status != 404 {
		t.Fatalf("bogus token: %d", r.Status)
	}
	pub.want(200, "POST", "/auth/password/reset/check", map[string]string{"token": token})
	if r := pub.do("POST", "/auth/password/reset", map[string]string{"token": token, "password": "short"}); r.Status != 400 {
		t.Fatalf("password policy: %d", r.Status)
	}
	if r := pub.do("POST", "/auth/password/reset", map[string]string{"token": token, "password": "rita"}); r.Status != 400 {
		t.Fatalf("password policy (username): %d", r.Status)
	}
	res := pub.want(200, "POST", "/auth/password/reset", map[string]string{"token": token, "password": "brand-new-password"})
	if res.Body["sessions_revoked"].(float64) < 1 {
		t.Fatalf("sessions must be revoked: %s", res.Raw)
	}
	if r := ritaSession.do("GET", "/me", nil); r.Status != 401 {
		t.Fatalf("an old session survived the reset: %d", r.Status)
	}
	if r := pub.do("POST", "/auth/password/reset", map[string]string{"token": token, "password": "another-password-1"}); r.Status != 404 || r.code() != "token_invalid" {
		t.Fatalf("a used token: %d", r.Status)
	}
	if r := h.client().login("rita", "rita-password-1"); r.Status != 401 {
		t.Fatal("the old password must no longer work")
	}
	h.as("rita", "brand-new-password")

	// Expired links are refused.
	pub.want(202, "POST", "/auth/password/forgot", map[string]string{"email": "rita@example.com"})
	testutil.Eventually(t, 5*time.Second, "second link", func() bool { return len(resetLinkRe.FindAllString(h.logs.String(), -1)) == 2 })
	second := resetLinkRe.FindAllStringSubmatch(h.logs.String(), -1)[1][1]
	h.app.Store.DB.Exec(`UPDATE password_resets SET expires_at = 1 WHERE used_at IS NULL`)
	if r := pub.do("POST", "/auth/password/reset", map[string]string{"token": second, "password": "expired-attempt-1"}); r.Status != 404 {
		t.Fatalf("expired token: %d", r.Status)
	}

	// Even a token that exists for an administrator account cannot be used.
	if err := h.app.Store.CreatePasswordReset(context.Background(), adminUser.ID, "admin@example.com", auth.HashToken("admin-token-xyz"), time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r := pub.do("POST", "/auth/password/reset", map[string]string{"token": "admin-token-xyz", "password": "attacker-password-1"}); r.Status != 404 {
		t.Fatalf("administrator reset: %d", r.Status)
	}
	h.as("admin", adminPassword) // unchanged

	if got := admin.want(200, "GET", "/admin/audit?action=PASSWORD_RESET&result=SUCCESS", nil).Body["entries"].([]any); len(got) != 1 {
		t.Fatalf("reset audit: %v", got)
	}
}

func TestPasswordResetRateLimits(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.RateLimits.PasswordReset = config.Rate{Requests: 2, Window: config.Duration(time.Minute)}
	})
	pub := h.client()
	pub.want(202, "POST", "/auth/password/forgot", map[string]string{"email": "x@example.com"})
	pub.want(202, "POST", "/auth/password/forgot", map[string]string{"email": "x@example.com"})
	if r := pub.do("POST", "/auth/password/forgot", map[string]string{"email": "y@example.com"}); r.Status != 429 {
		t.Fatalf("per-address limit: %d", r.Status)
	}
}

func TestEmailVerificationAndServerNotifications(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.admin()
	admin.want(201, "POST", "/servers", h.serverDef("nt"))
	nora := h.createUser("nora", "nora-password-1", false)
	h.grant(admin, nora.ID, "nt", 2)
	admin.want(200, "PATCH", fmt.Sprintf("/admin/users/%d", nora.ID), map[string]string{"email": "nora@example.com"})
	n := h.as("nora", "nora-password-1")
	me := n.want(200, "GET", "/me", nil).Body["user"].(map[string]any)
	if me["email"] != "nora@example.com" || me["email_verified"] != false {
		t.Fatalf("an address set by an administrator is unverified: %v", me)
	}
	all := map[string]any{"enabled": true, "on_ready": true, "on_stopped": true, "on_crash": true, "on_failure": true, "muted": []string{"ghost"}}
	got := n.want(200, "PUT", "/me/notifications", all)
	if prefs := got.Body["preferences"].(map[string]any); prefs["on_ready"] != true || len(prefs["muted"].([]any)) != 0 {
		t.Fatalf("preferences (unknown servers are not stored as mutes): %s", got.Raw)
	}

	// Others who must never be notified about "nt".
	outsider := h.createUser("outsider", "outsider-pass-1", false)
	h.verified(outsider, "outsider@example.com")
	unverified := h.createUser("unv", "unv-password-12", false)
	h.grant(admin, unverified.ID, "nt", 1)
	e := "unv@example.com"
	h.app.Store.UpdateUser(context.Background(), unverified.ID, db.UserUpdate{Email: &e})
	for _, u := range []*db.User{outsider, unverified} {
		h.app.Store.SetNotificationPrefs(context.Background(), u.ID, db.NotificationPrefs{Enabled: true, OnReady: true, OnStopped: true, OnCrash: true})
	}

	// Verify the address through the emailed link.
	n.want(202, "POST", "/me/email/verification", nil)
	m := regexp.MustCompile(`#/verify-email\?token=([A-Za-z0-9_-]{40,})`).FindStringSubmatch(h.logs.String())
	if m == nil {
		t.Fatal("verification link not found")
	}
	h.client().want(200, "POST", "/auth/email/verify", map[string]string{"token": m[1]})
	if v := n.want(200, "GET", "/me", nil).Body["user"].(map[string]any)["email_verified"]; v != true {
		t.Fatal("the address must be verified now")
	}
	if r := n.do("POST", "/me/email/verification", nil); r.Status != 409 {
		t.Fatalf("verifying twice: %d", r.Status)
	}

	deliveries := func(user int64, kind string) int {
		list, _ := h.app.Store.ListNotifications(context.Background(), 1000)
		c := 0
		for _, d := range list {
			if d.UserID == user && d.Kind == kind && d.ServerID == "nt" {
				c++
			}
		}
		return c
	}
	n.want(202, "POST", "/servers/nt/start", nil)
	h.waitState(admin, "nt", "RUNNING")
	testutil.Eventually(t, 5*time.Second, "ready notification", func() bool { return deliveries(nora.ID, "ready") == 1 })
	n.want(202, "POST", "/servers/nt/stop", nil)
	h.waitState(admin, "nt", "STOPPED")
	testutil.Eventually(t, 5*time.Second, "stopped notification", func() bool { return deliveries(nora.ID, "stopped") == 1 })
	if !strings.Contains(h.logs.String(), "Server Started — Server nt") || !strings.Contains(h.logs.String(), "Server Stopped — Server nt") {
		t.Fatal("notification subjects not found in the development log")
	}

	// The server exits by itself: an unexpected stop, not a normal stop.
	n.want(202, "POST", "/servers/nt/start", nil)
	h.waitState(admin, "nt", "RUNNING")
	admin.want(204, "POST", "/servers/nt/console/command", map[string]string{"command": "stop"})
	h.waitState(admin, "nt", "STOPPED")
	testutil.Eventually(t, 5*time.Second, "crash notification", func() bool { return deliveries(nora.ID, "crash") == 1 })
	time.Sleep(1500 * time.Millisecond)
	if deliveries(nora.ID, "stopped") != 1 || deliveries(nora.ID, "crash") != 1 {
		t.Fatal("an unexpected stop must produce exactly one (crash) notification")
	}
	if !strings.Contains(h.logs.String(), "Server Stopped Unexpectedly — Server nt") {
		t.Fatal("unexpected stop subject not found")
	}
	for _, u := range []*db.User{outsider, unverified} {
		for _, k := range []string{"ready", "stopped", "crash"} {
			if deliveries(u.ID, k) != 0 {
				t.Fatalf("%s must not be notified (%s)", u.Username, k)
			}
		}
	}

	// Turning notifications off stops them.
	all["enabled"] = false
	n.want(200, "PUT", "/me/notifications", all)
	n.want(202, "POST", "/servers/nt/start", nil)
	h.waitState(admin, "nt", "RUNNING")
	time.Sleep(time.Second)
	if deliveries(nora.ID, "ready") != 1 {
		t.Fatal("notifications must stop once disabled")
	}
	n.want(202, "POST", "/servers/nt/stop", nil)
	h.waitState(admin, "nt", "STOPPED")
}

// The acceptance scenario on a real tmux server: a Minecraft-like server
// backed up through its console, and a Save World action for a user who
// cannot type into the console.
func TestMinecraftTmuxScenario(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	h := newHarness(t, nil)
	socket := fmt.Sprintf("smapi%d%d", os.Getpid(), time.Now().UnixNano()%100000)
	t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
	admin := h.admin()
	bdir := filepath.Join(h.dir, "mc-backups")
	os.MkdirAll(bdir, 0o755)
	script := testutil.Script(t, h.dir, "mc.sh", `echo "Done (1.0s)! For help, type \"help\""
while read line; do
  echo "got: $line"
  [ "$line" = "/backup start" ] && { sleep 0.2; echo data > "`+bdir+`/world.tar"; echo "Backup complete"; }
  [ "$line" = "stop" ] && exit 0
done`)
	admin.want(201, "POST", "/servers", map[string]any{
		"id": "minecraft", "name": "Minecraft Survival", "backend": "tmux", "category": "Minecraft",
		"tmux":      map[string]any{"session": "mc-accept", "socket_name": socket, "start": map[string]any{"command": script}, "stop_text": "stop"},
		"readiness": map[string]any{"method": "console", "console": map[string]any{"pattern": "Done ("}, "interval": "200ms"},
		"backup":    map[string]any{"method": "tmux", "directory": bdir, "tmux": map[string]any{"text": "/backup start", "enter": true}},
		"timeouts":  map[string]any{"start": "10s", "stop": "5s"},
	})
	admin.want(201, "POST", "/servers/minecraft/actions", map[string]any{"id": "save_world", "name": "Save World", "method": "tmux", "tmux": map[string]any{"text": "save-all flush"}})
	steve := h.createUser("steve", "steve-password-1", false)
	h.grant(admin, steve.ID, "minecraft", 4) // CONSOLE_READ only
	h.grantAction(admin, steve.ID, "minecraft", "save_world", true)
	s := h.as("steve", "steve-password-1")

	admin.want(202, "POST", "/servers/minecraft/start", nil)
	h.waitState(admin, "minecraft", "RUNNING")

	// Backup through the console: submitted, not falsely "completed".
	admin.want(202, "POST", "/servers/minecraft/backups", nil)
	testutil.Eventually(t, 10*time.Second, "backup operation", func() bool {
		ops := admin.want(200, "GET", "/servers/minecraft/operations", nil).Body["operations"].([]any)
		return len(ops) > 0 && ops[0].(map[string]any)["status"] == "submitted"
	})
	testutil.Eventually(t, 10*time.Second, "backup file listed", func() bool {
		return strings.Contains(string(admin.want(200, "GET", "/servers/minecraft/backups", nil).Raw), "world.tar")
	})

	// Steve saves the world, but cannot type anything himself.
	if r := s.do("POST", "/servers/minecraft/console/command", map[string]string{"command": "op steve"}); r.Status != 403 {
		t.Fatalf("console write without CONSOLE_WRITE: %d", r.Status)
	}
	s.want(202, "POST", "/servers/minecraft/actions/save_world/run", nil)
	testutil.Eventually(t, 10*time.Second, "save-all in the console", func() bool {
		return strings.Contains(string(s.want(200, "GET", "/servers/minecraft/console", nil).Raw), "got: save-all flush")
	})
	runs := s.want(200, "GET", "/servers/minecraft/actions", nil).Body["actions"].([]any)
	testutil.Eventually(t, 5*time.Second, "action result", func() bool {
		runs = s.want(200, "GET", "/servers/minecraft/actions", nil).Body["actions"].([]any)
		lr, _ := runs[0].(map[string]any)["last_run"].(map[string]any)
		return lr != nil && lr["status"] == "submitted"
	})
	admin.want(202, "POST", "/servers/minecraft/stop", nil)
	h.waitState(admin, "minecraft", "STOPPED")
}

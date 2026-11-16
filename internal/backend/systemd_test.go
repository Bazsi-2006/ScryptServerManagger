package backend

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

// The systemd backend is tested against stand-in systemctl/journalctl
// scripts that mimic the real tools' output and record how they were
// called. Testing against a real systemd requires a host running it and a
// unit the test user may control; see docs/development.md.
func systemdEnv(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state"), []byte("inactive"), 0o644)
	os.WriteFile(filepath.Join(dir, "journal"), []byte("old entry\nserver listening\n"), 0o644)
	systemctl := testutil.Script(t, dir, "systemctl", `D=`+dir+`
echo "systemctl $*" >> $D/calls
user=""; [ "$1" = "--user" ] && { user=1; shift; }
verb=$1; shift
unit=""
for a in "$@"; do case "$a" in --*) ;; *) unit=$a ;; esac; done
case "$verb" in
  show)
    if [ "$unit" = "missing.service" ]; then echo "LoadState=not-found"; echo "ActiveState=inactive"; echo "SubState=dead"; echo "Result=success"; exit 0; fi
    echo "LoadState=loaded"; echo "ActiveState=$(cat $D/state)"; echo "SubState=x"; echo "Result=exit-code" ;;
  start|restart) if [ -f $D/deny ]; then echo "Failed to start $unit: Interactive authentication required." >&2; exit 1; fi; echo active > $D/state ;;
  stop) echo inactive > $D/state ;;
  *) exit 2 ;;
esac`)
	journalctl := testutil.Script(t, dir, "journalctl", `D=`+dir+`
echo "journalctl $*" >> $D/calls
follow=""; for a in "$@"; do [ "$a" = "--follow" ] && follow=1; done
cat $D/journal
[ -n "$follow" ] && exec tail -n 0 -f $D/journal
exit 0`)
	sudo := testutil.Script(t, dir, "sudo", `D=`+dir+`
echo "sudo $*" >> $D/calls
[ "$1" = "-n" ] && shift
exec "$@"`)
	e.cfg.Update(func(c *configT) error {
		c.Tools.Systemctl, c.Tools.Journalctl, c.Tools.Sudo = systemctl, journalctl, sudo
		return nil
	})
	return e, dir
}

func calls(t *testing.T, dir string) string {
	data, _ := os.ReadFile(filepath.Join(dir, "calls"))
	return string(data)
}

func TestSystemdStateComesFromSystemd(t *testing.T) {
	e, dir := systemdEnv(t)
	def := valid(t, &servers.Definition{ID: "sd", Name: "SD", Backend: servers.BackendSystemd, Systemd: &servers.Systemd{Unit: "game.service"}})
	b := newSystemd(def, e.deps)
	setState := func(s string) { os.WriteFile(filepath.Join(dir, "state"), []byte(s), 0o644) }

	cases := []struct {
		state string
		life  Life
		phase string
	}{
		{"inactive", LifeDead, ""}, {"active", LifeAlive, ""}, {"activating", LifeAlive, PhaseStarting},
		{"deactivating", LifeAlive, PhaseStopping}, {"failed", LifeDead, PhaseFailed}, {"reloading", LifeAlive, ""},
	}
	for _, c := range cases {
		setState(c.state)
		if p := b.Probe(bg); p.Life != c.life || p.Phase != c.phase {
			t.Errorf("ActiveState=%s: got %+v", c.state, p)
		}
	}
	setState("inactive")
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	if b.Probe(bg).Life != LifeAlive {
		t.Fatal("unit should be active after start")
	}
	if err := b.Restart(bg, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(bg, time.Second); err != nil {
		t.Fatal(err)
	}
	if b.Probe(bg).Life != LifeDead {
		t.Fatal("unit should be inactive after stop")
	}
	log := calls(t, dir)
	for _, want := range []string{"systemctl start --no-block game.service", "systemctl stop --no-block game.service", "systemctl restart game.service"} {
		if !strings.Contains(log, want) {
			t.Errorf("expected call %q in:\n%s", want, log)
		}
	}
	if strings.Contains(log, "sudo") {
		t.Error("sudo must not be used unless configured")
	}
	if err := b.Kill(bg); !errors.Is(err, ErrUnsupported) {
		t.Errorf("kill is left to systemd: %v", err)
	}
	if err := b.SendInput(bg, "x"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("console input is unsupported: %v", err)
	}
}

func TestSystemdMissingUnitAndMissingTool(t *testing.T) {
	e, _ := systemdEnv(t)
	def := valid(t, &servers.Definition{ID: "sd", Name: "SD", Backend: servers.BackendSystemd, Systemd: &servers.Systemd{Unit: "missing.service"}})
	p := newSystemd(def, e.deps).Probe(bg)
	if p.Life != LifeUnknown || !strings.Contains(p.Detail, "was not found") {
		t.Fatalf("missing unit: %+v", p)
	}
	e.cfg.Update(func(c *configT) error { c.Tools.Systemctl = "/nonexistent/systemctl"; return nil })
	p = newSystemd(def, e.deps).Probe(bg)
	if p.Life != LifeUnknown {
		t.Fatalf("missing systemctl: %+v", p)
	}
	if err := newSystemd(def, e.deps).Start(bg); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("start without systemctl: %v", err)
	}
}

func TestSystemdSudoUserScopeAndPermissionErrors(t *testing.T) {
	e, dir := systemdEnv(t)
	sudoDef := valid(t, &servers.Definition{ID: "s1", Name: "S", Backend: servers.BackendSystemd, Systemd: &servers.Systemd{Unit: "game.service", UseSudo: true}})
	b := newSystemd(sudoDef, e.deps)
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	b.Probe(bg)
	log := calls(t, dir)
	if !strings.Contains(log, "sudo -n ") || !strings.Contains(log, "systemctl start --no-block game.service") {
		t.Fatalf("state changes should go through sudo -n:\n%s", log)
	}
	if strings.Count(log, "sudo") != 1 {
		t.Fatalf("read-only queries must not use sudo:\n%s", log)
	}

	userDef := valid(t, &servers.Definition{ID: "s2", Name: "S", Backend: servers.BackendSystemd, Systemd: &servers.Systemd{Unit: "game.service", Scope: "user"}})
	ub := newSystemd(userDef, e.deps)
	ub.Stop(bg, time.Second)
	ub.Snapshot(bg, 5)
	log = calls(t, dir)
	if !strings.Contains(log, "systemctl --user stop --no-block game.service") || !strings.Contains(log, "journalctl --user --user-unit game.service") {
		t.Fatalf("user scope flags missing:\n%s", log)
	}

	os.WriteFile(filepath.Join(dir, "deny"), nil, 0o644)
	plain := valid(t, &servers.Definition{ID: "s3", Name: "S", Backend: servers.BackendSystemd, Systemd: &servers.Systemd{Unit: "game.service"}})
	err := newSystemd(plain, e.deps).Start(bg)
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("a polkit denial should be explained: %v", err)
	}
}

func TestSystemdJournalSnapshotAndFollow(t *testing.T) {
	e, dir := systemdEnv(t)
	def := valid(t, &servers.Definition{ID: "sd", Name: "SD", Backend: servers.BackendSystemd, Systemd: &servers.Systemd{Unit: "game.service"}})
	b := newSystemd(def, e.deps)
	lines, err := b.Snapshot(bg, 10)
	if err != nil || len(lines) != 2 || lines[1] != "server listening" {
		t.Fatalf("snapshot %q %v", lines, err)
	}
	ctx, cancel := contextWithCancel()
	done := make(chan error, 1)
	go func() { done <- b.Follow(ctx, FollowOptions{Tail: 10}) }()
	testutil.Eventually(t, 5*time.Second, "journal history", func() bool { return e.has("server listening") })
	f, _ := os.OpenFile(filepath.Join(dir, "journal"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("new entry while following\n")
	f.Close()
	testutil.Eventually(t, 5*time.Second, "live journal line", func() bool { return e.has("new entry while following") })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Follow must stop when cancelled")
	}
	if !strings.Contains(calls(t, dir), "--unit game.service --no-pager --output=cat --lines=10 --follow") {
		t.Fatalf("journalctl invocation:\n%s", calls(t, dir))
	}
}

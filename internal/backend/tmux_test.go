package backend

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

// These tests drive a real tmux server on a private socket. Requirement:
// the tmux binary must be installed; otherwise they are skipped.
func tmuxEnv(t *testing.T) (*env, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := fmt.Sprintf("smtest%d%d", os.Getpid(), time.Now().UnixNano()%100000)
	t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
	return newEnv(t), socket
}

func TestTmuxLifecycleLiveOutputAndInput(t *testing.T) {
	e, socket := tmuxEnv(t)
	dir := t.TempDir()
	script := testutil.Script(t, dir, "srv.sh", `echo "server up: $GREETING / $1"
while read line; do echo "got: $line"; [ "$line" = "stop" ] && exit 0; done`)
	def := valid(t, &servers.Definition{ID: "tm", Name: "TM", Backend: servers.BackendTmux, WorkingDir: dir,
		Env:  map[string]string{"GREETING": "hello world"},
		Tmux: &servers.Tmux{Session: "sm-test_1", SocketName: socket, Start: &servers.Command{Command: script, Args: []string{"an arg"}}, StopText: "stop"}})
	b := newTmux(def, e.deps)
	defer b.Close()

	if p := b.Probe(bg); p.Life != LifeDead {
		t.Fatalf("no session yet: %+v", p)
	}
	if err := b.SendInput(bg, "x"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("input without a session: %v", err)
	}
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	if p := b.Probe(bg); p.Life != LifeAlive {
		t.Fatalf("session should exist: %+v", p)
	}
	if err := b.Start(bg); err == nil {
		t.Fatal("starting an existing session must be refused")
	}
	// Environment and arguments arrive intact (spaces preserved).
	testutil.Eventually(t, 5*time.Second, "startup line", func() bool { return e.has("server up: hello world / an arg") })

	// Live streaming: output produced later shows up without polling the API.
	if err := b.SendInput(bg, "say hi there"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "live output of the command", func() bool { return e.has("got: say hi there") })

	// Text is typed literally: tmux key names and shell syntax are not interpreted.
	if err := b.SendInput(bg, "C-c Enter; $(id) -x"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "literal text", func() bool { return e.has("got: C-c Enter; $(id) -x") })
	if b.Probe(bg).Life != LifeAlive {
		t.Fatal(`"C-c" sent as text must not interrupt the server`)
	}
	if err := b.SendInput(bg, "bad\x03input"); err == nil {
		t.Fatal("control characters must be rejected")
	}

	snap, err := b.Snapshot(bg, 50)
	if err != nil || !strings.Contains(strings.Join(snap, "\n"), "got: say hi there") {
		t.Fatalf("snapshot: %v %q", err, snap)
	}

	if err := b.Stop(bg, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "session to end after graceful stop", func() bool { return b.Probe(bg).Life == LifeDead })
}

func TestTmuxKillAndExternalDisappearance(t *testing.T) {
	e, socket := tmuxEnv(t)
	def := valid(t, &servers.Definition{ID: "tk", Name: "TK", Backend: servers.BackendTmux,
		Tmux: &servers.Tmux{Session: "killme", SocketName: socket, Start: &servers.Command{Command: "sleep", Args: []string{"300"}}}})
	b := newTmux(def, e.deps)
	defer b.Close()
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	// No graceful method configured: Stop kills the session.
	if err := b.Stop(bg, time.Second); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "kill", func() bool { return b.Probe(bg).Life == LifeDead })

	// A session that vanishes behind our back is simply reported as stopped.
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	exec.Command("tmux", "-L", socket, "kill-server").Run()
	testutil.Eventually(t, 5*time.Second, "disappearance detected", func() bool { return b.Probe(bg).Life == LifeDead })
	if err := b.Kill(bg); err != nil {
		t.Fatalf("killing an absent session is not an error: %v", err)
	}
}

func TestTmuxSessionNamesAreMatchedExactly(t *testing.T) {
	e, socket := tmuxEnv(t)
	long := valid(t, &servers.Definition{ID: "a", Name: "A", Backend: servers.BackendTmux,
		Tmux: &servers.Tmux{Session: "game-eu", SocketName: socket, Start: &servers.Command{Command: "sleep", Args: []string{"300"}}}})
	short := valid(t, &servers.Definition{ID: "b", Name: "B", Backend: servers.BackendTmux,
		Tmux: &servers.Tmux{Session: "game", SocketName: socket, Start: &servers.Command{Command: "sleep", Args: []string{"300"}}}})
	bl, bs := newTmux(long, e.deps), newTmux(short, newEnv(t).deps)
	defer bl.Close()
	defer bs.Close()
	if err := bl.Start(bg); err != nil {
		t.Fatal(err)
	}
	// tmux would normally resolve "game" to "game-eu" by prefix.
	if bs.Probe(bg).Life != LifeDead {
		t.Fatal(`session "game" must not match the existing session "game-eu"`)
	}
	if err := bs.Kill(bg); err != nil {
		t.Fatal(err)
	}
	if bl.Probe(bg).Life != LifeAlive {
		t.Fatal(`killing "game" must not kill "game-eu"`)
	}
}

func TestTmuxAttachOnlyAndMissingBinary(t *testing.T) {
	e, socket := tmuxEnv(t)
	def := valid(t, &servers.Definition{ID: "att", Name: "A", Backend: servers.BackendTmux, Tmux: &servers.Tmux{Session: "external", SocketName: socket}})
	b := newTmux(def, e.deps)
	defer b.Close()
	if b.Caps().Start {
		t.Fatal("a session without a start command is attach-only")
	}
	if err := b.Start(bg); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("start of attach-only session: %v", err)
	}
	// Someone starts the session outside the manager: it is picked up.
	if out, err := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", "external", "sh", "-c", "echo preexisting; sleep 300").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	testutil.Eventually(t, 5*time.Second, "external session detected", func() bool { return b.Probe(bg).Life == LifeAlive })
	testutil.Eventually(t, 5*time.Second, "existing pane content loaded", func() bool { return e.has("preexisting") })

	// With no tmux binary the state is unknown, not "stopped".
	e2 := newEnv(t)
	e2.cfg.Update(func(c *configT) error { c.Tools.Tmux = "/nonexistent/tmux"; return nil })
	gone := newTmux(def, e2.deps)
	defer gone.Close()
	if p := gone.Probe(bg); p.Life != LifeUnknown || p.Detail == "" {
		t.Fatalf("missing tmux binary: %+v", p)
	}
}

func TestTmuxInstantFailureKeepsItsOutputAndExitCode(t *testing.T) {
	e, socket := tmuxEnv(t)
	dir := t.TempDir()
	// Fails within milliseconds, like a start script refusing to run.
	script := testutil.Script(t, dir, "fail.sh", `echo "[start.sh] ERROR: the EULA is not accepted"; exit 3`)
	def := valid(t, &servers.Definition{ID: "ff", Name: "FF", Backend: servers.BackendTmux,
		Tmux: &servers.Tmux{Session: "instant-fail", SocketName: socket, Start: &servers.Command{Command: script}}})
	b := newTmux(def, e.deps)
	defer b.Close()
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	var p Probe
	testutil.Eventually(t, 5*time.Second, "the failed start to be detected", func() bool { p = b.Probe(bg); return p.Life == LifeDead })
	if !strings.Contains(p.Detail, "exited with code 3") || p.Phase != PhaseFailed {
		t.Fatalf("a non-zero exit code should be reported as a failure: %+v", p)
	}
	testutil.Eventually(t, 3*time.Second, "the error message in the console", func() bool { return e.has("[start.sh] ERROR: the EULA is not accepted") })
	if out, err := exec.Command("tmux", "-L", socket, "has-session", "-t", "=instant-fail").CombinedOutput(); err == nil {
		t.Fatalf("the finished session should have been removed: %s", out)
	}
	// It can be started again straight away.
	if err := b.Start(bg); err != nil {
		t.Fatalf("restart after a failure: %v", err)
	}
}

func TestTmuxSingleCommandWithNologinShell(t *testing.T) {
	// Under systemd, SHELL is the service account's login shell, typically
	// /usr/sbin/nologin. A start command without arguments must still run.
	e, socket := tmuxEnv(t)
	nologin := "/usr/sbin/nologin"
	if _, err := os.Stat(nologin); err != nil {
		nologin = "/bin/false"
	}
	t.Setenv("SHELL", nologin)
	dir := t.TempDir()
	script := testutil.Script(t, dir, "srv.sh", `echo "single command up"; while read l; do echo "got: $l"; [ "$l" = stop ] && exit 0; done`)
	def := valid(t, &servers.Definition{ID: "nl", Name: "NL", Backend: servers.BackendTmux,
		Tmux: &servers.Tmux{Session: "nologin-shell", SocketName: socket, Start: &servers.Command{Command: script}, StopText: "stop"}})
	b := newTmux(def, e.deps)
	defer b.Close()
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "output of the single-command server", func() bool { return e.has("single command up") })
	if b.Probe(bg).Life != LifeAlive {
		t.Fatal("the server should be running")
	}
	if err := b.Stop(bg, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	var p Probe
	testutil.Eventually(t, 5*time.Second, "graceful stop", func() bool { p = b.Probe(bg); return p.Life == LifeDead })
	if p.Phase == PhaseFailed {
		t.Fatalf("exit code 0 after a stop is not a failure: %+v", p)
	}
	if _, err := exec.Command("tmux", "-L", socket, "has-session", "-t", "=nologin-shell").CombinedOutput(); err == nil {
		t.Fatal("a session whose server exited normally should be removed")
	}
}

func TestTmuxKilledProcessIsAFailure(t *testing.T) {
	e, socket := tmuxEnv(t)
	dir := t.TempDir()
	script := testutil.Script(t, dir, "crash.sh", `echo "running"; sleep 0.3; kill -KILL $$`)
	def := valid(t, &servers.Definition{ID: "kc", Name: "KC", Backend: servers.BackendTmux,
		Tmux: &servers.Tmux{Session: "killed-crash", SocketName: socket, Start: &servers.Command{Command: script}}})
	b := newTmux(def, e.deps)
	defer b.Close()
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	var p Probe
	testutil.Eventually(t, 5*time.Second, "the crash to be detected", func() bool { p = b.Probe(bg); return p.Life == LifeDead })
	if p.Phase != PhaseFailed || !strings.Contains(p.Detail, "signal 9") {
		t.Fatalf("a process killed by a signal is a failure: %+v", p)
	}
}

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

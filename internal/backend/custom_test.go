package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

var bg = context.Background()

const echoServer = `echo "started $1"
trap 'echo got-term; exit 0' TERM
while read line; do
  echo "got: $line"
  [ "$line" = "stop" ] && exit 0
done
sleep 300 & wait`

func TestCustomForegroundLifecycleAndStdin(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	def := valid(t, &servers.Definition{ID: "fg", Name: "FG", Backend: servers.BackendCustom,
		Custom:  &servers.Custom{Start: servers.Command{Command: testutil.Script(t, dir, "srv.sh", echoServer), Args: []string{"arg one"}}, StopInput: "stop"},
		Console: servers.Console{Input: servers.InputStdin}})
	b := newCustom(def, e.deps)
	defer b.Close()

	if p := b.Probe(bg); p.Life != LifeDead {
		t.Fatalf("initial life %v", p.Life)
	}
	if err := b.SendInput(bg, "x"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("input while stopped: %v", err)
	}
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(bg); err == nil {
		t.Fatal("a second start while running must be refused")
	}
	if p := b.Probe(bg); p.Life != LifeAlive {
		t.Fatalf("life after start %v", p.Life)
	}
	testutil.Eventually(t, 5*time.Second, "startup output", func() bool { return e.has("started arg one") })
	if e.rt == "" {
		t.Fatal("runtime state (pid) must be persisted for re-adoption")
	}
	if err := b.SendInput(bg, "hello world"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "echo of stdin", func() bool { return e.has("got: hello world") })

	if err := b.Stop(bg, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "process exit", func() bool { return b.Probe(bg).Life == LifeDead })
	if p := b.Probe(bg); p.Phase == PhaseFailed {
		t.Fatal("a requested stop must not be reported as a failure")
	}
	if e.rt != "" {
		t.Fatal("runtime state must be cleared after exit")
	}
}

func TestCustomCrashIsReportedAsFailure(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	def := valid(t, &servers.Definition{ID: "crash", Name: "C", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Start: servers.Command{Command: testutil.Script(t, dir, "crash.sh", "echo boom; exit 3")}}})
	b := newCustom(def, e.deps)
	defer b.Close()
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "exit", func() bool { return b.Probe(bg).Life == LifeDead })
	p := b.Probe(bg)
	if p.Phase != PhaseFailed || !strings.Contains(p.Detail, "code 3") {
		t.Fatalf("crash probe %+v", p)
	}
	testutil.Eventually(t, 3*time.Second, "captured output", func() bool { return e.has("boom") })
}

func TestCustomStartErrorsAreExplained(t *testing.T) {
	e := newEnv(t)
	def := valid(t, &servers.Definition{ID: "gone", Name: "G", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Start: servers.Command{Command: "/nonexistent/server.sh"}}})
	b := newCustom(def, e.deps)
	defer b.Close()
	err := b.Start(bg)
	if err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("missing executable: %v", err)
	}
	if b.Probe(bg).Life != LifeDead {
		t.Fatal("a failed start must leave the server stopped")
	}
}

func TestCustomSignalStopAndKill(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	// Ignores SIGTERM: only a kill ends it. It also spawns a child that must
	// die with the process group.
	childPid := filepath.Join(dir, "child.pid")
	script := testutil.Script(t, dir, "stubborn.sh", "trap '' TERM\nsleep 300 &\necho $! > "+childPid+"\necho up\nwait")
	def := valid(t, &servers.Definition{ID: "stub", Name: "S", Backend: servers.BackendCustom, Custom: &servers.Custom{Start: servers.Command{Command: script}}})
	b := newCustom(def, e.deps)
	defer b.Close()
	if !b.Caps().ForceKill {
		t.Fatal("foreground servers support force kill by default")
	}
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "child pid", func() bool { _, err := readPIDFile(childPid); return err == nil })
	if err := b.Stop(bg, time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if b.Probe(bg).Life != LifeAlive {
		t.Fatal("the process ignores SIGTERM and should still be alive")
	}
	if err := b.Kill(bg); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "kill", func() bool { return b.Probe(bg).Life == LifeDead })
	pid, _ := readPIDFile(childPid)
	testutil.Eventually(t, 5*time.Second, "child of the server to be killed too (no orphans)", func() bool { return !pidAlive(pid) })
}

func TestCustomReadoptionAfterManagerRestart(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	def := valid(t, &servers.Definition{ID: "adopt", Name: "A", Backend: servers.BackendCustom,
		Custom:  &servers.Custom{Start: servers.Command{Command: testutil.Script(t, dir, "srv.sh", echoServer)}, StopInput: "stop"},
		Console: servers.Console{Input: servers.InputStdin}})
	first := newCustom(def, e.deps)
	if err := first.Start(bg); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "startup", func() bool { return e.has("started ") })
	// Simulate the manager going away: the backend is closed, not stopped.
	first.Close()

	second := newCustom(def, e.deps)
	defer second.Close()
	if second.Probe(bg).Life != LifeAlive {
		t.Fatal("the still-running process must be re-adopted from persisted state")
	}
	// Console input keeps working through the reopened FIFO, and output
	// keeps flowing from the capture file.
	if err := second.SendInput(bg, "after restart"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "output after re-adoption", func() bool { return e.has("got: after restart") })
	if err := second.Stop(bg, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "exit of adopted process", func() bool { return second.Probe(bg).Life == LifeDead })
}

func TestCustomStaleRuntimeIsNotAdopted(t *testing.T) {
	e := newEnv(t)
	// Our own PID with a wrong start time: a reused PID must not be adopted.
	e.rt = `{"pid":` + itoa(os.Getpid()) + `,"start_ticks":1}`
	def := valid(t, &servers.Definition{ID: "stale", Name: "S", Backend: servers.BackendCustom, Custom: &servers.Custom{Start: servers.Command{Command: "/bin/true"}}})
	b := newCustom(def, e.deps)
	defer b.Close()
	if b.Probe(bg).Life != LifeDead {
		t.Fatal("a PID whose start time does not match must not be adopted")
	}
	if e.rt != "" {
		t.Fatal("stale runtime state should be cleared")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestCustomPTYConsole(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	script := testutil.Script(t, dir, "tty.sh", `if [ -t 0 ]; then echo "tty: yes"; else echo "tty: no"; fi
while read line; do echo "got: $line"; [ "$line" = "quit" ] && exit 0; done`)
	def := valid(t, &servers.Definition{ID: "pty", Name: "P", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Start: servers.Command{Command: script}, StopInput: "quit"}, Console: servers.Console{Input: servers.InputPTY}})
	b := newCustom(def, e.deps)
	defer b.Close()
	if !b.Caps().StopOnManagerExit {
		t.Fatal("PTY servers must be stopped when the manager exits")
	}
	if err := b.Start(bg); err != nil {
		t.Skipf("no PTY available here: %v", err)
	}
	testutil.Eventually(t, 5*time.Second, "terminal detection", func() bool { return e.has("tty: yes") })
	if err := b.SendInput(bg, "ping"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "pty echo", func() bool { return e.has("got: ping") })
	b.Stop(bg, 5*time.Second)
	testutil.Eventually(t, 5*time.Second, "exit", func() bool { return b.Probe(bg).Life == LifeDead })
}

func TestCustomForkingWithPIDFile(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "daemon.pid")
	// The start script launches a daemon and exits immediately.
	start := testutil.Script(t, dir, "start.sh", "echo launching\n(trap 'exit 0' TERM; while :; do sleep 0.1; done) > /dev/null 2>&1 &\necho $! > "+pidFile)
	def := valid(t, &servers.Definition{ID: "fork", Name: "F", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Mode: servers.ModeForking, Start: servers.Command{Command: start}, PIDFile: pidFile}})
	b := newCustom(def, e.deps)
	defer b.Close()
	if b.Probe(bg).Life != LifeDead {
		t.Fatal("no PID file yet: stopped")
	}
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	// The start script has exited, yet the server counts as running.
	if p := b.Probe(bg); p.Life != LifeAlive {
		t.Fatalf("daemon should be alive after its launcher exited: %+v", p)
	}
	testutil.Eventually(t, 3*time.Second, "launcher output in console", func() bool { return e.has("launching") })
	if err := b.Stop(bg, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "daemon exit", func() bool { return b.Probe(bg).Life == LifeDead })
}

func TestCustomForkingStartScriptFailure(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	def := valid(t, &servers.Definition{ID: "ff", Name: "F", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Mode: servers.ModeForking, Start: servers.Command{Command: testutil.Script(t, dir, "start.sh", "echo cannot bind; exit 4")},
			PIDFile: filepath.Join(dir, "x.pid")}})
	b := newCustom(def, e.deps)
	defer b.Close()
	err := b.Start(bg)
	if err == nil || !strings.Contains(err.Error(), "code 4") {
		t.Fatalf("failing start script: %v", err)
	}
}

func TestCustomForkingWithoutPIDFileDefersToExternalCheck(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	def := valid(t, &servers.Definition{ID: "ext", Name: "E", Backend: servers.BackendCustom,
		Custom:    &servers.Custom{Mode: servers.ModeForking, Start: servers.Command{Command: "/bin/true"}, Stop: &servers.Command{Command: "/bin/true"}},
		Readiness: servers.Readiness{Method: servers.ReadyScript, Script: &servers.ScriptCheck{Command: testutil.Script(t, dir, "st.sh", "exit 1")}}})
	b := newCustom(def, e.deps)
	defer b.Close()
	if b.Probe(bg).Life != LifeExternal {
		t.Fatal("without a PID file the backend cannot know; it must say so")
	}
	if b.Caps().ForceKill {
		t.Fatal("nothing to kill without a known PID")
	}
}

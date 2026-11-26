package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

// ---- concurrency limits ----

func (f *fixture) limits(global int, cats ...config.Category) {
	f.t.Helper()
	if err := f.cfg.Update(func(c *config.Config) error {
		c.Limits.MaxConcurrentServers = global
		c.Categories = cats
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

// idler is a server that runs until it reads "stop", ready immediately.
func (f *fixture) idler(id, category string) *servers.Definition {
	return &servers.Definition{ID: id, Name: id, Category: category, Backend: servers.BackendCustom,
		Custom:   &servers.Custom{Start: servers.Command{Command: f.script(id+".sh", `while read line; do [ "$line" = "stop" ] && exit 0; done`)}, StopInput: "stop"},
		Console:  servers.Console{Input: servers.InputStdin},
		Timeouts: servers.Timeouts{Start: dur(10 * time.Second), Stop: dur(5 * time.Second)}}
}

func (f *fixture) run(id string) {
	f.t.Helper()
	if _, err := f.m.StartServer(id, actor); err != nil {
		f.t.Fatalf("start %s: %v", id, err)
	}
	f.waitState(id, 5*time.Second, Running)
}

func (f *fixture) halt(id string) {
	f.t.Helper()
	if _, err := f.m.StopServer(id, actor); err != nil {
		f.t.Fatalf("stop %s: %v", id, err)
	}
	f.waitState(id, 5*time.Second, Stopped)
}

func limitErr(t *testing.T, err error, scope string) *LimitError {
	t.Helper()
	var le *LimitError
	if !errors.As(err, &le) || le.Scope != scope {
		t.Fatalf("expected a %s limit error, got %v", scope, err)
	}
	return le
}

func TestGlobalAndCategoryLimitsApplyTogether(t *testing.T) {
	f := newFixture(t)
	f.limits(4, config.Category{ID: "minecraft", Name: "Minecraft", MaxConcurrent: 2}, config.Category{ID: "source", Name: "Source Engine", MaxConcurrent: 3})
	for _, id := range []string{"mc1", "mc2", "mc3"} {
		f.add(f.idler(id, "minecraft"))
	}
	// Older definitions name the category instead of using its ID.
	for _, id := range []string{"src1", "src2", "src3"} {
		f.add(f.idler(id, "Source Engine"))
	}
	f.add(f.idler("free", ""))

	f.run("mc1")
	f.run("mc2")
	_, err := f.m.StartServer("mc3", actor)
	le := limitErr(t, err, "category")
	if le.CategoryName != "Minecraft" || le.Limit != 2 || le.InUse != 2 || !strings.Contains(le.Error(), "the Minecraft category has reached its limit of 2") {
		t.Fatalf("category limit details: %+v %q", le, le.Error())
	}
	if f.state("mc3") != Stopped {
		t.Fatal("a refused start must not touch the server")
	}
	// Global 4, two in use: a Source server may start (category 0/3).
	f.run("src1")
	f.run("src2")
	le = limitErr(t, func() error { _, err := f.m.StartServer("src3", actor); return err }(), "global")
	if le.Limit != 4 || le.InUse != 4 || !strings.Contains(le.Error(), "global limit of 4") {
		t.Fatalf("global limit details: %+v", le)
	}
	limitErr(t, func() error { _, err := f.m.StartServer("free", actor); return err }(), "global")

	cap := f.m.Capacity()
	if cap.Global.InUse != 4 || cap.Global.Limit != 4 || len(cap.Categories) != 2 {
		t.Fatalf("capacity: %+v", cap)
	}
	for _, c := range cap.Categories {
		if (c.ID == "minecraft" && (c.InUse != 2 || c.Servers != 3)) || (c.ID == "source" && (c.InUse != 2 || c.Limit != 3)) {
			t.Fatalf("category usage: %+v", c)
		}
	}
	// Stopping one frees exactly one slot.
	f.halt("mc1")
	f.run("free")
	limitErr(t, func() error { _, err := f.m.StartServer("mc3", actor); return err }(), "global")
	// Unlimited again.
	f.limits(0)
	f.run("mc3")
	f.run("src3")
	if got := f.m.Capacity().Global.InUse; got != 6 {
		t.Fatalf("in use without limits: %d", got)
	}
}

func TestConcurrentStartsCannotTakeTheSameSlot(t *testing.T) {
	f := newFixture(t)
	f.limits(2)
	ids := []string{"a", "b", "c", "d", "e"}
	for _, id := range ids {
		f.add(f.idler(id, ""))
	}
	f.run("a") // one slot left
	for round := 0; round < 3; round++ {
		var wg sync.WaitGroup
		var mu sync.Mutex
		var started []string
		for _, id := range ids[1:] {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				_, err := f.m.StartServer(id, actor)
				if err == nil {
					mu.Lock()
					started = append(started, id)
					mu.Unlock()
					return
				}
				var le *LimitError
				if !errors.As(err, &le) {
					t.Errorf("start %s: unexpected error %v", id, err)
				}
			}(id)
		}
		wg.Wait()
		if len(started) != 1 {
			t.Fatalf("round %d: %d servers took the last slot (%v)", round, len(started), started)
		}
		f.waitState(started[0], 5*time.Second, Running)
		if got := f.m.Capacity().Global.InUse; got != 2 {
			t.Fatalf("round %d: %d in use", round, got)
		}
		f.halt(started[0])
	}
}

func TestStartingAndStoppingServersKeepTheirSlot(t *testing.T) {
	f := newFixture(t)
	f.limits(1)
	release := filepath.Join(f.dir, "release")
	// Ready only once "release" exists.
	slow := f.consoleServer("slowstart")
	slow.Custom.Start = servers.Command{Command: f.script("slowstart.sh", "while [ ! -f "+release+" ]; do sleep 0.05; done\necho 'Done (1s)!'\nwhile read line; do [ \"$line\" = stop ] && exit 0; done")}
	f.add(slow)
	// Takes a while to shut down after SIGTERM.
	f.add(&servers.Definition{ID: "slowstop", Name: "slowstop", Backend: servers.BackendCustom,
		Custom:   &servers.Custom{Start: servers.Command{Command: f.script("slowstop.sh", "trap 'sleep 1; exit 0' TERM\nwhile true; do sleep 0.05; done")}},
		Timeouts: servers.Timeouts{Start: dur(10 * time.Second), Stop: dur(10 * time.Second)}})
	f.add(f.idler("other", ""))

	if _, err := f.m.StartServer("slowstart", actor); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "STARTING", func() bool { return f.state("slowstart") == Starting })
	limitErr(t, func() error { _, err := f.m.StartServer("other", actor); return err }(), "global")
	os.WriteFile(release, nil, 0o644)
	f.waitState("slowstart", 5*time.Second, Running)
	f.halt("slowstart")

	f.run("slowstop")
	if _, err := f.m.StopServer("slowstop", actor); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "STOPPING", func() bool { return f.state("slowstop") == Stopping })
	limitErr(t, func() error { _, err := f.m.StartServer("other", actor); return err }(), "global")
	f.waitState("slowstop", 10*time.Second, Stopped)
	f.run("other")
}

func TestExternalStartsCountAndUnknownStateKeepsTheSlot(t *testing.T) {
	f := newFixture(t)
	f.limits(1)
	state := filepath.Join(f.dir, "state")
	status := f.script("status.sh", `s=$(cat `+state+` 2>/dev/null)
[ "$s" = up ] && exit 0
[ "$s" = broken ] && exit 2
exit 1`)
	f.add(&servers.Definition{ID: "ext", Name: "ext", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Mode: servers.ModeForking,
			Start: servers.Command{Command: f.script("start.sh", "echo up > "+state)},
			Stop:  &servers.Command{Command: f.script("stop.sh", "echo down > "+state)}},
		Readiness: servers.Readiness{Method: servers.ReadyScript, Interval: dur(200 * time.Millisecond), Script: &servers.ScriptCheck{Command: status}},
		Timeouts:  servers.Timeouts{Start: dur(5 * time.Second), Stop: dur(5 * time.Second)}})
	f.add(f.idler("other", ""))

	// Started behind the manager's back: it occupies the slot.
	os.WriteFile(state, []byte("up"), 0o644)
	f.waitState("ext", 5*time.Second, Running)
	limitErr(t, func() error { _, err := f.m.StartServer("other", actor); return err }(), "global")
	// Monitoring becomes inconclusive: the slot is NOT released.
	os.WriteFile(state, []byte("broken"), 0o644)
	f.waitState("ext", 5*time.Second, Unknown)
	if got := f.m.Capacity().Global.InUse; got != 1 {
		t.Fatalf("an UNKNOWN server that was running must keep its slot, in use = %d", got)
	}
	limitErr(t, func() error { _, err := f.m.StartServer("other", actor); return err }(), "global")
	// Confirmed stopped: released.
	os.WriteFile(state, []byte("down"), 0o644)
	f.waitState("ext", 5*time.Second, Stopped)
	f.run("other")
}

func TestLimitsAreCorrectRightAfterARestart(t *testing.T) {
	f := newFixture(t)
	f.limits(1)
	f.add(f.consoleServer("keep"))
	f.add(f.idler("other", ""))
	f.run("keep")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	f.m.Shutdown(ctx)
	cancel()

	// A new manager: before it has even looked at "keep" again, a start of
	// "other" must already respect the slot "keep" still holds.
	f.boot()
	limitErr(t, func() error { _, err := f.m.StartServer("other", actor); return err }(), "global")
	f.waitState("keep", 5*time.Second, Running)
	f.halt("keep")
	f.run("other")
}

func TestAdministratorOverrideIsAudited(t *testing.T) {
	f := newFixture(t)
	f.limits(0, config.Category{ID: "mc", Name: "Minecraft", MaxConcurrent: 1})
	f.add(f.idler("a", "mc"))
	f.add(f.idler("b", "mc"))
	f.run("a")
	limitErr(t, func() error { _, err := f.m.StartServer("b", actor); return err }(), "category")
	if _, err := f.m.StartServerWith("b", actor, StartOptions{OverrideLimits: true}); err != nil {
		t.Fatalf("override: %v", err)
	}
	f.waitState("b", 5*time.Second, Running)
	entries, _ := f.store.QueryAudit(context.Background(), db.AuditFilter{Action: "LIMIT_OVERRIDE"})
	if len(entries) != 1 || entries[0].ServerID != "b" || entries[0].Username != "tester" || entries[0].Details["scope"] != "category" {
		t.Fatalf("override audit: %+v", entries)
	}
	if got := f.m.Capacity().Categories[0].InUse; got != 2 {
		t.Fatalf("an overridden start still counts: %d", got)
	}
}

// ---- transitions (notifications) ----

type transitionLog struct {
	mu   sync.Mutex
	list []Transition
}

func (l *transitionLog) add(t Transition) {
	l.mu.Lock()
	l.list = append(l.list, t)
	l.mu.Unlock()
}

func (l *transitionLog) of(server string) []Transition {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Transition
	for _, t := range l.list {
		if t.ServerID == server {
			out = append(out, t)
		}
	}
	return out
}

func (l *transitionLog) kinds(server string) string {
	var k []string
	for _, t := range l.of(server) {
		s := t.Kind
		if t.Kind == db.NotifyCrash && t.Confirmed {
			s += "!"
		}
		k = append(k, s)
	}
	return strings.Join(k, ",")
}

func TestTransitionsClassifyWhatHappened(t *testing.T) {
	log := &transitionLog{}
	f := newFixtureHook(t, log.add)

	// Normal start and stop through the manager.
	f.add(f.consoleServer("n"))
	f.run("n")
	f.halt("n")
	if got := log.kinds("n"); got != "ready,stopped" {
		t.Fatalf("normal lifecycle: %q", got)
	}
	tr := log.of("n")
	if tr[0].Operation != OpStart || tr[0].Actor != "tester" || tr[0].To != Running || tr[1].Operation != OpStop || tr[1].From != Stopping {
		t.Fatalf("transition details: %+v", tr)
	}

	// Ready is reported only after the readiness check passed.
	release := filepath.Join(f.dir, "go")
	slow := f.consoleServer("slow")
	slow.Custom.Start = servers.Command{Command: f.script("slow.sh", "while [ ! -f "+release+" ]; do sleep 0.05; done\necho 'Done (x)!'\nwhile read line; do [ \"$line\" = stop ] && exit 0; done")}
	f.add(slow)
	f.m.StartServer("slow", actor)
	time.Sleep(600 * time.Millisecond)
	if got := log.kinds("slow"); got != "" {
		t.Fatalf("notified before readiness: %q", got)
	}
	os.WriteFile(release, nil, 0o644)
	f.waitState("slow", 5*time.Second, Running)
	if got := log.kinds("slow"); got != "ready" {
		t.Fatalf("after readiness: %q", got)
	}

	// The server exits on its own (code 0): an unexpected stop, not a
	// confirmed crash, and no "stopped" notification on top.
	if err := f.m.SendCommand(context.Background(), "slow", "stop"); err != nil {
		t.Fatal(err)
	}
	f.waitState("slow", 5*time.Second, Stopped)
	time.Sleep(2500 * time.Millisecond) // several polls: still exactly one event
	if got := log.kinds("slow"); got != "ready,crash" {
		t.Fatalf("unexpected stop: %q", got)
	}

	// A real crash (non-zero exit) is confirmed.
	trigger := filepath.Join(f.dir, "die")
	f.add(&servers.Definition{ID: "flaky", Name: "flaky", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Start: servers.Command{Command: f.script("flaky.sh", "while [ ! -f "+trigger+" ]; do sleep 0.05; done\nexit 9")}}})
	f.run("flaky")
	os.WriteFile(trigger, nil, 0o644)
	f.waitState("flaky", 5*time.Second, Error)
	if got := log.kinds("flaky"); got != "ready,crash!" {
		t.Fatalf("crash: %q", got)
	}
	if d := log.of("flaky")[1].Detail; !strings.Contains(d, "code 9") {
		t.Fatalf("crash detail %q", d)
	}

	// A failed start is a failure, not a crash.
	f.add(&servers.Definition{ID: "broken", Name: "broken", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Start: servers.Command{Command: "/nonexistent/start.sh"}}})
	f.m.StartServer("broken", actor)
	f.waitState("broken", 5*time.Second, Error)
	time.Sleep(1500 * time.Millisecond)
	if got := log.kinds("broken"); got != "failure" {
		t.Fatalf("failed start: %q", got)
	}

	// Reconciliation after a restart produces no notifications.
	f.add(f.consoleServer("keep"))
	f.run("keep")
	before := len(log.of("keep"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	f.m.Shutdown(ctx)
	cancel()
	f.boot()
	f.waitState("keep", 5*time.Second, Running)
	time.Sleep(1500 * time.Millisecond)
	if after := len(log.of("keep")); after != before {
		t.Fatalf("re-adopting a running server must not notify: %s", log.kinds("keep"))
	}
	f.halt("keep")
}

// ---- tmux backups and actions ----

func tmuxFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	f := newFixture(t)
	socket := fmt.Sprintf("smmgr%d%d", os.Getpid(), time.Now().UnixNano()%100000)
	t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
	old := backupWatchInterval
	backupWatchInterval = 100 * time.Millisecond
	t.Cleanup(func() { backupWatchInterval = old })
	return f, socket
}

// gameServer is a tmux "game" that makes a backup when told "/backup start".
func (f *fixture) gameServer(id, socket, bdir string) *servers.Definition {
	script := f.script(id+".sh", `n=0
while read line; do
  echo "got: $line"
  if [ "$line" = "/backup start" ]; then
    n=$((n+1)); sleep 0.3; echo "data $n" > "`+bdir+`/world-$n.tar"; echo "Backup complete"
  fi
  [ "$line" = stop ] && exit 0
done`)
	return &servers.Definition{ID: id, Name: id, Backend: servers.BackendTmux,
		Tmux:     &servers.Tmux{Session: "sm-" + id, SocketName: socket, Start: &servers.Command{Command: script}, StopText: "stop"},
		Backup:   &servers.Backup{Directory: bdir, Method: servers.MethodTmux, Tmux: &servers.TmuxText{Text: "/backup start"}},
		Timeouts: servers.Timeouts{Start: dur(10 * time.Second), Stop: dur(5 * time.Second), Backup: dur(5 * time.Second)}}
}

func (f *fixture) lastOp(id string) *db.Operation {
	ops, _ := f.m.Operations(context.Background(), id, 1)
	if len(ops) == 0 {
		return nil
	}
	return ops[0]
}

func TestTmuxBackupDistinguishesSubmissionFromCompletion(t *testing.T) {
	f, socket := tmuxFixture(t)
	bdir := filepath.Join(f.dir, "backups")
	os.MkdirAll(bdir, 0o755)
	def := f.gameServer("game", socket, bdir)
	f.add(def)

	// No session yet: refused at once, naming the session.
	_, err := f.m.Backup("game", actor)
	if !errors.Is(err, ErrNotRunning) || !strings.Contains(err.Error(), `"sm-game"`) {
		t.Fatalf("backup without a session: %v", err)
	}
	f.run("game")
	if caps, _ := f.m.Capabilities("game"); !caps.BackupCreate || caps.BackupMethod != servers.MethodTmux {
		t.Fatalf("capabilities: %+v", caps)
	}

	// Without a completion pattern the outcome is "submitted", and the
	// directory watcher reports the new file when it appears.
	changes := f.rec.count("server.backups.changed", "game")
	if _, err := f.m.Backup("game", actor); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "submitted backup", func() bool { op := f.lastOp("game"); return op != nil && op.Status == "submitted" })
	if op := f.lastOp("game"); !strings.Contains(op.Message, "cannot be verified") {
		t.Fatalf("submitted message: %q", op.Message)
	}
	testutil.Eventually(t, 5*time.Second, "backup file", func() bool { _, err := os.Stat(filepath.Join(bdir, "world-1.tar")); return err == nil })
	// One event when the operation ends, at least one more from the watcher.
	testutil.Eventually(t, 5*time.Second, "file list refresh", func() bool { return f.rec.count("server.backups.changed", "game") >= changes+2 })
	snap, _ := f.m.ConsoleSnapshot(context.Background(), "game", 50)
	if !strings.Contains(strings.Join(snap.Lines, "\n"), "got: /backup start") {
		t.Fatalf("the command must arrive in the console: %q", snap.Lines)
	}

	// With a completion pattern the operation waits for the confirmation.
	def, _ = f.m.Definition("game")
	def.Backup.Completion = &servers.ConsoleMatch{Pattern: "Backup complete"}
	if _, err := f.m.Update(def); err != nil {
		t.Fatalf("backup settings can change while running: %v", err)
	}
	f.m.Backup("game", actor)
	testutil.Eventually(t, 5*time.Second, "confirmed backup", func() bool { op := f.lastOp("game"); return op != nil && op.Status == "succeeded" })

	// A confirmation that never comes is a failure, after the backup timeout.
	def.Backup.Completion = &servers.ConsoleMatch{Pattern: "this never appears"}
	def.Timeouts.Backup = dur(time.Second)
	f.m.Update(def)
	f.m.Backup("game", actor)
	testutil.Eventually(t, 5*time.Second, "unconfirmed backup", func() bool { op := f.lastOp("game"); return op != nil && op.Status == "failed" })
	if op := f.lastOp("game"); !strings.Contains(op.Message, "was sent, but the completion message did not appear") {
		t.Fatalf("failure message: %q", op.Message)
	}
	f.halt("game")
}

func TestCustomActions(t *testing.T) {
	f, socket := tmuxFixture(t)
	bdir := filepath.Join(f.dir, "b")
	os.MkdirAll(bdir, 0o755)
	def := f.gameServer("mc", socket, bdir)
	marker := filepath.Join(f.dir, "ran")
	def.Actions = []servers.Action{
		{ID: "save", Name: "Save World", Method: servers.MethodTmux, Tmux: &servers.TmuxText{Text: "save-all flush"}},
		{ID: "echo", Name: "Echo", Command: &servers.Command{Command: f.script("echo.sh", `printf '%s|' "$@"; echo "id=$SM_ACTION_ID user=$SM_ACTION_USER"; touch `+marker)}},
		{ID: "fail", Name: "Fail", Command: &servers.Command{Command: f.script("fail.sh", "echo broken >&2; exit 3")}},
		{ID: "slow", Name: "Slow", Command: &servers.Command{Command: f.script("slow.sh", "sleep 30")}, Timeout: dur(time.Second)},
		{ID: "danger", Name: "Wipe", Confirm: true, Command: &servers.Command{Command: "/bin/true"}},
		{ID: "off", Name: "Off", Disabled: true, Command: &servers.Command{Command: "/bin/true"}},
	}
	// Arguments are fixed by the administrator and passed without a shell.
	def.Actions[1].Command.Args = []string{"$(id)", "; rm -rf /", "a b"}
	f.add(def)
	completed := func(action string) int {
		n := 0
		f.rec.mu.Lock()
		defer f.rec.mu.Unlock()
		for _, e := range f.rec.events {
			if e.Type == "server.action.completed" && e.ActionID == action {
				n++
			}
		}
		return n
	}
	lastRun := func(action string) *db.Operation {
		runs, _ := f.m.LastActionRuns(context.Background(), "mc")
		return runs[action]
	}

	// Script action: runs while the server is stopped too.
	if _, err := f.m.RunAction("mc", "echo", actor, false); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "echo action", func() bool { return completed("echo") == 1 })
	r := lastRun("echo")
	if r.Status != "succeeded" || r.Output != "$(id)|; rm -rf /|a b|id=echo user=tester" || r.Username != "tester" {
		t.Fatalf("script action run: %+v", r)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the action did not run")
	}
	f.m.RunAction("mc", "fail", actor, false)
	testutil.Eventually(t, 5*time.Second, "failing action", func() bool { return completed("fail") == 1 })
	if r := lastRun("fail"); r.Status != "failed" || !strings.Contains(r.Message, "code 3") || !strings.Contains(r.Output, "broken") {
		t.Fatalf("failed action: %+v", r)
	}

	// One run at a time per action; the timeout kills it.
	if _, err := f.m.RunAction("mc", "slow", actor, false); err != nil {
		t.Fatal(err)
	}
	var busy *ActionBusyError
	if _, err := f.m.RunAction("mc", "slow", actor, false); !errors.As(err, &busy) || busy.Run.User != "tester" {
		t.Fatalf("duplicate run: %v", err)
	}
	if running := f.m.RunningActions("mc"); running["slow"].ActionID != "slow" {
		t.Fatalf("running actions: %+v", running)
	}
	testutil.Eventually(t, 5*time.Second, "timed out action", func() bool { return completed("slow") == 1 })
	if r := lastRun("slow"); r.Status != "failed" || !strings.Contains(r.Message, "timed out after 1s") {
		t.Fatalf("timeout: %+v", r)
	}

	// Confirmation, disabled and unknown actions.
	if _, err := f.m.RunAction("mc", "danger", actor, false); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("unconfirmed: %v", err)
	}
	if _, err := f.m.RunAction("mc", "danger", actor, true); err != nil {
		t.Fatalf("confirmed: %v", err)
	}
	if _, err := f.m.RunAction("mc", "off", actor, false); !errors.Is(err, ErrActionDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	if _, err := f.m.RunAction("mc", "nope", actor, false); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("unknown: %v", err)
	}

	// tmux action: refused without a session, "submitted" with one.
	if _, err := f.m.RunAction("mc", "save", actor, false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("tmux action without a session: %v", err)
	}
	f.run("mc")
	if _, err := f.m.RunAction("mc", "save", actor, false); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "tmux action", func() bool { return completed("save") == 1 })
	if r := lastRun("save"); r.Status != "submitted" {
		t.Fatalf("tmux action: %+v", r)
	}
	testutil.Eventually(t, 5*time.Second, "text in the console", func() bool {
		snap, _ := f.m.ConsoleSnapshot(context.Background(), "mc", 50)
		return strings.Contains(strings.Join(snap.Lines, "\n"), "got: save-all flush")
	})

	// Actions can be edited while the server runs; grants of removed
	// actions disappear.
	u := &db.User{Username: "granted", DisplayName: "granted", PasswordHash: "x"}
	if err := f.store.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"fail", "save"} {
		if err := f.store.SetActionPermission(context.Background(), u.ID, "mc", a, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.m.SaveAction("mc", servers.Action{ID: "new", Name: "New", Command: &servers.Command{Command: "/bin/true"}}, true); err != nil {
		t.Fatalf("add action while running: %v", err)
	}
	if _, err := f.m.SaveAction("mc", servers.Action{ID: "new", Name: "Dup", Command: &servers.Command{Command: "/bin/true"}}, true); !errors.Is(err, ErrActionExists) {
		t.Fatalf("duplicate id: %v", err)
	}
	if _, err := f.m.DeleteAction("mc", "fail"); err != nil {
		t.Fatal(err)
	}
	grants, _ := f.store.ActionPermissionsForUser(context.Background(), u.ID)
	if grants["mc"]["fail"] || !grants["mc"]["save"] {
		t.Fatalf("grants after delete: %v", grants)
	}
	if f.state("mc") != Running {
		t.Fatal("editing actions must not disturb the running server")
	}
	f.halt("mc")
}

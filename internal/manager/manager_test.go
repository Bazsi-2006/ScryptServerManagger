package manager

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/events"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/logging"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

// recorder captures published events.
type recorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recorder) Publish(e events.Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// states returns the sequence of states published for a server.
func (r *recorder) states(id string) []State {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []State
	for _, e := range r.events {
		if e.Type == "server.status.changed" && e.ServerID == id {
			st := e.Data.(Status).State
			if len(out) == 0 || out[len(out)-1] != st {
				out = append(out, st)
			}
		}
	}
	return out
}

func (r *recorder) count(typ, id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Type == typ && e.ServerID == id {
			n++
		}
	}
	return n
}

type fixture struct {
	t     *testing.T
	cfg   *config.Holder
	store *db.Store
	defs  *servers.Store
	rec   *recorder
	m     *Manager
	dir   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, cfg: testutil.Config(t), rec: &recorder{}, dir: t.TempDir()}
	var err error
	f.store, err = db.Open(f.cfg.Get().Paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	f.defs = servers.NewStore(f.cfg.Get().Paths.ServersDir)
	if err := f.defs.Load(); err != nil {
		t.Fatal(err)
	}
	f.boot()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		f.m.Shutdown(ctx)
		f.store.Close()
	})
	return f
}

func (f *fixture) boot() {
	f.m = New(f.cfg, f.defs, f.store, f.rec, audit.NewRecorder(f.store, logging.Discard()), logging.Discard())
	f.m.Load()
}

func (f *fixture) script(name, body string) string { return testutil.Script(f.t, f.dir, name, body) }

func (f *fixture) add(def *servers.Definition) *servers.Definition {
	f.t.Helper()
	saved, err := f.m.Create(def)
	if err != nil {
		f.t.Fatalf("create %s: %v", def.ID, err)
	}
	f.waitState(saved.ID, 5*time.Second, Stopped)
	return saved
}

func (f *fixture) state(id string) State {
	st, err := f.m.Status(id)
	if err != nil {
		f.t.Fatalf("status %s: %v", id, err)
	}
	return st.State
}

func (f *fixture) waitState(id string, timeout time.Duration, want State) Status {
	f.t.Helper()
	var last Status
	testutil.Eventually(f.t, timeout, id+" to reach "+string(want), func() bool {
		last, _ = f.m.Status(id)
		return last.State == want && last.Operation == nil
	})
	return last
}

var actor = audit.Actor{UserID: 1, Username: "tester"}

func dur(d time.Duration) servers.Duration { return servers.Duration(d) }

// A server that takes a moment to become ready and then serves stdin.
const slowServer = `echo "booting"
sleep 0.6
echo 'Done (0.6s)! For help, type "help"'
while read line; do echo "got: $line"; [ "$line" = "stop" ] && exit 0; done`

func (f *fixture) consoleServer(id string) *servers.Definition {
	return &servers.Definition{ID: id, Name: id, Backend: servers.BackendCustom,
		Custom:    &servers.Custom{Start: servers.Command{Command: f.script(id+".sh", slowServer)}, StopInput: "stop"},
		Console:   servers.Console{Input: servers.InputStdin},
		Readiness: servers.Readiness{Method: servers.ReadyConsole, Console: &servers.ConsoleMatch{Pattern: `Done \(`, Regex: true}, Interval: dur(200 * time.Millisecond)},
		Timeouts:  servers.Timeouts{Start: dur(10 * time.Second), Stop: dur(5 * time.Second)}}
}

func TestLifecycleDistinguishesStartingFromRunning(t *testing.T) {
	f := newFixture(t)
	f.add(f.consoleServer("mc"))

	op, err := f.m.StartServer("mc", actor)
	if err != nil || op.Type != OpStart || op.ID == 0 {
		t.Fatalf("start: %v %+v", err, op)
	}
	// The process is alive but has not printed the readiness line yet.
	testutil.Eventually(t, 2*time.Second, "STARTING", func() bool { return f.state("mc") == Starting })
	time.Sleep(200 * time.Millisecond)
	if st := f.state("mc"); st != Starting {
		t.Fatalf("state while booting = %s, want STARTING (a live process is not a ready server)", st)
	}
	f.waitState("mc", 5*time.Second, Running)

	if _, err := f.m.StopServer("mc", actor); err != nil {
		t.Fatal(err)
	}
	f.waitState("mc", 5*time.Second, Stopped)

	got := f.rec.states("mc")
	want := []State{Stopped, Starting, Running, Stopping, Stopped}
	if len(got) < len(want) || !equalStates(got[len(got)-len(want):], want) {
		t.Fatalf("published states %v, want suffix %v", got, want)
	}
	testutil.Eventually(t, 3*time.Second, "started and completed events for both operations", func() bool {
		return f.rec.count("server.operation.started", "mc") == 2 && f.rec.count("server.operation.completed", "mc") == 2
	})
	ops, _ := f.m.Operations(context.Background(), "mc", 10)
	if len(ops) != 2 || ops[0].Type != "stop" || ops[0].Status != "succeeded" || ops[1].Status != "succeeded" || ops[1].Username != "tester" {
		t.Fatalf("operation history %+v", ops)
	}
	entries, _ := f.store.QueryAudit(context.Background(), db.AuditFilter{ServerID: "mc", Action: audit.ServerStart})
	if len(entries) != 2 || entries[0].Result != audit.Success || entries[1].Result != audit.Requested {
		t.Fatalf("audit trail for start: %+v", entries)
	}
}

func equalStates(a, b []State) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestConsoleReadinessIgnoresOutputFromEarlierRuns(t *testing.T) {
	f := newFixture(t)
	// Prints the readiness line only on its first run.
	marker := filepath.Join(f.dir, "ran-before")
	def := f.consoleServer("once")
	def.Custom.Start.Command = f.script("once.sh", `if [ ! -f `+marker+` ]; then touch `+marker+`; echo 'Done (first run)'; fi
echo "alive"
while read line; do [ "$line" = "stop" ] && exit 0; done`)
	def.Timeouts.Start = dur(1500 * time.Millisecond)
	f.add(def)

	f.m.StartServer("once", actor)
	f.waitState("once", 5*time.Second, Running)
	f.m.StopServer("once", actor)
	f.waitState("once", 5*time.Second, Stopped)

	// Second run: "Done (" from the first run is still in the console
	// buffer, but it must not satisfy this startup attempt.
	if _, err := f.m.StartServer("once", actor); err != nil {
		t.Fatal(err)
	}
	st := f.waitState("once", 6*time.Second, Error)
	if !strings.Contains(st.Message, "timed out") || !strings.Contains(st.Message, "readiness text") {
		t.Fatalf("expected a readiness timeout, got %q", st.Message)
	}
	// The process is still alive after the timeout: it can be stopped.
	if _, err := f.m.StopServer("once", actor); err != nil {
		t.Fatalf("a server in ERROR with a live process must be stoppable: %v", err)
	}
	f.waitState("once", 5*time.Second, Stopped)
}

func TestStartFailuresBecomeErrorsWithMessages(t *testing.T) {
	f := newFixture(t)
	f.add(&servers.Definition{ID: "gone", Name: "gone", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Start: servers.Command{Command: "/nonexistent/start.sh"}}})
	f.add(&servers.Definition{ID: "crash", Name: "crash", Backend: servers.BackendCustom,
		Custom:    &servers.Custom{Start: servers.Command{Command: f.script("crash.sh", "echo fatal: bad config; exit 2")}},
		Readiness: servers.Readiness{Method: servers.ReadyConsole, Console: &servers.ConsoleMatch{Pattern: "never"}, Interval: dur(200 * time.Millisecond)}})

	f.m.StartServer("gone", actor)
	st := f.waitState("gone", 5*time.Second, Error)
	if !strings.Contains(st.Message, "was not found") {
		t.Fatalf("missing executable message: %q", st.Message)
	}
	f.m.StartServer("crash", actor)
	st = f.waitState("crash", 5*time.Second, Error)
	if !strings.Contains(st.Message, "stopped during startup") || !strings.Contains(st.Message, "code 2") {
		t.Fatalf("crash message: %q", st.Message)
	}
	testutil.Eventually(t, 3*time.Second, "server.operation.failed event for the crashed start", func() bool {
		return f.rec.count("server.operation.failed", "crash") == 1
	})
	// An unrelated server is unaffected and the failed one can be retried.
	f.add(f.consoleServer("ok"))
	f.m.StartServer("ok", actor)
	f.waitState("ok", 5*time.Second, Running)
	if _, err := f.m.StartServer("crash", actor); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	f.waitState("crash", 5*time.Second, Error)
}

func TestPortReadiness(t *testing.T) {
	f := newFixture(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	def := &servers.Definition{ID: "port", Name: "port", Backend: servers.BackendCustom,
		Custom:    &servers.Custom{Start: servers.Command{Command: f.script("port.sh", "trap 'exit 0' TERM\nwhile :; do sleep 0.1; done")}},
		Readiness: servers.Readiness{Method: servers.ReadyPort, Port: &servers.PortCheck{Host: "127.0.0.1", Port: port, Protocol: "tcp"}, Interval: dur(200 * time.Millisecond)},
		Timeouts:  servers.Timeouts{Start: dur(10 * time.Second), Stop: dur(5 * time.Second)}}
	f.add(def)
	f.m.StartServer("port", actor)
	testutil.Eventually(t, 2*time.Second, "STARTING", func() bool { return f.state("port") == Starting })
	time.Sleep(500 * time.Millisecond)
	if f.state("port") != Starting {
		t.Fatal("must stay STARTING while the port is closed")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Skipf("could not re-bind the test port: %v", err)
	}
	defer ln.Close()
	f.waitState("port", 5*time.Second, Running)
	f.m.StopServer("port", actor)
	f.waitState("port", 5*time.Second, Stopped)
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestScriptStatusDrivesForkingServer(t *testing.T) {
	f := newFixture(t)
	flag := filepath.Join(f.dir, "up")
	ready := filepath.Join(f.dir, "ready")
	// A "daemon" represented by flag files; no PID is known to the manager.
	def := &servers.Definition{ID: "fork", Name: "fork", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Mode: servers.ModeForking,
			Start: servers.Command{Command: f.script("start.sh", "touch "+flag+"\n(sleep 0.7; touch "+ready+") >/dev/null 2>&1 &")},
			Stop:  &servers.Command{Command: f.script("stop.sh", "rm -f "+flag+" "+ready)}},
		Readiness: servers.Readiness{Method: servers.ReadyScript, Interval: dur(200 * time.Millisecond),
			Script: &servers.ScriptCheck{Command: f.script("status.sh", "[ -f "+ready+" ] && exit 0\n[ -f "+flag+" ] && exit 1\nexit 1")}},
		Timeouts: servers.Timeouts{Start: dur(10 * time.Second), Stop: dur(5 * time.Second)}}
	f.add(def)
	f.m.StartServer("fork", actor)
	// The start script exits at once; the server must NOT be considered
	// stopped just because of that, nor running before the check passes.
	time.Sleep(350 * time.Millisecond)
	if st := f.state("fork"); st != Starting {
		t.Fatalf("state after the start script exited = %s, want STARTING", st)
	}
	f.waitState("fork", 5*time.Second, Running)
	if _, err := f.m.StartServer("fork", actor); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second start: %v", err)
	}
	f.m.StopServer("fork", actor)
	f.waitState("fork", 5*time.Second, Stopped)

	// Started behind the manager's back: monitoring notices on its own.
	os.WriteFile(flag, nil, 0o644)
	os.WriteFile(ready, nil, 0o644)
	f.waitState("fork", 5*time.Second, Running)
	os.Remove(flag)
	os.Remove(ready)
	st := f.waitState("fork", 5*time.Second, Stopped)
	if !strings.Contains(st.Message, "outside of the manager") {
		t.Fatalf("message %q", st.Message)
	}
}

func TestStatusScriptErrorMeansUnknown(t *testing.T) {
	f := newFixture(t)
	code := filepath.Join(f.dir, "code")
	os.WriteFile(code, []byte("1"), 0o644)
	def := &servers.Definition{ID: "unk", Name: "unk", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Mode: servers.ModeForking, Start: servers.Command{Command: "/bin/true"}, Stop: &servers.Command{Command: "/bin/true"}},
		Readiness: servers.Readiness{Method: servers.ReadyScript, Interval: dur(200 * time.Millisecond),
			Script: &servers.ScriptCheck{Command: f.script("st.sh", "exit $(cat "+code+")")}}}
	f.add(def)
	os.WriteFile(code, []byte("3"), 0o644)
	st := f.waitState("unk", 5*time.Second, Unknown)
	if !strings.Contains(st.Message, "code 3") {
		t.Fatalf("unknown-state message %q", st.Message)
	}
	os.WriteFile(code, []byte("0"), 0o644)
	f.waitState("unk", 5*time.Second, Running)
}

func TestConflictingOperationsAreRejected(t *testing.T) {
	f := newFixture(t)
	f.add(f.consoleServer("lock"))

	// Many simultaneous starts: exactly one is accepted.
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted, busy, running := 0, 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.m.StartServer("lock", actor)
			var be *BusyError
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.As(err, &be):
				busy++
			case errors.Is(err, ErrAlreadyRunning):
				running++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if accepted != 1 || busy+running != 19 {
		t.Fatalf("accepted=%d busy=%d alreadyRunning=%d; want exactly one accepted", accepted, busy, running)
	}
	f.waitState("lock", 5*time.Second, Running)
	if f.rec.count("server.operation.started", "lock") != 1 {
		t.Fatal("only one start operation may have been launched")
	}
	// Exactly one process exists.
	lines, _ := f.m.ConsoleSnapshot(context.Background(), "lock", 100)
	if n := strings.Count(strings.Join(lines.Lines, "\n"), "booting"); n != 1 {
		t.Fatalf("the server was started %d times", n)
	}

	if _, err := f.m.StartServer("lock", actor); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("start while running: %v", err)
	}
	// Concurrent stops: one accepted, the rest see the operation in progress
	// (or the already stopped server).
	accepted = 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.m.StopServer("lock", actor); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("%d stop operations accepted, want 1", accepted)
	}
	f.waitState("lock", 5*time.Second, Stopped)
	if _, err := f.m.StopServer("lock", actor); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stop while stopped: %v", err)
	}
	if _, err := f.m.StartServer("nope", actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown server: %v", err)
	}
}

func TestStopPreemptsAStuckStart(t *testing.T) {
	f := newFixture(t)
	def := f.consoleServer("stuck")
	def.Custom.Start.Command = f.script("stuck.sh", `echo "never ready"
while read line; do [ "$line" = "stop" ] && exit 0; done`)
	def.Timeouts.Start = dur(time.Minute)
	f.add(def)
	f.m.StartServer("stuck", actor)
	testutil.Eventually(t, 2*time.Second, "STARTING", func() bool { return f.state("stuck") == Starting })
	// A second start is refused, but a stop takes over.
	if _, err := f.m.StartServer("stuck", actor); err == nil {
		t.Fatal("start during start must be refused")
	}
	begin := time.Now()
	if _, err := f.m.StopServer("stuck", actor); err != nil {
		t.Fatalf("stop during a start must be possible: %v", err)
	}
	f.waitState("stuck", 5*time.Second, Stopped)
	if time.Since(begin) > 10*time.Second {
		t.Fatal("stop should not wait for the start timeout")
	}
	ops, _ := f.m.Operations(context.Background(), "stuck", 10)
	if len(ops) != 2 || ops[1].Type != "start" || ops[1].Status != "cancelled" || ops[0].Status != "succeeded" {
		t.Fatalf("history: %+v %+v", ops[0], ops[1])
	}
}

func TestHangingStopIsKilledAfterTimeout(t *testing.T) {
	f := newFixture(t)
	def := &servers.Definition{ID: "hang", Name: "hang", Backend: servers.BackendCustom,
		Custom:   &servers.Custom{Start: servers.Command{Command: f.script("hang.sh", "trap '' TERM\necho up\nwhile :; do sleep 0.1; done")}},
		Timeouts: servers.Timeouts{Start: dur(5 * time.Second), Stop: dur(time.Second)}}
	f.add(def)
	f.m.StartServer("hang", actor)
	f.waitState("hang", 5*time.Second, Running)
	f.m.StopServer("hang", actor)
	st := f.waitState("hang", 10*time.Second, Stopped)
	if !strings.Contains(st.Message, "was killed") {
		t.Fatalf("message should say the server was killed: %q", st.Message)
	}

	// With kill_on_timeout disabled the stop fails and the truth is shown.
	no := false
	def2 := &servers.Definition{ID: "nokill", Name: "nokill", Backend: servers.BackendCustom,
		Custom:   &servers.Custom{Start: servers.Command{Command: f.script("hang2.sh", "trap '' TERM\nwhile :; do sleep 0.1; done")}, KillOnTimeout: &no},
		Timeouts: servers.Timeouts{Start: dur(5 * time.Second), Stop: dur(time.Second)}}
	f.add(def2)
	f.m.StartServer("nokill", actor)
	f.waitState("nokill", 5*time.Second, Running)
	f.m.StopServer("nokill", actor)
	testutil.Eventually(t, 10*time.Second, "failed stop", func() bool { return f.rec.count("server.operation.failed", "nokill") == 1 })
	// Still alive: monitoring brings the state back to RUNNING.
	f.waitState("nokill", 5*time.Second, Running)
	// A hanging stop *script* is bounded by the same timeout.
	def3 := &servers.Definition{ID: "slowstop", Name: "slowstop", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Start: servers.Command{Command: f.script("s3.sh", "trap 'exit 0' TERM\nwhile :; do sleep 0.1; done")},
			Stop: &servers.Command{Command: f.script("stop3.sh", "sleep 300")}},
		Timeouts: servers.Timeouts{Start: dur(5 * time.Second), Stop: dur(time.Second)}}
	f.add(def3)
	f.m.StartServer("slowstop", actor)
	f.waitState("slowstop", 5*time.Second, Running)
	begin := time.Now()
	f.m.StopServer("slowstop", actor)
	f.waitState("slowstop", 10*time.Second, Stopped)
	if time.Since(begin) > 8*time.Second {
		t.Fatal("a hanging stop script must not block beyond the stop timeout")
	}
}

func TestCrashWhileRunningIsDetectedWithoutViewers(t *testing.T) {
	f := newFixture(t)
	trigger := filepath.Join(f.dir, "die")
	def := &servers.Definition{ID: "flaky", Name: "flaky", Backend: servers.BackendCustom,
		Custom: &servers.Custom{Start: servers.Command{Command: f.script("flaky.sh", "while [ ! -f "+trigger+" ]; do sleep 0.1; done\nexit 9")}}}
	f.add(def)
	f.m.StartServer("flaky", actor)
	f.waitState("flaky", 5*time.Second, Running)
	os.WriteFile(trigger, nil, 0o644)
	st := f.waitState("flaky", 5*time.Second, Error)
	if !strings.Contains(st.Message, "code 9") {
		t.Fatalf("crash message %q", st.Message)
	}
}

func TestRestartAndCooldown(t *testing.T) {
	f := newFixture(t)
	f.add(f.consoleServer("re"))
	f.m.StartServer("re", actor)
	f.waitState("re", 5*time.Second, Running)
	if _, err := f.m.RestartServer("re", actor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.StopServer("re", actor); err == nil {
		t.Fatal("a stop must not interleave with a restart")
	}
	f.waitState("re", 10*time.Second, Running)
	lines, _ := f.m.ConsoleSnapshot(context.Background(), "re", 100)
	if n := strings.Count(strings.Join(lines.Lines, "\n"), "booting"); n != 2 {
		t.Fatalf("expected two boots after a restart, saw %d", n)
	}

	f.cfg.Update(func(c *config.Config) error { c.RateLimits.ServerCooldown = config.Duration(time.Hour); return nil })
	var ce *CooldownError
	if _, err := f.m.StopServer("re", actor); !errors.As(err, &ce) || ce.Remaining <= 0 {
		t.Fatalf("cooldown should block the next operation: %v", err)
	}
	// A per-server override wins over the global setting.
	def, _ := f.m.Definition("re")
	zero := dur(0)
	def.Cooldown = &zero
	if _, err := f.m.Update(def); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.StopServer("re", actor); err != nil {
		t.Fatalf("per-server cooldown override: %v", err)
	}
	f.waitState("re", 5*time.Second, Stopped)
}

func TestSendCommandValidationAndDelivery(t *testing.T) {
	f := newFixture(t)
	f.add(f.consoleServer("cmd"))
	ctx := context.Background()
	if err := f.m.SendCommand(ctx, "cmd", "hello"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("command to a stopped server: %v", err)
	}
	f.m.StartServer("cmd", actor)
	f.waitState("cmd", 5*time.Second, Running)
	var ie *InputError
	for _, bad := range []string{"", "   ", "two\nlines", "ctrl\x03c", "tab\x1b[2J", strings.Repeat("x", 5000), "bad\xffutf8"} {
		if err := f.m.SendCommand(ctx, "cmd", bad); !errors.As(err, &ie) {
			t.Errorf("command %q should be rejected as invalid input, got %v", bad, err)
		}
	}
	if err := f.m.SendCommand(ctx, "cmd", "say hello; rm -rf /"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "command echoed by the server", func() bool {
		snap, _ := f.m.ConsoleSnapshot(ctx, "cmd", 50)
		return strings.Contains(strings.Join(snap.Lines, "\n"), "got: say hello; rm -rf /")
	})
	f.m.StopServer("cmd", actor)
	f.waitState("cmd", 5*time.Second, Stopped)

	// No input channel configured: a clear "unsupported", not silence.
	f.add(&servers.Definition{ID: "mute", Name: "mute", Backend: servers.BackendCustom, Custom: &servers.Custom{Start: servers.Command{Command: "/bin/true"}}})
	if err := f.m.SendCommand(ctx, "mute", "x"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("no input configured: %v", err)
	}
}

func TestHelperCommandInput(t *testing.T) {
	f := newFixture(t)
	got := filepath.Join(f.dir, "received")
	helper := f.script("rcon.sh", `printf '%s\n' "$#" "$1" "$2" > `+got+`; echo "helper says ok"`)
	f.add(&servers.Definition{ID: "rc", Name: "rc", Backend: servers.BackendCustom, Custom: &servers.Custom{Start: servers.Command{Command: "/bin/true"}},
		Console: servers.Console{Input: servers.InputCommand, InputCommand: &servers.Command{Command: helper, Args: []string{"--cmd", "{command}"}}}})
	line := `say "hi"; touch /tmp/x $(id) && echo`
	if err := f.m.SendCommand(context.Background(), "rc", line); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(got)
	if string(data) != "2\n--cmd\n"+line+"\n" {
		t.Fatalf("helper received %q; the line must arrive as exactly one argument", data)
	}
	snap, _ := f.m.ConsoleSnapshot(context.Background(), "rc", 10)
	if !strings.Contains(strings.Join(snap.Lines, "\n"), "helper says ok") {
		t.Fatalf("helper output should appear in the console: %q", snap.Lines)
	}
}

func TestBackupOperation(t *testing.T) {
	f := newFixture(t)
	bdir := filepath.Join(f.dir, "backups")
	os.MkdirAll(bdir, 0o755)
	def := f.consoleServer("bk")
	def.Backup = &servers.Backup{Directory: bdir, Command: &servers.Command{Command: f.script("backup.sh", `sleep 0.4; echo "data for $SM_SERVER_ID" > "$SM_BACKUP_DIR/world.tar"`)}}
	f.add(def)
	if _, err := f.m.Backup("bk", actor); err != nil {
		t.Fatal(err)
	}
	// Other operations wait their turn while the backup runs.
	var be *BusyError
	if _, err := f.m.StartServer("bk", actor); !errors.As(err, &be) || be.Op.Type != OpBackup {
		t.Fatalf("start during backup: %v", err)
	}
	if _, err := f.m.Backup("bk", actor); !errors.As(err, &be) {
		t.Fatalf("second backup: %v", err)
	}
	testutil.Eventually(t, 5*time.Second, "backup completion", func() bool { return f.rec.count("server.backups.changed", "bk") == 1 })
	data, err := os.ReadFile(filepath.Join(bdir, "world.tar"))
	if err != nil || string(data) != "data for bk\n" {
		t.Fatalf("backup script environment: %q %v", data, err)
	}
	if f.state("bk") != Stopped {
		t.Fatal("a backup must not change the server state")
	}

	// Failure and cancellation.
	def2 := f.consoleServer("bk2")
	def2.Backup = &servers.Backup{Directory: bdir, Command: &servers.Command{Command: f.script("b2.sh", "echo disk full >&2; exit 5")}}
	f.add(def2)
	f.m.Backup("bk2", actor)
	testutil.Eventually(t, 5*time.Second, "failed backup", func() bool { return f.rec.count("server.operation.failed", "bk2") == 1 })
	ops, _ := f.m.Operations(context.Background(), "bk2", 5)
	if ops[0].Status != "failed" || !strings.Contains(ops[0].Message, "code 5") || !strings.Contains(ops[0].Message, "disk full") {
		t.Fatalf("failed backup record: %+v", ops[0])
	}
	def3 := f.consoleServer("bk3")
	def3.Backup = &servers.Backup{Directory: bdir, Command: &servers.Command{Command: f.script("b3.sh", "sleep 300")}}
	f.add(def3)
	f.m.Backup("bk3", actor)
	if err := f.m.CancelBackup("bk3"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "cancelled backup", func() bool {
		ops, _ := f.m.Operations(context.Background(), "bk3", 5)
		return len(ops) == 1 && ops[0].Status == "cancelled"
	})
	if err := f.m.CancelBackup("bk3"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("cancel without a running backup: %v", err)
	}
	f.add(&servers.Definition{ID: "nobk", Name: "n", Backend: servers.BackendCustom, Custom: &servers.Custom{Start: servers.Command{Command: "/bin/true"}}})
	if _, err := f.m.Backup("nobk", actor); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("backup without a configured command: %v", err)
	}
}

func TestDefinitionChangesWhileRunning(t *testing.T) {
	f := newFixture(t)
	f.add(f.consoleServer("live"))
	f.m.StartServer("live", actor)
	f.waitState("live", 5*time.Second, Running)

	def, _ := f.m.Definition("live")
	def.Name = "Renamed while running"
	if _, err := f.m.Update(def); err != nil {
		t.Fatalf("safe fields can change at any time: %v", err)
	}
	def.Custom.Start.Args = []string{"--changed"}
	if _, err := f.m.Update(def); !errors.Is(err, ErrActive) {
		t.Fatalf("execution changes on a running server: %v", err)
	}
	if err := f.m.Delete("live", false); !errors.Is(err, ErrActive) {
		t.Fatalf("delete of a running server: %v", err)
	}
	if f.state("live") != Running {
		t.Fatal("the running server must be unaffected")
	}
	f.m.StopServer("live", actor)
	f.waitState("live", 5*time.Second, Stopped)
	if _, err := f.m.Update(def); err != nil {
		t.Fatalf("execution change once stopped: %v", err)
	}
	if err := f.m.Delete("live", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Status("live"); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted server should be gone")
	}
	if _, err := os.Stat(def.Custom.Start.Command); err != nil {
		t.Fatal("deleting a definition must never delete the server's own files")
	}
}

func TestStateIsReconciledAfterManagerRestart(t *testing.T) {
	f := newFixture(t)
	f.add(f.consoleServer("keep"))
	stopOnExit := f.consoleServer("temp")
	stopOnExit.Custom.OnManagerExit = "stop"
	f.add(stopOnExit)
	f.m.StartServer("keep", actor)
	f.m.StartServer("temp", actor)
	f.waitState("keep", 5*time.Second, Running)
	f.waitState("temp", 5*time.Second, Running)

	// Manager goes down while "keep" and "temp" are running.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	f.m.Shutdown(ctx)
	cancel()

	// A new manager instance: nothing is assumed, everything is probed.
	f.rec = &recorder{}
	f.boot()
	f.waitState("keep", 5*time.Second, Running)
	f.waitState("temp", 5*time.Second, Stopped)

	// The re-adopted server is fully controllable.
	if err := f.m.SendCommand(context.Background(), "keep", "still here"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "output from re-adopted server", func() bool {
		snap, _ := f.m.ConsoleSnapshot(context.Background(), "keep", 50)
		return strings.Contains(strings.Join(snap.Lines, "\n"), "got: still here")
	})
	if _, err := f.m.StopServer("keep", actor); err != nil {
		t.Fatal(err)
	}
	f.waitState("keep", 5*time.Second, Stopped)
}

func TestInterruptedOperationsAreMarkedAtStartup(t *testing.T) {
	f := newFixture(t)
	f.add(f.consoleServer("x"))
	f.store.InsertOperation(context.Background(), &db.Operation{ServerID: "x", Type: "backup", Status: "running", StartedAt: time.Now()})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	f.m.Shutdown(ctx)
	cancel()
	f.boot()
	ops, _ := f.m.Operations(context.Background(), "x", 5)
	if len(ops) != 1 || ops[0].Status != "interrupted" {
		t.Fatalf("stale running operation: %+v", ops)
	}
}

func TestConsoleSubscriptionDeliversLiveLines(t *testing.T) {
	f := newFixture(t)
	f.add(f.consoleServer("con"))
	a, err := f.m.ConsoleSubscribe("con", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _ := f.m.ConsoleSubscribe("con", "", 0)
	defer b.Close()
	f.m.StartServer("con", actor)
	for _, stream := range []*ConsoleStream{a, b} {
		seen := false
		deadline := time.After(5 * time.Second)
		for !seen {
			select {
			case ev := <-stream.Sub.C:
				seen = strings.Contains(ev.Line.Text, "Done (")
			case <-deadline:
				t.Fatal("both viewers must receive live output")
			}
		}
	}
	f.waitState("con", 5*time.Second, Running)
	f.m.StopServer("con", actor)
	f.waitState("con", 5*time.Second, Stopped)
}

func TestReloadFromDisk(t *testing.T) {
	f := newFixture(t)
	f.add(f.consoleServer("a"))
	dir := f.cfg.Get().Paths.ServersDir
	os.WriteFile(filepath.Join(dir, "b.json"), []byte(`{"id":"b","name":"B","backend":"custom","custom":{"start":{"command":"/bin/true"}}}`), 0o600)
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"id":"bad"}`), 0o600)
	os.Remove(filepath.Join(dir, "a.json"))
	res, err := f.m.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 1 || res.Added[0] != "b" || len(res.Removed) != 1 || res.Removed[0] != "a" || res.Invalid["bad.json"] == "" {
		t.Fatalf("reload result %+v", res)
	}
	if _, err := f.m.Status("a"); !errors.Is(err, ErrNotFound) {
		t.Fatal("removed definition should disappear")
	}
	f.waitState("b", 5*time.Second, Stopped)
}

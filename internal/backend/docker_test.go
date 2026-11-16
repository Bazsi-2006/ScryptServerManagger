package backend

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

// fakeDocker implements the small part of the Docker Engine API the backend
// uses, on a Unix socket. To run the backend against a real daemon instead,
// point tools.docker_socket at it and create a container to manage.
type fakeDocker struct {
	mu       sync.Mutex
	exists   bool
	running  bool
	health   string
	exitCode int
	calls    []string
	execCmd  []string
	logs     []string
}

func frame(stream byte, s string) []byte {
	b := make([]byte, 8+len(s))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:], uint32(len(s)))
	copy(b[8:], s)
	return b
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	notFound := func() {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"message": "No such container: mc"})
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/start"):
		w.Write(frame(1, "exec output\n"))
	case strings.HasPrefix(r.URL.Path, "/exec/"):
		json.NewEncoder(w).Encode(map[string]any{"ExitCode": 0, "Running": false})
	case !strings.HasPrefix(r.URL.Path, "/containers/mc/"):
		notFound()
	case !f.exists:
		notFound()
	case strings.HasSuffix(r.URL.Path, "/json"):
		state := map[string]any{"Status": "exited", "Running": f.running, "ExitCode": f.exitCode}
		if f.running {
			state["Status"] = "running"
			if f.health != "" {
				state["Health"] = map[string]string{"Status": f.health}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"State": state, "Config": map[string]any{"Tty": false}})
	case strings.HasSuffix(r.URL.Path, "/start"):
		f.running = true
		w.WriteHeader(204)
	case strings.HasSuffix(r.URL.Path, "/stop"), strings.HasSuffix(r.URL.Path, "/kill"):
		f.running = false
		w.WriteHeader(204)
	case strings.HasSuffix(r.URL.Path, "/restart"):
		f.running = true
		w.WriteHeader(204)
	case strings.HasSuffix(r.URL.Path, "/logs"):
		for i, l := range f.logs {
			w.Write(frame(byte(1+i%2), l+"\n")) // alternate stdout/stderr
		}
	case strings.HasSuffix(r.URL.Path, "/exec"):
		var body struct{ Cmd []string }
		json.NewDecoder(r.Body).Decode(&body)
		f.execCmd = body.Cmd
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"Id": "exec1"})
	default:
		w.WriteHeader(500)
	}
}

func dockerEnv(t *testing.T) (*env, *fakeDocker) {
	t.Helper()
	e := newEnv(t)
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeDocker{exists: true, logs: []string{"line one", "line two", "\x1b[31mred\x1b[0m line"}}
	srv := &http.Server{Handler: fake}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	e.cfg.Update(func(c *configT) error { c.Tools.DockerSocket = sock; return nil })
	return e, fake
}

func dockerDef(t *testing.T) *servers.Definition {
	return valid(t, &servers.Definition{ID: "dk", Name: "DK", Backend: servers.BackendDocker, Docker: &servers.Docker{Container: "mc"},
		Console: servers.Console{Input: servers.InputDockerExec, InputCommand: &servers.Command{Command: "rcon-cli", Args: []string{"{command}"}}}})
}

func TestDockerStateStartStopRestart(t *testing.T) {
	e, fake := dockerEnv(t)
	b := newDocker(dockerDef(t), e.deps)
	if p := b.Probe(bg); p.Life != LifeDead || !strings.Contains(p.Detail, "exited") {
		t.Fatalf("stopped container: %+v", p)
	}
	if err := b.Start(bg); err != nil {
		t.Fatal(err)
	}
	if p := b.Probe(bg); p.Life != LifeAlive || p.Phase != "" {
		t.Fatalf("running container: %+v", p)
	}
	fake.mu.Lock()
	fake.health = "starting"
	fake.mu.Unlock()
	if p := b.Probe(bg); p.Life != LifeAlive || p.Phase != PhaseStarting {
		t.Fatalf("health check starting must hold the server in STARTING: %+v", p)
	}
	fake.mu.Lock()
	fake.health = "healthy"
	fake.mu.Unlock()
	if p := b.Probe(bg); p.Phase != "" {
		t.Fatalf("healthy: %+v", p)
	}
	if err := b.Restart(bg, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(bg, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 3*time.Second, "container stop", func() bool { return b.Probe(bg).Life == LifeDead })
	no := false
	def := dockerDef(t)
	def.Docker.AllowRestart = &no
	if err := newDocker(def, e.deps).Restart(bg, time.Second); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("restart disabled: %v", err)
	}
}

func TestDockerRemovedContainerAndUnreachableDaemon(t *testing.T) {
	e, fake := dockerEnv(t)
	b := newDocker(dockerDef(t), e.deps)
	fake.mu.Lock()
	fake.exists = false
	fake.mu.Unlock()
	p := b.Probe(bg)
	if p.Life != LifeUnknown || !strings.Contains(p.Detail, "does not exist") {
		t.Fatalf("removed container: %+v", p)
	}
	if err := b.Start(bg); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("start of removed container: %v", err)
	}
	e.cfg.Update(func(c *configT) error { c.Tools.DockerSocket = "/nonexistent/docker.sock"; return nil })
	p = b.Probe(bg)
	if p.Life != LifeUnknown || !strings.Contains(p.Detail, "cannot reach the Docker daemon") {
		t.Fatalf("daemon down: %+v", p)
	}
	if err := b.Start(bg); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("start with daemon down: %v", err)
	}
}

func TestDockerLogsAreDemultiplexedAndCleaned(t *testing.T) {
	e, _ := dockerEnv(t)
	b := newDocker(dockerDef(t), e.deps)
	lines, err := b.Snapshot(bg, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"line one", "line two", "red line"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("snapshot %q, want %q", lines, want)
	}
}

func TestDockerFollowStreamsIntoBuffer(t *testing.T) {
	e, fake := dockerEnv(t)
	fake.mu.Lock()
	fake.running = true
	fake.mu.Unlock()
	b := newDocker(dockerDef(t), e.deps)
	ctx, cancel := contextWithCancel()
	done := make(chan error, 1)
	go func() { done <- b.Follow(ctx, FollowOptions{Tail: 10}) }()
	testutil.Eventually(t, 5*time.Second, "followed log lines", func() bool { return e.has("line two") && e.has("red line") })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Follow must end when its context is cancelled")
	}
}

func TestDockerExecInputUsesArgvTemplate(t *testing.T) {
	e, fake := dockerEnv(t)
	fake.mu.Lock()
	fake.running = true
	fake.mu.Unlock()
	b := newDocker(dockerDef(t), e.deps)
	line := `say hello; rm -rf / $(x)`
	if err := b.SendInput(bg, line); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	cmd := fake.execCmd
	fake.mu.Unlock()
	if len(cmd) != 2 || cmd[0] != "rcon-cli" || cmd[1] != line {
		t.Fatalf("exec argv %q: the console line must be exactly one argument", cmd)
	}
	testutil.Eventually(t, 2*time.Second, "exec output in console", func() bool { return e.has("exec output") })

	plain := valid(t, &servers.Definition{ID: "p", Name: "P", Backend: servers.BackendDocker, Docker: &servers.Docker{Container: "mc"}})
	if err := newDocker(plain, e.deps).SendInput(bg, "x"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("input without a configured helper must be unsupported: %v", err)
	}
}

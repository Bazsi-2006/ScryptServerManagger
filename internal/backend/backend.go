// Package backend contains the server execution backends (custom commands,
// tmux, systemd, Docker). A backend knows how to start and stop one server,
// report its native lifecycle state and expose its console; it knows nothing
// about users, permissions or readiness policy, which live in the manager.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/console"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// Life is the backend-native answer to "is the server process alive?".
type Life int

const (
	// LifeUnknown: the state could not be determined (tool missing, daemon
	// unreachable, unit/container not found, ...).
	LifeUnknown Life = iota
	LifeDead
	LifeAlive
	// LifeExternal: the backend has no native way to tell (a forking custom
	// server without PID file). The manager falls back to the configured
	// port/script check, with the limitations documented for those checks.
	LifeExternal
)

const (
	PhaseStarting = "starting"
	PhaseStopping = "stopping"
	PhaseFailed   = "failed"
)

// Probe is the result of a lifecycle query.
type Probe struct {
	Life Life
	// Phase is an optional native hint: starting, stopping or failed.
	Phase string
	// Detail is a short human-readable explanation.
	Detail string
}

// Caps describes what a backend instance supports with its configuration.
type Caps struct {
	Start         bool
	NativeRestart bool
	// ForceKill: Kill may be used when a graceful stop times out.
	ForceKill bool
	// LiveOnDemand: live output requires Follow to be running; otherwise the
	// backend feeds the console buffer continuously by itself.
	LiveOnDemand bool
	// StopOnManagerExit: the server must be stopped when the manager exits.
	StopOnManagerExit bool
}

// Typed errors let the API layer map failures to precise responses.
var (
	// ErrUnsupported: the action does not exist for this backend/config.
	ErrUnsupported = errors.New("not supported by this backend")
	// ErrUnavailable: the backend's tooling or daemon cannot be reached.
	ErrUnavailable = errors.New("backend unavailable")
	// ErrNotRunning: the action needs a running server.
	ErrNotRunning = errors.New("server is not running")
)

// FollowOptions selects where an on-demand log follower starts.
type FollowOptions struct {
	// Tail preloads this many recent lines (ignored when Since is set).
	Tail int
	// Since, when non-zero, starts at this instant without preloading.
	Since time.Time
}

// Backend controls exactly one server.
type Backend interface {
	Caps() Caps
	// Probe reports the native lifecycle state. It must be quick and safe to
	// call concurrently with other methods.
	Probe(ctx context.Context) Probe
	// Start launches the server and returns once the launch has been issued
	// (not once the server is ready).
	Start(ctx context.Context) error
	// Stop requests a graceful shutdown. grace is how long the caller will
	// wait before considering the stop timed out.
	Stop(ctx context.Context, grace time.Duration) error
	// Kill forcibly terminates the server.
	Kill(ctx context.Context) error
	// Restart uses the backend's native restart (only if Caps.NativeRestart).
	Restart(ctx context.Context, grace time.Duration) error
	// Snapshot returns up to n recent console lines straight from the
	// backend's native log source.
	Snapshot(ctx context.Context, n int) ([]string, error)
	// Follow streams live output into the console buffer until ctx ends
	// (only for Caps.LiveOnDemand backends).
	Follow(ctx context.Context, opts FollowOptions) error
	// SendInput delivers one console line using the backend's own channel
	// (stdin, PTY, tmux, docker exec).
	SendInput(ctx context.Context, line string) error
	// Close releases resources without stopping the server.
	Close()
}

// TextSender is implemented by backends that can type administrator-defined
// text into the server's own console (tmux). It is independent of the
// console input method users type into: it delivers fixed text configured
// for backups and custom actions.
type TextSender interface {
	// SendText types text literally into the server's console, followed by
	// the Enter key when enter is set. It fails with ErrNotRunning (wrapped
	// with a description) when the target does not exist.
	SendText(ctx context.Context, text string, enter bool) error
}

// Deps are the services a backend needs from its host.
type Deps struct {
	Config  *config.Holder
	Console *console.Buffer
	Log     *slog.Logger
	// LoadRuntime/SaveRuntime persist a small JSON blob across restarts.
	LoadRuntime func() string
	SaveRuntime func(string)
}

// New builds the backend for a (validated) definition.
func New(def *servers.Definition, deps Deps) (Backend, error) {
	switch def.Backend {
	case servers.BackendCustom:
		return newCustom(def, deps), nil
	case servers.BackendTmux:
		return newTmux(def, deps), nil
	case servers.BackendSystemd:
		return newSystemd(def, deps), nil
	case servers.BackendDocker:
		return newDocker(def, deps), nil
	}
	return nil, fmt.Errorf("unknown backend %q", def.Backend)
}

// scrubbed environment variables: our own secrets namespace plus variables
// that tie a child to the manager's systemd service or tmux client.
var envDenyPrefixes = []string{"SM_", "LISTEN_", "WATCHDOG_"}
var envDenyExact = map[string]bool{
	"NOTIFY_SOCKET": true, "INVOCATION_ID": true, "JOURNAL_STREAM": true, "CREDENTIALS_DIRECTORY": true,
	"MAINPID": true, "MANAGERPID": true, "SYSTEMD_EXEC_PID": true, "TMUX": true, "TMUX_PANE": true,
}

// BaseEnv returns the manager's environment with secrets and service-private
// variables removed. Child processes must never inherit SM_JWT_SECRET or
// SM_SMTP_PASSWORD.
func BaseEnv() []string {
	var out []string
outer:
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if envDenyExact[k] {
			continue
		}
		for _, p := range envDenyPrefixes {
			if strings.HasPrefix(k, p) {
				continue outer
			}
		}
		out = append(out, kv)
	}
	return out
}

// ServerEnv is BaseEnv plus the definition's variables and SM_SERVER_ID /
// SM_SERVER_NAME for use by scripts.
func ServerEnv(def *servers.Definition, extra ...string) []string {
	env := BaseEnv()
	env = append(env, "SM_SERVER_ID="+def.ID, "SM_SERVER_NAME="+def.Name)
	for k, v := range def.Env {
		env = append(env, k+"="+v)
	}
	return append(env, extra...)
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = append([]byte(nil), w.buf[len(w.buf)-w.max:]...)
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.TrimSpace(strings.ToValidUTF8(string(w.buf), "�"))
}

// RunSpec describes a helper command execution.
type RunSpec struct {
	Argv  []string
	Dir   string
	Env   []string
	Stdin string
	// Output additionally receives combined stdout+stderr (optional).
	Output io.Writer
	// OutputFile, when set, is handed to the child directly as stdout and
	// stderr (no pipe), so processes that outlive the command can keep
	// writing to it. RunResult.Output is empty in that case.
	OutputFile *os.File
	// TailBytes of combined output are returned (default 4096).
	TailBytes int
}

// RunResult is the outcome of Run.
type RunResult struct {
	ExitCode int
	Output   string
}

// ErrTimeout marks helper commands killed because their context ended.
var ErrTimeout = errors.New("command timed out")

// Run executes a helper command directly (no shell) in its own process
// group. When ctx ends the whole group is killed. A non-zero exit status is
// not an error: inspect RunResult.ExitCode. Errors mean the command could
// not be run or was killed.
func Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return RunResult{ExitCode: -1}, errors.New("no command configured")
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	if cmd.Env == nil {
		cmd.Env = BaseEnv()
	}
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	max := spec.TailBytes
	if max <= 0 {
		max = 4096
	}
	tail := &tailWriter{max: max}
	var out io.Writer = tail
	if spec.Output != nil {
		out = io.MultiWriter(tail, spec.Output)
	}
	cmd.Stdout, cmd.Stderr = out, out
	if spec.OutputFile != nil {
		cmd.Stdout, cmd.Stderr = spec.OutputFile, spec.OutputFile
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Do not hang forever on output pipes held open by grandchildren.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return RunResult{ExitCode: -1}, describeStartError(spec.Argv[0], spec.Dir, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-ctx.Done():
		timedOut = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}
	res := RunResult{Output: tail.String()}
	if timedOut {
		res.ExitCode = -1
		return res, ErrTimeout
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			if res.ExitCode < 0 {
				return res, fmt.Errorf("command was terminated by a signal")
			}
			return res, nil
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			return res, nil
		}
		res.ExitCode = -1
		return res, err
	}
	return res, nil
}

// describeStartError turns exec failures into messages that tell the
// administrator what to fix, without dumping internals.
func describeStartError(name, dir string, err error) error {
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		if dir != "" {
			if _, statErr := os.Stat(dir); statErr != nil {
				return fmt.Errorf("working directory %q is not accessible", dir)
			}
		}
		return fmt.Errorf("executable %q was not found", name)
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("executable %q is not executable by the service user (permission denied)", name)
	case errors.Is(err, syscall.ENOEXEC):
		return fmt.Errorf("executable %q has an invalid format (missing #! line?)", name)
	}
	return fmt.Errorf("could not execute %q: %v", name, err)
}

// SubstituteCommand expands the {command} placeholder in an input-command
// template. The user's text is inserted verbatim into single argv elements;
// since nothing is passed through a shell it cannot add arguments or
// commands. usedPlaceholder is false when no argument referenced it, in
// which case callers supply the text on stdin instead.
func SubstituteCommand(tmpl *servers.Command, line string) (argv []string, usedPlaceholder bool) {
	argv = make([]string, 0, len(tmpl.Args)+1)
	argv = append(argv, tmpl.Command)
	for _, a := range tmpl.Args {
		if strings.Contains(a, servers.CommandPlaceholder) {
			usedPlaceholder = true
			a = strings.ReplaceAll(a, servers.CommandPlaceholder, line)
		}
		argv = append(argv, a)
	}
	return argv, usedPlaceholder
}

// trimLines splits output into lines and keeps the last n non-trailing-empty
// lines.
func trimLines(lines []string, n int) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

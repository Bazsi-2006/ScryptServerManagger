package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/console"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// customRuntime is persisted so a foreground server started by a previous
// manager process can be re-adopted.
type customRuntime struct {
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"start_ticks"`
}

// customBackend runs administrator-configured commands.
//
// Foreground mode: the start command is the server. Its output goes to an
// append-only capture file that is tailed into the console, so the process
// survives manager restarts (stdin, when enabled, is a FIFO for the same
// reason; PTY-attached servers cannot survive and are stopped on exit).
//
// Forking mode: the start command launches the server and exits. Liveness
// then comes from the PID file or from the configured port/script check.
type customBackend struct {
	def      *servers.Definition
	c        *servers.Custom
	deps     Deps
	logPath  string
	fifoPath string
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu            sync.Mutex
	pid           int
	ticks         uint64
	own           bool // true when the process is our child (we get its exit status)
	exitDetail    string
	failed        bool
	stopRequested bool
	input         *os.File // FIFO writer or PTY master
}

func newCustom(def *servers.Definition, deps Deps) *customBackend {
	rt := deps.Config.Get().Paths.RuntimeDir
	b := &customBackend{
		def: def, c: def.Custom, deps: deps,
		logPath:  filepath.Join(rt, def.ID+".console.log"),
		fifoPath: filepath.Join(rt, def.ID+".stdin"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	cc := deps.Config.Get().Console

	// Capture file written by processes we start.
	san := console.NewSanitizer(cc.MaxLineBytes, deps.Console.Append)
	t := console.NewTailer(b.logPath, san)
	t.MaxBytes = cc.MaxLogFileBytes
	if pre := t.SkipToEnd(256 << 10); len(pre) > 0 {
		for _, l := range trimLines(console.CleanLines(pre, cc.MaxLineBytes), cc.BufferLines) {
			deps.Console.Append(l)
		}
	}
	b.wg.Add(1)
	go func() { defer b.wg.Done(); t.Run(ctx) }()

	// Optional server-written log file (never truncated by us).
	if b.c.LogFile != "" {
		san2 := console.NewSanitizer(cc.MaxLineBytes, deps.Console.Append)
		t2 := console.NewTailer(b.c.LogFile, san2)
		if pre := t2.SkipToEnd(64 << 10); len(pre) > 0 && deps.Console.LastSeq() == 0 {
			for _, l := range trimLines(console.CleanLines(pre, cc.MaxLineBytes), cc.BufferLines) {
				deps.Console.Append(l)
			}
		}
		b.wg.Add(1)
		go func() { defer b.wg.Done(); t2.Run(ctx) }()
	}

	b.adopt()
	return b
}

// adopt re-attaches to a foreground process recorded by a previous run.
func (b *customBackend) adopt() {
	if b.c.Mode != servers.ModeForeground || b.deps.LoadRuntime == nil {
		return
	}
	var rt customRuntime
	if err := json.Unmarshal([]byte(b.deps.LoadRuntime()), &rt); err != nil || rt.PID <= 0 {
		return
	}
	if !procAlive(rt.PID, rt.StartTicks) {
		b.deps.SaveRuntime("")
		return
	}
	b.pid, b.ticks, b.own = rt.PID, rt.StartTicks, false
	if b.def.InputMethod() == servers.InputStdin {
		if f, err := os.OpenFile(b.fifoPath, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			b.input = f
		} else {
			b.deps.Log.Warn("could not reopen stdin FIFO of adopted server; console input is unavailable until it is restarted", "server", b.def.ID)
		}
	}
	b.deps.Log.Info("re-adopted running server process", "server", b.def.ID, "pid", rt.PID)
}

func (b *customBackend) Caps() Caps {
	fg := b.c.Mode == servers.ModeForeground
	return Caps{
		Start:             true,
		ForceKill:         b.c.KillAfterTimeout() && (fg || b.c.PIDFile != ""),
		StopOnManagerExit: b.c.OnManagerExit == "stop" || b.def.InputMethod() == servers.InputPTY,
	}
}

func (b *customBackend) Probe(ctx context.Context) Probe {
	if b.c.Mode == servers.ModeForking {
		if b.c.PIDFile == "" {
			return Probe{Life: LifeExternal}
		}
		pid, err := readPIDFile(b.c.PIDFile)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return Probe{Life: LifeDead}
			}
			if errors.Is(err, os.ErrPermission) {
				return Probe{Life: LifeUnknown, Detail: "PID file is not readable by the service user"}
			}
			return Probe{Life: LifeDead, Detail: err.Error()}
		}
		if pidAlive(pid) {
			return Probe{Life: LifeAlive}
		}
		return Probe{Life: LifeDead, Detail: "process from PID file is not running"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pid != 0 && !b.own && !procAlive(b.pid, b.ticks) {
		// An adopted process is not our child, so we learn of its exit by
		// polling and cannot know its exit status.
		b.clearLocked("process exited", false)
	}
	if b.pid == 0 {
		p := Probe{Life: LifeDead, Detail: b.exitDetail}
		if b.failed {
			p.Phase = PhaseFailed
		}
		return p
	}
	return Probe{Life: LifeAlive}
}

func (b *customBackend) clearLocked(detail string, failed bool) {
	b.pid, b.ticks, b.own = 0, 0, false
	b.exitDetail, b.failed = detail, failed
	if b.input != nil && b.def.InputMethod() == servers.InputStdin {
		b.input.Close()
	}
	b.input = nil
	if b.deps.SaveRuntime != nil {
		b.deps.SaveRuntime("")
	}
}

func (b *customBackend) openLog() (*os.File, error) {
	f, err := os.OpenFile(b.logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open console capture file: %w", err)
	}
	return f, nil
}

func (b *customBackend) Start(ctx context.Context) error {
	if b.c.Mode == servers.ModeForking {
		return b.startForking(ctx)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pid != 0 {
		return errors.New("the server process is already running")
	}
	logFile, err := b.openLog()
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(b.c.Start.Command, b.c.Start.Args...)
	cmd.Dir = b.def.WorkingDir
	cmd.Env = ServerEnv(b.def)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = logFile, logFile

	var closeAfterStart []*os.File
	var master *os.File
	switch b.def.InputMethod() {
	case servers.InputPTY:
		t := 200
		m, slave, err := openPTY(t, 50)
		if err != nil {
			return err
		}
		master = m
		closeAfterStart = append(closeAfterStart, slave)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.SysProcAttr.Setctty = true
		cmd.SysProcAttr.Ctty = 0
		if _, ok := b.def.Env["TERM"]; !ok {
			cmd.Env = append(cmd.Env, "TERM=xterm-256color")
		}
	case servers.InputStdin:
		// The child gets the FIFO opened read+write: because it holds a
		// write end itself it never sees EOF, even while no manager is
		// running, and a restarted manager can simply reopen the FIFO.
		_ = os.Remove(b.fifoPath)
		if err := syscall.Mkfifo(b.fifoPath, 0o600); err != nil {
			return fmt.Errorf("create stdin FIFO: %w", err)
		}
		rw, err := os.OpenFile(b.fifoPath, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open stdin FIFO: %w", err)
		}
		closeAfterStart = append(closeAfterStart, rw)
		cmd.Stdin = rw
	}
	closeAll := func() {
		for _, f := range closeAfterStart {
			f.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		closeAll()
		if master != nil {
			master.Close()
		}
		return describeStartError(b.c.Start.Command, b.def.WorkingDir, err)
	}
	pid := cmd.Process.Pid
	_, ticks, _ := procStat(pid)
	b.pid, b.ticks, b.own = pid, ticks, true
	b.exitDetail, b.failed, b.stopRequested = "", false, false

	switch b.def.InputMethod() {
	case servers.InputPTY:
		b.input = master
		b.wg.Add(1)
		go b.readPTY(master)
	case servers.InputStdin:
		if w, err := os.OpenFile(b.fifoPath, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			b.input = w
		} else {
			b.deps.Log.Warn("could not open stdin FIFO for writing", "server", b.def.ID, "error", err)
		}
	}
	closeAll()
	if b.deps.SaveRuntime != nil {
		data, _ := json.Marshal(customRuntime{PID: pid, StartTicks: ticks})
		b.deps.SaveRuntime(string(data))
	}
	go b.wait(cmd, pid)
	return nil
}

// wait reaps our child and records how it ended.
func (b *customBackend) wait(cmd *exec.Cmd, pid int) {
	err := cmd.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pid != pid {
		return
	}
	detail, failed := "process exited", false
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			detail = fmt.Sprintf("process was terminated by signal %s", ws.Signal())
			failed = !b.stopRequested
		} else {
			detail = fmt.Sprintf("process exited with code %d", ee.ExitCode())
			failed = !b.stopRequested
		}
	} else if err == nil {
		detail = "process exited with code 0"
	}
	b.clearLocked(detail, failed)
}

func (b *customBackend) readPTY(master *os.File) {
	defer b.wg.Done()
	defer master.Close()
	san := console.NewSanitizer(b.deps.Config.Get().Console.MaxLineBytes, b.deps.Console.Append)
	buf := make([]byte, 32<<10)
	for {
		n, err := master.Read(buf)
		if n > 0 {
			_, _ = san.Write(buf[:n])
		}
		if err != nil {
			san.Flush()
			return
		}
	}
}

func (b *customBackend) startForking(ctx context.Context) error {
	logFile, err := b.openLog()
	if err != nil {
		return err
	}
	defer logFile.Close()
	// The capture file itself (not a pipe) becomes the script's stdout, so a
	// daemon that inherits it can keep writing after the script has exited.
	res, err := Run(ctx, RunSpec{Argv: b.c.Start.Argv(), Dir: b.def.WorkingDir, Env: ServerEnv(b.def), OutputFile: logFile})
	if err != nil {
		if errors.Is(err, ErrTimeout) {
			return fmt.Errorf("start command did not finish in time")
		}
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("start command exited with code %d (see console output)", res.ExitCode)
	}
	return nil
}

func (b *customBackend) runHelper(ctx context.Context, what string, c *servers.Command) error {
	logFile, err := b.openLog()
	if err != nil {
		return err
	}
	defer logFile.Close()
	res, err := Run(ctx, RunSpec{Argv: c.Argv(), Dir: b.def.WorkingDir, Env: ServerEnv(b.def), Output: logFile})
	if err != nil {
		if errors.Is(err, ErrTimeout) {
			return fmt.Errorf("%s command timed out", what)
		}
		return fmt.Errorf("%s command failed: %w", what, err)
	}
	if res.ExitCode != 0 {
		msg := fmt.Sprintf("%s command exited with code %d", what, res.ExitCode)
		if res.Output != "" {
			msg += ": " + lastLine(res.Output)
		}
		return errors.New(msg)
	}
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func (b *customBackend) Stop(ctx context.Context, grace time.Duration) error {
	if b.c.Mode == servers.ModeForking {
		if b.c.Stop != nil {
			return b.runHelper(ctx, "stop", b.c.Stop)
		}
		pid, err := readPIDFile(b.c.PIDFile)
		if err != nil || !pidAlive(pid) {
			return ErrNotRunning
		}
		return syscall.Kill(pid, signalByName(b.c.StopSignal))
	}
	b.mu.Lock()
	if b.pid == 0 {
		b.mu.Unlock()
		return ErrNotRunning
	}
	b.stopRequested = true
	pid := b.pid
	b.mu.Unlock()

	switch {
	case b.c.Stop != nil:
		return b.runHelper(ctx, "stop", b.c.Stop)
	case b.c.StopInput != "":
		return b.SendInput(ctx, b.c.StopInput)
	default:
		// Negative PID: signal the whole process group (the server was
		// started as a session/group leader).
		if err := syscall.Kill(-pid, signalByName(b.c.StopSignal)); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signal server process: %w", err)
		}
		return nil
	}
}

func (b *customBackend) Kill(ctx context.Context) error {
	if b.c.Mode == servers.ModeForking {
		if b.c.PIDFile == "" {
			return ErrUnsupported
		}
		pid, err := readPIDFile(b.c.PIDFile)
		if err != nil || !pidAlive(pid) {
			return nil
		}
		return syscall.Kill(pid, syscall.SIGKILL)
	}
	b.mu.Lock()
	pid, ticks := b.pid, b.ticks
	b.stopRequested = true
	b.mu.Unlock()
	if pid == 0 || !procAlive(pid, ticks) {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (b *customBackend) Restart(ctx context.Context, grace time.Duration) error {
	return ErrUnsupported
}

func (b *customBackend) Snapshot(ctx context.Context, n int) ([]string, error) {
	lines, _ := b.deps.Console.Tail(n)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out, nil
}

func (b *customBackend) Follow(ctx context.Context, opts FollowOptions) error {
	return ErrUnsupported
}

func (b *customBackend) SendInput(ctx context.Context, line string) error {
	in := b.def.InputMethod()
	if in != servers.InputStdin && in != servers.InputPTY {
		return ErrUnsupported
	}
	b.mu.Lock()
	f := b.input
	running := b.pid != 0
	b.mu.Unlock()
	if !running {
		return ErrNotRunning
	}
	if f == nil {
		return errors.New("the input channel to this server was lost; restart the server to restore console input")
	}
	_ = f.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := f.Write([]byte(line + "\n")); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return errors.New("the server is not reading its input")
		}
		return fmt.Errorf("write to server input: %w", err)
	}
	return nil
}

func (b *customBackend) Close() {
	b.cancel()
	b.mu.Lock()
	if b.input != nil {
		// Closing a PTY master hangs up the server; callers stop PTY
		// servers first (Caps.StopOnManagerExit).
		b.input.Close()
		b.input = nil
	}
	b.mu.Unlock()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// RemoveRuntimeFiles deletes the capture files the application created for a
// server. It never touches the server's own files.
func RemoveRuntimeFiles(runtimeDir, id string) {
	for _, suffix := range []string{".console.log", ".stdin", ".tmux.log"} {
		_ = os.Remove(filepath.Join(runtimeDir, id+suffix))
	}
}

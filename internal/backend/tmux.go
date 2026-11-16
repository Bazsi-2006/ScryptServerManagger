package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/console"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// tmuxBackend manages a server living in a tmux session.
//
// Liveness is "the session exists (and its pane's process has not died)".
// Live output is captured with `pipe-pane` into an application-owned file
// that is tailed into the console buffer; this keeps working while nobody is
// watching, which console-based readiness detection depends on. Input is
// typed into the pane with `send-keys -l`.
//
// The tmux server must belong to the user the manager runs as (or be
// reachable through the configured socket).
type tmuxBackend struct {
	def      *servers.Definition
	t        *servers.Tmux
	deps     Deps
	pipePath string
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu       sync.Mutex
	attached string // identity (pane id + session creation time) we piped
}

func newTmux(def *servers.Definition, deps Deps) *tmuxBackend {
	b := &tmuxBackend{def: def, t: def.Tmux, deps: deps,
		pipePath: filepath.Join(deps.Config.Get().Paths.RuntimeDir, def.ID+".tmux.log")}
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	cc := deps.Config.Get().Console
	san := console.NewSanitizer(cc.MaxLineBytes, deps.Console.Append)
	tail := console.NewTailer(b.pipePath, san)
	tail.MaxBytes = cc.MaxLogFileBytes
	tail.SkipToEnd(0)
	b.wg.Add(1)
	go func() { defer b.wg.Done(); tail.Run(ctx) }()
	return b
}

func (b *tmuxBackend) Caps() Caps {
	return Caps{Start: b.t.Start != nil, ForceKill: b.t.KillAfterTimeout()}
}

// sessionTarget matches the session name exactly ("=" disables tmux's
// prefix and fnmatch matching).
func (b *tmuxBackend) sessionTarget() string { return "=" + b.t.Session }

// paneTarget addresses the configured pane.
func (b *tmuxBackend) paneTarget() string {
	s := "=" + b.t.Session + ":" + b.t.Window
	if b.t.Pane != "" {
		s += "." + b.t.Pane
	}
	return s
}

type tmuxResult struct {
	stdout, stderr string
	code           int
}

// tmux runs one tmux command with a scrubbed environment (a tmux server
// started by us must not inherit the manager's secrets).
func (b *tmuxBackend) tmux(ctx context.Context, args ...string) (tmuxResult, error) {
	var full []string
	if b.t.SocketName != "" {
		full = append(full, "-L", b.t.SocketName)
	} else if b.t.SocketPath != "" {
		full = append(full, "-S", b.t.SocketPath)
	}
	full = append(full, args...)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.deps.Config.Get().Tools.Tmux, full...)
	cmd.Env = BaseEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := tmuxResult{stdout: out.String(), stderr: strings.TrimSpace(errb.String())}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.code = ee.ExitCode()
			return res, nil
		}
		if ctx.Err() != nil {
			return res, fmt.Errorf("%w: tmux did not respond", ErrUnavailable)
		}
		return res, fmt.Errorf("%w: cannot run tmux (%v)", ErrUnavailable, describeStartError(b.deps.Config.Get().Tools.Tmux, "", err))
	}
	return res, nil
}

func tmuxSessionMissing(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "can't find session") || strings.Contains(s, "no server running") ||
		strings.Contains(s, "can't find window") || strings.Contains(s, "can't find pane") ||
		(strings.Contains(s, "error connecting") && strings.Contains(s, "no such file"))
}

type paneInfo struct {
	exists bool
	piped  bool
	dead   bool
	ident  string
}

// windowTarget addresses the configured window (all of its panes).
func (b *tmuxBackend) windowTarget() string { return "=" + b.t.Session + ":" + b.t.Window }

// paneInfo looks up the configured pane. It uses list-panes because, unlike
// `display-message -p`, that command fails when the target does not exist
// instead of printing empty values.
func (b *tmuxBackend) paneInfo(ctx context.Context) (paneInfo, error) {
	res, err := b.tmux(ctx, "list-panes", "-t", b.windowTarget(), "-F",
		"#{pane_index} #{pane_active} #{pane_pipe} #{pane_dead} #{pane_id}/#{session_created}")
	if err != nil {
		return paneInfo{}, err
	}
	if res.code != 0 {
		if tmuxSessionMissing(res.stderr) {
			return paneInfo{}, nil
		}
		return paneInfo{}, fmt.Errorf("tmux: %s", res.stderr)
	}
	for _, line := range strings.Split(strings.TrimSpace(res.stdout), "\n") {
		f := strings.Fields(line)
		if len(f) != 5 {
			continue
		}
		// The configured pane index, or the window's active pane.
		if (b.t.Pane != "" && f[0] == b.t.Pane) || (b.t.Pane == "" && f[1] == "1") {
			return paneInfo{exists: true, piped: f[2] == "1", dead: f[3] == "1", ident: f[4]}, nil
		}
	}
	return paneInfo{}, nil
}

func (b *tmuxBackend) Probe(ctx context.Context) Probe {
	info, err := b.paneInfo(ctx)
	if err != nil {
		return Probe{Life: LifeUnknown, Detail: err.Error()}
	}
	if !info.exists {
		b.mu.Lock()
		b.attached = ""
		b.mu.Unlock()
		return Probe{Life: LifeDead}
	}
	if info.dead {
		return Probe{Life: LifeDead, Detail: "the pane's process has exited (session kept by remain-on-exit)"}
	}
	b.ensurePipe(ctx, info)
	return Probe{Life: LifeAlive}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ensurePipe makes sure the pane's output is piped into our capture file.
// On first contact with a pane the visible history is loaded into the
// console buffer so viewers get context.
func (b *tmuxBackend) ensurePipe(ctx context.Context, info paneInfo) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.attached == info.ident && info.piped {
		return
	}
	first := b.attached != info.ident
	if first {
		b.deps.Console.Reset()
		if lines, err := b.capture(ctx, b.deps.Config.Get().Console.BufferLines); err == nil {
			for _, l := range lines {
				b.deps.Console.Append(l)
			}
		}
	}
	// Make sure the capture file exists with tight permissions before the
	// shell-spawned `cat` appends to it. The path is application-controlled
	// (runtime dir + validated server ID) and shell-quoted regardless.
	if f, err := os.OpenFile(b.pipePath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
		f.Close()
	}
	res, err := b.tmux(ctx, "pipe-pane", "-t", b.paneTarget(), "exec cat >> "+shellQuote(b.pipePath))
	if err != nil || res.code != 0 {
		b.deps.Log.Warn("tmux pipe-pane failed; live console output is unavailable", "server", b.def.ID, "detail", res.stderr, "error", err)
		return
	}
	b.attached = info.ident
}

func (b *tmuxBackend) capture(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		n = 200
	}
	res, err := b.tmux(ctx, "capture-pane", "-p", "-J", "-t", b.paneTarget(), "-S", "-"+strconv.Itoa(n))
	if err != nil {
		return nil, err
	}
	if res.code != 0 {
		if tmuxSessionMissing(res.stderr) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("tmux: %s", res.stderr)
	}
	max := b.deps.Config.Get().Console.MaxLineBytes
	lines := strings.Split(strings.ReplaceAll(res.stdout, "\r", ""), "\n")
	for i, l := range lines {
		l = strings.TrimRight(strings.ToValidUTF8(l, "�"), " ")
		if len(l) > max {
			l = l[:max]
		}
		lines[i] = l
	}
	return trimLines(lines, n), nil
}

func (b *tmuxBackend) tmuxVersionAtLeast(ctx context.Context, major, minor int) bool {
	res, err := b.tmux(ctx, "-V")
	if err != nil {
		return false
	}
	// "tmux 3.4", "tmux 3.2a", "tmux next-3.5"
	v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(res.stdout), "tmux"))
	v = strings.TrimPrefix(v, "next-")
	var ma, mi int
	num := func(s string) int {
		end := 0
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}
		n, _ := strconv.Atoi(s[:end])
		return n
	}
	parts := strings.SplitN(v, ".", 2)
	ma = num(parts[0])
	if len(parts) > 1 {
		mi = num(parts[1])
	}
	return ma > major || (ma == major && mi >= minor)
}

func (b *tmuxBackend) Start(ctx context.Context) error {
	if b.t.Start == nil {
		return fmt.Errorf("%w: no start command is configured for this tmux session (attach-only)", ErrUnsupported)
	}
	info, err := b.paneInfo(ctx)
	if err != nil {
		return err
	}
	if info.exists {
		if !info.dead {
			return fmt.Errorf("tmux session %q already exists", b.t.Session)
		}
		// Leftover session whose process died: clear it away.
		if _, err := b.tmux(ctx, "kill-session", "-t", b.sessionTarget()); err != nil {
			return err
		}
	} else if res, err := b.tmux(ctx, "has-session", "-t", b.sessionTarget()); err == nil && res.code == 0 {
		return fmt.Errorf("tmux session %q exists but the configured window/pane does not", b.t.Session)
	}
	if b.def.WorkingDir != "" {
		if st, err := os.Stat(b.def.WorkingDir); err != nil || !st.IsDir() {
			return fmt.Errorf("working directory %q is not accessible", b.def.WorkingDir)
		}
	}
	if strings.Contains(b.t.Start.Command, "/") {
		if _, err := os.Stat(b.t.Start.Command); err != nil {
			return describeStartError(b.t.Start.Command, "", err)
		}
	} else if _, err := exec.LookPath(b.t.Start.Command); err != nil {
		return fmt.Errorf("executable %q was not found", b.t.Start.Command)
	}

	args := []string{"new-session", "-d", "-s", b.t.Session, "-x", strconv.Itoa(b.t.Width), "-y", strconv.Itoa(b.t.Height)}
	if b.t.Window != "" && !isDigits(b.t.Window) {
		args = append(args, "-n", b.t.Window)
	}
	if b.def.WorkingDir != "" {
		args = append(args, "-c", b.def.WorkingDir)
	}
	env := map[string]string{"SM_SERVER_ID": b.def.ID, "SM_SERVER_NAME": b.def.Name}
	for k, v := range b.def.Env {
		env[k] = v
	}
	argv := b.t.Start.Argv()
	if b.tmuxVersionAtLeast(ctx, 3, 2) {
		for k, v := range env {
			args = append(args, "-e", k+"="+v)
		}
	} else {
		// Older tmux has no -e: wrap the command in env(1).
		wrapped := []string{"env"}
		for k, v := range env {
			wrapped = append(wrapped, k+"="+v)
		}
		argv = append(wrapped, argv...)
	}
	args = append(args, "--")
	if len(argv) == 1 {
		// tmux passes a single argument to `sh -c`; several arguments are
		// executed directly. Quote the lone program so both cases behave the
		// same and nothing in the path is interpreted by the shell.
		args = append(args, "exec "+shellQuote(argv[0]))
	} else {
		args = append(args, argv...)
	}
	res, err := b.tmux(ctx, args...)
	if err != nil {
		return err
	}
	if res.code != 0 {
		return fmt.Errorf("tmux could not create the session: %s", res.stderr)
	}
	// Attach the output pipe right away to keep the uncaptured window small;
	// anything printed before this point is picked up from the pane history.
	if info, err := b.paneInfo(ctx); err == nil && info.exists && !info.dead {
		b.ensurePipe(ctx, info)
	}
	return nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func (b *tmuxBackend) sendKeys(ctx context.Context, literal bool, keys ...string) error {
	args := []string{"send-keys", "-t", b.paneTarget()}
	if literal {
		args = append(args, "-l")
	}
	args = append(args, "--")
	args = append(args, keys...)
	res, err := b.tmux(ctx, args...)
	if err != nil {
		return err
	}
	if res.code != 0 {
		if tmuxSessionMissing(res.stderr) {
			return ErrNotRunning
		}
		return fmt.Errorf("tmux: %s", res.stderr)
	}
	return nil
}

func (b *tmuxBackend) Stop(ctx context.Context, grace time.Duration) error {
	if len(b.t.StopKeys) == 0 && b.t.StopText == "" {
		return b.Kill(ctx)
	}
	if len(b.t.StopKeys) > 0 {
		if err := b.sendKeys(ctx, false, b.t.StopKeys...); err != nil {
			return err
		}
	}
	if b.t.StopText != "" {
		if err := b.sendKeys(ctx, true, b.t.StopText); err != nil {
			return err
		}
		return b.sendKeys(ctx, false, "Enter")
	}
	return nil
}

func (b *tmuxBackend) Kill(ctx context.Context) error {
	res, err := b.tmux(ctx, "kill-session", "-t", b.sessionTarget())
	if err != nil {
		return err
	}
	if res.code != 0 && !tmuxSessionMissing(res.stderr) {
		return fmt.Errorf("tmux: %s", res.stderr)
	}
	return nil
}

func (b *tmuxBackend) Restart(ctx context.Context, grace time.Duration) error { return ErrUnsupported }

func (b *tmuxBackend) Snapshot(ctx context.Context, n int) ([]string, error) {
	return b.capture(ctx, n)
}

func (b *tmuxBackend) Follow(ctx context.Context, opts FollowOptions) error { return ErrUnsupported }

// SendInput types one line into the pane. The text is sent literally (-l),
// so tmux key names such as "C-c" are not interpreted, and control
// characters are rejected so a line cannot smuggle in extra key presses.
func (b *tmuxBackend) SendInput(ctx context.Context, line string) error {
	if b.def.InputMethod() != servers.InputTmux {
		return ErrUnsupported
	}
	for _, r := range line {
		if unicode.IsControl(r) {
			return errors.New("commands must not contain control characters")
		}
	}
	if line != "" {
		if err := b.sendKeys(ctx, true, line); err != nil {
			return err
		}
	}
	return b.sendKeys(ctx, false, "Enter")
}

func (b *tmuxBackend) Close() {
	b.cancel()
	b.wg.Wait()
}

package backend

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// systemdBackend controls an existing systemd unit through systemctl and
// reads its output from the journal. State always comes from systemd itself
// (ActiveState/SubState), never from process sniffing.
//
// Privileges: `systemctl show` and reading the journal need no root (journal
// access requires membership of the systemd-journal group). Starting and
// stopping system units requires a polkit rule or a sudoers rule limited to
// the exact systemctl invocations; see docs/permissions.md.
type systemdBackend struct {
	def  *servers.Definition
	s    *servers.Systemd
	deps Deps
}

func newSystemd(def *servers.Definition, deps Deps) *systemdBackend {
	return &systemdBackend{def: def, s: def.Systemd, deps: deps}
}

func (b *systemdBackend) Caps() Caps {
	return Caps{Start: true, NativeRestart: b.s.RestartAllowed(), LiveOnDemand: true}
}

func (b *systemdBackend) env() []string {
	env := BaseEnv()
	if b.s.Scope == "user" && os.Getenv("XDG_RUNTIME_DIR") == "" {
		// systemctl --user needs the user's runtime directory to find the bus.
		env = append(env, "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(os.Getuid()))
	}
	return env
}

// systemctlArgv builds the command line. sudo is only used for the
// state-changing verbs.
func (b *systemdBackend) systemctlArgv(privileged bool, args ...string) []string {
	tools := b.deps.Config.Get().Tools
	var argv []string
	if privileged && b.s.UseSudo {
		argv = append(argv, tools.Sudo, "-n")
	}
	argv = append(argv, tools.Systemctl)
	if b.s.Scope == "user" {
		argv = append(argv, "--user")
	}
	return append(argv, args...)
}

func (b *systemdBackend) run(ctx context.Context, argv []string) (string, string, int, error) {
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = b.env()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.String(), strings.TrimSpace(errb.String()), ee.ExitCode(), nil
		}
		if ctx.Err() != nil {
			return "", "", -1, fmt.Errorf("%w: %s did not respond", ErrUnavailable, argv[0])
		}
		return "", "", -1, fmt.Errorf("%w: %v", ErrUnavailable, describeStartError(argv[0], "", err))
	}
	return out.String(), strings.TrimSpace(errb.String()), 0, nil
}

func (b *systemdBackend) Probe(ctx context.Context) Probe {
	out, stderr, code, err := b.run(ctx, b.systemctlArgv(false, "show", b.s.Unit, "--no-pager",
		"--property=LoadState,ActiveState,SubState,Result"))
	if err != nil {
		return Probe{Life: LifeUnknown, Detail: err.Error()}
	}
	if code != 0 {
		return Probe{Life: LifeUnknown, Detail: "systemctl: " + firstNonEmpty(stderr, "exit status "+strconv.Itoa(code))}
	}
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	sub := props["SubState"]
	switch props["LoadState"] {
	case "not-found":
		return Probe{Life: LifeUnknown, Detail: fmt.Sprintf("systemd unit %s was not found", b.s.Unit)}
	case "masked":
		return Probe{Life: LifeUnknown, Detail: fmt.Sprintf("systemd unit %s is masked", b.s.Unit)}
	case "":
		return Probe{Life: LifeUnknown, Detail: "systemctl returned no unit state"}
	}
	switch props["ActiveState"] {
	case "active", "reloading", "refreshing":
		return Probe{Life: LifeAlive}
	case "activating":
		return Probe{Life: LifeAlive, Phase: PhaseStarting, Detail: "activating (" + sub + ")"}
	case "deactivating":
		return Probe{Life: LifeAlive, Phase: PhaseStopping, Detail: "deactivating (" + sub + ")"}
	case "inactive":
		return Probe{Life: LifeDead}
	case "failed":
		return Probe{Life: LifeDead, Phase: PhaseFailed, Detail: "unit failed (result: " + props["Result"] + ")"}
	}
	return Probe{Life: LifeUnknown, Detail: "unexpected ActiveState " + props["ActiveState"]}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (b *systemdBackend) verb(ctx context.Context, verb string, block bool) error {
	args := []string{verb}
	if !block {
		args = append(args, "--no-block")
	}
	_, stderr, code, err := b.run(ctx, b.systemctlArgv(true, append(args, b.s.Unit)...))
	if err != nil {
		return err
	}
	if code != 0 {
		msg := firstNonEmpty(stderr, "exit status "+strconv.Itoa(code))
		low := strings.ToLower(msg)
		if strings.Contains(low, "a password is required") || strings.Contains(low, "access denied") ||
			strings.Contains(low, "interactive authentication required") || strings.Contains(low, "not in the sudoers") {
			return fmt.Errorf("the service user is not permitted to %s %s (configure polkit or sudoers; see the documentation): %s", verb, b.s.Unit, lastLine(msg))
		}
		return fmt.Errorf("systemctl %s failed: %s", verb, lastLine(msg))
	}
	return nil
}

func (b *systemdBackend) Start(ctx context.Context) error { return b.verb(ctx, "start", false) }
func (b *systemdBackend) Stop(ctx context.Context, grace time.Duration) error {
	return b.verb(ctx, "stop", false)
}
func (b *systemdBackend) Kill(ctx context.Context) error { return ErrUnsupported }

func (b *systemdBackend) Restart(ctx context.Context, grace time.Duration) error {
	if !b.s.RestartAllowed() {
		return ErrUnsupported
	}
	// Blocking: returns once systemd has processed the restart job, so the
	// state observed afterwards belongs to the new instance.
	return b.verb(ctx, "restart", true)
}

func (b *systemdBackend) journalArgv(args ...string) []string {
	argv := []string{b.deps.Config.Get().Tools.Journalctl}
	if b.s.Scope == "user" {
		argv = append(argv, "--user", "--user-unit", b.s.Unit)
	} else {
		argv = append(argv, "--unit", b.s.Unit)
	}
	return append(append(argv, "--no-pager", "--output=cat"), args...)
}

func (b *systemdBackend) Snapshot(ctx context.Context, n int) ([]string, error) {
	out, stderr, code, err := b.run(ctx, b.journalArgv("--lines="+strconv.Itoa(n)))
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("journalctl failed: %s", lastLine(firstNonEmpty(stderr, "exit status "+strconv.Itoa(code))))
	}
	max := b.deps.Config.Get().Console.MaxLineBytes
	lines := strings.Split(strings.ToValidUTF8(out, "�"), "\n")
	for i, l := range lines {
		if len(l) > max {
			lines[i] = l[:max]
		}
	}
	return trimLines(lines, n), nil
}

// Follow runs `journalctl --follow` and feeds its lines into the console
// buffer until ctx ends. The journal survives unit restarts, so one follower
// covers stop/start cycles.
func (b *systemdBackend) Follow(ctx context.Context, opts FollowOptions) error {
	var args []string
	if !opts.Since.IsZero() {
		args = append(args, "--since=@"+strconv.FormatInt(opts.Since.Unix(), 10))
	} else {
		args = append(args, "--lines="+strconv.Itoa(opts.Tail))
	}
	argv := b.journalArgv(append(args, "--follow")...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = b.env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var errb tailWriter
	errb.max = 1024
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, describeStartError(argv[0], "", err))
	}
	go func() {
		<-ctx.Done()
		_ = cmd.Process.Kill()
	}()
	max := b.deps.Config.Get().Console.MaxLineBytes
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := strings.ToValidUTF8(sc.Text(), "�")
		if len(l) > max {
			l = l[:max]
		}
		b.deps.Console.Append(l)
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return nil
	}
	if msg := errb.String(); msg != "" {
		return fmt.Errorf("journalctl: %s", lastLine(msg))
	}
	if err != nil {
		return fmt.Errorf("journalctl exited: %v", err)
	}
	return errors.New("journalctl exited unexpectedly")
}

func (b *systemdBackend) SendInput(ctx context.Context, line string) error { return ErrUnsupported }
func (b *systemdBackend) Close()                                           {}

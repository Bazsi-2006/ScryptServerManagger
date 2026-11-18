package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/backend"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/console"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// Snapshot is a point-in-time view of recent console output.
type Snapshot struct {
	Lines []string `json:"lines"`
	// Source is "buffer" (the live capture buffer) or "backend" (fetched
	// from the backend's native log just now).
	Source string `json:"source"`
	// Note explains why no output is available, when that is the case.
	Note string `json:"note,omitempty"`
}

// ConsoleSnapshot returns up to n recent console lines.
func (m *Manager) ConsoleSnapshot(ctx context.Context, id string, n int) (*Snapshot, error) {
	s, err := m.get(id)
	if err != nil {
		return nil, err
	}
	cc := m.cfg.Get().Console
	if n <= 0 {
		n = cc.SnapshotLines
	}
	if n > cc.BufferLines {
		n = cc.BufferLines
	}
	_, be := s.snapshot()
	if be.Caps().LiveOnDemand && !s.followerActive() {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		lines, err := be.Snapshot(ctx, n)
		if err != nil {
			// Not being able to read logs is an expected condition (journal
			// permissions, daemon down); explain instead of failing.
			return &Snapshot{Lines: []string{}, Source: "backend", Note: "Console output is not available: " + err.Error()}, nil
		}
		if lines == nil {
			lines = []string{}
		}
		return &Snapshot{Lines: lines, Source: "backend"}, nil
	}
	buffered, _ := s.buf.Tail(n)
	out := make([]string, len(buffered))
	for i, l := range buffered {
		out[i] = l.Text
	}
	snap := &Snapshot{Lines: out, Source: "buffer"}
	if len(out) == 0 {
		snap.Note = "No console output has been captured yet."
	}
	return snap, nil
}

// ConsoleStream is a live console subscription.
type ConsoleStream struct {
	Backlog []console.Line
	Reset   bool
	Epoch   string
	Sub     *console.Subscriber
	close   func()
}

// Close ends the subscription and releases its resources.
func (c *ConsoleStream) Close() { c.close() }

// ConsoleSubscribe attaches to a server's live console. epoch/afterSeq come
// from the client's Last-Event-ID so a reconnect continues without gaps or
// duplicates when possible.
func (m *Manager) ConsoleSubscribe(id, epoch string, afterSeq uint64) (*ConsoleStream, error) {
	s, err := m.get(id)
	if err != nil {
		return nil, err
	}
	f := s.acquireFollower(time.Time{})
	backlog, reset, cur, sub := s.buf.Subscribe(epoch, afterSeq, m.cfg.Get().Console.SnapshotLines, 1024)
	buf := s.buf
	return &ConsoleStream{Backlog: backlog, Reset: reset, Epoch: cur, Sub: sub, close: func() {
		buf.Unsubscribe(sub)
		s.releaseFollower(f)
	}}, nil
}

// SendCommand delivers one console line to the server through its
// configured input channel. The text is data for the server's own console;
// it is never interpreted by a shell on the host.
func (m *Manager) SendCommand(ctx context.Context, id, line string) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	def, be := s.snapshot()
	max := m.cfg.Get().Console.MaxCommandLength
	switch {
	case strings.TrimSpace(line) == "":
		return &InputError{Msg: "the command is empty"}
	case len(line) > max:
		return &InputError{Msg: fmt.Sprintf("the command is longer than %d bytes", max)}
	case !utf8.ValidString(line):
		return &InputError{Msg: "the command is not valid UTF-8"}
	}
	for _, r := range line {
		if unicode.IsControl(r) {
			return &InputError{Msg: "the command must be a single line without control characters"}
		}
	}
	method := def.InputMethod()
	ctx, cancel := context.WithTimeout(ctx, def.Timeouts.Command.D())
	defer cancel()
	switch method {
	case servers.InputNone:
		return fmt.Errorf("%w: console input is not configured for this server", ErrUnsupported)
	case servers.InputCommand:
		argv, used := backend.SubstituteCommand(def.Console.InputCommand, line)
		spec := backend.RunSpec{Argv: argv, Dir: def.WorkingDir, Env: backend.ServerEnv(def), TailBytes: 8192}
		if !used {
			spec.Stdin = line + "\n"
		}
		s.buf.Append("> " + line)
		res, err := backend.Run(ctx, spec)
		for _, l := range console.CleanLines([]byte(res.Output), m.cfg.Get().Console.MaxLineBytes) {
			s.buf.Append(l)
		}
		if err != nil {
			if errors.Is(err, backend.ErrTimeout) {
				return fmt.Errorf("the command helper timed out after %s", def.Timeouts.Command.D())
			}
			return fmt.Errorf("the command helper failed: %w", err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("the command helper exited with code %d", res.ExitCode)
		}
		return nil
	}
	if method == servers.InputStdin || method == servers.InputDockerExec {
		// These channels do not echo what was sent.
		s.buf.Append("> " + line)
	}
	if err := be.SendInput(ctx, line); err != nil {
		if errors.Is(err, backend.ErrNotRunning) {
			return ErrNotRunning
		}
		return err
	}
	return nil
}

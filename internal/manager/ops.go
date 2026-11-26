package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/backend"
	backupfiles "github.com/Bazsi-2006/ScryptServerManagger/internal/backup"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/events"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/perm"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// Outcome statuses stored in the operations table.
const (
	statusSucceeded   = "succeeded"
	statusFailed      = "failed"
	statusCancelled   = "cancelled"
	statusInterrupted = "interrupted"
	// statusSubmitted: text was delivered to the server's console (tmux
	// backups and actions); whether the server acted on it is unknown.
	statusSubmitted = "submitted"
)

// result is what a lifecycle step reports back.
type result struct {
	state   State  // state to enter ("" = leave unchanged)
	message string // status message for that state
	status  string // operation outcome
	note    string // outcome description (error text or remark)
	unready bool   // the start did not reach readiness (process may be alive)
}

func ok(state State, note string) result {
	return result{state: state, message: note, status: statusSucceeded, note: note}
}

func failed(state State, msg string) result {
	return result{state: state, message: msg, status: statusFailed, note: msg}
}

var auditAction = map[string]string{
	OpStart: audit.ServerStart, OpStop: audit.ServerStop, OpRestart: audit.ServerRestart, OpBackup: audit.BackupCreate,
}

// begin reserves the server's single operation slot. This is the lock that
// prevents concurrent or conflicting operations. With preemptStart, a start
// in progress is cancelled so that a stop can take over (a server stuck in
// STARTING must remain stoppable).
func (s *server) begin(typ string, actor audit.Actor, preemptStart bool) (*operation, error) {
	s.mu.Lock()
	if s.deleted {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	if s.op != nil {
		prev := s.op
		if !(preemptStart && prev.info.Type == OpStart) {
			info := prev.info
			s.mu.Unlock()
			return nil, &BusyError{Op: info}
		}
		prev.preempted = true
		prev.cancel()
		s.mu.Unlock()
		select {
		case <-prev.done:
		case <-time.After(20 * time.Second):
			return nil, &BusyError{Op: prev.info}
		}
		s.mu.Lock()
		if s.op != nil {
			info := s.op.info
			s.mu.Unlock()
			return nil, &BusyError{Op: info}
		}
	}
	ctx, cancel := context.WithCancel(s.ctx)
	op := &operation{
		info:  OpInfo{Type: typ, User: actor.Username, StartedAt: time.Now().UTC()},
		actor: actor, ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	s.op = op
	s.mu.Unlock()
	return op, nil
}

// release abandons a reserved slot before the operation was launched.
func (s *server) release(op *operation) {
	s.mu.Lock()
	if s.op == op {
		s.op = nil
	}
	s.mu.Unlock()
	op.cancel()
	close(op.done)
}

// launch records the operation and announces it.
func (s *server) launch(op *operation) {
	row := &db.Operation{ServerID: s.id, Type: op.info.Type, Username: op.actor.Username, Status: "running", StartedAt: op.info.StartedAt}
	if op.actor.UserID != 0 {
		id := op.actor.UserID
		row.UserID = &id
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s.m.store.InsertOperation(ctx, row); err != nil {
		s.m.log.Error("could not record operation", "server", s.id, "error", err)
	}
	cancel()
	s.mu.Lock()
	op.info.ID = row.ID
	info := op.info
	s.mu.Unlock()
	s.m.audit.Record(op.actor, s.id, auditAction[info.Type], audit.Requested, nil)
	s.m.publish(events.Event{Type: "server.operation.started", Audience: events.AudienceServer, ServerID: s.id,
		Data: map[string]any{"server_id": s.id, "operation": info}})
}

// end applies the final state, frees the slot and records the outcome.
func (s *server) end(op *operation, r result) {
	s.mu.Lock()
	if s.op == op {
		s.op = nil
	}
	info := op.info
	if info.Type != OpBackup {
		s.unready = r.unready
	}
	prev := s.state
	if r.state != "" && (s.state != r.state || s.message != r.message) {
		s.setLocked(r.state, r.message)
	} else {
		s.publishStatusLocked() // the operation field changed
	}
	s.operationTransitionLocked(info, prev, r)
	s.mu.Unlock()

	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s.m.store.FinishOperation(ctx, info.ID, r.status, r.note, now); err != nil {
		s.m.log.Error("could not record operation result", "server", s.id, "error", err)
	}
	cancel()

	details := map[string]any{"duration_ms": now.Sub(info.StartedAt).Milliseconds()}
	if r.note != "" {
		details["message"] = r.note
	}
	evType, auditResult := "server.operation.completed", audit.Success
	switch r.status {
	case statusSucceeded:
	case statusSubmitted:
		auditResult = audit.Submitted
	default:
		evType, auditResult = "server.operation.failed", audit.Failure
		details["outcome"] = r.status
	}
	s.m.audit.Record(op.actor, s.id, auditAction[info.Type], auditResult, details)
	s.m.publish(events.Event{Type: evType, Audience: events.AudienceServer, ServerID: s.id,
		Data: map[string]any{"server_id": s.id, "operation": info, "status": r.status, "message": r.note}})
	if info.Type == OpBackup {
		s.m.publish(events.Event{Type: "server.backups.changed", Audience: events.AudienceServer, ServerID: s.id, Perm: perm.BackupDownload,
			Data: map[string]any{"server_id": s.id}})
	} else {
		s.m.cooldown.Mark(s.id)
	}
	op.cancel()
	close(op.done)
	s.poke()
}

// operationTransitionLocked reports the lifecycle change an operation caused.
func (s *server) operationTransitionLocked(info OpInfo, prev State, r result) {
	t := Transition{From: prev, To: s.state, Operation: info.Type, Actor: info.User, Detail: r.note}
	switch info.Type {
	case OpStart, OpRestart:
		switch {
		case r.status == statusSucceeded && r.state == Running:
			t.Kind = db.NotifyReady
		case r.status == statusFailed:
			t.Kind = db.NotifyFailure
		}
	case OpStop:
		switch {
		case r.status == statusSucceeded && r.state == Stopped:
			t.Kind = db.NotifyStopped
		case r.status == statusFailed:
			t.Kind = db.NotifyFailure
		}
	}
	if t.Kind != "" {
		s.emitLocked(t)
	}
}

func (s *server) snapshot() (*servers.Definition, backend.Backend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.def, s.be
}

func (m *Manager) checkCooldown(s *server) error {
	s.mu.Lock()
	interval := m.cfg.Get().RateLimits.ServerCooldown.D()
	if s.def.Cooldown != nil {
		interval = s.def.Cooldown.D()
	}
	s.mu.Unlock()
	if rem := m.cooldown.Remaining(s.id, interval); rem > 0 {
		return &CooldownError{Remaining: rem}
	}
	return nil
}

// probeNow performs a synchronous lifecycle check for request validation.
func (s *server) probeNow() (life, backend.Probe) {
	def, be := s.snapshot()
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer cancel()
	p := be.Probe(ctx)
	l, _, _ := resolve(ctx, def, p)
	return l, p
}

// StartServer begins starting a server and returns as soon as the operation has
// been accepted; progress is reported through status and events.
func (m *Manager) StartServer(id string, actor audit.Actor) (*OpInfo, error) {
	return m.StartServerWith(id, actor, StartOptions{})
}

// StartServerWith is StartServer with options. The concurrency limits are
// checked and a slot is reserved atomically before anything is executed.
func (m *Manager) StartServerWith(id string, actor audit.Actor, opts StartOptions) (*OpInfo, error) {
	s, err := m.get(id)
	if err != nil {
		return nil, err
	}
	_, be := s.snapshot()
	if !be.Caps().Start {
		return nil, fmt.Errorf("%w: this server has no start configuration", ErrUnsupported)
	}
	if err := m.checkCooldown(s); err != nil {
		return nil, err
	}
	op, err := s.begin(OpStart, actor, false)
	if err != nil {
		return nil, err
	}
	if l, _ := s.probeNow(); l == lifeAlive {
		s.release(op)
		s.poke()
		return nil, ErrAlreadyRunning
	}
	if err := m.reserve(s, actor, opts); err != nil {
		s.release(op)
		s.poke()
		return nil, err
	}
	s.launch(op)
	go func() { s.end(op, s.doStart(op)) }()
	info := op.info
	return &info, nil
}

// StopServer begins stopping a server. A start in progress is cancelled first.
func (m *Manager) StopServer(id string, actor audit.Actor) (*OpInfo, error) {
	s, err := m.get(id)
	if err != nil {
		return nil, err
	}
	if err := m.checkCooldown(s); err != nil {
		return nil, err
	}
	op, err := s.begin(OpStop, actor, true)
	if err != nil {
		return nil, err
	}
	l, p := s.probeNow()
	if l == lifeDead && (p.Life != backend.LifeExternal || s.status().State == Stopped) {
		s.release(op)
		s.poke()
		return nil, ErrNotRunning
	}
	s.launch(op)
	go func() { s.end(op, s.doStop(op)) }()
	info := op.info
	return &info, nil
}

// RestartServer restarts a running server (or starts a stopped one). Backends
// with a native restart use it; others stop and then start.
func (m *Manager) RestartServer(id string, actor audit.Actor) (*OpInfo, error) {
	return m.RestartServerWith(id, actor, StartOptions{})
}

// RestartServerWith is RestartServer with options. A running server keeps
// its slot through the restart; a stopped one needs a free slot.
func (m *Manager) RestartServerWith(id string, actor audit.Actor, opts StartOptions) (*OpInfo, error) {
	s, err := m.get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	allowed := s.restartAllowedLocked()
	caps := s.be.Caps()
	s.mu.Unlock()
	if !allowed {
		return nil, fmt.Errorf("%w: restart is disabled for this server", ErrUnsupported)
	}
	if !caps.Start {
		return nil, fmt.Errorf("%w: this server has no start configuration, so it cannot be restarted", ErrUnsupported)
	}
	if err := m.checkCooldown(s); err != nil {
		return nil, err
	}
	op, err := s.begin(OpRestart, actor, false)
	if err != nil {
		return nil, err
	}
	l, _ := s.probeNow()
	if l != lifeAlive {
		if err := m.reserve(s, actor, opts); err != nil {
			s.release(op)
			s.poke()
			return nil, err
		}
	}
	s.launch(op)
	go func() {
		if l != lifeAlive {
			s.end(op, s.doStart(op))
			return
		}
		if caps.NativeRestart {
			s.end(op, s.doNativeRestart(op))
			return
		}
		r := s.doStop(op)
		if r.status != statusSucceeded {
			s.end(op, r)
			return
		}
		s.end(op, s.doStart(op))
	}()
	info := op.info
	return &info, nil
}

func (s *server) opFlags(op *operation) (preempted, cancelled, interrupted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return op.preempted, op.cancelled, op.interrupted || s.m.ctx.Err() != nil
}

func (s *server) doStart(op *operation) result {
	def, be := s.snapshot()
	s.setState(Starting, "")
	timeout := def.Timeouts.Start.D()
	ctx, cancel := context.WithTimeout(op.ctx, timeout)
	defer cancel()

	var matched <-chan struct{}
	if def.Readiness.Method == servers.ReadyConsole {
		// Live output must be flowing before the server starts so the
		// readiness line cannot be missed, even with no viewers connected.
		f := s.acquireFollower(time.Now().Add(-time.Second))
		defer s.releaseFollower(f)
		mt := newMatcher(s.buf, def.Readiness.Console)
		defer mt.close()
		matched = mt.C
	}
	if err := be.Start(ctx); err != nil {
		if pre, _, intr := s.opFlags(op); pre || intr {
			return s.aborted(pre)
		}
		msg := err.Error()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg = fmt.Sprintf("startup timed out after %s: %s", timeout, msg)
		}
		return failed(Error, "start failed: "+msg)
	}
	return s.awaitReady(ctx, op, def, be, matched, timeout)
}

func (s *server) aborted(preempted bool) result {
	if preempted {
		return result{status: statusCancelled, note: "cancelled by a stop request"}
	}
	return result{status: statusInterrupted, note: "server-manager is shutting down"}
}

// awaitReady polls until the server is alive and its readiness condition
// holds, it dies, or the timeout expires.
func (s *server) awaitReady(ctx context.Context, op *operation, def *servers.Definition, be backend.Backend, matched <-chan struct{}, timeout time.Duration) result {
	interval := def.Readiness.Interval.D()
	if interval < 200*time.Millisecond {
		interval = 200 * time.Millisecond
	}
	// Backends that start asynchronously may briefly still report "dead".
	var grace time.Duration
	if def.Backend == servers.BackendSystemd || (def.Custom != nil && def.Custom.Mode == servers.ModeForking) {
		grace = 10 * time.Second
	}
	begun := time.Now()
	isMatched := false
	reason := "the readiness condition was not met"
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if ctx.Err() == nil {
			p := be.Probe(ctx)
			switch p.Life {
			case backend.LifeUnknown:
				reason = firstNonEmpty(p.Detail, "the server state could not be determined")
			case backend.LifeDead:
				if p.Phase == backend.PhaseFailed || time.Since(begun) >= grace {
					if ctx.Err() == nil {
						return failed(Error, "the server stopped during startup"+paren(p.Detail))
					}
				}
			case backend.LifeAlive, backend.LifeExternal:
				if p.Phase == backend.PhaseStarting {
					reason = firstNonEmpty(p.Detail, reason)
					break
				}
				ready := false
				switch def.Readiness.Method {
				case servers.ReadyConsole:
					ready = isMatched
					if !ready {
						reason = "the readiness text did not appear in the console output"
					}
				case servers.ReadyPort, servers.ReadyScript:
					okNow, err := checkReady(ctx, def)
					ready = okNow && err == nil
					if err != nil {
						reason = err.Error()
					} else if !okNow {
						reason = "the " + def.Readiness.Method + " readiness check did not succeed"
					}
				default:
					ready = true
				}
				if ready && ctx.Err() == nil {
					return ok(Running, "")
				}
			}
		}
		select {
		case <-ctx.Done():
			if pre, _, intr := s.opFlags(op); pre || intr {
				return s.aborted(pre)
			}
			r := failed(Error, fmt.Sprintf("startup timed out after %s: %s", timeout, reason))
			r.unready = true
			return r
		case <-matched:
			isMatched = true
			matched = nil
		case <-tick.C:
		}
	}
}

func paren(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func isDead(ctx context.Context, def *servers.Definition, be backend.Backend) bool {
	p := be.Probe(ctx)
	l, _, _ := resolve(ctx, def, p)
	return l == lifeDead
}

func (s *server) doStop(op *operation) result {
	def, be := s.snapshot()
	s.setState(Stopping, "")
	grace := def.Timeouts.Stop.D()
	ctx, cancel := context.WithTimeout(op.ctx, grace)
	defer cancel()

	err := be.Stop(ctx, grace)
	switch {
	case err == nil:
	case errors.Is(err, backend.ErrNotRunning):
		return ok(Stopped, "")
	case ctx.Err() != nil:
		// The stop command itself ran out of time; fall through to the
		// timeout handling below.
	default:
		return failed(Error, "stop failed: "+err.Error())
	}

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for ctx.Err() == nil {
		if isDead(ctx, def, be) && ctx.Err() == nil {
			return ok(Stopped, "")
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
	if _, _, intr := s.opFlags(op); intr {
		return s.aborted(false)
	}
	if !be.Caps().ForceKill {
		return failed(Error, fmt.Sprintf("the server did not stop within %s and is still running", grace))
	}
	kctx, kcancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer kcancel()
	if err := be.Kill(kctx); err != nil {
		return failed(Error, fmt.Sprintf("the server did not stop within %s and could not be killed: %v", grace, err))
	}
	for kctx.Err() == nil {
		if isDead(kctx, def, be) {
			return ok(Stopped, fmt.Sprintf("the server did not stop within %s and was killed", grace))
		}
		select {
		case <-kctx.Done():
		case <-time.After(300 * time.Millisecond):
		}
	}
	return failed(Error, fmt.Sprintf("the server did not stop within %s and survived a kill attempt", grace))
}

func (s *server) doNativeRestart(op *operation) result {
	def, be := s.snapshot()
	s.setState(Stopping, "restarting")
	stopT, startT := def.Timeouts.Stop.D(), def.Timeouts.Start.D()
	ctx, cancel := context.WithTimeout(op.ctx, stopT+startT)
	defer cancel()

	var matched <-chan struct{}
	if def.Readiness.Method == servers.ReadyConsole {
		f := s.acquireFollower(time.Now().Add(-time.Second))
		defer s.releaseFollower(f)
		mt := newMatcher(s.buf, def.Readiness.Console)
		defer mt.close()
		matched = mt.C
	}
	// Native restarts block until the backend has brought the service back
	// up, so the state seen afterwards belongs to the new instance.
	if err := be.Restart(ctx, stopT); err != nil {
		if _, _, intr := s.opFlags(op); intr {
			return s.aborted(false)
		}
		return failed(Error, "restart failed: "+err.Error())
	}
	s.setState(Starting, "")
	return s.awaitReady(ctx, op, def, be, matched, stopT+startT)
}

// Backup runs the configured backup command as a tracked operation.
func (m *Manager) Backup(id string, actor audit.Actor) (*OpInfo, error) {
	s, err := m.get(id)
	if err != nil {
		return nil, err
	}
	def, be := s.snapshot()
	if !def.Backup.CanCreate() {
		return nil, fmt.Errorf("%w: no backup command is configured for this server", ErrUnsupported)
	}
	if def.Backup.Method == servers.MethodTmux {
		// Check up front so the user gets the reason right away instead of
		// a failed operation.
		if err := s.requireTmuxTarget(be, "backup command"); err != nil {
			return nil, err
		}
	}
	op, err := s.begin(OpBackup, actor, false)
	if err != nil {
		return nil, err
	}
	s.launch(op)
	s.mu.Lock()
	s.publishStatusLocked()
	s.mu.Unlock()
	go func() { s.end(op, s.doBackup(op, def, be)) }()
	info := op.info
	return &info, nil
}

// requireTmuxTarget checks that predefined text can be typed into the
// server's tmux session right now.
func (s *server) requireTmuxTarget(be backend.Backend, what string) error {
	if _, ok := be.(backend.TextSender); !ok {
		return fmt.Errorf("%w: this server's backend cannot receive a %s", ErrUnsupported, what)
	}
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer cancel()
	switch p := be.Probe(ctx); p.Life {
	case backend.LifeAlive:
		return nil
	case backend.LifeDead:
		session := ""
		s.mu.Lock()
		if t := s.def.Tmux; t != nil {
			session = t.Session
		}
		s.mu.Unlock()
		return fmt.Errorf("%w: tmux session %q is not running, so the %s cannot be sent", ErrNotRunning, session, what)
	default:
		return fmt.Errorf("%w: %s", backend.ErrUnavailable, firstNonEmpty(p.Detail, "the tmux session cannot be reached"))
	}
}

func (s *server) doBackup(op *operation, def *servers.Definition, be backend.Backend) result {
	if def.Backup.Method == servers.MethodTmux {
		return s.doTmuxBackup(op, def, be)
	}
	timeout := def.Timeouts.Backup.D()
	ctx, cancel := context.WithTimeout(op.ctx, timeout)
	defer cancel()
	res, err := backend.Run(ctx, backend.RunSpec{
		Argv: def.Backup.Command.Argv(), Dir: def.WorkingDir,
		Env:       backend.ServerEnv(def, "SM_BACKUP_DIR="+def.Backup.Directory),
		TailBytes: 2000,
	})
	if err != nil {
		if errors.Is(err, backend.ErrTimeout) {
			_, cancelled, intr := s.opFlags(op)
			switch {
			case cancelled:
				return result{status: statusCancelled, note: "the backup was cancelled"}
			case intr:
				return result{status: statusInterrupted, note: "server-manager is shutting down"}
			}
			return result{status: statusFailed, note: fmt.Sprintf("the backup command timed out after %s and was killed", timeout)}
		}
		return result{status: statusFailed, note: "backup failed: " + err.Error()}
	}
	if res.ExitCode != 0 {
		note := fmt.Sprintf("the backup command exited with code %d", res.ExitCode)
		if res.Output != "" {
			note += ": " + tailLine(res.Output)
		}
		return result{status: statusFailed, note: note}
	}
	return result{status: statusSucceeded, note: "backup completed"}
}

// doTmuxBackup types the configured backup command into the server's tmux
// pane. Delivery is not completion: without a completion pattern the result
// is "submitted"; with one, the operation waits for that console line.
func (s *server) doTmuxBackup(op *operation, def *servers.Definition, be backend.Backend) result {
	b := def.Backup
	ts, ok := be.(backend.TextSender)
	if !ok {
		return result{status: statusFailed, note: "this server's backend cannot receive a backup command"}
	}
	var matched <-chan struct{}
	if b.Completion != nil {
		// Watch before sending so a fast confirmation cannot be missed;
		// output from earlier runs is never considered.
		mt := newMatcher(s.buf, b.Completion)
		defer mt.close()
		matched = mt.C
	}
	baseline := ""
	if b.Directory != "" {
		baseline = dirSignature(b.Directory)
	}
	sctx, cancel := context.WithTimeout(op.ctx, 20*time.Second)
	err := ts.SendText(sctx, b.Tmux.Text, b.Tmux.PressEnter())
	cancel()
	if err != nil {
		if _, cancelled, intr := s.opFlags(op); intr {
			return result{status: statusInterrupted, note: "server-manager is shutting down"}
		} else if cancelled {
			return result{status: statusCancelled, note: "the backup was cancelled"}
		}
		return result{status: statusFailed, note: "the backup command could not be sent: " + err.Error()}
	}
	timeout := def.Timeouts.Backup.D()
	if b.Directory != "" {
		s.watchBackupDir(b.Directory, baseline, timeout)
	}
	if matched == nil {
		return result{status: statusSubmitted, note: "the backup command was sent to the tmux session; whether the backup has finished cannot be verified (new files appear in the list as soon as they are written)"}
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-matched:
		return result{status: statusSucceeded, note: "backup completed (confirmed by the console output)"}
	case <-op.ctx.Done():
		if _, _, intr := s.opFlags(op); intr {
			return result{status: statusInterrupted, note: "server-manager is shutting down"}
		}
		return result{status: statusCancelled, note: "stopped waiting for the completion message; the server may still finish the backup"}
	case <-t.C:
		return result{status: statusFailed, note: fmt.Sprintf("the backup command was sent, but the completion message did not appear within %s", timeout)}
	}
}

// backupWatchInterval is how often a watched backup directory is checked.
var backupWatchInterval = 2 * time.Second

// watchBackupDir tells clients when files in the backup directory change
// after a backup was requested whose completion cannot be observed
// directly. It stops once the directory has been quiet for a minute after a
// change, after maxWait, or when the server goes away.
func (s *server) watchBackupDir(dir, baseline string, maxWait time.Duration) {
	s.mu.Lock()
	if s.stopWatch != nil {
		s.stopWatch()
	}
	ctx, cancel := context.WithTimeout(s.ctx, maxWait)
	s.stopWatch = cancel
	s.mu.Unlock()
	go func() {
		defer cancel()
		sig := baseline
		var lastChange time.Time
		t := time.NewTicker(backupWatchInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if now := dirSignature(dir); now != sig {
				sig, lastChange = now, time.Now()
				s.m.NotifyBackupsChanged(s.id)
			} else if !lastChange.IsZero() && time.Since(lastChange) > time.Minute {
				return
			}
		}
	}()
}

// dirSignature summarises the backup files (names, sizes, times).
func dirSignature(dir string) string {
	files, err := backupfiles.List(dir)
	if err != nil {
		return "unavailable"
	}
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "%s|%d|%d\n", f.Name, f.Size, f.ModTime.UnixNano())
	}
	return b.String()
}

func tailLine(s string) string {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			s = s[i+1:]
			break
		}
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// CancelBackup aborts a running backup operation.
func (m *Manager) CancelBackup(id string) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.op == nil || s.op.info.Type != OpBackup {
		return ErrNotRunning
	}
	s.op.cancelled = true
	s.op.cancel()
	return nil
}

// stopForExit runs at manager shutdown: operations are interrupted, and
// servers that cannot survive without the manager are stopped.
func (s *server) stopForExit(ctx context.Context) {
	s.mu.Lock()
	op := s.op
	if op != nil {
		op.interrupted = true
		op.cancel()
	}
	def, be := s.def, s.be
	s.mu.Unlock()
	if op != nil {
		select {
		case <-op.done:
		case <-time.After(10 * time.Second):
		}
	}
	caps := be.Caps()
	if !caps.StopOnManagerExit {
		return
	}
	if isDead(ctx, def, be) {
		return
	}
	s.m.log.Info("stopping server because the manager is exiting", "server", s.id)
	grace := def.Timeouts.Stop.D()
	sctx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	if err := be.Stop(sctx, grace); err != nil && !errors.Is(err, backend.ErrNotRunning) {
		s.m.log.Warn("stop at exit failed", "server", s.id, "error", err)
	}
	for sctx.Err() == nil {
		if isDead(sctx, def, be) {
			return
		}
		select {
		case <-sctx.Done():
		case <-time.After(300 * time.Millisecond):
		}
	}
	if caps.ForceKill {
		kctx, kcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer kcancel()
		_ = be.Kill(kctx)
	}
}

// Operations returns the recent operation history of a server.
func (m *Manager) Operations(ctx context.Context, id string, limit int) ([]*db.Operation, error) {
	if _, err := m.get(id); err != nil {
		return nil, err
	}
	return m.store.ListOperations(ctx, id, limit)
}

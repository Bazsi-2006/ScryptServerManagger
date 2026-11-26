package manager

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/backend"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/console"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/events"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

type operation struct {
	info   OpInfo
	actor  audit.Actor
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// flags, guarded by server.mu
	preempted   bool // a start superseded by a stop request
	cancelled   bool // cancelled by a user (backups)
	interrupted bool // manager is shutting down
}

type follower struct {
	refs   int
	cancel context.CancelFunc
	timer  *time.Timer
}

// server is the runtime state of one definition.
type server struct {
	m      *Manager
	id     string
	buf    *console.Buffer
	wake   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	def     *servers.Definition
	be      backend.Backend
	state   State
	message string
	since   time.Time
	op      *operation
	deleted bool
	follow  *follower
	// unready is set when a start failed to reach readiness while the
	// process stayed alive. Until the next operation (or the process dying),
	// only a verifiable check (port/script) may promote it to RUNNING;
	// console/none readiness cannot be re-evaluated and must not be assumed.
	unready bool

	// occupied: the server holds a concurrency slot (see capacity.go).
	occupied bool
	// probed is set after the first completed observation; firstProbe is
	// closed at the same moment.
	probed     bool
	firstProbe chan struct{}
	// lastKnown is the last state other than UNKNOWN.
	lastKnown State
	// actions holds the custom actions currently running, by action ID.
	actions map[string]*runningAction
	// stopWatch ends the backup directory watcher, if one runs.
	stopWatch context.CancelFunc
}

func newServer(m *Manager, def *servers.Definition) *server {
	ctx, cancel := context.WithCancel(m.ctx)
	s := &server{m: m, id: def.ID, buf: m.newBuffer(), wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel,
		def: def, state: Unknown, message: "checking current state", since: time.Now(),
		firstProbe: make(chan struct{}), actions: map[string]*runningAction{}}
	s.be = s.newBackend(def)
	return s
}

func (s *server) newBackend(def *servers.Definition) backend.Backend {
	id := def.ID
	deps := backend.Deps{
		Config:  s.m.cfg,
		Console: s.buf,
		Log:     s.m.log,
		LoadRuntime: func() string {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			data, _ := s.m.store.GetRuntime(ctx, id)
			return data
		},
		SaveRuntime: func(data string) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var err error
			if data == "" {
				err = s.m.store.DeleteRuntime(ctx, id)
			} else {
				err = s.m.store.SetRuntime(ctx, id, data)
			}
			if err != nil {
				s.m.log.Error("could not persist runtime state", "server", id, "error", err)
			}
		},
	}
	be, err := backend.New(def, deps)
	if err != nil {
		// Unreachable for validated definitions; fail safe.
		s.m.log.Error("could not create backend", "server", id, "error", err)
		return unavailableBackend{reason: err.Error()}
	}
	return be
}

// rebuildLocked swaps in a new definition and a fresh backend. Only called
// while the server is inactive.
func (s *server) rebuildLocked(def *servers.Definition) {
	if f := s.follow; f != nil {
		if f.cancel != nil {
			f.cancel()
		}
		if f.timer != nil {
			f.timer.Stop()
		}
		s.follow = nil
	}
	s.be.Close()
	// Viewers reconnect and pick up the new backend's output.
	s.buf.CloseAll()
	s.buf.Reset()
	s.def = def
	s.be = s.newBackend(def)
}

// shutdown releases everything held for this server without stopping it.
func (s *server) shutdown() {
	s.cancel()
	s.mu.Lock()
	if f := s.follow; f != nil && f.cancel != nil {
		f.cancel()
	}
	if s.stopWatch != nil {
		s.stopWatch()
		s.stopWatch = nil
	}
	be := s.be
	s.mu.Unlock()
	s.buf.CloseAll()
	be.Close()
}

func (s *server) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *server) statusLocked() Status {
	st := Status{ServerID: s.id, State: s.state, Message: s.message, Since: s.since}
	if s.op != nil {
		i := s.op.info
		st.Operation = &i
	}
	return st
}

func (s *server) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *server) publishStatusLocked() {
	s.m.publish(events.Event{Type: "server.status.changed", Audience: events.AudienceServer, ServerID: s.id, Data: s.statusLocked()})
}

// setLocked updates the state and notifies subscribers if anything changed.
// Active states take a concurrency slot; only STOPPED (a confirmed stop)
// releases it. ERROR and UNKNOWN leave occupancy as it was.
func (s *server) setLocked(state State, message string) {
	switch {
	case isActive(state):
		s.setOccupiedLocked(true)
	case state == Stopped:
		s.setOccupiedLocked(false)
	}
	if state != Unknown {
		s.lastKnown = state
	}
	if s.state == state && s.message == message {
		return
	}
	if s.state != state {
		s.since = time.Now()
		s.m.log.Info("server state changed", "server", s.id, "from", string(s.state), "to", string(state), "detail", message)
	}
	s.state, s.message = state, message
	s.publishStatusLocked()
}

func (s *server) setState(state State, message string) {
	s.mu.Lock()
	s.setLocked(state, message)
	s.mu.Unlock()
}

func (s *server) restartAllowedLocked() bool {
	switch s.def.Backend {
	case servers.BackendSystemd:
		return s.def.Systemd.RestartAllowed()
	case servers.BackendDocker:
		return s.def.Docker.RestartAllowed()
	}
	return true
}

func (s *server) pollInterval() time.Duration {
	s.mu.Lock()
	d := s.def.StatusPollInterval.D()
	s.mu.Unlock()
	if d <= 0 {
		d = s.m.cfg.Get().Monitoring.StatusPollInterval.D()
	}
	if d < time.Second {
		d = time.Second
	}
	return d
}

// monitor keeps the state current for the lifetime of the server entry,
// whether or not anyone is watching.
func (s *server) monitor() {
	defer s.m.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		case <-timer.C:
		}
		s.observe()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(s.pollInterval())
	}
}

type life int

const (
	lifeUnknown life = iota
	lifeDead
	lifeAlive
)

// checkReady evaluates the stateless readiness checks (port, script).
// Console and "none" readiness cannot be evaluated after the fact and count
// as satisfied here.
func checkReady(ctx context.Context, def *servers.Definition) (bool, error) {
	switch def.Readiness.Method {
	case servers.ReadyPort:
		return backend.CheckPort(ctx, def.Readiness.Port)
	case servers.ReadyScript:
		return backend.CheckScript(ctx, def, def.Readiness.Script)
	}
	return true, nil
}

// verifiable reports whether readiness can be checked at any time.
func verifiable(def *servers.Definition) bool {
	return def.Readiness.Method == servers.ReadyPort || def.Readiness.Method == servers.ReadyScript
}

// resolve turns a backend probe into alive/dead/unknown. Backends without a
// native lifecycle signal (LifeExternal) are judged by the configured
// external check; external reports whether that happened.
func resolve(ctx context.Context, def *servers.Definition, p backend.Probe) (l life, detail string, external bool) {
	switch p.Life {
	case backend.LifeAlive:
		return lifeAlive, p.Detail, false
	case backend.LifeDead:
		return lifeDead, p.Detail, false
	case backend.LifeExternal:
		ok, err := checkReady(ctx, def)
		if err != nil {
			return lifeUnknown, err.Error(), true
		}
		if ok {
			return lifeAlive, "", true
		}
		return lifeDead, "", true
	}
	detail = p.Detail
	if detail == "" {
		detail = "the server state cannot be determined"
	}
	return lifeUnknown, detail, false
}

// observe reconciles the recorded state with reality. It runs when no
// lifecycle operation is in progress (those drive the state themselves).
func (s *server) observe() {
	s.mu.Lock()
	if s.deleted || (s.op != nil && s.op.info.Type != OpBackup) {
		s.mu.Unlock()
		return
	}
	be, def, cur := s.be, s.def, s.state
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	p := be.Probe(ctx)
	l, detail, external := resolve(ctx, def, p)
	if s.ctx.Err() != nil {
		return
	}
	ready, readyDetail := true, ""
	if l == lifeAlive && !external && p.Phase == "" && cur != Running {
		ok, err := checkReady(ctx, def)
		ready = ok && err == nil
		if err != nil {
			readyDetail = err.Error()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.be != be || s.deleted || (s.op != nil && s.op.info.Type != OpBackup) {
		return // superseded while probing
	}
	defer s.markProbedLocked()
	prev, prevKnown := s.state, s.lastKnown
	switch l {
	case lifeUnknown:
		// Inconclusive: neither the state nor the concurrency slot changes
		// meaning; the last conclusive observation stands.
		s.setLocked(Unknown, detail)
	case lifeDead:
		s.unready = false
		s.setOccupiedLocked(false)
		failed := p.Phase == backend.PhaseFailed
		lostWhileUp := prev == Unknown && s.probed && (prevKnown == Running || prevKnown == Starting)
		switch {
		case failed && prev != Stopped && prev != Error:
			s.setLocked(Error, firstNonEmpty(detail, "the server failed"))
		case prev == Error || prev == Stopped:
			// Keep the message that explains how we got here.
		case prev == Running || prev == Starting || prev == Stopping || lostWhileUp:
			msg := "the server stopped outside of the manager"
			if lostWhileUp {
				msg = "the server stopped while its state could not be monitored"
			}
			if detail != "" {
				msg += " (" + detail + ")"
			}
			s.setLocked(Stopped, msg)
		default:
			s.setLocked(Stopped, "")
		}
		s.observedTransitionLocked(prev, prevKnown, s.state, failed, detail)
	case lifeAlive:
		s.setOccupiedLocked(true)
		defer func() { s.observedTransitionLocked(prev, prevKnown, s.state, false, detail) }()
		switch {
		case p.Phase == backend.PhaseStopping:
			s.setLocked(Stopping, detail)
		case p.Phase == backend.PhaseStarting:
			if s.state != Error {
				s.setLocked(Starting, detail)
			}
		case s.state == Running:
			s.setLocked(Running, detail)
		case ready && !(s.unready && !verifiable(def)):
			s.unready = false
			s.setLocked(Running, detail)
		case s.state == Error:
			// A failed start whose process lingers stays ERROR until it
			// either becomes ready or is stopped.
		default:
			s.setLocked(Starting, firstNonEmpty(readyDetail, "waiting for the readiness check to pass"))
		}
	}
}

// markProbedLocked records that the server's real state is known.
func (s *server) markProbedLocked() {
	if !s.probed {
		s.probed = true
		close(s.firstProbe)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---- console readiness matcher ----

type matcher struct {
	C    chan struct{}
	stop chan struct{}
	once sync.Once
}

// newMatcher watches console output produced from now on and closes C when
// a line matches. Output that predates the call is never considered, which
// is what separates this startup attempt from earlier ones.
func newMatcher(buf *console.Buffer, cm *servers.ConsoleMatch) *matcher {
	m := &matcher{C: make(chan struct{}), stop: make(chan struct{})}
	match := func(line string) bool { return strings.Contains(line, cm.Pattern) }
	if cm.Regex {
		if re, err := regexp.Compile(cm.Pattern); err == nil {
			match = re.MatchString
		}
	}
	_, _, _, sub := buf.Subscribe(buf.Epoch(), buf.LastSeq(), 1, 2048)
	go func() {
		for {
			select {
			case <-m.stop:
				buf.Unsubscribe(sub)
				return
			case ev, ok := <-sub.C:
				if !ok {
					// Dropped for lagging (or buffer closed): resubscribe.
					select {
					case <-m.stop:
						return
					case <-time.After(50 * time.Millisecond):
					}
					_, _, _, sub = buf.Subscribe(buf.Epoch(), buf.LastSeq(), 1, 2048)
					continue
				}
				if ev.Reset {
					continue
				}
				if match(ev.Line.Text) {
					close(m.C)
					buf.Unsubscribe(sub)
					return
				}
			}
		}
	}()
	return m
}

func (m *matcher) close() { m.once.Do(func() { close(m.stop) }) }

// ---- on-demand log followers (systemd, docker) ----

// acquireFollower makes sure live output is flowing into the buffer for
// backends that only stream on demand. since selects where a newly started
// follower begins (zero = preload recent lines).
func (s *server) acquireFollower(since time.Time) *follower {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.be.Caps().LiveOnDemand {
		return nil
	}
	f := s.follow
	if f == nil {
		f = &follower{}
		s.follow = f
	}
	f.refs++
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
	}
	if f.cancel == nil {
		ctx, cancel := context.WithCancel(s.ctx)
		f.cancel = cancel
		s.buf.Reset()
		opts := backend.FollowOptions{Tail: s.m.cfg.Get().Console.SnapshotLines, Since: since}
		go s.runFollower(ctx, s.be, opts)
	}
	return f
}

func (s *server) releaseFollower(f *follower) {
	if f == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f.refs--
	if f.refs > 0 || s.follow != f {
		return
	}
	// Keep the stream for a short while: viewers often reconnect.
	f.timer = time.AfterFunc(30*time.Second, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if f.refs == 0 && f.cancel != nil {
			f.cancel()
			f.cancel = nil
		}
	})
}

func (s *server) followerActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.follow != nil && s.follow.cancel != nil
}

func (s *server) runFollower(ctx context.Context, be backend.Backend, opts backend.FollowOptions) {
	last := ""
	for {
		err := be.Follow(ctx, opts)
		if ctx.Err() != nil {
			return
		}
		msg := "[server-manager] live console output is unavailable"
		if err != nil {
			msg += ": " + err.Error()
		}
		if msg != last {
			s.buf.Append(msg)
			last = msg
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		opts = backend.FollowOptions{Since: time.Now()}
	}
}

// unavailableBackend stands in when a backend cannot be constructed.
type unavailableBackend struct{ reason string }

func (u unavailableBackend) Caps() backend.Caps { return backend.Caps{} }
func (u unavailableBackend) Probe(context.Context) backend.Probe {
	return backend.Probe{Life: backend.LifeUnknown, Detail: u.reason}
}
func (u unavailableBackend) Start(context.Context) error               { return backend.ErrUnavailable }
func (u unavailableBackend) Stop(context.Context, time.Duration) error { return backend.ErrUnavailable }
func (u unavailableBackend) Kill(context.Context) error                { return backend.ErrUnavailable }
func (u unavailableBackend) Restart(context.Context, time.Duration) error {
	return backend.ErrUnavailable
}
func (u unavailableBackend) Snapshot(context.Context, int) ([]string, error) {
	return nil, backend.ErrUnavailable
}
func (u unavailableBackend) Follow(context.Context, backend.FollowOptions) error {
	return backend.ErrUnavailable
}
func (u unavailableBackend) SendInput(context.Context, string) error { return backend.ErrUnavailable }
func (u unavailableBackend) Close()                                  {}

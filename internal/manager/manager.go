// Package manager owns the runtime side of every configured server: its
// backend instance, lifecycle state machine, operation locking, background
// status monitoring and console buffer.
package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/backend"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/console"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/events"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/perm"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/ratelimit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// State is the lifecycle state shown to users.
type State string

const (
	Stopped  State = "STOPPED"
	Starting State = "STARTING"
	Running  State = "RUNNING"
	Stopping State = "STOPPING"
	Error    State = "ERROR"
	Unknown  State = "UNKNOWN"
)

// Operation types.
const (
	OpStart   = "start"
	OpStop    = "stop"
	OpRestart = "restart"
	OpBackup  = "backup"
)

// OpInfo describes an operation in progress.
type OpInfo struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	User      string    `json:"user"`
	StartedAt time.Time `json:"started_at"`
}

// Status is a snapshot of a server's runtime state.
type Status struct {
	ServerID  string    `json:"server_id"`
	State     State     `json:"state"`
	Message   string    `json:"message"`
	Since     time.Time `json:"since"`
	Operation *OpInfo   `json:"operation"`
}

// Capabilities tells clients which actions exist for a server, independent
// of the caller's permissions.
type Capabilities struct {
	Start        bool `json:"start"`
	Stop         bool `json:"stop"`
	Restart      bool `json:"restart"`
	ConsoleInput bool `json:"console_input"`
	Backups      bool `json:"backups"`
	BackupCreate bool `json:"backup_create"`
	// BackupMethod is "command" or "tmux" when backups can be created.
	BackupMethod string `json:"backup_method,omitempty"`
}

// Errors returned by manager operations. The API layer maps them to
// distinct HTTP responses.
var (
	ErrNotFound       = servers.ErrNotFound
	ErrAlreadyRunning = errors.New("the server is already running")
	ErrNotRunning     = errors.New("the server is not running")
	ErrUnsupported    = backend.ErrUnsupported
	ErrActive         = errors.New("the server is running or busy; stop it first")
)

// BusyError: another operation holds the server.
type BusyError struct{ Op OpInfo }

func (e *BusyError) Error() string {
	return fmt.Sprintf("a %s operation is already in progress for this server", e.Op.Type)
}

// CooldownError: the per-server cooldown has not elapsed.
type CooldownError struct{ Remaining time.Duration }

func (e *CooldownError) Error() string {
	return fmt.Sprintf("this server was operated on a moment ago; try again in %s", e.Remaining.Round(time.Second))
}

// InputError: a console command was rejected before delivery.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

// Publisher receives events (implemented by events.Hub).
type Publisher interface{ Publish(events.Event) }

type Manager struct {
	cfg   *config.Holder
	defs  *servers.Store
	store *db.Store
	pub   Publisher
	audit *audit.Recorder
	log   *slog.Logger

	cooldown *ratelimit.Cooldown

	mu      sync.RWMutex
	servers map[string]*server

	// capMu serialises concurrency-limit checks with their reservations.
	capMu sync.Mutex
	// editMu serialises read-modify-write edits of definitions (actions).
	editMu sync.Mutex
	// onTransition receives lifecycle transitions (notifications).
	onTransition func(Transition)

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	actionsWG sync.WaitGroup
}

func New(cfg *config.Holder, defs *servers.Store, store *db.Store, pub Publisher, rec *audit.Recorder, log *slog.Logger) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{cfg: cfg, defs: defs, store: store, pub: pub, audit: rec, log: log,
		cooldown: ratelimit.NewCooldown(), servers: map[string]*server{}, ctx: ctx, cancel: cancel}
}

// Load creates runtime entries for every stored definition and begins
// monitoring. Each server's real state is reconciled by its first probe;
// nothing recorded by a previous process is trusted.
func (m *Manager) Load() {
	if err := m.store.InterruptRunningOperations(m.ctx, time.Now()); err != nil {
		m.log.Error("could not mark interrupted operations", "error", err)
	}
	for _, def := range m.defs.List() {
		m.pruneActionGrants(def)
		m.add(def)
	}
}

func (m *Manager) add(def *servers.Definition) *server {
	s := newServer(m, def)
	m.mu.Lock()
	m.servers[def.ID] = s
	m.mu.Unlock()
	m.wg.Add(1)
	go s.monitor()
	return s
}

func (m *Manager) get(id string) (*server, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.servers[id]
	if !ok {
		return nil, ErrNotFound
	}
	return s, nil
}

// Status returns the current status of one server.
func (m *Manager) Status(id string) (Status, error) {
	s, err := m.get(id)
	if err != nil {
		return Status{}, err
	}
	return s.status(), nil
}

// Capabilities reports the actions available for a server.
func (m *Manager) Capabilities(id string) (Capabilities, error) {
	s, err := m.get(id)
	if err != nil {
		return Capabilities{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	caps := s.be.Caps()
	_, sendsText := s.be.(backend.TextSender)
	b := s.def.Backup
	c := Capabilities{
		Start:        caps.Start,
		Stop:         true,
		Restart:      caps.Start && s.restartAllowedLocked(),
		ConsoleInput: s.def.InputMethod() != servers.InputNone,
		Backups:      b != nil && b.Directory != "",
		BackupCreate: b.CanCreate() && (b.Method != servers.MethodTmux || sendsText),
		BackupMethod: "",
	}
	if c.BackupCreate {
		c.BackupMethod = b.Method
	}
	return c, nil
}

// Definition returns a copy of the live definition.
func (m *Manager) Definition(id string) (*servers.Definition, error) {
	return m.defs.Get(id)
}

func (m *Manager) Definitions() []*servers.Definition { return m.defs.List() }

func (m *Manager) publish(ev events.Event) {
	if m.pub != nil {
		m.pub.Publish(ev)
	}
}

// ---- definition management ----

// Create stores a new definition and starts managing it.
func (m *Manager) Create(def *servers.Definition) (*servers.Definition, error) {
	saved, err := m.defs.Create(def)
	if err != nil {
		return nil, err
	}
	m.add(saved)
	m.publish(events.Event{Type: "server.created", Audience: events.AudienceServer, ServerID: saved.ID, Data: map[string]any{"server_id": saved.ID}})
	return saved, nil
}

// Update replaces a definition. Changes to anything that affects execution
// are refused while the server is running or an operation is in progress.
func (m *Manager) Update(def *servers.Definition) (*servers.Definition, error) {
	s, err := m.get(def.ID)
	if err != nil {
		return nil, err
	}
	next := def.Clone()
	next.Normalize()
	if err := next.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	active := s.op != nil || s.state == Starting || s.state == Running || s.state == Stopping
	if active && !servers.RuntimeEqual(s.def, next) {
		s.mu.Unlock()
		return nil, ErrActive
	}
	saved, err := m.defs.Update(next)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if active || onlyOnDemandChanged(s.def, saved) {
		// Nothing that controls the process changed (presentation,
		// timeouts, backups, custom actions); keep the live backend.
		s.def = saved
	} else {
		s.rebuildLocked(saved)
	}
	s.mu.Unlock()
	m.pruneActionGrants(saved)
	s.poke()
	m.publish(events.Event{Type: "server.updated", Audience: events.AudienceServer, ServerID: saved.ID, Data: map[string]any{"server_id": saved.ID}})
	return saved, nil
}

// Delete removes a definition from management. The server's own files are
// never touched. Unless force is set, a running server must be stopped
// first; with force it is detached and left running.
func (m *Manager) Delete(id string, force bool) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.op != nil {
		op := s.op.info
		s.mu.Unlock()
		return &BusyError{Op: op}
	}
	if !force && (s.state == Starting || s.state == Running || s.state == Stopping) {
		s.mu.Unlock()
		return ErrActive
	}
	s.deleted = true
	s.mu.Unlock()

	if err := m.defs.Delete(id); err != nil {
		s.mu.Lock()
		s.deleted = false
		s.mu.Unlock()
		return err
	}
	m.mu.Lock()
	delete(m.servers, id)
	m.mu.Unlock()
	// Notify while permission rows still exist so affected users hear of it.
	m.publish(events.Event{Type: "server.deleted", Audience: events.AudienceServer, ServerID: id, Data: map[string]any{"server_id": id}})
	s.shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.store.DeleteServerPermissions(ctx, id); err != nil {
		m.log.Error("could not delete permissions of removed server", "server", id, "error", err)
	}
	_ = m.store.DeleteRuntime(ctx, id)
	backend.RemoveRuntimeFiles(m.cfg.Get().Paths.RuntimeDir, id)
	m.cooldown.Forget(id)
	m.NotifyCapacityChanged()
	return nil
}

// ReloadResult summarises a reload from disk.
type ReloadResult struct {
	Added   []string          `json:"added"`
	Removed []string          `json:"removed"`
	Updated []string          `json:"updated"`
	Skipped map[string]string `json:"skipped"`
	Invalid map[string]string `json:"invalid"`
}

// Reload re-reads the definitions directory (after manual edits). Changes
// to servers that are currently active are skipped and reported.
func (m *Manager) Reload() (*ReloadResult, error) {
	old := map[string]*servers.Definition{}
	for _, d := range m.defs.List() {
		old[d.ID] = d
	}
	if err := m.defs.Load(); err != nil {
		return nil, err
	}
	res := &ReloadResult{Added: []string{}, Removed: []string{}, Updated: []string{}, Skipped: map[string]string{}, Invalid: m.defs.Invalid()}
	seen := map[string]bool{}
	for _, d := range m.defs.List() {
		seen[d.ID] = true
		prev, existed := old[d.ID]
		if !existed {
			m.add(d)
			res.Added = append(res.Added, d.ID)
			m.publish(events.Event{Type: "server.created", Audience: events.AudienceServer, ServerID: d.ID, Data: map[string]any{"server_id": d.ID}})
			continue
		}
		s, err := m.get(d.ID)
		if err != nil {
			continue
		}
		s.mu.Lock()
		same := servers.RuntimeEqual(prev, d)
		active := s.op != nil || s.state == Starting || s.state == Running || s.state == Stopping
		switch {
		case active && !same:
			res.Skipped[d.ID] = "server is active; execution changes take effect after it is stopped and the definitions are reloaded again"
			s.def = mergeSafe(s.def, d)
		case active || onlyOnDemandChanged(s.def, d):
			s.def = d
		default:
			s.rebuildLocked(d)
		}
		s.mu.Unlock()
		m.pruneActionGrants(d)
		res.Updated = append(res.Updated, d.ID)
		m.publish(events.Event{Type: "server.updated", Audience: events.AudienceServer, ServerID: d.ID, Data: map[string]any{"server_id": d.ID}})
	}
	for id := range old {
		if seen[id] {
			continue
		}
		m.mu.Lock()
		s := m.servers[id]
		delete(m.servers, id)
		m.mu.Unlock()
		m.publish(events.Event{Type: "server.deleted", Audience: events.AudienceServer, ServerID: id, Data: map[string]any{"server_id": id}})
		if s != nil {
			s.mu.Lock()
			s.deleted = true
			s.mu.Unlock()
			s.shutdown()
		}
		res.Removed = append(res.Removed, id)
	}
	m.NotifyCapacityChanged()
	return res, nil
}

// mergeSafe applies the fields of next that do not control the running
// process onto cur: presentation, timeouts, backups and custom actions.
func mergeSafe(cur, next *servers.Definition) *servers.Definition {
	out := cur.Clone()
	out.Name, out.Description, out.Category = next.Name, next.Description, next.Category
	out.Timeouts, out.Cooldown, out.StatusPollInterval = next.Timeouts, next.Cooldown, next.StatusPollInterval
	n := next.Clone()
	out.Backup, out.Actions = n.Backup, n.Actions
	return out
}

// onlyOnDemandChanged reports whether a and b differ at most in fields no
// backend reads: the backup configuration and custom actions (executed on
// demand by the manager), the category and the description. Such changes
// keep the live backend and its console buffer.
func onlyOnDemandChanged(a, b *servers.Definition) bool {
	x, y := a.Clone(), b.Clone()
	x.Backup, y.Backup = nil, nil
	x.Actions, y.Actions = nil, nil
	x.Category, y.Category = "", ""
	x.Description, y.Description = "", ""
	xj, _ := json.Marshal(x)
	yj, _ := json.Marshal(y)
	return string(xj) == string(yj)
}

// Shutdown stops monitoring, stops the servers that cannot outlive the
// manager and releases every backend. Other servers keep running and are
// re-adopted on the next start.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.RLock()
	all := make([]*server, 0, len(m.servers))
	for _, s := range m.servers {
		all = append(all, s)
	}
	m.mu.RUnlock()

	var wg sync.WaitGroup
	for _, s := range all {
		wg.Add(1)
		go func(s *server) {
			defer wg.Done()
			s.stopForExit(ctx)
		}(s)
	}
	wg.Wait()
	m.cancel()
	for _, s := range all {
		s.shutdown()
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); m.actionsWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		m.log.Warn("some server monitors did not stop in time")
	}
}

func (m *Manager) newBuffer() *console.Buffer {
	return console.NewBuffer(m.cfg.Get().Console.BufferLines)
}

// InvalidFiles lists definition files that failed to load, with reasons.
func (m *Manager) InvalidFiles() map[string]string { return m.defs.Invalid() }

// NotifyBackupsChanged tells clients holding BACKUP_DOWNLOAD that the file list of
// a server changed.
func (m *Manager) NotifyBackupsChanged(id string) {
	m.publish(events.Event{Type: "server.backups.changed", Audience: events.AudienceServer, ServerID: id, Perm: perm.BackupDownload,
		Data: map[string]any{"server_id": id}})
}

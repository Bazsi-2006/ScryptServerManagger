package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/backend"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/events"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// Custom actions are administrator-defined command modules. A run executes
// exactly what the definition says; callers only choose which action.
//
// Each action runs at most once at a time per server (a second request
// while it runs is refused), independently of the lifecycle operation slot,
// so predefined console commands stay available while, say, a backup runs.

// OpAction is the operation type recorded for action runs.
const OpAction = "action"

var (
	ErrActionNotFound       = errors.New("custom action not found")
	ErrActionExists         = errors.New("a custom action with this ID already exists")
	ErrActionDisabled       = errors.New("this action is disabled")
	ErrConfirmationRequired = errors.New("this action must be confirmed before it runs")
)

// ActionRun describes an action run in progress.
type ActionRun struct {
	ID        int64     `json:"id"`
	ServerID  string    `json:"server_id"`
	ActionID  string    `json:"action_id"`
	User      string    `json:"user"`
	StartedAt time.Time `json:"started_at"`
}

// ActionBusyError: the same action is already running on this server.
type ActionBusyError struct{ Run ActionRun }

func (e *ActionBusyError) Error() string {
	return fmt.Sprintf("this action is already running (started by %s)", firstNonEmpty(e.Run.User, "the system"))
}

type runningAction struct {
	run   ActionRun
	actor audit.Actor
}

// actionOutputBytes bounds the output kept per run.
const actionOutputBytes = 8192

// RunAction starts a custom action and returns once it has been accepted;
// the outcome arrives as a server.action.completed event and in the
// operation history. confirmed must be true for actions that require
// confirmation.
func (m *Manager) RunAction(serverID, actionID string, actor audit.Actor, confirmed bool) (*ActionRun, error) {
	s, err := m.get(serverID)
	if err != nil {
		return nil, err
	}
	def, be := s.snapshot()
	a := def.Action(actionID)
	if a == nil {
		return nil, ErrActionNotFound
	}
	if a.Disabled {
		return nil, ErrActionDisabled
	}
	if a.Confirm && !confirmed {
		return nil, ErrConfirmationRequired
	}
	if a.Method == servers.MethodTmux {
		if err := s.requireTmuxTarget(be, "command"); err != nil {
			return nil, err
		}
	}
	action := *a // the definition snapshot is immutable; keep our own copy

	s.mu.Lock()
	if s.deleted {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	if cur, busy := s.actions[actionID]; busy {
		s.mu.Unlock()
		return nil, &ActionBusyError{Run: cur.run}
	}
	ra := &runningAction{run: ActionRun{ServerID: serverID, ActionID: actionID, User: actor.Username, StartedAt: time.Now().UTC()}, actor: actor}
	s.actions[actionID] = ra
	s.mu.Unlock()

	row := &db.Operation{ServerID: serverID, Type: OpAction, ActionID: actionID, Username: actor.Username, Status: "running", StartedAt: ra.run.StartedAt}
	if actor.UserID != 0 {
		id := actor.UserID
		row.UserID = &id
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := m.store.InsertOperation(ctx, row); err != nil {
		m.log.Error("could not record action run", "server", serverID, "action", actionID, "error", err)
	}
	cancel()
	s.mu.Lock()
	ra.run.ID = row.ID
	run := ra.run
	s.mu.Unlock()

	m.audit.Record(actor, serverID, audit.ActionRun, audit.Requested, map[string]any{"action_id": action.ID, "action_name": action.Name})
	m.publish(events.Event{Type: "server.action.started", Audience: events.AudienceAction, ServerID: serverID, ActionID: actionID,
		Data: map[string]any{"server_id": serverID, "action_id": actionID, "run": run}})

	m.actionsWG.Add(1)
	go func() {
		defer m.actionsWG.Done()
		status, note, output := s.doAction(def, action, be, actor)
		s.finishAction(ra, action, status, note, output)
	}()
	return &run, nil
}

// doAction executes one action and classifies the result.
func (s *server) doAction(def *servers.Definition, a servers.Action, be backend.Backend, actor audit.Actor) (status, note, output string) {
	if a.Method == servers.MethodTmux {
		ts, ok := be.(backend.TextSender)
		if !ok {
			return statusFailed, "this server's backend cannot receive console commands", ""
		}
		ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
		defer cancel()
		if err := ts.SendText(ctx, a.Tmux.Text, a.Tmux.PressEnter()); err != nil {
			if s.ctx.Err() != nil {
				return statusInterrupted, "server-manager is shutting down or the server was removed", ""
			}
			return statusFailed, "the command could not be sent: " + err.Error(), ""
		}
		return statusSubmitted, "the command was sent to the tmux session; its effect on the server cannot be verified", ""
	}

	timeout := a.Timeout.D()
	if timeout <= 0 {
		timeout = servers.DefaultActionTimeout
	}
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()
	res, err := backend.Run(ctx, backend.RunSpec{
		Argv: a.Command.Argv(), Dir: firstNonEmpty(a.WorkingDir, def.WorkingDir),
		Env:       backend.ServerEnv(def, "SM_ACTION_ID="+a.ID, "SM_ACTION_USER="+actor.Username),
		TailBytes: actionOutputBytes,
	})
	output = res.Output
	if err != nil {
		if errors.Is(err, backend.ErrTimeout) {
			if s.ctx.Err() != nil {
				return statusInterrupted, "server-manager is shutting down or the server was removed; the command was stopped", output
			}
			return statusFailed, fmt.Sprintf("the command timed out after %s and was killed", timeout), output
		}
		return statusFailed, err.Error(), output
	}
	if res.ExitCode != 0 {
		return statusFailed, fmt.Sprintf("the command exited with code %d", res.ExitCode), output
	}
	return statusSucceeded, "the command completed successfully", output
}

func (s *server) finishAction(ra *runningAction, a servers.Action, status, note, output string) {
	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s.m.store.FinishActionOperation(ctx, ra.run.ID, status, note, output, now); err != nil {
		s.m.log.Error("could not record action result", "server", s.id, "action", a.ID, "error", err)
	}
	cancel()

	details := map[string]any{"action_id": a.ID, "action_name": a.Name, "duration_ms": now.Sub(ra.run.StartedAt).Milliseconds(), "message": note}
	result := audit.Success
	switch status {
	case statusSucceeded:
	case statusSubmitted:
		result = audit.Submitted
	default:
		result = audit.Failure
		details["outcome"] = status
	}
	s.m.audit.Record(ra.actor, s.id, audit.ActionRun, result, details)

	s.mu.Lock()
	if s.actions[a.ID] == ra {
		delete(s.actions, a.ID)
	}
	s.mu.Unlock()
	s.m.publish(events.Event{Type: "server.action.completed", Audience: events.AudienceAction, ServerID: s.id, ActionID: a.ID,
		Data: map[string]any{"server_id": s.id, "action_id": a.ID, "action_name": a.Name, "run": ra.run,
			"status": status, "message": note, "output": output, "finished_at": now.UTC()}})
}

// RunningActions returns the actions of a server that are running now.
func (m *Manager) RunningActions(serverID string) map[string]ActionRun {
	s, err := m.get(serverID)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]ActionRun, len(s.actions))
	for id, ra := range s.actions {
		out[id] = ra.run
	}
	return out
}

// LastActionRuns returns the most recent recorded run of each action.
func (m *Manager) LastActionRuns(ctx context.Context, serverID string) (map[string]*db.Operation, error) {
	if _, err := m.get(serverID); err != nil {
		return nil, err
	}
	return m.store.LastActionRuns(ctx, serverID)
}

// ---- administration ----

// SaveAction adds (create) or replaces a custom action of a server. On
// update the action keeps its ID. The whole definition is validated and
// saved as usual; actions can change while the server is running.
func (m *Manager) SaveAction(serverID string, a servers.Action, create bool) (*servers.Definition, error) {
	m.editMu.Lock()
	defer m.editMu.Unlock()
	def, err := m.defs.Get(serverID)
	if err != nil {
		return nil, err
	}
	existing := def.Action(a.ID)
	switch {
	case create && existing != nil:
		return nil, ErrActionExists
	case !create && existing == nil:
		return nil, ErrActionNotFound
	case create:
		def.Actions = append(def.Actions, a)
	default:
		*existing = a
	}
	return m.Update(def)
}

// DeleteAction removes a custom action and every grant for it.
func (m *Manager) DeleteAction(serverID, actionID string) (*servers.Definition, error) {
	m.editMu.Lock()
	defer m.editMu.Unlock()
	def, err := m.defs.Get(serverID)
	if err != nil {
		return nil, err
	}
	kept := def.Actions[:0:0]
	found := false
	for _, a := range def.Actions {
		if a.ID == actionID {
			found = true
			continue
		}
		kept = append(kept, a)
	}
	if !found {
		return nil, ErrActionNotFound
	}
	def.Actions = kept
	return m.Update(def)
}

// pruneActionGrants drops permissions for actions that no longer exist.
func (m *Manager) pruneActionGrants(def *servers.Definition) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.store.PruneActionPermissions(ctx, def.ID, def.ActionIDs()); err != nil {
		m.log.Error("could not remove permissions of deleted actions", "server", def.ID, "error", err)
	}
}

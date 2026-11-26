package manager

import (
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
)

// Transition is a meaningful lifecycle change, reported to the notifier.
//
// Transitions are derived from state changes the manager is certain about,
// never from the mere current state, so a server that stays crashed does
// not produce an event on every poll. Reconciliation right after startup
// (the first observation of each server) produces none, and neither does
// losing or regaining the ability to monitor a server.
type Transition struct {
	ServerID   string
	ServerName string
	// Kind is one of the db.Notify* kinds: ready, stopped, crash, failure.
	Kind string
	From State
	To   State
	// Confirmed is set for a crash backed by evidence of failure (non-zero
	// exit, failed unit, container error). Otherwise the server merely
	// stopped without being asked to.
	Confirmed bool
	// Operation is "start", "stop" or "restart" when a user operation
	// caused the transition, "" when monitoring observed it.
	Operation string
	Actor     string
	Detail    string
	Time      time.Time
}

// SetTransitionHook registers fn to receive transitions. fn must not block
// (it is called while server state is locked); it is set once, before Load.
func (m *Manager) SetTransitionHook(fn func(Transition)) { m.onTransition = fn }

func (s *server) emitLocked(t Transition) {
	if s.m.onTransition == nil || s.deleted {
		return
	}
	t.ServerID, t.ServerName = s.id, s.def.Name
	if t.Time.IsZero() {
		t.Time = time.Now()
	}
	s.m.onTransition(t)
}

func isActive(st State) bool { return st == Starting || st == Running || st == Stopping }

// observedTransitionLocked classifies a state change found by monitoring
// (not caused by an operation). prev/prevKnown are the state and the last
// conclusive state before the change.
func (s *server) observedTransitionLocked(prev, prevKnown, next State, failed bool, detail string) {
	if !s.probed {
		return // first look after startup: reconciliation, not an event
	}
	wasUp := prev == Running || prev == Starting || (prev == Unknown && (prevKnown == Running || prevKnown == Starting))
	switch {
	case next == Running && prev != Running && !(prev == Unknown && prevKnown == Running):
		s.emitLocked(Transition{Kind: db.NotifyReady, From: prev, To: next, Detail: detail})
	case (next == Stopped || next == Error) && wasUp:
		from := prev
		if prev == Unknown {
			from = prevKnown
		}
		s.emitLocked(Transition{Kind: db.NotifyCrash, From: from, To: next, Confirmed: failed, Detail: detail})
	case next == Stopped && prev == Stopping:
		s.emitLocked(Transition{Kind: db.NotifyStopped, From: prev, To: next, Detail: detail})
	case next == Error && prev == Stopping:
		s.emitLocked(Transition{Kind: db.NotifyFailure, From: prev, To: next, Detail: detail})
	}
}

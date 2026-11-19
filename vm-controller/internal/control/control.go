// Package control turns Vexa-VM's view of a VM into what the dashboard
// shows, and serializes start/stop requests.
//
// Status reads are cached per VM and concurrent readers share one in-flight
// Vexa request, so the load on Vexa does not grow with the number of users
// watching the dashboard. Power requests are accepted only when the VM is in
// the matching state and no other request for that VM is pending. Because a
// Vexa power request only queues a job, an accepted request is tracked as
// "starting" or "stopping" until Vexa reports the target state or the
// configured timeout passes.
package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/vexa"
)

// Vexa is the subset of *vexa.Client the controller needs.
type Vexa interface {
	GetVM(ctx context.Context, id string) (vexa.VM, error)
	Start(ctx context.Context, id string) (vexa.Job, error)
	Shutdown(ctx context.Context, id string) (vexa.Job, error)
	GetJob(ctx context.Context, jobID string) (vexa.Job, error)
}

// State is what the dashboard displays.
type State string

const (
	StateRunning     State = "running"
	StateStopped     State = "stopped"
	StateStarting    State = "starting"
	StateStopping    State = "stopping"
	StatePaused      State = "paused"
	StateBusy        State = "busy"        // creating, reinstalling or migrating
	StateError       State = "error"       // Vexa reports an error state
	StateUnknown     State = "unknown"     // Vexa reports "unknown"
	StateUnavailable State = "unavailable" // Vexa could not be asked
)

// Status is the combined view of one VM.
type Status struct {
	State   State
	Pending config.Action // "" when nothing is pending
	// Message is a short, user-facing explanation (may be empty).
	Message   string
	CheckedAt time.Time
}

// ErrorKind classifies a refused or failed power request.
type ErrorKind int

const (
	// ErrBusy: another request for this VM is pending or just happened.
	ErrBusy ErrorKind = iota + 1
	// ErrWrongState: the VM is not in a state where the action makes sense.
	ErrWrongState
	// ErrForbidden: the VM does not allow this action at all.
	ErrForbidden
	// ErrUnavailable: Vexa could not be asked or asked us to wait.
	ErrUnavailable
	// ErrUncertain: Vexa did not answer the power request; it may or may
	// not have been applied.
	ErrUncertain
	// ErrUpstream: Vexa refused the request.
	ErrUpstream
)

// RequestError is returned by Request. Message is safe to show to users.
type RequestError struct {
	Kind       ErrorKind
	Message    string
	RetryAfter time.Duration
	Err        error // underlying cause, for logs
}

func (e *RequestError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *RequestError) Unwrap() error { return e.Err }

// Options tune the controller. Zero values get sensible defaults.
type Options struct {
	CacheTTL     time.Duration // normal status cache lifetime
	PendingTTL   time.Duration // cache lifetime while an operation is pending
	StartTimeout time.Duration
	StopTimeout  time.Duration
	Cooldown     time.Duration // minimum gap between power requests per VM
	NoticeTTL    time.Duration // how long a failure notice stays visible
	TrackJobs    bool
	Logger       *slog.Logger
	Now          func() time.Time
}

// Controller is safe for concurrent use.
type Controller struct {
	vexa Vexa
	opts Options
	log  *slog.Logger
	now  func() time.Time

	mu           sync.Mutex
	vms          map[string]*entry
	jobsDisabled bool
}

type entry struct {
	id        string
	allowStop bool
	opMu      sync.Mutex // held while a power request is being made

	// guarded by Controller.mu
	obs        *observation
	inflight   *fetchCall
	pending    *pending
	notice     *notice
	cooldown   time.Time
	jobPolling bool
}

type observation struct {
	state   vexa.State
	err     error
	kind    vexa.ErrorKind
	at      time.Time
	retryAt time.Time
}

type fetchCall struct {
	done chan struct{}
	obs  observation
}

type pending struct {
	action    config.Action
	jobID     string
	jobDone   bool
	jobPolled time.Time
	since     time.Time
	deadline  time.Time
	actor     string
}

type notice struct {
	text  string
	until time.Time
}

// New creates a controller for the configured VMs.
func New(v Vexa, vms []config.VM, opts Options) *Controller {
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = 5 * time.Second
	}
	if opts.PendingTTL <= 0 || opts.PendingTTL > opts.CacheTTL {
		opts.PendingTTL = min(2*time.Second, opts.CacheTTL)
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 3 * time.Minute
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = 7 * time.Minute
	}
	if opts.Cooldown <= 0 {
		opts.Cooldown = 5 * time.Second
	}
	if opts.NoticeTTL <= 0 {
		opts.NoticeTTL = 10 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c := &Controller{vexa: v, opts: opts, log: opts.Logger, now: opts.Now, vms: map[string]*entry{}}
	for _, vm := range vms {
		c.vms[vm.ID] = &entry{id: vm.ID, allowStop: vm.AllowStop}
	}
	return c
}

// PollInterval suggests how often a dashboard showing ids should refresh.
func (c *Controller) PollInterval(ids []string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		if e := c.vms[id]; e != nil && e.pending != nil {
			return 3 * time.Second
		}
	}
	return 10 * time.Second
}

// Status returns the current status of one configured VM.
func (c *Controller) Status(ctx context.Context, id string) Status {
	e := c.vms[id]
	if e == nil {
		return Status{State: StateUnavailable, Message: "Unknown VM."}
	}
	c.mu.Lock()
	maxAge := c.opts.CacheTTL
	if e.pending != nil {
		maxAge = c.opts.PendingTTL
	}
	c.mu.Unlock()

	obs := c.observe(ctx, e, maxAge)
	c.pollJob(ctx, e)

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.reconcile(e, obs, now)
	return c.compose(e, obs, now)
}

// StatusMany returns statuses for ids, fetching uncached ones concurrently.
func (c *Controller) StatusMany(ctx context.Context, ids []string) []Status {
	out := make([]Status, len(ids))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = c.Status(ctx, id)
		}()
	}
	wg.Wait()
	return out
}

// Request asks Vexa to start or gracefully stop a VM. The caller must have
// already checked the user's permission; Request additionally refuses stop
// for VMs whose stopping is disabled.
func (c *Controller) Request(ctx context.Context, id string, action config.Action, actor string) error {
	e := c.vms[id]
	if e == nil {
		return &RequestError{Kind: ErrForbidden, Message: "Unknown VM."}
	}
	if action != config.ActionStart && action != config.ActionStop {
		return &RequestError{Kind: ErrForbidden, Message: "Unsupported action."}
	}
	if action == config.ActionStop && !e.allowStop {
		return &RequestError{Kind: ErrForbidden, Message: "Stopping this VM is disabled."}
	}
	if !e.opMu.TryLock() {
		return &RequestError{Kind: ErrBusy, Message: "Another request for this VM is being processed."}
	}
	defer e.opMu.Unlock()

	if err := c.checkIdle(e); err != nil {
		return err
	}
	// Always decide on fresh information.
	obs := c.observe(ctx, e, 0)
	c.mu.Lock()
	c.reconcile(e, obs, c.now())
	c.mu.Unlock()
	if err := c.checkIdle(e); err != nil {
		return err
	}
	if obs.err != nil {
		re := &RequestError{Kind: ErrUnavailable, Message: "Vexa-VM is not available right now. Try again shortly.", Err: obs.err}
		if obs.kind == vexa.KindRateLimited {
			re.Message = "Vexa-VM is busy. Try again in a moment."
			re.RetryAfter = max(0, obs.retryAt.Sub(c.now()))
		}
		return re
	}
	switch {
	case action == config.ActionStart && obs.state != vexa.StateStopped:
		return &RequestError{Kind: ErrWrongState, Message: fmt.Sprintf("The VM cannot be started while it is %s.", describe(obs.state))}
	case action == config.ActionStop && obs.state != vexa.StateRunning:
		return &RequestError{Kind: ErrWrongState, Message: fmt.Sprintf("The VM cannot be stopped while it is %s.", describe(obs.state))}
	}

	// Do not let a browser disconnect cancel a request that Vexa may
	// already be processing; the client's own timeout still applies.
	reqCtx := context.WithoutCancel(ctx)
	var job vexa.Job
	var err error
	if action == config.ActionStart {
		job, err = c.vexa.Start(reqCtx, id)
	} else {
		job, err = c.vexa.Shutdown(reqCtx, id)
	}

	c.mu.Lock()
	now := c.now()
	e.cooldown = now.Add(c.opts.Cooldown)
	e.obs = nil // the next status read asks Vexa again
	if err == nil {
		timeout := c.opts.StartTimeout
		if action == config.ActionStop {
			timeout = c.opts.StopTimeout
		}
		e.pending = &pending{action: action, jobID: job.ID, since: now, deadline: now.Add(timeout), actor: actor}
		e.notice = nil
	}
	c.mu.Unlock()

	if err != nil {
		c.log.Warn("power request failed", "vm", id, "action", action, "user", actor, "error", err)
		return powerError(err)
	}
	c.log.Info("power request accepted", "vm", id, "action", action, "user", actor, "job", job.ID)
	return nil
}

func (c *Controller) checkIdle(e *entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := e.pending; p != nil {
		verb := "start"
		if p.action == config.ActionStop {
			verb = "shutdown"
		}
		return &RequestError{Kind: ErrBusy, Message: fmt.Sprintf("A %s is already in progress for this VM.", verb)}
	}
	if wait := e.cooldown.Sub(c.now()); wait > 0 {
		return &RequestError{Kind: ErrBusy, Message: "Please wait a few seconds before sending another request.", RetryAfter: wait}
	}
	return nil
}

func powerError(err error) *RequestError {
	re := &RequestError{Kind: ErrUpstream, Message: "Vexa-VM rejected the request.", Err: err}
	var ve *vexa.Error
	if errors.As(err, &ve) {
		switch ve.Kind {
		case vexa.KindUnavailable:
			re.Kind = ErrUncertain
			re.Message = "Vexa-VM did not answer in time. The request may still have been applied; check the status before trying again."
		case vexa.KindRateLimited:
			re.Kind = ErrUnavailable
			re.Message = "Vexa-VM is busy. Try again in a moment."
			re.RetryAfter = ve.RetryAfter
		case vexa.KindConflict:
			re.Kind = ErrWrongState
			re.Message = "Vexa-VM refused the request because of the VM's current state."
		case vexa.KindNotFound:
			re.Message = "This VM was not found in Vexa-VM."
		case vexa.KindAuth:
			re.Message = "Vexa-VM refused the controller's credentials."
		case vexa.KindServer:
			re.Kind = ErrUnavailable
			re.Message = "Vexa-VM reported an internal error. Try again later."
		}
	}
	return re
}

// observe returns a cached observation younger than maxAge, joins an
// in-flight fetch, or fetches. maxAge 0 forces a new fetch unless one is
// already running (whose result is then fresh enough).
func (c *Controller) observe(ctx context.Context, e *entry, maxAge time.Duration) observation {
	c.mu.Lock()
	now := c.now()
	if o := e.obs; o != nil && (now.Sub(o.at) < maxAge || now.Before(o.retryAt)) {
		obs := *o
		c.mu.Unlock()
		return obs
	}
	if call := e.inflight; call != nil {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.obs
		case <-ctx.Done():
			return observation{err: ctx.Err(), kind: vexa.KindUnavailable, at: now}
		}
	}
	call := &fetchCall{done: make(chan struct{})}
	e.inflight = call
	c.mu.Unlock()

	// The fetch is shared by every waiter, so it must not depend on the
	// context of whichever request happened to start it.
	vm, err := c.vexa.GetVM(context.WithoutCancel(ctx), e.id)
	obs := observation{state: vm.State, at: c.now()}
	if err != nil {
		obs.state = ""
		obs.err = err
		obs.kind = vexa.KindOf(err)
		var ve *vexa.Error
		if errors.As(err, &ve) && ve.RetryAfter > 0 {
			obs.retryAt = obs.at.Add(ve.RetryAfter)
		}
		c.logFetchError(e.id, err)
	}

	c.mu.Lock()
	e.obs = &obs
	e.inflight = nil
	call.obs = obs
	close(call.done)
	c.mu.Unlock()
	return obs
}

func (c *Controller) logFetchError(id string, err error) {
	switch vexa.KindOf(err) {
	case vexa.KindAuth:
		c.log.Error("Vexa-VM rejected the API key; it needs the vms:read and vms:power scopes", "vm", id, "error", err)
	case vexa.KindNotFound:
		c.log.Error("VM not found in Vexa-VM; check its id in the configuration", "vm", id, "error", err)
	default:
		c.log.Warn("could not read VM status from Vexa-VM", "vm", id, "error", err)
	}
}

// pollJob checks the Vexa job of a pending request when job tracking is on.
func (c *Controller) pollJob(ctx context.Context, e *entry) {
	if !c.opts.TrackJobs {
		return
	}
	c.mu.Lock()
	p := e.pending
	now := c.now()
	if c.jobsDisabled || p == nil || p.jobID == "" || p.jobDone || e.jobPolling || now.Sub(p.jobPolled) < c.opts.PendingTTL {
		c.mu.Unlock()
		return
	}
	e.jobPolling = true
	jobID := p.jobID
	c.mu.Unlock()

	job, err := c.vexa.GetJob(context.WithoutCancel(ctx), jobID)

	c.mu.Lock()
	defer c.mu.Unlock()
	e.jobPolling = false
	now = c.now()
	if e.pending != p { // resolved meanwhile
		return
	}
	p.jobPolled = now
	if err != nil {
		switch vexa.KindOf(err) {
		case vexa.KindAuth:
			c.jobsDisabled = true
			c.log.Warn("job tracking disabled: the API key lacks the jobs:read scope", "error", err)
		case vexa.KindNotFound:
			p.jobDone = true
		}
		return
	}
	switch job.Status {
	case vexa.JobFailed, vexa.JobCancelled:
		c.log.Warn("Vexa-VM job did not succeed", "vm", e.id, "action", p.action, "job", job.ID, "status", job.Status, "error", job.Error)
		e.pending = nil
		verb := "start"
		if p.action == config.ActionStop {
			verb = "shutdown"
		}
		e.notice = &notice{text: fmt.Sprintf("Vexa-VM reported that the %s request failed.", verb), until: now.Add(c.opts.NoticeTTL)}
	case vexa.JobSucceeded:
		p.jobDone = true
		if p.action == config.ActionStart {
			e.obs = nil // Vexa stores the new state when the job finishes
		}
	}
}

// reconcile resolves a pending operation against an observation. Callers
// hold c.mu.
func (c *Controller) reconcile(e *entry, obs observation, now time.Time) {
	p := e.pending
	if p == nil {
		return
	}
	if obs.err == nil {
		switch {
		case p.action == config.ActionStart && (obs.state == vexa.StateRunning):
			e.pending = nil
			return
		case p.action == config.ActionStop && obs.state == vexa.StateStopped:
			e.pending = nil
			return
		case obs.state == vexa.StateError:
			e.pending = nil
			e.notice = &notice{text: "Vexa-VM reports an error for this VM.", until: now.Add(c.opts.NoticeTTL)}
			return
		}
	}
	if now.After(p.deadline) {
		e.pending = nil
		var text string
		if p.action == config.ActionStart {
			text = fmt.Sprintf("The VM did not report running within %s.", humanDuration(c.opts.StartTimeout))
		} else {
			text = "The VM is still reported as running. The guest may have ignored the shutdown request."
		}
		e.notice = &notice{text: text, until: now.Add(c.opts.NoticeTTL)}
		c.log.Warn("pending operation timed out", "vm", e.id, "action", p.action, "user", p.actor, "vexa_state", obs.state)
	}
}

// compose builds the displayed status. Callers hold c.mu.
func (c *Controller) compose(e *entry, obs observation, now time.Time) Status {
	st := Status{CheckedAt: obs.at}
	if n := e.notice; n != nil {
		if now.Before(n.until) {
			st.Message = n.text
		} else {
			e.notice = nil
		}
	}
	if p := e.pending; p != nil {
		st.Pending = p.action
	}
	if obs.err != nil {
		st.State = StateUnavailable
		if st.Message == "" {
			switch obs.kind {
			case vexa.KindNotFound:
				st.Message = "This VM was not found in Vexa-VM."
			case vexa.KindRateLimited:
				st.Message = "Vexa-VM is busy. Retrying shortly."
			case vexa.KindUnavailable:
				st.Message = "Vexa-VM is not responding. Retrying automatically."
			default:
				st.Message = "The status could not be read from Vexa-VM."
			}
		}
		return st
	}
	if p := e.pending; p != nil {
		if p.action == config.ActionStart {
			st.State = StateStarting
			if st.Message == "" {
				st.Message = "Start requested. Waiting for Vexa-VM to report the VM as running."
			}
		} else {
			st.State = StateStopping
			if st.Message == "" {
				st.Message = "Shutdown requested. Waiting for the guest to power off; this can take a few minutes."
			}
		}
		return st
	}
	switch obs.state {
	case vexa.StateRunning:
		st.State = StateRunning
	case vexa.StateStopped:
		st.State = StateStopped
	case vexa.StatePaused:
		st.State = StatePaused
		if st.Message == "" {
			st.Message = "The VM is paused in Vexa-VM. Ask an administrator to resume it."
		}
	case vexa.StateCreating, vexa.StateReinstalling, vexa.StateMigrating:
		st.State = StateBusy
		if st.Message == "" {
			st.Message = fmt.Sprintf("Vexa-VM is %s this VM.", describe(obs.state))
		}
	case vexa.StateError:
		st.State = StateError
		if st.Message == "" {
			st.Message = "Vexa-VM reports an error for this VM."
		}
	default:
		st.State = StateUnknown
	}
	return st
}

func describe(s vexa.State) string {
	switch s {
	case vexa.StateRunning:
		return "running"
	case vexa.StateStopped:
		return "stopped"
	case vexa.StatePaused:
		return "paused"
	case vexa.StateCreating:
		return "creating"
	case vexa.StateReinstalling:
		return "reinstalling"
	case vexa.StateMigrating:
		return "migrating"
	case vexa.StateError:
		return "in an error state"
	default:
		return "in an unknown state"
	}
}

func humanDuration(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		if d == time.Minute {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	return d.String()
}

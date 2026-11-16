// Package events is the application-wide publish/subscribe hub behind the
// SSE endpoint. Every event carries an audience and the hub decides, per
// subscriber and at delivery time, whether that subscriber may see it.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/perm"
)

// Audience selects who may receive an event.
type Audience int

const (
	// AudienceAdmins: administrators only.
	AudienceAdmins Audience = iota
	// AudienceServer: administrators and users holding at least one
	// permission (or the specific Perm, if set) on ServerID.
	AudienceServer
	// AudienceUser: exactly the user UserID.
	AudienceUser
	// AudienceAll: every authenticated subscriber.
	AudienceAll
	// AudienceMetrics: administrators, plus everyone when host metrics are
	// enabled for regular users.
	AudienceMetrics
)

// Event is a notification. Data should be minimal: clients re-fetch details
// through the API, which applies its own authorization.
type Event struct {
	Type     string
	Audience Audience
	ServerID string
	UserID   int64
	Perm     perm.Mask
	Data     any
}

// Envelope is an event as queued for a subscriber.
type Envelope struct {
	ID   string
	Type string
	Data any
}

// Identity is what the hub knows about a subscriber for authorization. It is
// refreshed through the Resolver whenever authorization data may have
// changed.
type Identity struct {
	UserID    int64
	SessionID string
	IsAdmin   bool
	Perms     map[string]perm.Mask
}

// Resolver reloads a subscriber's identity. ok=false means the subscriber is
// no longer allowed to be connected (account disabled/deleted, session
// revoked).
type Resolver func(userID int64, sessionID string) (id Identity, ok bool)

// Subscriber is one connected SSE client.
type Subscriber struct {
	C    chan Envelope
	done chan struct{}
	hub  *Hub

	mu sync.Mutex
	id Identity
	// Reason is set when the hub closed the subscription.
	reason string
	closed bool
}

// Done is closed when the hub terminates the subscription.
func (s *Subscriber) Done() <-chan struct{} { return s.done }

// Reason explains why the hub closed the subscription ("lagged",
// "unauthorized" or "shutdown").
func (s *Subscriber) Reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

func (s *Subscriber) close(reason string) {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.reason = reason
		close(s.done)
	}
	s.mu.Unlock()
}

type stored struct {
	seq uint64
	ev  Event
}

// Hub fans events out to subscribers and keeps a short history for replay.
type Hub struct {
	mu       sync.Mutex
	subs     map[*Subscriber]struct{}
	seq      uint64
	epoch    string
	history  []stored
	trimmed  uint64 // highest sequence number dropped from history
	maxHist  int
	resolve  Resolver
	metricsU func() bool

	authMu sync.Mutex
	authCh chan struct{}
}

// New creates a hub. metricsForUsers reports whether non-administrators may
// receive host metrics.
func New(resolve Resolver, metricsForUsers func() bool) *Hub {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return &Hub{
		subs: map[*Subscriber]struct{}{}, epoch: hex.EncodeToString(b), maxHist: 512,
		resolve: resolve, metricsU: metricsForUsers, authCh: make(chan struct{}),
	}
}

func (h *Hub) allowed(id *Identity, ev *Event) bool {
	switch ev.Audience {
	case AudienceAdmins:
		return id.IsAdmin
	case AudienceServer:
		if id.IsAdmin {
			return true
		}
		m := id.Perms[ev.ServerID]
		if ev.Perm != 0 {
			return m.Has(ev.Perm)
		}
		return m.Any()
	case AudienceUser:
		return id.UserID == ev.UserID
	case AudienceAll:
		return true
	case AudienceMetrics:
		return id.IsAdmin || (h.metricsU != nil && h.metricsU())
	}
	return false
}

func (h *Hub) eventID(seq uint64) string { return h.epoch + "-" + strconv.FormatUint(seq, 10) }

// Publish delivers ev to every authorized subscriber. It never blocks: a
// subscriber whose queue is full is disconnected and will resynchronise.
func (h *Hub) Publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	// Host metrics are high-frequency and worthless to replay.
	if ev.Audience != AudienceMetrics {
		h.history = append(h.history, stored{seq: h.seq, ev: ev})
		if len(h.history) > h.maxHist {
			cut := len(h.history) - h.maxHist
			h.trimmed = h.history[cut-1].seq
			h.history = append([]stored(nil), h.history[cut:]...)
		}
	}
	env := Envelope{ID: h.eventID(h.seq), Type: ev.Type, Data: ev.Data}
	for s := range h.subs {
		s.mu.Lock()
		ok := h.allowed(&s.id, &ev)
		s.mu.Unlock()
		if !ok {
			continue
		}
		select {
		case s.C <- env:
		default:
			delete(h.subs, s)
			s.close("lagged")
		}
	}
}

// Subscribe registers a client. lastEventID is the SSE Last-Event-ID the
// client presented (may be empty). The returned backlog holds the events
// the client missed and may see; resync is true when the gap cannot be
// replayed and the client must refetch its state through the API.
func (h *Hub) Subscribe(id Identity, lastEventID string) (sub *Subscriber, backlog []Envelope, resync bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sub = &Subscriber{C: make(chan Envelope, 256), done: make(chan struct{}), hub: h, id: id}
	// A client without a usable Last-Event-ID must (re)load its state.
	resync = true
	if epoch, n, ok := strings.Cut(lastEventID, "-"); ok && epoch == h.epoch {
		// Replay is only sound if nothing after `last` has been dropped
		// from the history.
		if last, err := strconv.ParseUint(n, 10, 64); err == nil && last <= h.seq && last >= h.trimmed {
			resync = false
			for _, st := range h.history {
				if st.seq > last && h.allowed(&id, &st.ev) {
					backlog = append(backlog, Envelope{ID: h.eventID(st.seq), Type: st.ev.Type, Data: st.ev.Data})
				}
			}
		}
	}
	h.subs[sub] = struct{}{}
	return sub, backlog, resync
}

// CurrentID returns the ID of the most recently published event.
func (h *Hub) CurrentID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.eventID(h.seq)
}

func (h *Hub) Unsubscribe(s *Subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
	s.close("")
}

// AuthChanged returns a channel that is closed the next time authorization
// data changes anywhere. Long-lived streams outside the hub (console
// streams) select on it and re-validate their own access.
func (h *Hub) AuthChanged() <-chan struct{} {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	return h.authCh
}

// Revalidate must be called after any change to users, sessions or
// permissions. Each subscriber's identity is reloaded; subscribers that are
// no longer authorized are disconnected immediately.
func (h *Hub) Revalidate() {
	h.mu.Lock()
	subs := make([]*Subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		s.mu.Lock()
		uid, sid := s.id.UserID, s.id.SessionID
		s.mu.Unlock()
		id, ok := h.resolve(uid, sid)
		if !ok {
			h.mu.Lock()
			delete(h.subs, s)
			h.mu.Unlock()
			s.close("unauthorized")
			continue
		}
		s.mu.Lock()
		s.id = id
		s.mu.Unlock()
	}
	h.authMu.Lock()
	close(h.authCh)
	h.authCh = make(chan struct{})
	h.authMu.Unlock()
}

// Run periodically revalidates all subscribers as a safety net (e.g. for
// sessions that simply expire) until stop is closed.
func (h *Hub) Run(stop <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			h.Revalidate()
		}
	}
}

// Close disconnects everyone (application shutdown).
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		delete(h.subs, s)
		s.close("shutdown")
	}
}

// Count returns the number of connected subscribers.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

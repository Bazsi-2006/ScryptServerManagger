package events

import (
	"sync"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/perm"
)

// world is a tiny mutable authorization model for the resolver.
type world struct {
	mu      sync.Mutex
	users   map[int64]Identity
	revoked map[string]bool
	metrics bool
}

func (w *world) resolve(uid int64, sid string) (Identity, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id, ok := w.users[uid]
	if !ok || w.revoked[sid] {
		return Identity{}, false
	}
	id.SessionID = sid
	return id, true
}

func newWorld() (*world, *Hub) {
	w := &world{users: map[int64]Identity{
		1: {UserID: 1, IsAdmin: true},
		2: {UserID: 2, Perms: map[string]perm.Mask{"mc": perm.Start}},
		3: {UserID: 3, Perms: map[string]perm.Mask{"web": perm.ConsoleRead | perm.BackupDownload}},
	}, revoked: map[string]bool{}}
	return w, New(w.resolve, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.metrics })
}

func (w *world) sub(t *testing.T, h *Hub, uid int64, last string) (*Subscriber, []Envelope, bool) {
	t.Helper()
	id, ok := w.resolve(uid, "sess")
	if !ok {
		t.Fatal("unknown user")
	}
	return h.Subscribe(id, last)
}

func recv(t *testing.T, s *Subscriber) Envelope {
	t.Helper()
	select {
	case e := <-s.C:
		return e
	case <-time.After(time.Second):
		t.Fatal("expected an event")
		return Envelope{}
	}
}

func none(t *testing.T, s *Subscriber, what string) {
	t.Helper()
	select {
	case e := <-s.C:
		t.Fatalf("%s: unexpected event %s", what, e.Type)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestServerEventsOnlyReachAuthorizedSubscribers(t *testing.T) {
	w, h := newWorld()
	admin, _, _ := w.sub(t, h, 1, "")
	mcUser, _, _ := w.sub(t, h, 2, "")
	webUser, _, _ := w.sub(t, h, 3, "")

	h.Publish(Event{Type: "server.status.changed", Audience: AudienceServer, ServerID: "mc", Data: "x"})
	if recv(t, admin).Type != "server.status.changed" || recv(t, mcUser).Type != "server.status.changed" {
		t.Fatal("admin and the permitted user should receive the event")
	}
	none(t, webUser, "user without access to mc")

	// A specific permission requirement narrows the audience further.
	h.Publish(Event{Type: "server.backups.changed", Audience: AudienceServer, ServerID: "web", Perm: perm.BackupDownload})
	recv(t, admin)
	recv(t, webUser)
	none(t, mcUser, "no access to web")
	h.Publish(Event{Type: "server.backups.changed", Audience: AudienceServer, ServerID: "mc", Perm: perm.BackupDownload})
	recv(t, admin)
	none(t, mcUser, "START does not include BACKUPS")

	h.Publish(Event{Type: "users.changed", Audience: AudienceAdmins})
	recv(t, admin)
	none(t, mcUser, "admin-only event")

	h.Publish(Event{Type: "permissions.changed", Audience: AudienceUser, UserID: 2})
	recv(t, mcUser)
	none(t, admin, "user-targeted event is not for admins")
	none(t, webUser, "user-targeted event is not for others")
}

func TestMetricsAudienceFollowsSetting(t *testing.T) {
	w, h := newWorld()
	admin, _, _ := w.sub(t, h, 1, "")
	user, _, _ := w.sub(t, h, 2, "")
	h.Publish(Event{Type: "host.metrics", Audience: AudienceMetrics})
	recv(t, admin)
	none(t, user, "metrics disabled for users")
	w.mu.Lock()
	w.metrics = true
	w.mu.Unlock()
	h.Publish(Event{Type: "host.metrics", Audience: AudienceMetrics})
	recv(t, admin)
	recv(t, user)
}

func TestPermissionChangesApplyToLiveSubscribers(t *testing.T) {
	w, h := newWorld()
	user, _, _ := w.sub(t, h, 2, "")
	authCh := h.AuthChanged()

	// Grant access to web, revoke mc.
	w.mu.Lock()
	w.users[2] = Identity{UserID: 2, Perms: map[string]perm.Mask{"web": perm.Start}}
	w.mu.Unlock()
	h.Revalidate()
	select {
	case <-authCh:
	default:
		t.Fatal("AuthChanged channel must fire on revalidation")
	}
	h.Publish(Event{Type: "a", Audience: AudienceServer, ServerID: "mc"})
	none(t, user, "revoked server")
	h.Publish(Event{Type: "b", Audience: AudienceServer, ServerID: "web"})
	if recv(t, user).Type != "b" {
		t.Fatal("newly granted server should deliver")
	}
}

func TestRevokedSessionOrDeletedUserIsDisconnected(t *testing.T) {
	w, h := newWorld()
	user, _, _ := w.sub(t, h, 2, "")
	other, _, _ := w.sub(t, h, 3, "")
	w.mu.Lock()
	delete(w.users, 2)
	w.mu.Unlock()
	h.Revalidate()
	select {
	case <-user.Done():
	case <-time.After(time.Second):
		t.Fatal("subscriber of a removed account must be disconnected")
	}
	if user.Reason() != "unauthorized" {
		t.Fatalf("reason %q", user.Reason())
	}
	select {
	case <-other.Done():
		t.Fatal("unrelated subscribers must stay connected")
	default:
	}
	if h.Count() != 1 {
		t.Fatalf("subscriber count %d", h.Count())
	}
}

func TestReplayIsFilteredAndGapsForceResync(t *testing.T) {
	w, h := newWorld()
	first, _, resync := w.sub(t, h, 2, "")
	if !resync {
		t.Fatal("a first connection must load state")
	}
	h.Publish(Event{Type: "e1", Audience: AudienceServer, ServerID: "mc"})
	seen := recv(t, first).ID
	h.Unsubscribe(first)

	// Missed while disconnected: one visible, one for another server, one metric.
	h.Publish(Event{Type: "e2", Audience: AudienceServer, ServerID: "mc"})
	h.Publish(Event{Type: "secret", Audience: AudienceServer, ServerID: "web"})
	h.Publish(Event{Type: "host.metrics", Audience: AudienceMetrics})
	h.Publish(Event{Type: "e3", Audience: AudienceServer, ServerID: "mc"})

	_, backlog, resync := w.sub(t, h, 2, seen)
	if resync {
		t.Fatal("the gap is fully in history; no resync expected")
	}
	if len(backlog) != 2 || backlog[0].Type != "e2" || backlog[1].Type != "e3" {
		t.Fatalf("replay must contain only authorized events in order: %+v", backlog)
	}

	// An ID from another hub instance (server restart) cannot be replayed.
	if _, _, resync := w.sub(t, h, 2, "deadbeef0000-3"); !resync {
		t.Fatal("foreign epoch must force a resync")
	}
	if _, _, resync := w.sub(t, h, 2, "garbage"); !resync {
		t.Fatal("malformed id must force a resync")
	}
	// Once history has been trimmed past the client's position: resync.
	for i := 0; i < 600; i++ {
		h.Publish(Event{Type: "filler", Audience: AudienceAdmins})
	}
	if _, _, resync := w.sub(t, h, 2, seen); !resync {
		t.Fatal("a position older than the retained history must force a resync")
	}
}

func TestSlowSubscriberIsDisconnected(t *testing.T) {
	w, h := newWorld()
	slow, _, _ := w.sub(t, h, 1, "")
	for i := 0; i < 400; i++ {
		h.Publish(Event{Type: "flood", Audience: AudienceAll})
	}
	select {
	case <-slow.Done():
		if slow.Reason() != "lagged" {
			t.Fatalf("reason %q", slow.Reason())
		}
	default:
		t.Fatal("a subscriber that does not read must be dropped, not buffered forever")
	}
}

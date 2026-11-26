package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/logging"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/mail"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/manager"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

type fakeMailer struct {
	mu        sync.Mutex
	sent      []mail.Message
	failNext  int  // fail this many sends
	failAll   bool // fail every send
	available bool
	block     chan struct{}
}

func (f *fakeMailer) Available() bool { return f.available }

func (f *fakeMailer) Send(ctx context.Context, m mail.Message) (mail.Outcome, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll || f.failNext > 0 {
		if f.failNext > 0 {
			f.failNext--
		}
		return mail.Sent, errors.New("550 mailbox <" + m.To + "> unavailable")
	}
	f.sent = append(f.sent, m)
	return mail.Sent, nil
}

func (f *fakeMailer) to(addr string) []mail.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mail.Message
	for _, m := range f.sent {
		if m.To == addr {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeMailer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type world struct {
	t     *testing.T
	cfg   *config.Holder
	store *db.Store
	mail  *fakeMailer
	n     *Notifier
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, cfg: testutil.Config(t), mail: &fakeMailer{available: true}}
	var err error
	w.store, err = db.Open(w.cfg.Get().Paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	w.cfg.Update(func(c *config.Config) error {
		c.Notifications.Cooldown = 0
		c.PublicURL = "https://sm.example.com"
		return nil
	})
	w.n = New(w.cfg, w.store, w.mail, logging.Discard())
	w.n.RetryDelays = []time.Duration{20 * time.Millisecond, 20 * time.Millisecond}
	w.n.Start()
	t.Cleanup(func() { w.n.Close(); w.store.Close() })
	return w
}

// user creates an account; verified/optIn control its email state.
func (w *world) user(name string, admin, verified bool, prefs db.NotificationPrefs) *db.User {
	w.t.Helper()
	u := &db.User{Username: name, DisplayName: strings.ToUpper(name[:1]) + name[1:], Email: name + "@example.com", PasswordHash: "x", IsAdmin: admin}
	if verified {
		now := time.Now().UTC().Truncate(time.Second)
		u.EmailVerifiedAt = &now
	}
	if err := w.store.CreateUser(context.Background(), u); err != nil {
		w.t.Fatal(err)
	}
	if err := w.store.SetNotificationPrefs(context.Background(), u.ID, prefs); err != nil {
		w.t.Fatal(err)
	}
	return u
}

var all = db.NotificationPrefs{Enabled: true, OnReady: true, OnStopped: true, OnCrash: true, OnFailure: true}

func tr(kind string, at time.Time) manager.Transition {
	return manager.Transition{ServerID: "mc", ServerName: "Minecraft Survival", Kind: kind, From: manager.Starting, To: manager.Running,
		Operation: manager.OpStart, Actor: "alice", Time: at}
}

func (w *world) settle() {
	time.Sleep(300 * time.Millisecond)
}

func TestOnlyEligibleRecipientsAreNotified(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin := w.user("admin", true, true, all)
	fan := w.user("fan", false, true, all)
	w.user("stranger", false, true, all) // no access to the server
	unverified := w.user("unverified", false, false, all)
	muted := w.user("muted", false, true, all)
	optedOut := w.user("optedout", false, true, db.NotificationPrefs{Enabled: true, OnStopped: true})
	masterOff := w.user("masteroff", false, true, db.NotificationPrefs{Enabled: false, OnReady: true})
	for _, u := range []*db.User{fan, unverified, muted, optedOut, masterOff} {
		w.store.SetPermission(ctx, u.ID, "mc", 4)
	}
	w.store.SetNotificationMutes(ctx, muted.ID, []string{"mc"})

	w.n.Notify(tr(db.NotifyReady, time.Now()))
	testutil.Eventually(t, 5*time.Second, "deliveries", func() bool { return w.mail.count() == 2 })
	w.settle()
	if w.mail.count() != 2 || len(w.mail.to(admin.Email)) != 1 || len(w.mail.to(fan.Email)) != 1 {
		t.Fatalf("recipients: %+v", w.mail.sent)
	}
	m := w.mail.to(fan.Email)[0]
	if m.Subject != "Server Started — Minecraft Survival" {
		t.Errorf("subject %q", m.Subject)
	}
	for _, want := range []string{"Hello Fan,", `The server "Minecraft Survival" has successfully started and is ready.`, "Status: Running", "Requested by: alice", "Time: ", "https://sm.example.com/#/servers/mc", "#/profile"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, m.Body)
		}
	}
}

func TestEachEventIsMailedOnceAndRevocationApplies(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	fan := w.user("fan", false, true, all)
	w.store.SetPermission(ctx, fan.ID, "mc", 1)
	at := time.Now()
	w.n.Notify(tr(db.NotifyStopped, at))
	w.n.Notify(tr(db.NotifyStopped, at)) // the very same event again
	testutil.Eventually(t, 5*time.Second, "delivery", func() bool { return w.mail.count() == 1 })
	w.settle()
	if w.mail.count() != 1 {
		t.Fatalf("a duplicate event was mailed: %d", w.mail.count())
	}

	// Access revoked while a delivery waits for its retry: the retry
	// re-checks eligibility and the email is never sent.
	w.n.RetryDelays = []time.Duration{400 * time.Millisecond}
	w.mail.failNext = 1
	w.n.Notify(tr(db.NotifyCrash, at.Add(time.Second)))
	testutil.Eventually(t, 5*time.Second, "first attempt failed", func() bool {
		l, _ := w.store.ListNotifications(ctx, 1)
		return len(l) == 1 && l[0].Kind == db.NotifyCrash && l[0].Attempts == 1
	})
	w.store.SetPermission(ctx, fan.ID, "mc", 0)
	testutil.Eventually(t, 5*time.Second, "retry skipped", func() bool {
		l, _ := w.store.ListNotifications(ctx, 1)
		return l[0].Status == db.DeliverySkipped
	})
	// And new events do not reach them at all.
	w.n.Notify(tr(db.NotifyStopped, at.Add(2*time.Second)))
	w.settle()
	if got := w.mail.count(); got != 1 {
		t.Fatalf("a user without access was notified: %d mails", got)
	}
}

func TestCooldownSuppressesFloodsAndReportsThem(t *testing.T) {
	w := newWorld(t)
	w.cfg.Update(func(c *config.Config) error { c.Notifications.Cooldown = config.Duration(time.Minute); return nil })
	fan := w.user("fan", false, true, all)
	w.store.SetPermission(context.Background(), fan.ID, "mc", 1)
	start := time.Now()
	crash := func(at time.Time) manager.Transition {
		t := tr(db.NotifyCrash, at)
		t.From, t.To, t.Operation, t.Actor, t.Confirmed, t.Detail = manager.Running, manager.Error, "", "", true, "process exited with code 1"
		return t
	}
	for i := 0; i < 5; i++ { // a crash loop
		w.n.Notify(crash(start.Add(time.Duration(i) * time.Second)))
	}
	testutil.Eventually(t, 5*time.Second, "first crash mail", func() bool { return w.mail.count() == 1 })
	w.settle()
	if w.mail.count() != 1 {
		t.Fatalf("crash loop produced %d mails", w.mail.count())
	}
	first := w.mail.to(fan.Email)[0]
	if first.Subject != "Server Crashed — Minecraft Survival" || !strings.Contains(first.Body, "appears to have crashed") ||
		!strings.Contains(first.Body, "Previous Status: Running") || !strings.Contains(first.Body, "Current Status: Error") || !strings.Contains(first.Body, "code 1") {
		t.Fatalf("crash mail:\n%s\n%s", first.Subject, first.Body)
	}
	// Other kinds are not held back by the crash cooldown.
	w.n.Notify(tr(db.NotifyReady, start.Add(10*time.Second)))
	testutil.Eventually(t, 5*time.Second, "ready mail", func() bool { return w.mail.count() == 2 })
	// After the cooldown, the next crash mail says what was suppressed.
	w.n.Notify(crash(start.Add(2 * time.Minute)))
	testutil.Eventually(t, 5*time.Second, "crash after cooldown", func() bool { return w.mail.count() == 3 })
	last := w.mail.to(fan.Email)[2]
	if !strings.Contains(last.Body, "4 similar notifications about this server were suppressed during the last 1 minute.") {
		t.Fatalf("suppression note missing:\n%s", last.Body)
	}
}

func TestDeliveryRetriesAndFailuresNeverBlock(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	fan := w.user("fan", false, true, all)
	w.store.SetPermission(ctx, fan.ID, "mc", 1)

	// Two failures, then success: delivered on the third attempt.
	w.mail.failNext = 2
	w.n.Notify(tr(db.NotifyReady, time.Now()))
	testutil.Eventually(t, 5*time.Second, "retried delivery", func() bool { return w.mail.count() == 1 })
	testutil.Eventually(t, 5*time.Second, "log", func() bool {
		l, _ := w.store.ListNotifications(ctx, 1)
		return len(l) == 1 && l[0].Status == db.DeliverySent && l[0].Attempts == 3
	})

	// Permanent failure: recorded, address redacted, and Notify still
	// returns immediately however many events arrive.
	w.mail.failAll = true
	began := time.Now()
	for i := 0; i < 500; i++ {
		w.n.Notify(tr(db.NotifyStopped, time.Now().Add(time.Duration(i)*time.Millisecond)))
	}
	if time.Since(began) > time.Second {
		t.Fatal("Notify must never block the caller")
	}
	testutil.Eventually(t, 10*time.Second, "failed delivery", func() bool {
		l, _ := w.store.ListNotifications(ctx, 1000)
		for _, d := range l {
			if d.Status == db.DeliveryFailed {
				if strings.Contains(d.Error, "@") || !strings.Contains(d.Error, "<address>") {
					t.Fatalf("stored error must not contain the address: %q", d.Error)
				}
				return d.Attempts == 3
			}
		}
		return false
	})
}

func TestNothingIsSentWhenDisabledOrUnavailable(t *testing.T) {
	w := newWorld(t)
	fan := w.user("fan", false, true, all)
	w.store.SetPermission(context.Background(), fan.ID, "mc", 1)
	w.cfg.Update(func(c *config.Config) error { c.Notifications.Enabled = false; return nil })
	w.n.Notify(tr(db.NotifyReady, time.Now()))
	w.settle()
	w.cfg.Update(func(c *config.Config) error { c.Notifications.Enabled = true; return nil })
	w.mail.available = false
	w.n.Notify(tr(db.NotifyReady, time.Now().Add(time.Second)))
	w.settle()
	if w.mail.count() != 0 {
		t.Fatalf("sent %d mails while notifications were off or SMTP missing", w.mail.count())
	}
}

func TestSubjectsDistinguishWhatHappened(t *testing.T) {
	base := manager.Transition{ServerName: "MC"}
	cases := []struct {
		kind      string
		confirmed bool
		op        string
		from      manager.State
		want      string
	}{
		{db.NotifyReady, false, manager.OpStart, manager.Starting, "Server Started — MC"},
		{db.NotifyStopped, false, manager.OpStop, manager.Stopping, "Server Stopped — MC"},
		{db.NotifyCrash, true, "", manager.Running, "Server Crashed — MC"},
		{db.NotifyCrash, false, "", manager.Running, "Server Stopped Unexpectedly — MC"},
		{db.NotifyFailure, false, manager.OpStart, manager.Starting, "Server Start Failed — MC"},
		{db.NotifyFailure, false, manager.OpStop, manager.Stopping, "Server Stop Failed — MC"},
		{db.NotifyFailure, false, manager.OpRestart, manager.Running, "Server Restart Failed — MC"},
	}
	for _, c := range cases {
		tr := base
		tr.Kind, tr.Confirmed, tr.Operation, tr.From = c.kind, c.confirmed, c.op, c.from
		if got := Subject(tr); got != c.want {
			t.Errorf("%s/%v/%s: %q, want %q", c.kind, c.confirmed, c.op, got, c.want)
		}
	}
}

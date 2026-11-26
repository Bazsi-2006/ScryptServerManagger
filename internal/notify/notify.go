// Package notify sends the optional server activity emails.
//
// The manager reports lifecycle transitions (see manager.Transition); this
// package decides who gets an email and delivers it in the background, so
// server operations never wait for SMTP:
//
//   - Recipients are accounts that opted in to the kind of event, have a
//     verified email address, are not disabled, may see the server
//     (administrators, or any permission on it) and did not mute it.
//     Eligibility is checked again before every delivery attempt, so a
//     revoked permission or a changed address takes effect immediately.
//   - Each (event, recipient) pair is recorded once in notification_log;
//     the same transition can never be mailed twice to the same person.
//   - Repeated events of the same kind for the same server within the
//     configured cooldown are suppressed and counted; the next email that
//     goes out mentions how many were suppressed.
//   - Failed deliveries are retried with backoff; errors are logged
//     without recipient addresses.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/mail"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/manager"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/perm"
)

// Mailer is the part of mail.Mailer the notifier needs.
type Mailer interface {
	Available() bool
	Send(ctx context.Context, msg mail.Message) (mail.Outcome, error)
}

type delivery struct {
	id       int64
	userID   int64
	t        manager.Transition
	note     string // suppressed-events remark
	attempts int
}

// Notifier turns transitions into emails.
type Notifier struct {
	cfg    *config.Holder
	store  *db.Store
	mailer Mailer
	log    *slog.Logger

	// RetryDelays are the waits before each retry; the number of attempts
	// is len(RetryDelays)+1.
	RetryDelays []time.Duration

	events     chan manager.Transition
	deliveries chan *delivery

	mu         sync.Mutex
	last       map[string]time.Time
	suppressed map[string]int
	timers     map[*time.Timer]struct{}
	closed     bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a notifier; call Start to begin processing.
func New(cfg *config.Holder, store *db.Store, mailer Mailer, log *slog.Logger) *Notifier {
	ctx, cancel := context.WithCancel(context.Background())
	return &Notifier{
		cfg: cfg, store: store, mailer: mailer, log: log,
		RetryDelays: []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute},
		events:      make(chan manager.Transition, 256),
		deliveries:  make(chan *delivery, 1024),
		last:        map[string]time.Time{}, suppressed: map[string]int{}, timers: map[*time.Timer]struct{}{},
		ctx: ctx, cancel: cancel,
	}
}

// Start launches the background workers. Deliveries left pending by a
// previous process are marked abandoned (their retry state was in memory).
func (n *Notifier) Start() {
	ctx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
	if err := n.store.AbandonPendingNotifications(ctx, time.Now()); err != nil {
		n.log.Warn("could not mark interrupted notifications", "error", err)
	}
	cancel()
	n.wg.Add(3)
	go n.eventLoop()
	go n.deliveryLoop()
	go n.deliveryLoop()
}

// Notify queues a transition. It never blocks: if the queue is full the
// event is dropped and logged, because server operations come first.
func (n *Notifier) Notify(t manager.Transition) {
	select {
	case n.events <- t:
	default:
		n.log.Warn("notification queue is full; dropping a notification", "server", t.ServerID, "kind", t.Kind)
	}
}

// Close stops the workers; pending retries are abandoned.
func (n *Notifier) Close() {
	n.mu.Lock()
	n.closed = true
	for t := range n.timers {
		t.Stop()
	}
	n.mu.Unlock()
	n.cancel()
	n.wg.Wait()
}

func (n *Notifier) eventLoop() {
	defer n.wg.Done()
	for {
		select {
		case <-n.ctx.Done():
			return
		case t := <-n.events:
			n.handle(t)
		}
	}
}

func (n *Notifier) deliveryLoop() {
	defer n.wg.Done()
	for {
		select {
		case <-n.ctx.Done():
			return
		case d := <-n.deliveries:
			n.deliver(d)
		}
	}
}

// handle applies the cooldown and fans an event out to its recipients.
func (n *Notifier) handle(t manager.Transition) {
	c := n.cfg.Get()
	if !c.Notifications.Enabled || !n.mailer.Available() {
		return
	}
	key := t.ServerID + "|" + t.Kind
	n.mu.Lock()
	cooldown := c.Notifications.Cooldown.D()
	if last, ok := n.last[key]; ok && cooldown > 0 && t.Time.Sub(last) < cooldown {
		n.suppressed[key]++
		n.mu.Unlock()
		n.log.Debug("notification suppressed by cooldown", "server", t.ServerID, "kind", t.Kind)
		return
	}
	n.last[key] = t.Time
	suppressed := n.suppressed[key]
	delete(n.suppressed, key)
	n.mu.Unlock()

	note := ""
	if suppressed > 0 {
		note = fmt.Sprintf("%d similar notification%s about this server %s suppressed during the last %s.",
			suppressed, plural(suppressed), wereWas(suppressed), humanDuration(cooldown))
	}
	ctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
	defer cancel()
	cands, err := n.store.NotificationCandidates(ctx, t.ServerID, t.Kind)
	if err != nil {
		n.log.Error("could not determine notification recipients", "server", t.ServerID, "error", err)
		return
	}
	eventKey := fmt.Sprintf("%s|%s|%d", t.ServerID, t.Kind, t.Time.UnixNano())
	for _, cand := range cands {
		if cand.Muted || !(cand.User.IsAdmin || perm.Mask(cand.Mask).Any()) {
			continue
		}
		id, fresh, err := n.store.RecordNotification(ctx, eventKey, cand.User.ID, t.ServerID, t.Kind, time.Now())
		if err != nil {
			n.log.Error("could not record notification", "server", t.ServerID, "user_id", cand.User.ID, "error", err)
			continue
		}
		if !fresh {
			continue // already handled: never mail the same event twice
		}
		n.enqueue(&delivery{id: id, userID: cand.User.ID, t: t, note: note})
	}
}

func (n *Notifier) enqueue(d *delivery) {
	select {
	case n.deliveries <- d:
	default:
		n.finish(d, db.DeliveryFailed, "delivery queue is full")
		n.log.Warn("notification delivery queue is full", "server", d.t.ServerID, "user_id", d.userID)
	}
}

// eligible re-checks a recipient right before an attempt.
func (n *Notifier) eligible(ctx context.Context, d *delivery) (*db.User, bool) {
	u, err := n.store.UserByID(ctx, d.userID)
	if err != nil || u.Disabled || !u.EmailVerified || u.Email == "" {
		return nil, false
	}
	prefs, err := n.store.NotificationPrefs(ctx, u.ID)
	if err != nil || !prefs.Wants(d.t.Kind) {
		return nil, false
	}
	if !u.IsAdmin {
		perms, err := n.store.PermissionsForUser(ctx, u.ID)
		if err != nil || !perm.Mask(perms[d.t.ServerID]).Any() {
			return nil, false
		}
	}
	mutes, err := n.store.NotificationMutes(ctx, u.ID)
	if err != nil {
		return nil, false
	}
	for _, m := range mutes {
		if m == d.t.ServerID {
			return nil, false
		}
	}
	return u, true
}

func (n *Notifier) deliver(d *delivery) {
	ctx, cancel := context.WithTimeout(n.ctx, 60*time.Second)
	defer cancel()
	u, ok := n.eligible(ctx, d)
	if !ok {
		n.finish(d, db.DeliverySkipped, "recipient is no longer eligible")
		return
	}
	d.attempts++
	msg := n.compose(u, d)
	outcome, err := n.mailer.Send(ctx, msg)
	if err == nil {
		status := db.DeliverySent
		if outcome == mail.Logged {
			status = db.DeliveryLogged
		}
		n.finish(d, status, "")
		return
	}
	reason := redact(err.Error())
	if d.attempts <= len(n.RetryDelays) && n.ctx.Err() == nil {
		n.update(d, db.DeliveryPending, reason)
		n.log.Warn("notification email could not be delivered; will retry", "server", d.t.ServerID, "kind", d.t.Kind,
			"user_id", d.userID, "attempt", d.attempts, "error", reason)
		n.retryLater(d, n.RetryDelays[d.attempts-1])
		return
	}
	n.finish(d, db.DeliveryFailed, reason)
	n.log.Warn("notification email could not be delivered; giving up", "server", d.t.ServerID, "kind", d.t.Kind,
		"user_id", d.userID, "attempts", d.attempts, "error", reason)
}

func (n *Notifier) retryLater(d *delivery, wait time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	var t *time.Timer
	t = time.AfterFunc(wait, func() {
		n.mu.Lock()
		delete(n.timers, t)
		closed := n.closed
		n.mu.Unlock()
		if !closed {
			n.enqueue(d)
		}
	})
	n.timers[t] = struct{}{}
}

func (n *Notifier) update(d *delivery, status, errText string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.store.UpdateNotification(ctx, d.id, status, d.attempts, errText, time.Now()); err != nil {
		n.log.Error("could not record notification delivery", "error", err)
	}
}

func (n *Notifier) finish(d *delivery, status, errText string) { n.update(d, status, errText) }

// ---- message composition ----

var stateLabel = map[manager.State]string{
	manager.Stopped: "Stopped", manager.Starting: "Starting", manager.Running: "Running",
	manager.Stopping: "Stopping", manager.Error: "Error", manager.Unknown: "Unknown",
}

func label(s manager.State) string {
	if l, ok := stateLabel[s]; ok {
		return l
	}
	return string(s)
}

// Subject returns the email subject for a transition.
func Subject(t manager.Transition) string {
	var what string
	switch t.Kind {
	case db.NotifyReady:
		what = "Server Started"
	case db.NotifyStopped:
		what = "Server Stopped"
	case db.NotifyCrash:
		what = "Server Stopped Unexpectedly"
		if t.Confirmed {
			what = "Server Crashed"
		}
	case db.NotifyFailure:
		what = "Server Start Failed"
		if t.Operation == manager.OpStop || (t.Operation == "" && t.From == manager.Stopping) {
			what = "Server Stop Failed"
		} else if t.Operation == manager.OpRestart {
			what = "Server Restart Failed"
		}
	default:
		what = "Server Notification"
	}
	return what + " — " + t.ServerName
}

func (n *Notifier) compose(u *db.User, d *delivery) mail.Message {
	t := d.t
	c := n.cfg.Get()
	var b strings.Builder
	fmt.Fprintf(&b, "Hello %s,\n\n", firstNonEmpty(u.DisplayName, u.Username))
	name := t.ServerName
	switch t.Kind {
	case db.NotifyReady:
		fmt.Fprintf(&b, "The server %q has successfully started and is ready.\n\n", name)
		fmt.Fprintf(&b, "Status: %s\n", label(t.To))
	case db.NotifyStopped:
		fmt.Fprintf(&b, "The server %q has stopped.\n\n", name)
		fmt.Fprintf(&b, "Status: %s\n", label(t.To))
	case db.NotifyCrash:
		if t.Confirmed {
			fmt.Fprintf(&b, "The server %q appears to have crashed.\n\n", name)
		} else {
			fmt.Fprintf(&b, "The server %q appears to have stopped unexpectedly. It was not stopped through Server Manager and no error was reported, so it may also have been shut down deliberately outside of the application.\n\n", name)
		}
		fmt.Fprintf(&b, "Previous Status: %s\nCurrent Status: %s\n", label(t.From), label(t.To))
	case db.NotifyFailure:
		verb := "Starting"
		switch {
		case t.Operation == manager.OpStop || (t.Operation == "" && t.From == manager.Stopping):
			verb = "Stopping"
		case t.Operation == manager.OpRestart:
			verb = "Restarting"
		}
		fmt.Fprintf(&b, "%s the server %q failed.\n\n", verb, name)
		fmt.Fprintf(&b, "Status: %s\n", label(t.To))
	}
	if t.Detail != "" {
		fmt.Fprintf(&b, "Detail: %s\n", t.Detail)
	}
	if t.Actor != "" && t.Operation != "" {
		fmt.Fprintf(&b, "Requested by: %s\n", t.Actor)
	} else if t.Operation == "" && (t.Kind == db.NotifyReady || t.Kind == db.NotifyStopped) {
		b.WriteString("Note: this change was observed by monitoring; it was not requested through Server Manager.\n")
	}
	fmt.Fprintf(&b, "Time: %s\n", t.Time.Local().Format("2006-01-02 15:04 MST"))
	if d.note != "" {
		fmt.Fprintf(&b, "\n%s\n", d.note)
	}
	base := strings.TrimRight(c.PublicURL, "/")
	if base != "" {
		fmt.Fprintf(&b, "\nOpen the server: %s/#/servers/%s\n", base, t.ServerID)
	}
	b.WriteString("\n-- \nYou receive this email because you enabled server notifications in your Server Manager profile.")
	if base != "" {
		fmt.Fprintf(&b, " Change them at %s/#/profile.", base)
	}
	b.WriteString("\n")
	return mail.Message{To: u.Email, Subject: Subject(t), Body: b.String()}
}

var emailRe = regexp.MustCompile(`[^\s<>"@]+@[^\s<>"@]+`)

// redact removes email addresses from error text before it is logged or
// stored (SMTP servers like to quote the recipient).
func redact(s string) string { return emailRe.ReplaceAllString(s, "<address>") }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func wereWas(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%d hour%s", int(d.Hours()), plural(int(d.Hours())))
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d minute%s", int(d.Minutes()), plural(int(d.Minutes())))
	}
	return d.String()
}

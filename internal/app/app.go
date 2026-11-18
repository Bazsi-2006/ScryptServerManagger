// Package app wires the modules together and runs the HTTP server.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/api"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/events"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/logging"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/mail"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/manager"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/monitor"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
	"github.com/Bazsi-2006/ScryptServerManagger/web"
)

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,31}$`)

// ValidUsername reports whether name is an acceptable login name.
func ValidUsername(name string) bool { return usernameRe.MatchString(name) }

// App is a fully wired application instance.
type App struct {
	Config  *config.Holder
	Store   *db.Store
	Hub     *events.Hub
	Manager *manager.Manager
	Monitor *monitor.Monitor
	API     *api.Server
	Handler http.Handler
	Log     *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
}

// Options tune Build.
type Options struct {
	Version  string
	SetLevel func(string)
	// Static overrides the embedded frontend (tests pass nil-able FS).
	Static fs.FS
	// NoStatic disables frontend serving entirely.
	NoStatic bool
}

// Build constructs every module from a loaded configuration and starts the
// background work (server monitoring, host metrics, housekeeping).
func Build(holder *config.Holder, log *slog.Logger, opts Options) (*App, error) {
	c := holder.Get()
	if err := c.EnsureDirs(); err != nil {
		return nil, err
	}
	store, err := db.Open(c.Paths.Database)
	if err != nil {
		return nil, err
	}
	key, err := c.JWTSecret()
	if err != nil {
		store.Close()
		return nil, err
	}
	a := &App{Config: holder, Store: store, Log: log, Monitor: monitor.New()}
	a.ctx, a.cancel = context.WithCancel(context.Background())

	rec := audit.NewRecorder(store, log)
	a.Hub = events.New(
		func(uid int64, sid string) (events.Identity, bool) { return a.API.ResolveIdentity(uid, sid) },
		func() bool { return holder.Get().Monitoring.HostMetricsForUsers },
	)

	defs := servers.NewStore(c.Paths.ServersDir)
	if err := defs.Load(); err != nil {
		store.Close()
		return nil, err
	}
	for name, problem := range defs.Invalid() {
		log.Error("skipping invalid server definition", "file", name, "problem", problem)
	}
	a.Manager = manager.New(holder, defs, store, a.Hub, rec, log)

	static := opts.Static
	if static == nil && !opts.NoStatic {
		static, err = web.FS()
		if err != nil {
			store.Close()
			return nil, err
		}
	}
	a.API, err = api.New(api.Deps{
		Config: holder, Store: store, Manager: a.Manager, Hub: a.Hub, Signer: auth.NewSigner(key),
		Mailer: mail.New(holder, log), Audit: rec, Monitor: a.Monitor, Log: log, Static: static,
		Version: opts.Version, SetLevel: opts.SetLevel,
	})
	if err != nil {
		store.Close()
		return nil, err
	}
	a.Handler = a.API.Handler()

	a.Manager.Load()
	go a.Hub.Run(a.ctx.Done(), time.Minute)
	go a.Monitor.Run(a.ctx,
		func() time.Duration { return holder.Get().Monitoring.HostMetricsInterval.D() },
		func(s monitor.Sample) {
			a.Hub.Publish(events.Event{Type: "host.metrics", Audience: events.AudienceMetrics, Data: s})
		},
		func(err error) { log.Warn("could not sample host metrics", "error", err) },
	)
	go a.housekeeping()
	return a, nil
}

// housekeeping prunes expired sessions and applies retention settings.
func (a *App) housekeeping() {
	run := func() {
		ctx, cancel := context.WithTimeout(a.ctx, time.Minute)
		defer cancel()
		now := time.Now()
		if err := a.Store.PurgeSessions(ctx, now.Add(-7*24*time.Hour)); err != nil {
			a.Log.Warn("session cleanup failed", "error", err)
		}
		if days := a.Config.Get().Audit.RetentionDays; days > 0 {
			cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
			if n, err := a.Store.PurgeAudit(ctx, cutoff); err != nil {
				a.Log.Warn("audit retention failed", "error", err)
			} else if n > 0 {
				a.Log.Info("removed expired audit entries", "count", n)
			}
			_ = a.Store.PurgeOperations(ctx, cutoff)
		}
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	run()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// Close shuts everything down in dependency order. Servers that can
// outlive the manager keep running.
func (a *App) Close(ctx context.Context) {
	a.Hub.Close()
	a.Manager.Shutdown(ctx)
	a.cancel()
	if err := a.Store.Close(); err != nil {
		a.Log.Warn("closing database", "error", err)
	}
}

// Run loads the configuration and serves until SIGINT/SIGTERM.
func Run(configPath, version string) error {
	holder, err := config.Load(configPath)
	if err != nil {
		return err
	}
	c := holder.Get()
	if err := c.EnsureDirs(); err != nil {
		return err
	}
	logger, err := logging.New(c.Logging)
	if err != nil {
		return err
	}
	log := logger.Logger
	log.Info("starting server-manager", "version", version, "mode", c.Mode, "config", holder.Path())
	if c.Development() {
		log.Warn("development mode is enabled: outgoing email is printed to this log; do not use in production")
	}

	a, err := Build(holder, log, Options{Version: version, SetLevel: logger.SetLevel})
	if err != nil {
		return err
	}
	if n, err := a.Store.CountUsers(context.Background()); err == nil && n == 0 {
		log.Warn("no accounts exist yet; create the first administrator with: server-manager admin create --username <name>")
	}

	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		a.Close(context.Background())
		return fmt.Errorf("listen on %s: %w", c.Listen, err)
	}
	srv := &http.Server{
		Handler:           a.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	log.Info("listening", "address", ln.Addr().String())

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case err := <-errCh:
			a.Close(context.Background())
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case s := <-sig:
			if s == syscall.SIGHUP {
				// Reopen the log file (logrotate) and pick up edited
				// server definitions.
				if err := logger.Reopen(); err != nil {
					log.Error("could not reopen log file", "error", err)
				}
				if res, err := a.Manager.Reload(); err != nil {
					log.Error("reload failed", "error", err)
				} else {
					log.Info("reloaded server definitions", "added", len(res.Added), "removed", len(res.Removed), "updated", len(res.Updated), "invalid", len(res.Invalid))
					a.Hub.Revalidate()
				}
				continue
			}
			log.Info("shutting down", "signal", s.String())
			// Streams end first so Shutdown is not kept waiting by them.
			a.Hub.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = srv.Shutdown(ctx)
			cancel()
			_ = srv.Close()
			// Stopping servers that must not outlive us may take a while.
			ctx, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
			a.Close(ctx)
			cancel()
			log.Info("stopped")
			return nil
		}
	}
}

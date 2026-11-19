// Package server is the HTTP layer: routing, authentication, CSRF and
// origin checks, per-request authorization and the embedded frontend.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/control"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/store"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/web"
)

const (
	maxSessionsPerUser = 10
	loginBodyLimit     = 4 << 10
)

// Server serves the controller. Create it with New.
type Server struct {
	cfg   *config.Config
	store *store.Store
	ctl   *control.Controller
	log   *slog.Logger
	now   func() time.Time

	loginIP   *failureLimiter
	loginUser *failureLimiter
	actions   *tokenBucket

	index  []byte
	assets map[string]asset

	handler http.Handler
}

type asset struct {
	data  []byte
	ctype string
	hash  string
}

// Options are optional dependencies, mainly for tests.
type Options struct {
	Logger *slog.Logger
	Now    func() time.Time
}

// New builds the server and its routes.
func New(cfg *config.Config, st *store.Store, ctl *control.Controller, opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{
		cfg:   cfg,
		store: st,
		ctl:   ctl,
		log:   opts.Logger,
		now:   opts.Now,
		// 10 failures per 15 minutes per client address, 8 per account.
		loginIP:   newFailureLimiter(10, 15*time.Minute, 50_000),
		loginUser: newFailureLimiter(8, 15*time.Minute, 10_000),
		// Up to 6 power requests in a burst, then one every 20 seconds.
		actions: newTokenBucket(6, 20*time.Second),
	}
	if err := s.loadFrontend(); err != nil {
		return nil, err
	}
	s.handler = s.routes()
	return s, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes() http.Handler {
	b := s.cfg.BasePath // "" or "/prefix"
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+b+"/{$}", s.handleIndex)
	if b != "" {
		mux.HandleFunc("GET "+b, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, b+"/", http.StatusMovedPermanently)
		})
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, b+"/", http.StatusFound)
		})
	}
	mux.HandleFunc("GET "+b+"/static/{file}", s.handleAsset)
	mux.HandleFunc("GET "+b+"/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("GET "+b+"/api/session", s.handleSession)
	mux.HandleFunc("POST "+b+"/api/login", s.handleLogin)
	mux.Handle("POST "+b+"/api/logout", s.requireSession(s.requireCSRF(http.HandlerFunc(s.handleLogout))))
	mux.Handle("GET "+b+"/api/vms", s.requireSession(http.HandlerFunc(s.handleListVMs)))
	mux.Handle("GET "+b+"/api/vms/{id}", s.requireSession(http.HandlerFunc(s.handleGetVM)))
	mux.Handle("POST "+b+"/api/vms/{id}/start", s.requireSession(s.requireCSRF(s.powerHandler(config.ActionStart))))
	mux.Handle("POST "+b+"/api/vms/{id}/stop", s.requireSession(s.requireCSRF(s.powerHandler(config.ActionStop))))
	mux.HandleFunc(b+"/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
	})

	// Reject cross-origin state-changing requests (Sec-Fetch-Site / Origin)
	// before any handler runs. This complements the per-session CSRF token
	// and also protects the login form.
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Warn("cross-origin request rejected", "path", r.URL.Path, "origin", safeLogValue(r.Header.Get("Origin")),
			"client", clientInfoFor(r, s.cfg.TrustedProxies).IP)
		writeError(w, http.StatusForbidden, "cross_origin", "Cross-origin requests are not allowed.")
	}))
	return recoverPanics(s.log, securityHeaders(cop.Handler(mux)))
}

// ---- frontend --------------------------------------------------------------

func (s *Server) loadFrontend() error {
	s.assets = map[string]asset{}
	err := fs.WalkDir(web.Files, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := web.Files.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		ctype := mime.TypeByExtension(path.Ext(p))
		switch path.Ext(p) {
		case ".js":
			ctype = "text/javascript; charset=utf-8"
		case ".css":
			ctype = "text/css; charset=utf-8"
		case ".svg":
			ctype = "image/svg+xml"
		}
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		s.assets[path.Base(p)] = asset{data: data, ctype: ctype, hash: hex.EncodeToString(sum[:8])}
		return nil
	})
	if err != nil {
		return fmt.Errorf("server: loading frontend: %w", err)
	}
	tmpl, err := template.ParseFS(web.Files, "index.html")
	if err != nil {
		return fmt.Errorf("server: parsing index.html: %w", err)
	}
	urls := map[string]string{}
	for name, a := range s.assets {
		urls[name] = s.cfg.BasePath + "/static/" + name + "?v=" + a.hash
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{
		"Title":  s.cfg.Title,
		"Base":   s.cfg.BasePath + "/",
		"Assets": urls,
	}); err != nil {
		return fmt.Errorf("server: rendering index.html: %w", err)
	}
	s.index = buf.Bytes()
	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(s.index)
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	a, ok := s.assets[r.PathValue("file")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", a.ctype)
	w.Header().Set("ETag", `"`+a.hash+`"`)
	if r.URL.Query().Get("v") == a.hash {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.data))
}

// ---- background maintenance ------------------------------------------------

// Prepare runs startup maintenance: sessions of users that are no longer
// configured or are disabled are deleted, so access is revoked even before
// those sessions are next used.
func (s *Server) Prepare(ctx context.Context) error {
	var active []string
	for _, u := range s.cfg.Usernames() {
		if s.cfg.UserActive(u) {
			active = append(active, u)
		}
	}
	n, err := s.store.DeleteSessionsExcept(ctx, active)
	if err != nil {
		return fmt.Errorf("revoking sessions of removed users: %w", err)
	}
	if n > 0 {
		s.log.Info("revoked sessions of users that are not active in the configuration", "sessions", n)
	}
	if _, err := s.store.DeleteExpiredSessions(ctx, s.now(), s.cfg.IdleTimeout); err != nil {
		return fmt.Errorf("deleting expired sessions: %w", err)
	}
	return nil
}

// RunMaintenance periodically deletes expired sessions and prunes the
// in-memory rate limiters until ctx is cancelled.
func (s *Server) RunMaintenance(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := s.now()
			if _, err := s.store.DeleteExpiredSessions(ctx, now, s.cfg.IdleTimeout); err != nil && ctx.Err() == nil {
				s.log.Warn("deleting expired sessions failed", "error", err)
			}
			s.loginIP.prune(now)
			s.loginUser.prune(now)
		}
	}
}

func basePathOr(b string) string {
	if b == "" {
		return "/"
	}
	return b
}

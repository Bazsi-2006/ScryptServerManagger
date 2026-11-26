package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/backup"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/manager"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/perm"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// serverView is what any user with access to a server may see. It carries
// no paths, commands or environment: those are only available through the
// configuration endpoint, which requires CONFIG.
type serverView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Category is the definition's raw value; CategoryID is set when it
	// refers to a managed category, CategoryName is what to display.
	Category     string               `json:"category"`
	CategoryID   string               `json:"category_id"`
	CategoryName string               `json:"category_name"`
	Backend      string               `json:"backend"`
	Status       manager.Status       `json:"status"`
	Capabilities manager.Capabilities `json:"capabilities"`
	Permissions  int                  `json:"permissions"`
	// Actions are the custom actions the caller may run (administrators:
	// all of them, disabled ones flagged). Never includes what they execute.
	Actions []actionView `json:"actions"`
}

// actionView is the public description of a custom action.
type actionView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Method      string `json:"method"`
	Confirm     bool   `json:"confirm"`
	Disabled    bool   `json:"disabled,omitempty"`
}

// visibleActions lists the actions of def the caller may run.
func (s *Server) visibleActions(r *http.Request, id *identity, def *servers.Definition, mask perm.Mask) []actionView {
	out := []actionView{}
	var grants map[string]bool
	if !id.User.IsAdmin {
		if !mask.Any() {
			return out
		}
		all, err := s.loadActionGrants(r, id)
		if err != nil {
			return out
		}
		grants = all[def.ID]
	}
	for _, a := range def.Actions {
		if !id.User.IsAdmin && (a.Disabled || !grants[a.ID]) {
			continue
		}
		out = append(out, actionView{ID: a.ID, Name: a.Name, Description: a.Description, Icon: a.Icon, Method: a.Method, Confirm: a.Confirm, Disabled: a.Disabled})
	}
	return out
}

func (s *Server) view(r *http.Request, def *servers.Definition, mask perm.Mask) serverView {
	st, _ := s.Manager.Status(def.ID)
	caps, _ := s.Manager.Capabilities(def.ID)
	c := s.Config.Get()
	v := serverView{ID: def.ID, Name: def.Name, Description: def.Description, Category: def.Category, CategoryName: c.CategoryLabel(def.Category),
		Backend: def.Backend, Status: st, Capabilities: caps, Permissions: int(mask)}
	if cat, ok := c.ResolveCategory(def.Category); ok {
		v.CategoryID = cat.ID
	}
	v.Actions = s.visibleActions(r, s.ident(r), def, mask)
	return v
}

// access resolves a server for the caller and checks one permission.
//
// A caller with no permission at all on the server gets 404, exactly as if
// it did not exist, so server IDs cannot be probed. A caller who can see
// the server but lacks the specific permission gets 403.
func (s *Server) access(w http.ResponseWriter, r *http.Request, need perm.Mask, auditAction string) (*servers.Definition, perm.Mask, bool) {
	return s.accessCheck(w, r, need, false, auditAction)
}

// accessAny is access satisfied by any one of the flags in need.
func (s *Server) accessAny(w http.ResponseWriter, r *http.Request, need perm.Mask, auditAction string) (*servers.Definition, perm.Mask, bool) {
	return s.accessCheck(w, r, need, true, auditAction)
}

func (s *Server) accessCheck(w http.ResponseWriter, r *http.Request, need perm.Mask, anyOf bool, auditAction string) (*servers.Definition, perm.Mask, bool) {
	id := s.ident(r)
	serverID := r.PathValue("id")
	def, err := s.Manager.Definition(serverID)
	if err != nil {
		s.fail(w, r, errNotFound)
		return nil, 0, false
	}
	mask, err := s.effective(r, id, serverID)
	if err != nil {
		s.fail(w, r, err)
		return nil, 0, false
	}
	if !mask.Any() {
		s.fail(w, r, errNotFound)
		return nil, 0, false
	}
	granted := mask.Has(need)
	if anyOf {
		granted = mask.HasAny(need)
	}
	if need != 0 && !granted {
		names := need.String()
		if anyOf {
			names = strings.ReplaceAll(names, "|", " or ")
		}
		if auditAction != "" {
			s.Audit.Record(s.actor(r), serverID, auditAction, audit.Denied, map[string]any{"required": names})
		}
		s.fail(w, r, &apiError{Status: http.StatusForbidden, Code: "permission_denied",
			Message: "You do not have the " + names + " permission for this server."})
		return nil, 0, false
	}
	return def, mask, true
}

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	id := s.ident(r)
	out := []serverView{}
	for _, def := range s.Manager.Definitions() {
		mask, err := s.effective(r, id, def.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !mask.Any() {
			continue
		}
		out = append(out, s.view(r, def, mask))
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	def, mask, ok := s.access(w, r, 0, "")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": s.view(r, def, mask)})
}

func (s *Server) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	var def servers.Definition
	if err := s.decode(w, r, &def); err != nil {
		s.fail(w, r, err)
		return
	}
	def.RestoreMaskedEnv(nil)
	saved, err := s.Manager.Create(&def)
	if err != nil {
		if s.toAPIError(err) == nil {
			s.Log.Error("could not create server", "error", err)
			err = errf(http.StatusInternalServerError, "not_saved", "The server definition could not be saved. Check that the service user may write to the servers directory.")
		}
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), saved.ID, audit.ServerCreate, audit.Success, map[string]any{"backend": saved.Backend, "name": saved.Name, "actions": len(saved.Actions)})
	s.Manager.NotifyCapacityChanged()
	writeJSON(w, http.StatusCreated, map[string]any{"server": s.view(r, saved, perm.All), "config": saved})
}

// fullConfigAccess reports whether the caller may edit execution settings.
func (s *Server) fullConfigAccess(id *identity) bool {
	return id.User.IsAdmin || s.Config.Get().Security.ConfigPermissionFullAccess
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	def, _, ok := s.access(w, r, perm.Config, "")
	if !ok {
		return
	}
	id := s.ident(r)
	scope := "safe"
	if s.fullConfigAccess(id) {
		scope = "full"
	}
	if !id.User.IsAdmin {
		// Environment values may hold credentials, and custom actions are
		// managed by administrators only: neither is shown to others.
		def = def.MaskEnv().WithoutActions()
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": def, "edit_scope": scope})
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	old, mask, ok := s.access(w, r, perm.Config, audit.ServerUpdate)
	if !ok {
		return
	}
	var def servers.Definition
	if err := s.decode(w, r, &def); err != nil {
		s.fail(w, r, err)
		return
	}
	if def.ID != "" && def.ID != old.ID {
		s.fail(w, r, badRequest("The server ID cannot be changed."))
		return
	}
	def.ID = old.ID
	def.RestoreMaskedEnv(old)
	id := s.ident(r)
	if !id.User.IsAdmin {
		// Non-administrators never see the custom actions, so whatever
		// they send cannot change them.
		def.Actions = old.Clone().Actions
	}
	def.Normalize()
	if err := def.Validate(); err != nil {
		s.fail(w, r, err)
		return
	}
	if !id.User.IsAdmin && strings.TrimSpace(old.Category) != def.Category {
		// The category decides which concurrency limit applies.
		s.Audit.Record(s.actor(r), old.ID, audit.ServerUpdate, audit.Denied, map[string]any{"reason": "category changes require an administrator"})
		s.fail(w, r, &apiError{Status: http.StatusForbidden, Code: "permission_denied",
			Message: "Only administrators can change a server's category, because it decides which concurrency limit applies."})
		return
	}
	if !s.fullConfigAccess(id) && !servers.ExecutionEqual(old, &def) {
		// Holders of CONFIG may tune presentation, timeouts and the console
		// readiness pattern, but not anything that decides what is executed
		// on the host: that would be arbitrary command execution.
		s.Audit.Record(s.actor(r), old.ID, audit.ServerUpdate, audit.Denied, map[string]any{"reason": "execution settings require an administrator"})
		s.fail(w, r, &apiError{Status: http.StatusForbidden, Code: "permission_denied",
			Message: "Only administrators can change execution settings (backend, commands, paths, environment, console input, backups). You can edit the name, description, category, timeouts and console readiness pattern."})
		return
	}
	saved, err := s.Manager.Update(&def)
	if err != nil {
		if s.toAPIError(err) == nil {
			s.Log.Error("could not update server", "server", old.ID, "error", err)
			err = errf(http.StatusInternalServerError, "not_saved", "The server definition could not be saved.")
		}
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), saved.ID, audit.ServerUpdate, audit.Success, map[string]any{"execution_changed": !servers.ExecutionEqual(old, saved)})
	out := saved
	if !id.User.IsAdmin {
		out = saved.MaskEnv().WithoutActions()
	}
	if old.Category != saved.Category {
		s.Manager.NotifyCapacityChanged()
	}
	if len(old.Actions) != len(saved.Actions) || !servers.ExecutionEqual(old, saved) {
		// Removed actions lose their grants; refresh live identities.
		s.Hub.Revalidate()
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": s.view(r, saved, mask), "config": out})
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	force := r.URL.Query().Get("force") == "true"
	if err := s.Manager.Delete(serverID, force); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), serverID, audit.ServerDelete, audit.Success, map[string]any{"force": force})
	s.Hub.Revalidate()
	w.WriteHeader(http.StatusNoContent)
}

// operate runs one of the lifecycle actions.
func (s *Server) operate(action string, need perm.Mask, auditAction string) handler {
	return func(w http.ResponseWriter, r *http.Request) {
		def, _, ok := s.access(w, r, need, auditAction)
		if !ok {
			return
		}
		rl := s.Config.Get().RateLimits
		key := itoa(s.ident(r).User.ID) + ":" + def.ID
		switch action {
		case manager.OpStart:
			if !s.rateLimit(w, r, "start:"+key, rl.ServerStart) {
				return
			}
		case manager.OpStop:
			if !s.rateLimit(w, r, "stop:"+key, rl.ServerStop) {
				return
			}
		case manager.OpRestart:
			if !s.rateLimit(w, r, "stop:"+key, rl.ServerStop) || !s.rateLimit(w, r, "start:"+key, rl.ServerStart) {
				return
			}
		}
		var req struct {
			// OverrideLimits starts the server although a concurrency
			// limit is reached (administrators only; audited).
			OverrideLimits bool `json:"override_limits"`
		}
		if r.ContentLength != 0 && action != manager.OpStop {
			if err := s.decode(w, r, &req); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		if req.OverrideLimits && !s.ident(r).User.IsAdmin {
			s.Audit.Record(s.actor(r), def.ID, auditAction, audit.Denied, map[string]any{"reason": "limit override requires an administrator"})
			s.fail(w, r, &apiError{Status: http.StatusForbidden, Code: "permission_denied", Message: "Only administrators can override the concurrency limits."})
			return
		}
		opts := manager.StartOptions{OverrideLimits: req.OverrideLimits}
		var op *manager.OpInfo
		var err error
		switch action {
		case manager.OpStart:
			op, err = s.Manager.StartServerWith(def.ID, s.actor(r), opts)
		case manager.OpStop:
			op, err = s.Manager.StopServer(def.ID, s.actor(r))
		case manager.OpRestart:
			op, err = s.Manager.RestartServerWith(def.ID, s.actor(r), opts)
		}
		if err != nil {
			var le *manager.LimitError
			if errors.As(err, &le) {
				s.Audit.Record(s.actor(r), def.ID, auditAction, audit.Denied, map[string]any{
					"reason": "concurrency limit", "scope": le.Scope, "category": le.CategoryName, "limit": le.Limit, "in_use": le.InUse})
			}
			s.failOp(w, r, err)
			return
		}
		st, _ := s.Manager.Status(def.ID)
		// 202: the operation runs in the background; follow it through the
		// status endpoint or the event stream.
		writeJSON(w, http.StatusAccepted, map[string]any{"operation": op, "status": st})
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	def, _, ok := s.access(w, r, 0, "")
	if !ok {
		return
	}
	st, err := s.Manager.Status(def.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": st})
}

func (s *Server) handleOperations(w http.ResponseWriter, r *http.Request) {
	def, _, ok := s.access(w, r, 0, "")
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ops, err := s.Manager.Operations(r.Context(), def.ID, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": ops})
}

// ---- console ----

func (s *Server) handleConsoleSnapshot(w http.ResponseWriter, r *http.Request) {
	def, _, ok := s.access(w, r, perm.ConsoleRead, "")
	if !ok {
		return
	}
	n := 0
	if v := r.URL.Query().Get("lines"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 {
			s.fail(w, r, badRequest("lines must be a positive number."))
			return
		}
	}
	snap, err := s.Manager.ConsoleSnapshot(r.Context(), def.ID, n)
	if err != nil {
		s.failOp(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleConsoleCommand(w http.ResponseWriter, r *http.Request) {
	def, _, ok := s.access(w, r, perm.ConsoleWrite, audit.ConsoleCommand)
	if !ok {
		return
	}
	var req struct {
		Command string `json:"command"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !s.rateLimit(w, r, "cmd:"+itoa(s.ident(r).User.ID)+":"+def.ID, s.Config.Get().RateLimits.ConsoleCommand) {
		return
	}
	logged := req.Command
	if len(logged) > 500 {
		logged = logged[:500] + "…"
	}
	if err := s.Manager.SendCommand(r.Context(), def.ID, req.Command); err != nil {
		var ie *manager.InputError
		if !errors.As(err, &ie) {
			s.Audit.Record(s.actor(r), def.ID, audit.ConsoleCommand, audit.Failure, map[string]any{"command": logged, "error": err.Error()})
		}
		s.failOp(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), def.ID, audit.ConsoleCommand, audit.Success, map[string]any{"command": logged})
	w.WriteHeader(http.StatusNoContent)
}

// handleConsoleHistory returns the caller's own recent commands for this
// server (for input recall), oldest first.
func (s *Server) handleConsoleHistory(w http.ResponseWriter, r *http.Request) {
	def, _, ok := s.access(w, r, perm.ConsoleWrite, "")
	if !ok {
		return
	}
	uid := s.ident(r).User.ID
	entries, err := s.Store.QueryAudit(r.Context(), db.AuditFilter{UserID: &uid, ServerID: def.ID, Action: audit.ConsoleCommand, Result: audit.Success, Limit: 50})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []map[string]any{}
	for i := len(entries) - 1; i >= 0; i-- {
		if cmd, ok := entries[i].Details["command"].(string); ok {
			out = append(out, map[string]any{"command": cmd, "time": entries[i].Time})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": out})
}

// ---- backups ----

func (s *Server) canDeleteBackups(id *identity, def *servers.Definition) bool {
	return id.User.IsAdmin || (def.Backup != nil && def.Backup.AllowUserDelete)
}

// handleListBackups serves the backups tab. Either backup permission opens
// it; only BACKUP_DOWNLOAD reveals the files, only BACKUP_CREATE allows
// starting a backup.
func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	def, mask, ok := s.accessAny(w, r, perm.AnyBackup, "")
	if !ok {
		return
	}
	caps, _ := s.Manager.Capabilities(def.ID)
	st, _ := s.Manager.Status(def.ID)
	canList := mask.Has(perm.BackupDownload)
	out := map[string]any{"files": []backup.File{}, "configured": caps.Backups, "can_list": canList,
		"can_create": caps.BackupCreate && mask.Has(perm.BackupCreate), "create_supported": caps.BackupCreate,
		"method": caps.BackupMethod, "can_delete": canList && s.canDeleteBackups(s.ident(r), def), "operation": nil}
	if st.Operation != nil && st.Operation.Type == manager.OpBackup {
		out["operation"] = st.Operation
	}
	if !canList {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if caps.Backups {
		files, err := backup.List(def.Backup.Directory)
		if err != nil {
			// Explain rather than fail: the directory may simply not exist
			// until the first backup has run.
			out["note"] = "The backup directory is not accessible to the service user (it may not exist yet)."
		} else {
			out["files"] = files
		}
	} else {
		out["note"] = "No backup directory is configured for this server."
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	def, _, ok := s.access(w, r, perm.BackupCreate, audit.BackupCreate)
	if !ok {
		return
	}
	if !s.rateLimit(w, r, "backup:"+itoa(s.ident(r).User.ID)+":"+def.ID, s.Config.Get().RateLimits.BackupCreate) {
		return
	}
	op, err := s.Manager.Backup(def.ID, s.actor(r))
	if err != nil {
		s.failOp(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": op})
}

func (s *Server) handleCancelBackup(w http.ResponseWriter, r *http.Request) {
	def, _, ok := s.access(w, r, perm.BackupCreate, audit.BackupCancel)
	if !ok {
		return
	}
	if err := s.Manager.CancelBackup(def.ID); err != nil {
		if errors.Is(err, manager.ErrNotRunning) {
			s.fail(w, r, errf(http.StatusConflict, "not_running", "No backup is in progress."))
			return
		}
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), def.ID, audit.BackupCancel, audit.Success, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	def, _, ok := s.access(w, r, perm.BackupDownload, audit.BackupDelete)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !s.canDeleteBackups(s.ident(r), def) {
		s.Audit.Record(s.actor(r), def.ID, audit.BackupDelete, audit.Denied, map[string]any{"file": name})
		s.fail(w, r, &apiError{Status: http.StatusForbidden, Code: "permission_denied", Message: "Only administrators can delete backups of this server."})
		return
	}
	if def.Backup == nil || def.Backup.Directory == "" {
		s.fail(w, r, errNotFound)
		return
	}
	if err := backup.Delete(def.Backup.Directory, name); err != nil {
		if s.toAPIError(err) == nil {
			s.Log.Warn("backup deletion failed", "server", def.ID, "error", err)
			err = errf(http.StatusBadGateway, "operation_failed", "The backup file could not be deleted (check directory permissions).")
		}
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), def.ID, audit.BackupDelete, audit.Success, map[string]any{"file": name})
	s.Manager.NotifyBackupsChanged(def.ID)
	w.WriteHeader(http.StatusNoContent)
}

// Download tickets: browsers cannot attach an Authorization header to a
// plain file download, and buffering multi-gigabyte backups in JavaScript
// is not an option. An authorized API call therefore mints a ticket that is
// random, bound to one file, valid for one minute and consumed on first
// use; the browser then fetches /api/v1/downloads/<ticket> natively.
type ticket struct {
	serverID string
	file     string
	dir      string
	actor    audit.Actor
	expires  time.Time
}

type ticketStore struct {
	mu sync.Mutex
	m  map[string]ticket
}

func newTicketStore() *ticketStore { return &ticketStore{m: map[string]ticket{}} }

func (t *ticketStore) issue(tk ticket) string {
	id := auth.MustToken(32)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for k, v := range t.m {
		if now.After(v.expires) {
			delete(t.m, k)
		}
	}
	t.m[auth.HashToken(id)] = tk
	return id
}

func (t *ticketStore) take(id string) (ticket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := auth.HashToken(id)
	tk, ok := t.m[key]
	delete(t.m, key)
	if !ok || time.Now().After(tk.expires) {
		return ticket{}, false
	}
	return tk, true
}

func (s *Server) handleDownloadTicket(w http.ResponseWriter, r *http.Request) {
	def, _, ok := s.access(w, r, perm.BackupDownload, audit.BackupDownload)
	if !ok {
		return
	}
	if def.Backup == nil || def.Backup.Directory == "" {
		s.fail(w, r, errNotFound)
		return
	}
	name := r.PathValue("name")
	f, info, err := backup.Open(def.Backup.Directory, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f.Close()
	if max := s.Config.Get().Backups.MaxDownloadBytes; max > 0 && info.Size() > max {
		s.fail(w, r, errf(http.StatusRequestEntityTooLarge, "download_too_large", "This file is larger than the configured download limit."))
		return
	}
	id := s.tickets.issue(ticket{serverID: def.ID, file: name, dir: def.Backup.Directory, actor: s.actor(r), expires: time.Now().Add(time.Minute)})
	writeJSON(w, http.StatusOK, map[string]any{"url": "/api/v1/downloads/" + id, "expires_in": 60, "size": info.Size(), "name": name})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	tk, ok := s.tickets.take(r.PathValue("ticket"))
	if !ok {
		s.fail(w, r, errf(http.StatusNotFound, "ticket_invalid", "This download link is invalid or has expired."))
		return
	}
	c := s.Config.Get()
	if n := s.downloads.Add(1); n > int64(c.Backups.MaxConcurrentDownloads) {
		s.downloads.Add(-1)
		s.fail(w, r, &apiError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "Too many downloads are in progress. Try again shortly.", RetryAfter: 10 * time.Second})
		return
	}
	defer s.downloads.Add(-1)
	// The file is re-opened through the traversal-safe root at use time.
	f, info, err := backup.Open(tk.dir, tk.file)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer f.Close()
	if c.Backups.MaxDownloadBytes > 0 && info.Size() > c.Backups.MaxDownloadBytes {
		s.fail(w, r, errf(http.StatusRequestEntityTooLarge, "download_too_large", "This file is larger than the configured download limit."))
		return
	}
	s.Audit.Record(tk.actor, tk.serverID, audit.BackupDownload, audit.Success, map[string]any{"file": tk.file, "size": info.Size()})
	noDeadline(w)
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": tk.file}))
	h.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// Copy exactly the size we announced, even if the file is still growing.
	if _, err := io.CopyN(w, f, info.Size()); err != nil && !errors.Is(err, io.EOF) {
		s.Log.Debug("download aborted", "server", tk.serverID, "error", err)
	}
}

// ---- host metrics ----

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	c := s.Config.Get()
	if !s.ident(r).User.IsAdmin && !c.Monitoring.HostMetricsForUsers {
		s.fail(w, r, errForbidden)
		return
	}
	latest, ok := s.Monitor.Latest()
	out := map[string]any{"interval_ms": c.Monitoring.HostMetricsInterval.D().Milliseconds(), "history": s.Monitor.History(), "latest": nil}
	if ok {
		out["latest"] = latest
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"name": "Server Manager", "version": s.Version, "registration": "invitation",
		"password_reset": s.passwordResetAvailable()})
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(OpenAPISpec)
}

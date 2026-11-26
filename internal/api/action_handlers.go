package api

import (
	"net/http"
	"sort"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/manager"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// Custom actions.
//
// Authorization: running an action needs access to the server (any server
// permission) plus an explicit grant for that action; administrators can
// run everything. Console write, start or stop permissions never imply an
// action grant, and an action grant never implies anything else. What an
// action executes is only ever visible to administrators.

// actionStatusView is an action as listed for one caller, with its current
// and last run.
type actionStatusView struct {
	actionView
	Running *manager.ActionRun `json:"running"`
	LastRun *db.Operation      `json:"last_run"`
}

// handleListActions lists the actions the caller may run, with their state.
func (s *Server) handleListActions(w http.ResponseWriter, r *http.Request) {
	def, mask, ok := s.access(w, r, 0, "")
	if !ok {
		return
	}
	visible := s.visibleActions(r, s.ident(r), def, mask)
	running := s.Manager.RunningActions(def.ID)
	last, err := s.Manager.LastActionRuns(r.Context(), def.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]actionStatusView, 0, len(visible))
	for _, a := range visible {
		v := actionStatusView{actionView: a, LastRun: last[a.ID]}
		if run, ok := running[a.ID]; ok {
			run := run
			v.Running = &run
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": out})
}

// handleRunAction runs one custom action. The request can only name the
// action and confirm it; nothing about what is executed comes from it.
func (s *Server) handleRunAction(w http.ResponseWriter, r *http.Request) {
	def, _, ok := s.access(w, r, 0, "")
	if !ok {
		return
	}
	actionID := r.PathValue("aid")
	id := s.ident(r)
	allowed, err := s.actionAllowed(r, id, def.ID, actionID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	a := def.Action(actionID)
	if !allowed || a == nil || (a.Disabled && !id.User.IsAdmin) {
		// Unknown, disabled and not-granted actions look the same to
		// regular users, so action IDs cannot be probed.
		if a != nil && !allowed {
			s.Audit.Record(s.actor(r), def.ID, audit.ActionRun, audit.Denied, map[string]any{"action_id": actionID})
		}
		if id.User.IsAdmin && a == nil {
			s.fail(w, r, errf(http.StatusNotFound, "not_found", "The custom action was not found."))
			return
		}
		s.fail(w, r, &apiError{Status: http.StatusForbidden, Code: "permission_denied", Message: "You are not allowed to run this action."})
		return
	}
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if r.ContentLength != 0 {
		if err := s.decode(w, r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if !s.rateLimit(w, r, "action:"+itoa(id.User.ID)+":"+def.ID, s.Config.Get().RateLimits.ActionRun) {
		return
	}
	run, err := s.Manager.RunAction(def.ID, actionID, s.actor(r), req.Confirm)
	if err != nil {
		s.failOp(w, r, err)
		return
	}
	// 202: the action runs in the background; its result arrives as a
	// server.action.completed event and in GET .../actions.
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
}

// ---- administration ----

func (s *Server) handleCreateAction(w http.ResponseWriter, r *http.Request) {
	var a servers.Action
	if err := s.decode(w, r, &a); err != nil {
		s.fail(w, r, err)
		return
	}
	s.saveAction(w, r, a, true)
}

func (s *Server) handleUpdateAction(w http.ResponseWriter, r *http.Request) {
	var a servers.Action
	if err := s.decode(w, r, &a); err != nil {
		s.fail(w, r, err)
		return
	}
	if a.ID != "" && a.ID != r.PathValue("aid") {
		s.fail(w, r, badRequest("The action ID cannot be changed; create a new action instead."))
		return
	}
	a.ID = r.PathValue("aid")
	s.saveAction(w, r, a, false)
}

func (s *Server) saveAction(w http.ResponseWriter, r *http.Request, a servers.Action, create bool) {
	serverID := r.PathValue("id")
	saved, err := s.Manager.SaveAction(serverID, a, create)
	if err != nil {
		if s.toAPIError(err) == nil {
			s.Log.Error("could not save custom action", "server", serverID, "error", err)
			err = errf(http.StatusInternalServerError, "not_saved", "The server definition could not be saved.")
		}
		s.fail(w, r, err)
		return
	}
	act := audit.ActionUpdate
	status := http.StatusOK
	if create {
		act, status = audit.ActionCreate, http.StatusCreated
	}
	stored := saved.Action(a.ID)
	s.Audit.Record(s.actor(r), serverID, act, audit.Success, map[string]any{"action_id": stored.ID, "action_name": stored.Name, "method": stored.Method})
	writeJSON(w, status, map[string]any{"action": stored, "config": saved})
}

func (s *Server) handleDeleteAction(w http.ResponseWriter, r *http.Request) {
	serverID, actionID := r.PathValue("id"), r.PathValue("aid")
	if _, err := s.Manager.DeleteAction(serverID, actionID); err != nil {
		if s.toAPIError(err) == nil {
			s.Log.Error("could not delete custom action", "server", serverID, "error", err)
			err = errf(http.StatusInternalServerError, "not_saved", "The server definition could not be saved.")
		}
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), serverID, audit.ActionDelete, audit.Success, map[string]any{"action_id": actionID})
	s.Hub.Revalidate()
	w.WriteHeader(http.StatusNoContent)
}

// handleSetActionPermission grants or revokes one action for one user.
func (s *Server) handleSetActionPermission(w http.ResponseWriter, r *http.Request) {
	uid, aerr := pathID(r, "uid")
	if aerr != nil {
		s.fail(w, r, aerr)
		return
	}
	serverID, actionID := r.PathValue("id"), r.PathValue("aid")
	var req struct {
		Allowed *bool `json:"allowed"`
	}
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Allowed == nil {
		s.fail(w, r, badRequest("allowed (true or false) is required."))
		return
	}
	u, err := s.Store.UserByID(r.Context(), uid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	def, err := s.Manager.Definition(serverID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	a := def.Action(actionID)
	if a == nil {
		s.fail(w, r, errf(http.StatusNotFound, "not_found", "The custom action was not found."))
		return
	}
	if err := s.Store.SetActionPermission(r.Context(), uid, serverID, actionID, *req.Allowed); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit.Record(s.actor(r), serverID, audit.ActionPermissionChange, audit.Success, map[string]any{
		"target_user": u.Username, "target_user_id": u.ID, "action_id": actionID, "action_name": a.Name, "allowed": *req.Allowed})
	s.Hub.Revalidate()
	s.notifyUser(uid, "permissions.changed")
	s.notifyAdmins("users.changed")
	writeJSON(w, http.StatusOK, map[string]any{"user_id": uid, "server_id": serverID, "action_id": actionID, "allowed": *req.Allowed})
}

// actionGrantsByUser groups a server's grants as user -> sorted action IDs.
func actionGrantsByUser(grants []db.ActionGrant) map[int64][]string {
	out := map[int64][]string{}
	for _, g := range grants {
		out[g.UserID] = append(out[g.UserID], g.ActionID)
	}
	for _, ids := range out {
		sort.Strings(ids)
	}
	return out
}

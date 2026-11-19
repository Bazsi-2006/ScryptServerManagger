package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/control"
)

// vmJSON is one dashboard card.
type vmJSON struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	URL         string   `json:"url,omitempty"`
	State       string   `json:"state"`
	Message     string   `json:"message,omitempty"`
	Pending     string   `json:"pending,omitempty"`
	Permission  string   `json:"permission"` // "start" or "start-stop"
	Actions     []string `json:"actions"`    // actions possible right now
	CheckedAt   int64    `json:"checked_at,omitempty"`
}

func toJSON(v config.VisibleVM, st control.Status) vmJSON {
	out := vmJSON{
		ID:          v.VM.ID,
		Name:        v.VM.Name,
		Description: v.VM.Description,
		URL:         v.VM.URL,
		State:       string(st.State),
		Message:     st.Message,
		Pending:     string(st.Pending),
		Permission:  v.Permission.String(),
		Actions:     []string{},
	}
	if !st.CheckedAt.IsZero() {
		out.CheckedAt = st.CheckedAt.Unix()
	}
	if st.Pending == "" {
		if st.State == control.StateStopped && v.Permission.CanStart() {
			out.Actions = append(out.Actions, "start")
		}
		if st.State == control.StateRunning && v.Permission.CanStop() {
			out.Actions = append(out.Actions, "stop")
		}
	}
	return out
}

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	visible := s.cfg.VisibleVMs(sess.username)
	ids := make([]string, len(visible))
	for i, v := range visible {
		ids[i] = v.VM.ID
	}
	statuses := s.ctl.StatusMany(r.Context(), ids)
	out := make([]vmJSON, len(visible))
	for i, v := range visible {
		out[i] = toJSON(v, statuses[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"vms":          out,
		"poll_seconds": int(s.ctl.PollInterval(ids).Seconds()),
	})
}

// visibleVM resolves the {id} path value for the current user. VMs that are
// not configured and VMs the user may not see are indistinguishable: both
// are 404, so the response does not reveal which IDs exist.
func (s *Server) visibleVM(r *http.Request) (config.VisibleVM, bool) {
	sess := sessionFrom(r.Context())
	id := r.PathValue("id")
	vm, ok := s.cfg.VM(id)
	if !ok {
		return config.VisibleVM{}, false
	}
	p := s.cfg.Permission(sess.username, vm.ID)
	if !p.CanView() {
		return config.VisibleVM{}, false
	}
	return config.VisibleVM{VM: vm, Permission: p}, true
}

func (s *Server) handleGetVM(w http.ResponseWriter, r *http.Request) {
	v, ok := s.visibleVM(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "VM not found.")
		return
	}
	st := s.ctl.Status(r.Context(), v.VM.ID)
	writeJSON(w, http.StatusOK, map[string]any{"vm": toJSON(v, st), "poll_seconds": int(s.ctl.PollInterval([]string{v.VM.ID}).Seconds())})
}

func (s *Server) powerHandler(action config.Action) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		client := clientInfoFor(r, s.cfg.TrustedProxies).IP
		v, ok := s.visibleVM(r)
		if !ok {
			s.log.Warn("power request denied: VM not visible to user", "user", sess.username, "vm", safeLogValue(r.PathValue("id")), "action", action, "client", client)
			writeError(w, http.StatusNotFound, "not_found", "VM not found.")
			return
		}
		// Authorization is decided here, before anything is sent to Vexa.
		if !v.Permission.Allows(action) {
			s.log.Warn("power request denied: insufficient permission", "user", sess.username, "vm", v.VM.ID, "action", action, "client", client)
			writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("You are not allowed to %s this VM.", action))
			return
		}
		if wait, ok := s.actions.take(sess.username, s.now()); !ok {
			setRetryAfter(w, wait)
			writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Please wait a moment.")
			return
		}
		s.log.Info("power request", "user", sess.username, "vm", v.VM.ID, "action", action, "client", client)
		if err := s.ctl.Request(r.Context(), v.VM.ID, action, sess.username); err != nil {
			s.writeRequestError(w, err)
			return
		}
		st := s.ctl.Status(r.Context(), v.VM.ID)
		writeJSON(w, http.StatusAccepted, map[string]any{"vm": toJSON(v, st), "poll_seconds": 3})
	})
}

func (s *Server) writeRequestError(w http.ResponseWriter, err error) {
	var re *control.RequestError
	if !errors.As(err, &re) {
		s.log.Error("power request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error.")
		return
	}
	setRetryAfter(w, re.RetryAfter)
	switch re.Kind {
	case control.ErrBusy:
		writeError(w, http.StatusConflict, "busy", re.Message)
	case control.ErrWrongState:
		writeError(w, http.StatusConflict, "wrong_state", re.Message)
	case control.ErrForbidden:
		writeError(w, http.StatusForbidden, "forbidden", re.Message)
	case control.ErrUnavailable:
		writeError(w, http.StatusServiceUnavailable, "vexa_unavailable", re.Message)
	case control.ErrUncertain:
		writeError(w, http.StatusGatewayTimeout, "vexa_timeout", re.Message)
	default:
		writeError(w, http.StatusBadGateway, "vexa_error", re.Message)
	}
}

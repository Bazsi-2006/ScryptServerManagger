package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/events"
)

// Categories live in the configuration file (they are settings, like the
// global limit); server definitions refer to them by ID. Definitions
// written before categories were managed refer to a category by its name,
// which still resolves - and is rewritten to the ID as soon as an
// administrator creates or edits that category, so later renames keep the
// link.

// handleCapacity reports how many servers are active against the limits.
// Regular users see the global figure and the categories of servers they
// can access.
func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	view := s.Manager.Capacity()
	id := s.ident(r)
	if !id.User.IsAdmin {
		c := s.Config.Get()
		visible := map[string]bool{}
		for _, def := range s.Manager.Definitions() {
			mask, err := s.effective(r, id, def.ID)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			if cat, ok := c.ResolveCategory(def.Category); ok && mask.Any() {
				visible[cat.ID] = true
			}
		}
		kept := view.Categories[:0]
		for _, cu := range view.Categories {
			if visible[cu.ID] {
				cu.Description = ""
				kept = append(kept, cu)
			}
		}
		view.Categories = kept
	}
	writeJSON(w, http.StatusOK, view)
}

type unmanagedCategory struct {
	Value   string   `json:"value"`
	Servers []string `json:"servers"`
}

// handleListCategories is the administrators' view: every managed category
// with usage, plus category values used by servers that are not managed yet.
func (s *Server) handleListCategories(w http.ResponseWriter, r *http.Request) {
	c := s.Config.Get()
	members := map[string][]string{}
	unmanaged := map[string][]string{}
	for _, def := range s.Manager.Definitions() {
		if cat, ok := c.ResolveCategory(def.Category); ok {
			members[cat.ID] = append(members[cat.ID], def.ID)
		} else if def.Category != "" {
			unmanaged[def.Category] = append(unmanaged[def.Category], def.ID)
		}
	}
	um := []unmanagedCategory{}
	for v, ids := range unmanaged {
		um = append(um, unmanagedCategory{Value: v, Servers: ids})
	}
	sort.Slice(um, func(i, j int) bool { return strings.ToLower(um[i].Value) < strings.ToLower(um[j].Value) })
	writeJSON(w, http.StatusOK, map[string]any{"capacity": s.Manager.Capacity(), "members": members, "unmanaged": um})
}

func (s *Server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	var req config.Category
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	req.ID, req.Name, req.Description = strings.TrimSpace(req.ID), strings.TrimSpace(req.Name), strings.TrimSpace(req.Description)
	err := s.Config.Update(func(c *config.Config) error {
		if _, exists := c.CategoryByID(req.ID); exists {
			return errf(http.StatusConflict, "already_exists", "A category with this ID already exists.")
		}
		c.Categories = append(c.Categories, req)
		return nil
	})
	if err != nil {
		s.failSettings(w, r, err)
		return
	}
	rebound := s.rebindCategory(req.ID, req.Name)
	s.Audit.Record(s.actor(r), "", audit.CategoryCreate, audit.Success, map[string]any{"category": req.ID, "name": req.Name,
		"max_concurrent": req.MaxConcurrent, "servers_rebound": rebound})
	s.categoriesChanged()
	writeJSON(w, http.StatusCreated, map[string]any{"category": req})
}

func (s *Server) handleUpdateCategory(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	var req config.Category
	if err := s.decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.ID != "" && req.ID != cid {
		s.fail(w, r, badRequest("The category ID cannot be changed."))
		return
	}
	req.ID, req.Name, req.Description = cid, strings.TrimSpace(req.Name), strings.TrimSpace(req.Description)
	oldName := ""
	err := s.Config.Update(func(c *config.Config) error {
		cur, ok := c.CategoryByID(cid)
		if !ok {
			return errNotFound
		}
		oldName = cur.Name
		*cur = req
		return nil
	})
	if err != nil {
		s.failSettings(w, r, err)
		return
	}
	// Servers that still referred to the old name follow the rename.
	rebound := s.rebindCategory(cid, oldName)
	s.Audit.Record(s.actor(r), "", audit.CategoryUpdate, audit.Success, map[string]any{"category": cid, "name": req.Name,
		"max_concurrent": req.MaxConcurrent, "servers_rebound": rebound})
	s.categoriesChanged()
	writeJSON(w, http.StatusOK, map[string]any{"category": req})
}

func (s *Server) handleDeleteCategory(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	var removed config.Category
	err := s.Config.Update(func(c *config.Config) error {
		kept := c.Categories[:0:0]
		found := false
		for _, cat := range c.Categories {
			if cat.ID == cid {
				removed, found = cat, true
				continue
			}
			kept = append(kept, cat)
		}
		if !found {
			return errNotFound
		}
		c.Categories = kept
		return nil
	})
	if err != nil {
		s.failSettings(w, r, err)
		return
	}
	// Servers keep their grouping as an unmanaged category with the same
	// name (and no limit).
	n := 0
	for _, def := range s.Manager.Definitions() {
		if def.Category != cid {
			continue
		}
		def.Category = removed.Name
		if _, err := s.Manager.Update(def); err != nil {
			s.Log.Warn("could not reassign server of a deleted category", "server", def.ID, "error", err)
			continue
		}
		n++
	}
	s.Audit.Record(s.actor(r), "", audit.CategoryDelete, audit.Success, map[string]any{"category": cid, "name": removed.Name, "servers_reassigned": n})
	s.categoriesChanged()
	w.WriteHeader(http.StatusNoContent)
}

// rebindCategory rewrites server definitions that refer to the category by
// name (oldName) to refer to it by ID. It returns how many changed.
func (s *Server) rebindCategory(id, oldName string) int {
	n := 0
	for _, def := range s.Manager.Definitions() {
		if def.Category == id || oldName == "" || !strings.EqualFold(def.Category, oldName) {
			continue
		}
		def.Category = id
		if _, err := s.Manager.Update(def); err != nil {
			s.Log.Warn("could not assign server to category", "server", def.ID, "category", id, "error", err)
			continue
		}
		n++
	}
	return n
}

func (s *Server) categoriesChanged() {
	s.Hub.Publish(events.Event{Type: "categories.changed", Audience: events.AudienceAll, Data: map[string]any{}})
	s.Manager.NotifyCapacityChanged()
}

// failSettings reports a configuration update failure.
func (s *Server) failSettings(w http.ResponseWriter, r *http.Request, err error) {
	if s.toAPIError(err) == nil {
		s.Log.Error("could not save settings", "error", err)
		err = errf(http.StatusInternalServerError, "settings_not_saved", "The settings could not be written to the configuration file. Check that the service user may write to it.")
	}
	s.fail(w, r, err)
}

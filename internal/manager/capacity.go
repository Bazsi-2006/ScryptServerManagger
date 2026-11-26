package manager

import (
	"fmt"
	"sort"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/events"
)

// Concurrency limits.
//
// A server occupies a slot while it is starting, running or stopping, and
// keeps it until it is confirmed to have stopped: occupancy follows the
// last *conclusive* observation. ERROR and UNKNOWN states keep whatever was
// last known, so a server whose state cannot be determined is never assumed
// to have freed its slot. Servers started outside the manager occupy a slot
// as soon as monitoring sees them alive.
//
// A start reserves its slot under Manager.capMu together with the check, so
// two concurrent starts can never both take the last free slot.

// LimitError reports that a start was refused because a limit is reached.
type LimitError struct {
	Scope        string `json:"scope"` // "global" or "category"
	CategoryID   string `json:"category_id,omitempty"`
	CategoryName string `json:"category_name,omitempty"`
	Limit        int    `json:"limit"`
	InUse        int    `json:"in_use"`
}

func (e *LimitError) Error() string {
	if e.Scope == "category" {
		return fmt.Sprintf("the %s category has reached its limit of %d concurrently running server%s", e.CategoryName, e.Limit, plural(e.Limit))
	}
	return fmt.Sprintf("the global limit of %d concurrently running server%s has been reached", e.Limit, plural(e.Limit))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// StartOptions modify a start or restart request.
type StartOptions struct {
	// OverrideLimits lets an administrator start a server although a
	// concurrency limit is reached. The override is audited.
	OverrideLimits bool
}

// setOccupiedLocked updates whether the server holds a concurrency slot and
// announces changes in capacity usage.
func (s *server) setOccupiedLocked(v bool) {
	if s.occupied == v {
		return
	}
	s.occupied = v
	s.m.publish(events.Event{Type: "capacity.changed", Audience: events.AudienceAll, Data: map[string]any{"server_id": s.id}})
}

// counts reports whether the server currently occupies a slot. Servers not
// probed yet (right after startup) count, because nothing is known about
// them; the caller waits for the first probes before relying on this.
func (s *server) countsLocked() bool {
	return s.occupied || !s.probed
}

// waitInitialProbes blocks until every server has been probed once (or the
// timeout passes), so capacity decisions right after a restart are based on
// reality rather than on an empty slate. Servers whose state is being driven
// by a lifecycle operation (skip included) are not waited for: monitoring
// pauses for them, and the operation itself keeps their slot accurate.
func (m *Manager) waitInitialProbes(skip *server, timeout time.Duration) {
	m.mu.RLock()
	all := make([]*server, 0, len(m.servers))
	for _, s := range m.servers {
		all = append(all, s)
	}
	m.mu.RUnlock()
	var chans []chan struct{}
	for _, s := range all {
		if s == skip {
			continue
		}
		s.mu.Lock()
		driven := s.op != nil && s.op.info.Type != OpBackup
		s.mu.Unlock()
		if !driven {
			chans = append(chans, s.firstProbe)
		}
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, c := range chans {
		select {
		case <-c:
		case <-deadline.C:
			return
		case <-m.ctx.Done():
			return
		}
	}
}

type usage struct {
	total int
	byCat map[string]int
}

// usageLocked counts occupied servers, excluding skip. Caller holds capMu.
func (m *Manager) usageLocked(c *config.Config, skip *server) usage {
	m.mu.RLock()
	all := make([]*server, 0, len(m.servers))
	for _, s := range m.servers {
		all = append(all, s)
	}
	m.mu.RUnlock()
	u := usage{byCat: map[string]int{}}
	for _, s := range all {
		if s == skip {
			continue
		}
		s.mu.Lock()
		counts := !s.deleted && s.countsLocked()
		cat := s.def.Category
		s.mu.Unlock()
		if !counts {
			continue
		}
		u.total++
		if rc, ok := c.ResolveCategory(cat); ok {
			u.byCat[rc.ID]++
		}
	}
	return u
}

// reserve checks the limits for starting s and, if they allow it, marks s
// as occupying a slot - atomically with respect to other reservations.
func (m *Manager) reserve(s *server, actor audit.Actor, opts StartOptions) error {
	m.waitInitialProbes(s, 20*time.Second)
	m.capMu.Lock()
	defer m.capMu.Unlock()
	c := m.cfg.Get()
	s.mu.Lock()
	catValue := s.def.Category
	s.mu.Unlock()
	cat, hasCat := c.ResolveCategory(catValue)
	global := c.Limits.MaxConcurrentServers
	var lerr *LimitError
	if global > 0 || (hasCat && cat.MaxConcurrent > 0) {
		u := m.usageLocked(c, s)
		switch {
		case global > 0 && u.total >= global:
			lerr = &LimitError{Scope: "global", Limit: global, InUse: u.total}
		case hasCat && cat.MaxConcurrent > 0 && u.byCat[cat.ID] >= cat.MaxConcurrent:
			lerr = &LimitError{Scope: "category", CategoryID: cat.ID, CategoryName: cat.Name, Limit: cat.MaxConcurrent, InUse: u.byCat[cat.ID]}
		}
	}
	if lerr != nil {
		if !opts.OverrideLimits {
			return lerr
		}
		m.audit.Record(actor, s.id, audit.LimitOverride, audit.Success, map[string]any{
			"scope": lerr.Scope, "category": lerr.CategoryName, "limit": lerr.Limit, "in_use": lerr.InUse})
		m.log.Warn("concurrency limit overridden by an administrator", "server", s.id, "user", actor.Username, "scope", lerr.Scope)
	}
	s.mu.Lock()
	s.setOccupiedLocked(true)
	s.mu.Unlock()
	return nil
}

// Usage is the current use of one limit.
type Usage struct {
	InUse int `json:"in_use"`
	Limit int `json:"limit"` // 0 = unlimited
}

// CategoryUsage is the use of one category's limit.
type CategoryUsage struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Servers     int    `json:"servers"` // servers assigned to the category
	Usage
}

// CapacityView summarises how many servers are active against the limits.
type CapacityView struct {
	Global     Usage           `json:"global"`
	Categories []CategoryUsage `json:"categories"`
}

// Capacity returns current usage of the global and per-category limits.
func (m *Manager) Capacity() CapacityView {
	m.capMu.Lock()
	c := m.cfg.Get()
	u := m.usageLocked(c, nil)
	m.capMu.Unlock()
	assigned := map[string]int{}
	for _, d := range m.defs.List() {
		if rc, ok := c.ResolveCategory(d.Category); ok {
			assigned[rc.ID]++
		}
	}
	out := CapacityView{Global: Usage{InUse: u.total, Limit: c.Limits.MaxConcurrentServers}, Categories: []CategoryUsage{}}
	for _, cat := range c.Categories {
		out.Categories = append(out.Categories, CategoryUsage{ID: cat.ID, Name: cat.Name, Description: cat.Description,
			Servers: assigned[cat.ID], Usage: Usage{InUse: u.byCat[cat.ID], Limit: cat.MaxConcurrent}})
	}
	sort.Slice(out.Categories, func(i, j int) bool { return out.Categories[i].Name < out.Categories[j].Name })
	return out
}

// NotifyCapacityChanged tells clients to refresh usage (limits or category
// assignments changed).
func (m *Manager) NotifyCapacityChanged() {
	m.publish(events.Event{Type: "capacity.changed", Audience: events.AudienceAll, Data: map[string]any{}})
}

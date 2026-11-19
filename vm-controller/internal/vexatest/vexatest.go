// Package vexatest provides an in-memory imitation of the parts of the
// Vexa-VM API that vm-controller uses, for tests and local development.
//
// It mirrors the behaviour observed in Vexa-VM 0.1.9:
//   - GET /api/v1/vms/{id} returns the stored state;
//   - a power action returns 202 with a queued job;
//   - "start" updates the stored state to running when the job finishes;
//   - "shutdown" only sends ACPI: the stored state stays "running" until the
//     inventory sweep notices the guest has powered off (ShutdownDelay
//     simulates that; zero means "never, until SetState is called").
//
// It also implements force-off, reboot and the other actions so tests can
// prove that vm-controller never calls them.
package vexatest

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Request is one request the mock received.
type Request struct {
	Method string
	Path   string
	Authed bool
}

// Failure makes matching requests fail with the given status.
type Failure struct {
	Status     int
	Code       string
	Message    string
	RetryAfter string
}

// Server is the mock. Create it with New or NewHandler.
type Server struct {
	APIKey string
	// StartDelay is how long a start job takes before the VM is running.
	StartDelay time.Duration
	// ShutdownDelay is how long after a shutdown request the stored state
	// becomes "stopped". Zero leaves it running until SetState.
	ShutdownDelay time.Duration
	// JobsForbidden makes GET /api/v1/jobs/{id} return 403 (key without
	// jobs:read).
	JobsForbidden bool
	// FailJobs makes every power job end in "failed".
	FailJobs bool

	mu       sync.Mutex
	vms      map[string]*vm
	order    []string
	jobs     map[string]*job
	requests []Request
	failures map[string]Failure // key: "METHOD path-prefix"
	timers   []*time.Timer

	httpServer *httptest.Server
}

type vm struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type job struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"`
	VMID     string  `json:"vm_id"`
	Status   string  `json:"status"`
	Error    *string `json:"error"`
	Progress float64 `json:"progress_percent"`
	Created  int64   `json:"created_at"`
	Updated  int64   `json:"updated_at"`
}

// NewHandler returns a mock that is not listening anywhere; use it as an
// http.Handler.
func NewHandler(apiKey string) *Server {
	return &Server{
		APIKey:   apiKey,
		vms:      map[string]*vm{},
		jobs:     map[string]*job{},
		failures: map[string]Failure{},
	}
}

// New starts the mock on a random loopback port. Close it when done.
func New(apiKey string) *Server {
	s := NewHandler(apiKey)
	s.httpServer = httptest.NewServer(s)
	return s
}

// URL is the base URL of a server created with New.
func (s *Server) URL() string { return s.httpServer.URL }

// Close stops the HTTP server and pending timers.
func (s *Server) Close() {
	s.mu.Lock()
	for _, t := range s.timers {
		t.Stop()
	}
	s.mu.Unlock()
	if s.httpServer != nil {
		s.httpServer.Close()
	}
}

// AddVM registers a VM with an initial state.
func (s *Server) AddVM(id, name, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.vms[id]; !ok {
		s.order = append(s.order, id)
	}
	s.vms[id] = &vm{ID: id, Name: name, State: state}
}

// SetState changes a VM's stored state, e.g. to simulate the inventory sweep.
func (s *Server) SetState(id, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vms[id]; ok {
		v.State = state
	}
}

// State returns a VM's stored state.
func (s *Server) State(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vms[id]; ok {
		return v.State
	}
	return ""
}

// Fail makes requests whose "METHOD /path" starts with prefix fail. Use an
// empty Failure{} via ClearFailures to undo.
func (s *Server) Fail(prefix string, f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[prefix] = f
}

// ClearFailures removes all injected failures.
func (s *Server) ClearFailures() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = map[string]Failure{}
}

// Requests returns a copy of every request received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// PowerCalls returns "vm-id action" for every power request received.
func (s *Server) PowerCalls() []string {
	var out []string
	for _, r := range s.Requests() {
		if r.Method != http.MethodPost {
			continue
		}
		rest, ok := strings.CutPrefix(r.Path, "/api/v1/vms/")
		if !ok {
			continue
		}
		if id, action, ok := strings.Cut(rest, "/actions/"); ok {
			out = append(out, id+" "+action)
		}
	}
	return out
}

// CountRequests returns how many requests matched method and path prefix.
func (s *Server) CountRequests(method, prefix string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Method == method && strings.HasPrefix(r.Path, prefix) {
			n++
		}
	}
	return n
}

// ResetRequests forgets recorded requests.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	authed := r.Header.Get("Authorization") == "Bearer "+s.APIKey
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Authed: authed})
	key := r.Method + " " + r.URL.Path
	var fail *Failure
	for prefix, f := range s.failures {
		if strings.HasPrefix(key, prefix) {
			f := f
			fail = &f
		}
	}
	s.mu.Unlock()

	if !authed {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid credential")
		return
	}
	if fail != nil {
		if fail.RetryAfter != "" {
			w.Header().Set("Retry-After", fail.RetryAfter)
		}
		writeError(w, fail.Status, fail.Code, fail.Message)
		return
	}

	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/jobs/"):
		s.getJob(w, strings.TrimPrefix(path, "/api/v1/jobs/"))
	case strings.HasPrefix(path, "/api/v1/vms/"):
		rest := strings.TrimPrefix(path, "/api/v1/vms/")
		if id, action, ok := strings.Cut(rest, "/actions/"); ok && r.Method == http.MethodPost {
			s.power(w, id, action)
			return
		}
		if r.Method == http.MethodGet && !strings.Contains(rest, "/") {
			s.getVM(w, rest)
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	default:
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	}
}

func (s *Server) lookup(id string) *vm {
	if v, ok := s.vms[id]; ok {
		return v
	}
	for _, v := range s.vms { // Vexa also accepts a case-insensitive name
		if strings.EqualFold(v.Name, id) {
			return v
		}
	}
	return nil
}

func (s *Server) getVM(w http.ResponseWriter, id string) {
	s.mu.Lock()
	v := s.lookup(id)
	var out map[string]any
	if v != nil {
		out = map[string]any{"vm": map[string]any{
			"id": v.ID, "name": v.Name, "hostname": v.Name, "state": v.State, "desired_state": v.State,
			"vcpus": 2, "memory_mib": 2048, "disk_gib": 20, "metadata": map[string]any{},
		}}
	}
	s.mu.Unlock()
	if v == nil {
		writeError(w, http.StatusNotFound, "not_found", "VM not found")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var validActions = map[string]string{
	"start": "start", "on": "start",
	"shutdown": "shutdown", "stop": "shutdown", "off": "shutdown",
	"force-off": "force-off", "hard-stop": "force-off",
	"reboot": "reboot", "reset": "reset", "hard-reboot": "reset", "force-reboot": "reset",
	"suspend": "suspend", "pause": "suspend", "resume": "resume",
}

func (s *Server) power(w http.ResponseWriter, id, rawAction string) {
	action, ok := validActions[rawAction]
	if !ok {
		writeError(w, http.StatusBadRequest, "validation_error", "unknown power action")
		return
	}
	s.mu.Lock()
	v := s.lookup(id)
	if v == nil {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "not_found", "VM not found")
		return
	}
	now := time.Now().Unix()
	j := &job{ID: newUUID(), Kind: "vm.power", VMID: v.ID, Status: "queued", Created: now, Updated: now}
	s.jobs[j.ID] = j
	vmID, jobID := v.ID, j.ID
	delayed := action == "start" && s.StartDelay > 0
	if delayed {
		j.Status = "running"
		s.timers = append(s.timers, time.AfterFunc(s.StartDelay, func() { s.finishJob(vmID, jobID, action) }))
	}
	s.mu.Unlock()
	if !delayed {
		s.finishJob(vmID, jobID, action)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": map[string]any{
		"id": jobID, "kind": "vm.power", "vm_id": vmID, "status": "queued", "error": nil,
		"progress_percent": 0, "created_at": now, "updated_at": now,
	}})
}

// finishJob completes a power job the way Vexa's job runner does.
func (s *Server) finishJob(vmID, jobID, action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, j := s.vms[vmID], s.jobs[jobID]
	if v == nil || j == nil {
		return
	}
	j.Updated = time.Now().Unix()
	j.Progress = 100
	if s.FailJobs {
		msg := "hypervisor error: simulated failure"
		j.Status, j.Error = "failed", &msg
		return
	}
	j.Status = "succeeded"
	switch action {
	case "start":
		if v.State == "stopped" || v.State == "paused" {
			v.State = "running"
		}
	case "force-off":
		v.State = "stopped"
	case "suspend":
		v.State = "paused"
	case "resume":
		v.State = "running"
	case "shutdown":
		// ACPI only: the stored state stays "running" until the simulated
		// inventory sweep (ShutdownDelay) or SetState.
		if s.ShutdownDelay > 0 && v.State == "running" {
			s.timers = append(s.timers, time.AfterFunc(s.ShutdownDelay, func() { s.SetState(vmID, "stopped") }))
		}
	}
}

func (s *Server) getJob(w http.ResponseWriter, id string) {
	if s.JobsForbidden {
		writeError(w, http.StatusForbidden, "forbidden", "missing scope jobs:read")
		return
	}
	s.mu.Lock()
	j, ok := s.jobs[id]
	var copyJob job
	if ok {
		copyJob = *j
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"operation": copyJob})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	if code == "" {
		code = "internal_error"
	}
	writeJSON(w, status, map[string]any{"success": false, "error": map[string]any{
		"code": code, "message": msg, "request_id": newUUID(),
	}})
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

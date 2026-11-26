// Package api implements the HTTP API, the SSE endpoints and static asset
// serving. Every route enforces authentication and authorization here, on
// the server; the frontend's permission checks are presentation only.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/backend"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/backup"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/events"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/mail"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/manager"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/monitor"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/perm"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/ratelimit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// OpenAPISpec is the embedded API description served at /api/v1/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPISpec []byte

// Deps are the services the API is built on.
type Deps struct {
	Config   *config.Holder
	Store    *db.Store
	Manager  *manager.Manager
	Hub      *events.Hub
	Signer   *auth.Signer
	Mailer   *mail.Mailer
	Audit    *audit.Recorder
	Monitor  *monitor.Monitor
	Log      *slog.Logger
	Static   fs.FS // embedded frontend (may be nil in tests)
	Version  string
	SetLevel func(string) // applies logging.level changes at runtime
}

type Server struct {
	Deps
	limiter   *ratelimit.Limiter
	tickets   *ticketStore
	downloads atomic.Int64
	static    *staticFiles
	// dummyHash equalises login timing for unknown usernames.
	dummyHash string
}

func New(d Deps) (*Server, error) {
	s := &Server{Deps: d, limiter: ratelimit.New(), tickets: newTicketStore()}
	p := d.Config.Get().Auth.Argon2
	h, err := auth.HashPassword(auth.MustToken(16), auth.Params{MemoryKiB: p.MemoryKiB, Iterations: p.Iterations, Parallelism: p.Parallelism})
	if err != nil {
		return nil, err
	}
	s.dummyHash = h
	if d.Static != nil {
		st, err := loadStatic(d.Static)
		if err != nil {
			return nil, err
		}
		s.static = st
	}
	return s, nil
}

// Limiter exposes the rate limiter (tests adjust its clock).
func (s *Server) Limiter() *ratelimit.Limiter { return s.limiter }

// apiError is the uniform error payload.
type apiError struct {
	Status     int           `json:"-"`
	Code       string        `json:"code"`
	Message    string        `json:"message"`
	Details    any           `json:"details,omitempty"`
	RetryAfter time.Duration `json:"-"`
}

func (e *apiError) Error() string { return e.Message }

func errf(status int, code, format string, a ...any) *apiError {
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

var (
	errUnauthenticated = &apiError{Status: http.StatusUnauthorized, Code: "unauthenticated", Message: "Authentication is required."}
	errForbidden       = &apiError{Status: http.StatusForbidden, Code: "permission_denied", Message: "You do not have permission to do this."}
	errNotFound        = &apiError{Status: http.StatusNotFound, Code: "not_found", Message: "The requested resource was not found."}
	errAdminOnly       = &apiError{Status: http.StatusForbidden, Code: "permission_denied", Message: "Administrator privileges are required."}
)

func badRequest(format string, a ...any) *apiError {
	return errf(http.StatusBadRequest, "invalid_request", format, a...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// toAPIError classifies an error so clients can tell apart missing
// permissions, invalid configuration, operation failures, unavailable
// backends, timeouts and conflicts.
func (s *Server) toAPIError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	var ve *servers.ValidationError
	if errors.As(err, &ve) {
		return &apiError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: "The server definition is invalid.", Details: ve.Problems}
	}
	var cve *config.ValidationError
	if errors.As(err, &cve) {
		return &apiError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: "The settings are invalid.", Details: cve.Problems}
	}
	var be *manager.BusyError
	if errors.As(err, &be) {
		return &apiError{Status: http.StatusConflict, Code: "operation_in_progress", Message: capitalize(be.Error()) + ".", Details: map[string]any{"operation": be.Op}}
	}
	var ce *manager.CooldownError
	if errors.As(err, &ce) {
		return &apiError{Status: http.StatusTooManyRequests, Code: "cooldown", Message: capitalize(ce.Error()) + ".", RetryAfter: ce.Remaining}
	}
	var ie *manager.InputError
	if errors.As(err, &ie) {
		return badRequest("%s", capitalize(ie.Msg)+".")
	}
	var le *manager.LimitError
	if errors.As(err, &le) {
		return &apiError{Status: http.StatusConflict, Code: "concurrency_limit", Message: "Cannot start the server: " + le.Error() + ".", Details: le}
	}
	var abe *manager.ActionBusyError
	if errors.As(err, &abe) {
		return &apiError{Status: http.StatusConflict, Code: "action_running", Message: capitalize(abe.Error()) + ".", Details: map[string]any{"run": abe.Run}}
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return errf(http.StatusRequestEntityTooLarge, "request_too_large", "The request body is too large.")
	}
	switch {
	case errors.Is(err, manager.ErrNotFound), errors.Is(err, db.ErrNotFound), errors.Is(err, backup.ErrNotFound):
		return errNotFound
	case errors.Is(err, manager.ErrAlreadyRunning):
		return errf(http.StatusConflict, "already_running", "The server is already running.")
	case errors.Is(err, manager.ErrNotRunning), errors.Is(err, backend.ErrNotRunning):
		msg := "The server is not running."
		// Keep a more specific explanation (for example which tmux session
		// is missing) when the error carries one.
		if e := err.Error(); e != manager.ErrNotRunning.Error() && e != backend.ErrNotRunning.Error() {
			msg = capitalize(strings.TrimPrefix(e, backend.ErrNotRunning.Error()+": ")) + "."
		}
		return errf(http.StatusConflict, "not_running", "%s", msg)
	case errors.Is(err, manager.ErrActionNotFound):
		return errf(http.StatusNotFound, "not_found", "The custom action was not found.")
	case errors.Is(err, manager.ErrActionExists):
		return errf(http.StatusConflict, "already_exists", "A custom action with this ID already exists for this server.")
	case errors.Is(err, manager.ErrActionDisabled):
		return errf(http.StatusConflict, "action_disabled", "This action is disabled.")
	case errors.Is(err, manager.ErrConfirmationRequired):
		return errf(http.StatusConflict, "confirmation_required", "This action must be confirmed before it runs (send {\"confirm\": true}).")
	case errors.Is(err, manager.ErrActive):
		return errf(http.StatusConflict, "server_active", "The server is running or busy. Stop it before changing execution settings or deleting it.")
	case errors.Is(err, servers.ErrExists):
		return errf(http.StatusConflict, "already_exists", "A server with this ID already exists.")
	case errors.Is(err, manager.ErrUnsupported):
		return errf(http.StatusConflict, "unsupported_action", "%s.", capitalize(err.Error()))
	case errors.Is(err, backend.ErrUnavailable), errors.Is(err, backup.ErrUnavailable):
		return errf(http.StatusServiceUnavailable, "backend_unavailable", "%s.", capitalize(err.Error()))
	case errors.Is(err, backend.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return errf(http.StatusGatewayTimeout, "command_timeout", "The command timed out.")
	case errors.Is(err, backup.ErrInvalidName):
		return badRequest("Invalid backup file name.")
	case errors.Is(err, db.ErrLastAdmin):
		return errf(http.StatusConflict, "last_admin", "This would leave the system without an active administrator.")
	}
	return nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// fail writes an error response. Unclassified errors are logged in full but
// reported generically so internals do not leak.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	ae := s.toAPIError(err)
	if ae == nil {
		s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		ae = &apiError{Status: http.StatusInternalServerError, Code: "internal_error", Message: "An internal error occurred."}
	}
	if ae.RetryAfter > 0 {
		secs := int(ae.RetryAfter.Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	writeJSON(w, ae.Status, map[string]any{"error": ae})
}

// failOp reports a failure of an operation against a server (as opposed to
// a malformed request): HTTP 502 with code operation_failed unless the
// error is one of the classified kinds.
func (s *Server) failOp(w http.ResponseWriter, r *http.Request, err error) {
	if ae := s.toAPIError(err); ae != nil {
		s.fail(w, r, ae)
		return
	}
	s.fail(w, r, errf(http.StatusBadGateway, "operation_failed", "%s.", capitalize(err.Error())))
}

// decode reads a JSON body strictly: unknown fields and trailing data are
// rejected and the size is bounded.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, s.Config.Get().Security.MaxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return badRequest("The request body is not valid JSON for this endpoint: %s.", sanitizeJSONError(err))
	}
	if _, err := dec.Token(); err != io.EOF {
		return badRequest("The request body must contain a single JSON value.")
	}
	return nil
}

func sanitizeJSONError(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "json: ")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// rateLimit applies one configured limit and writes a 429 when exceeded.
func (s *Server) rateLimit(w http.ResponseWriter, r *http.Request, key string, rate config.Rate) bool {
	ok, wait := s.limiter.Allow(key, rate.Requests, rate.Window.D())
	if ok {
		return true
	}
	s.fail(w, r, &apiError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "Too many requests. Please slow down and try again shortly.", RetryAfter: wait})
	return false
}

func maskOf(m int) perm.Mask { return perm.Mask(m) }

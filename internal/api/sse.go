package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/console"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/events"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/perm"
)

// sseWriter writes Server-Sent Events.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startSSE(w http.ResponseWriter) *sseWriter {
	noDeadline(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store, no-transform")
	h.Set("X-Accel-Buffering", "no") // tell nginx not to buffer the stream
	w.WriteHeader(http.StatusOK)
	s := &sseWriter{w: w, rc: http.NewResponseController(w)}
	// Ask clients to wait a little before reconnecting.
	fmt.Fprint(w, "retry: 3000\n\n")
	s.flush()
	return s
}

func (s *sseWriter) flush() { _ = s.rc.Flush() }

// event writes one event. A write deadline keeps a stalled client from
// pinning the handler forever.
func (s *sseWriter) event(id, typ string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_ = s.rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	var b strings.Builder
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	b.WriteString("event: " + typ + "\ndata: ")
	b.Write(payload) // JSON never contains raw newlines
	b.WriteString("\n\n")
	if _, err := s.w.Write([]byte(b.String())); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *sseWriter) ping() error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := s.w.Write([]byte(": ping\n\n")); err != nil {
		return err
	}
	return s.rc.Flush()
}

// resolveIdentity reloads authorization data for an event subscriber.
// It is the events.Resolver used by the hub.
func (s *Server) ResolveIdentity(userID int64, sessionID string) (events.Identity, bool) {
	ctx, cancel := contextTimeout(5 * time.Second)
	defer cancel()
	u, err := s.Store.UserByID(ctx, userID)
	if err != nil || u.Disabled {
		return events.Identity{}, false
	}
	if active, err := s.Store.SessionActive(ctx, sessionID, time.Now()); err != nil || !active {
		return events.Identity{}, false
	}
	raw, err := s.Store.PermissionsForUser(ctx, userID)
	if err != nil {
		return events.Identity{}, false
	}
	perms := make(map[string]perm.Mask, len(raw))
	for k, v := range raw {
		perms[k] = perm.Mask(v)
	}
	return events.Identity{UserID: userID, SessionID: sessionID, IsAdmin: u.IsAdmin, Perms: perms}, true
}

// handleEvents is the application-wide event stream. Authentication uses
// the Authorization header (the frontend reads the stream with fetch), so
// no token ever appears in a URL.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := s.ident(r)
	ident, ok := s.ResolveIdentity(id.User.ID, id.SessionID)
	if !ok {
		s.fail(w, r, errUnauthenticated)
		return
	}
	sub, backlog, resync := s.Hub.Subscribe(ident, r.Header.Get("Last-Event-ID"))
	defer s.Hub.Unsubscribe(sub)

	out := startSSE(w)
	// "hello" tells the client whether it must reload its state: always on a
	// first connection, and after a reconnect whose gap could not be
	// replayed.
	if err := out.event(s.Hub.CurrentID(), "hello", map[string]any{"resync": resync, "time": time.Now().UTC()}); err != nil {
		return
	}
	for _, env := range backlog {
		if err := out.event(env.ID, env.Type, env.Data); err != nil {
			return
		}
	}
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.Done():
			// Closed by the hub: lagging, or no longer authorized.
			_ = out.event("", "bye", map[string]any{"reason": sub.Reason()})
			return
		case env := <-sub.C:
			if err := out.event(env.ID, env.Type, env.Data); err != nil {
				return
			}
		case <-heartbeat.C:
			if err := out.ping(); err != nil {
				return
			}
		}
	}
}

// consoleAllowed re-checks that the caller may still read this console.
func (s *Server) consoleAllowed(userID int64, sessionID, serverID string) bool {
	ident, ok := s.ResolveIdentity(userID, sessionID)
	if !ok {
		return false
	}
	if _, err := s.Manager.Definition(serverID); err != nil {
		return false
	}
	return ident.IsAdmin || ident.Perms[serverID].Has(perm.ConsoleRead)
}

// handleConsoleStream streams one server's console. Event IDs are
// "<epoch>:<seq>"; on reconnect the stream resumes after the last line the
// client saw, or sends a "reset" with a fresh snapshot when that is no
// longer possible, so output is neither lost silently nor duplicated.
func (s *Server) handleConsoleStream(w http.ResponseWriter, r *http.Request) {
	def, _, ok := s.access(w, r, perm.ConsoleRead, "")
	if !ok {
		return
	}
	id := s.ident(r)
	epoch, seq := "", uint64(0)
	if e, n, found := strings.Cut(r.Header.Get("Last-Event-ID"), ":"); found {
		if v, err := strconv.ParseUint(n, 10, 64); err == nil {
			epoch, seq = e, v
		}
	}
	stream, err := s.Manager.ConsoleSubscribe(def.ID, epoch, seq)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer stream.Close()

	out := startSSE(w)
	cur := stream.Epoch
	sendLines := func(lines []console.Line) error {
		if len(lines) == 0 {
			return nil
		}
		return out.event(cur+":"+strconv.FormatUint(lines[len(lines)-1].Seq, 10), "lines", map[string]any{"lines": lines})
	}
	if stream.Reset {
		if err := out.event(cur+":0", "reset", map[string]any{"epoch": cur}); err != nil {
			return
		}
	}
	if err := sendLines(stream.Backlog); err != nil {
		return
	}
	if err := out.event("", "ready", map[string]any{}); err != nil {
		return
	}

	authCh := s.Hub.AuthChanged()
	recheck := time.NewTicker(60 * time.Second)
	defer recheck.Stop()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	batch := make([]console.Line, 0, 256)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-authCh:
			authCh = s.Hub.AuthChanged()
			if !s.consoleAllowed(id.User.ID, id.SessionID, def.ID) {
				_ = out.event("", "bye", map[string]any{"reason": "unauthorized"})
				return
			}
		case <-recheck.C:
			if !s.consoleAllowed(id.User.ID, id.SessionID, def.ID) {
				_ = out.event("", "bye", map[string]any{"reason": "unauthorized"})
				return
			}
		case <-heartbeat.C:
			if err := out.ping(); err != nil {
				return
			}
		case ev, open := <-stream.Sub.C:
			if !open {
				// Dropped for falling behind (or the server definition
				// changed): the client reconnects and resynchronises.
				_ = out.event("", "bye", map[string]any{"reason": "resync"})
				return
			}
			batch = batch[:0]
			reset := false
			handle := func(ev console.Event) {
				if ev.Reset {
					reset, cur = true, ev.Epoch
					batch = batch[:0]
					return
				}
				batch = append(batch, ev.Line)
			}
			handle(ev)
			// Coalesce whatever else is already queued into one event.
		drain:
			for len(batch) < 256 {
				select {
				case more, open := <-stream.Sub.C:
					if !open {
						break drain
					}
					handle(more)
				default:
					break drain
				}
			}
			if reset {
				if err := out.event(cur+":0", "reset", map[string]any{"epoch": cur}); err != nil {
					return
				}
			}
			if err := sendLines(batch); err != nil {
				return
			}
		}
	}
}

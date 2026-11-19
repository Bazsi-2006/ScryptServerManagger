package server

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// apiError is the JSON error body: {"error": {"code": ..., "message": ...}}.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: message}})
}

func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	if d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((d+time.Second-1)/time.Second)))
	}
}

// contentSecurityPolicy allows only same-origin scripts, styles, images and
// fetches; no inline code, no framing, no plugins.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()")
		next.ServeHTTP(w, r)
	})
}

func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Error("panic while handling request", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// clientInfo is what we know about the origin of a request.
type clientInfo struct {
	IP    netip.Addr
	HTTPS bool
}

// clientInfoFor determines the client address and scheme. Forwarded headers
// are only believed when the TCP peer is a configured trusted proxy; the
// X-Forwarded-For chain is walked from the right, skipping trusted proxies,
// so a client cannot inject an address in front of the one Caddy appends.
func clientInfoFor(r *http.Request, trusted []netip.Prefix) clientInfo {
	info := clientInfo{HTTPS: r.TLS != nil}
	peer := remoteAddr(r)
	info.IP = peer
	if !peer.IsValid() || !isTrusted(peer, trusted) {
		return info
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		var hops []string
		for _, v := range xff {
			hops = append(hops, strings.Split(v, ",")...)
		}
		for i := len(hops) - 1; i >= 0; i-- {
			ip, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if err != nil {
				break // malformed: stop and keep the last good address
			}
			ip = ip.Unmap()
			info.IP = ip
			if !isTrusted(ip, trusted) {
				break
			}
		}
	}
	if protos := r.Header.Values("X-Forwarded-Proto"); len(protos) > 0 {
		last := protos[len(protos)-1]
		if i := strings.LastIndexByte(last, ','); i >= 0 {
			last = last[i+1:]
		}
		info.HTTPS = info.HTTPS || strings.EqualFold(strings.TrimSpace(last), "https")
	}
	return info
}

func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}

func isTrusted(ip netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// limiterKey groups IPv6 clients by /64, since a single host usually
// controls a whole /64.
func limiterKey(ip netip.Addr) string {
	if !ip.IsValid() {
		return "invalid"
	}
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}

// safeLogValue makes untrusted strings (such as attempted usernames) short
// and printable before they reach the log.
func safeLogValue(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return '?'
		}
		return r
	}, s)
	if len(s) > 64 {
		cut := 0
		for i := range s {
			if i > 64 {
				break
			}
			cut = i
		}
		s = s[:cut] + "…"
	}
	return s
}

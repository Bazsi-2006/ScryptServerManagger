// Package config loads and validates the JSON configuration and resolves it
// into the authorization policy used by every request.
//
// The JSON file is the source of truth for which VMs exist, which users exist
// and what each user may do. Validation is strict: unknown fields, duplicate
// keys, unknown permissions, references to undefined VMs and contradictory
// permissions all stop the service from starting.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// DefaultPath is used when neither --config nor VM_CONTROLLER_CONFIG is set.
const DefaultPath = "/etc/vm-controller/config.json"

// Mode selects the operating mode.
type Mode string

const (
	ModeSingleUser Mode = "single-user"
	ModeMultiuser  Mode = "multiuser"
)

// CookieSecure controls the Secure attribute of the session cookie.
type CookieSecure string

const (
	// CookieSecureAuto sets Secure when the request arrived over HTTPS,
	// directly or through a trusted proxy that sent X-Forwarded-Proto: https.
	CookieSecureAuto   CookieSecure = "auto"
	CookieSecureAlways CookieSecure = "always"
	CookieSecureNever  CookieSecure = "never"
)

// Permission is what a user may do with one VM.
type Permission int

const (
	PermNone Permission = iota
	PermStart
	PermStartStop
)

func (p Permission) String() string {
	switch p {
	case PermStart:
		return "start"
	case PermStartStop:
		return "start-stop"
	default:
		return "none"
	}
}

// CanView reports whether the VM is visible at all.
func (p Permission) CanView() bool { return p >= PermStart }

// CanStart reports whether the user may start the VM.
func (p Permission) CanStart() bool { return p >= PermStart }

// CanStop reports whether the user may request a graceful shutdown.
func (p Permission) CanStop() bool { return p == PermStartStop }

// Action is one of the two power operations this application supports.
type Action string

const (
	ActionStart Action = "start"
	ActionStop  Action = "stop"
)

// Allows reports whether permission p covers action a.
func (p Permission) Allows(a Action) bool {
	switch a {
	case ActionStart:
		return p.CanStart()
	case ActionStop:
		return p.CanStop()
	default:
		return false
	}
}

func parsePermission(s string) (Permission, bool) {
	switch s {
	case "none":
		return PermNone, true
	case "start":
		return PermStart, true
	case "start-stop":
		return PermStartStop, true
	}
	return PermNone, false
}

// VM is one controllable virtual machine.
type VM struct {
	// ID is the Vexa-VM identifier (normally the VM UUID).
	ID          string
	Name        string
	Description string
	// AllowStop is the global switch for graceful shutdown of this VM. When
	// false nobody can stop it, whatever their assignment says.
	AllowStop bool
	// URL is an optional link shown on the card while the VM is running,
	// e.g. the web interface of the service inside the VM.
	URL string
}

// Vexa holds the resolved Vexa-VM API settings.
type Vexa struct {
	BaseURL        *url.URL
	APIKeyEnv      string
	APIKeyFile     string
	CAFile         string
	Timeout        time.Duration
	StatusCacheTTL time.Duration
	StartTimeout   time.Duration
	StopTimeout    time.Duration
	TrackJobs      bool
}

// Config is the validated, resolved configuration.
type Config struct {
	Path            string
	Mode            Mode
	Title           string
	Listen          string
	BasePath        string // "" for the site root, otherwise e.g. "/vmctl"
	Database        string
	CookieSecure    CookieSecure
	TrustedProxies  []netip.Prefix
	SessionLifetime time.Duration
	IdleTimeout     time.Duration
	Vexa            Vexa
	VMs             []VM

	// Warnings are non-fatal findings worth logging at startup.
	Warnings []string

	vmByID map[string]int
	users  map[string]*user
}

type user struct {
	name     string
	disabled bool
	perms    map[string]Permission // VM ID -> permission, already capped by AllowStop
}

// ---- raw JSON schema -------------------------------------------------------

type fileConfig struct {
	Mode           string      `json:"mode"`
	Title          string      `json:"title"`
	Listen         string      `json:"listen"`
	BasePath       string      `json:"base_path"`
	Database       string      `json:"database"`
	CookieSecure   string      `json:"cookie_secure"`
	TrustedProxies []string    `json:"trusted_proxies"`
	Session        fileSession `json:"session"`
	Vexa           fileVexa    `json:"vexa"`
	VMs            []fileVM    `json:"vms"`
	SingleUser     *fileSingle `json:"single_user"`
	Users          []fileUser  `json:"users"`
}

type fileSession struct {
	LifetimeHours      *int `json:"lifetime_hours"`
	IdleTimeoutMinutes *int `json:"idle_timeout_minutes"`
}

type fileVexa struct {
	BaseURL             string `json:"base_url"`
	APIKeyEnv           string `json:"api_key_env"`
	APIKeyFile          string `json:"api_key_file"`
	CAFile              string `json:"ca_file"`
	AllowInsecureHTTP   bool   `json:"allow_insecure_http"`
	TimeoutSeconds      *int   `json:"timeout_seconds"`
	StatusCacheSeconds  *int   `json:"status_cache_seconds"`
	StartTimeoutSeconds *int   `json:"start_timeout_seconds"`
	StopTimeoutSeconds  *int   `json:"stop_timeout_seconds"`
	TrackJobs           bool   `json:"track_jobs"`
}

type fileVM struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	AllowStop   bool   `json:"allow_stop"`
	URL         string `json:"url"`
}

type fileSingle struct {
	Username string `json:"username"`
}

type fileUser struct {
	Username    string            `json:"username"`
	Disabled    bool              `json:"disabled"`
	Permissions map[string]string `json:"permissions"`
}

// ---- loading ---------------------------------------------------------------

// maxConfigSize bounds how much of the file is read.
const maxConfigSize = 4 << 20

// Load reads, parses and validates the configuration file at path.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", abs, err)
	}
	if len(data) > maxConfigSize {
		return nil, fmt.Errorf("config %s: file is larger than %d bytes", abs, maxConfigSize)
	}
	cfg, err := Parse(data, filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", abs, err)
	}
	cfg.Path = abs
	return cfg, nil
}

// Parse validates configuration JSON. Relative file paths in the
// configuration are resolved against baseDir.
func Parse(data []byte, baseDir string) (*Config, error) {
	if err := checkDuplicateKeys(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var fc fileConfig
	if err := dec.Decode(&fc); err != nil {
		return nil, describeJSONError(data, err)
	}
	if dec.More() {
		return nil, errors.New("unexpected data after the top-level JSON object")
	}
	v := &validator{}
	cfg := v.resolve(&fc, baseDir)
	if err := v.err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func describeJSONError(data []byte, err error) error {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		line, col := position(data, syn.Offset)
		return fmt.Errorf("invalid JSON at line %d, column %d: %v", line, col, err)
	}
	var typ *json.UnmarshalTypeError
	if errors.As(err, &typ) {
		line, col := position(data, typ.Offset)
		return fmt.Errorf("line %d, column %d: field %q must be %s, not %s", line, col, typ.Field, typ.Type, typ.Value)
	}
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		return fmt.Errorf("%s (check the spelling; unknown fields are rejected)", strings.TrimPrefix(err.Error(), "json: "))
	}
	return fmt.Errorf("invalid JSON: %w", err)
}

func position(data []byte, offset int64) (line, col int) {
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	line = 1 + bytes.Count(data[:offset], []byte("\n"))
	col = int(offset) - bytes.LastIndexByte(data[:offset], '\n')
	return line, col
}

// checkDuplicateKeys rejects objects that repeat a key. encoding/json would
// silently keep the last value, which can hide contradictory permissions.
func checkDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	type frame struct {
		object    bool
		keys      map[string]struct{}
		expectKey bool
		lastKey   string
		index     int
		path      string
	}
	var stack []*frame
	childPath := func() string {
		if len(stack) == 0 {
			return "$"
		}
		top := stack[len(stack)-1]
		if top.object {
			return top.path + "." + top.lastKey
		}
		return fmt.Sprintf("%s[%d]", top.path, top.index)
	}
	valueDone := func() {
		if n := len(stack); n > 0 {
			if top := stack[n-1]; top.object {
				top.expectKey = true
			} else {
				top.index++
			}
		}
	}
	for {
		off := dec.InputOffset()
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return describeJSONError(data, err)
		}
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].expectKey {
			top := stack[n-1]
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:n-1]
				valueDone()
				continue
			}
			key, _ := tok.(string)
			if _, dup := top.keys[key]; dup {
				line, _ := position(data, off)
				return fmt.Errorf("line %d: duplicate key %q in %s", line, key, top.path)
			}
			top.keys[key] = struct{}{}
			top.lastKey = key
			top.expectKey = false
			continue
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, &frame{object: true, keys: map[string]struct{}{}, expectKey: true, path: childPath()})
			case '[':
				stack = append(stack, &frame{path: childPath()})
			default: // ']' (a '}' is handled above)
				stack = stack[:len(stack)-1]
				valueDone()
			}
			continue
		}
		valueDone()
	}
}

// ---- validation ------------------------------------------------------------

type validator struct {
	errs     []string
	warnings []string
}

func (v *validator) errorf(format string, args ...any) {
	v.errs = append(v.errs, fmt.Sprintf(format, args...))
}

func (v *validator) warnf(format string, args ...any) {
	v.warnings = append(v.warnings, fmt.Sprintf(format, args...))
}

func (v *validator) err() error {
	if len(v.errs) == 0 {
		return nil
	}
	if len(v.errs) == 1 {
		return errors.New(v.errs[0])
	}
	return fmt.Errorf("%d problems:\n  - %s", len(v.errs), strings.Join(v.errs, "\n  - "))
}

var (
	vmIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	envNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	pathSegment     = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
	uuidPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

func (v *validator) resolve(fc *fileConfig, baseDir string) *Config {
	cfg := &Config{
		vmByID: map[string]int{},
		users:  map[string]*user{},
	}

	switch Mode(fc.Mode) {
	case ModeSingleUser, ModeMultiuser:
		cfg.Mode = Mode(fc.Mode)
	case "":
		v.errorf(`mode: required; use "single-user" or "multiuser"`)
	default:
		v.errorf(`mode: unknown mode %q; use "single-user" or "multiuser"`, fc.Mode)
	}

	cfg.Title = strings.TrimSpace(fc.Title)
	if cfg.Title == "" {
		cfg.Title = "VM Controller"
	}
	if !cleanText(cfg.Title, 80) {
		v.errorf("title: must be at most 80 characters without control characters")
	}

	cfg.Listen = fc.Listen
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8090"
	}
	v.checkListen(cfg.Listen)

	cfg.BasePath = v.basePath(fc.BasePath)

	cfg.Database = fc.Database
	if cfg.Database == "" {
		cfg.Database = "/var/lib/vm-controller/vm-controller.db"
	}
	cfg.Database = resolvePath(baseDir, cfg.Database)

	switch CookieSecure(fc.CookieSecure) {
	case "", CookieSecureAuto:
		cfg.CookieSecure = CookieSecureAuto
	case CookieSecureAlways:
		cfg.CookieSecure = CookieSecureAlways
	case CookieSecureNever:
		cfg.CookieSecure = CookieSecureNever
		v.warnf("cookie_secure is \"never\": session cookies will be sent over plain HTTP")
	default:
		v.errorf(`cookie_secure: unknown value %q; use "auto", "always" or "never"`, fc.CookieSecure)
	}

	cfg.TrustedProxies = v.trustedProxies(fc.TrustedProxies)

	lifetime := intOr(fc.Session.LifetimeHours, 12)
	if lifetime < 1 || lifetime > 720 {
		v.errorf("session.lifetime_hours: must be between 1 and 720")
	}
	idle := intOr(fc.Session.IdleTimeoutMinutes, 120)
	if idle < 5 || idle > lifetime*60 {
		v.errorf("session.idle_timeout_minutes: must be between 5 and lifetime_hours*60")
	}
	cfg.SessionLifetime = time.Duration(lifetime) * time.Hour
	cfg.IdleTimeout = time.Duration(idle) * time.Minute

	cfg.Vexa = v.vexa(&fc.Vexa, baseDir)

	v.vms(cfg, fc.VMs)

	switch cfg.Mode {
	case ModeSingleUser:
		v.singleUser(cfg, fc)
	case ModeMultiuser:
		v.multiuser(cfg, fc)
	}

	cfg.Warnings = v.warnings
	return cfg
}

func (v *validator) checkListen(addr string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		v.errorf("listen: %q is not host:port (example: \"127.0.0.1:8090\")", addr)
		return
	}
	if port == "" || strings.Trim(port, "0123456789") != "" {
		v.errorf("listen: port in %q must be numeric", addr)
	}
	ip, err := netip.ParseAddr(host)
	switch {
	case host == "localhost":
	case err != nil:
		v.errorf("listen: host in %q must be an IP address (use 127.0.0.1 behind a reverse proxy)", addr)
	case !ip.IsLoopback():
		v.warnf("listen address %s is not loopback; make sure only the reverse proxy can reach it", addr)
	}
}

func (v *validator) basePath(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		v.errorf("base_path: %q must start with \"/\" (example: \"/vmctl\")", raw)
		return ""
	}
	p = strings.TrimSuffix(p, "/")
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" || seg == "." || seg == ".." || !pathSegment.MatchString(seg) {
			v.errorf("base_path: %q may contain only letters, digits and . _ ~ - between slashes", raw)
			return ""
		}
	}
	if p == "/api" || p == "/static" {
		v.warnf("base_path %q may be confusing; a dedicated prefix such as /vmctl is recommended", p)
	}
	return p
}

func (v *validator) trustedProxies(raw []string) []netip.Prefix {
	if raw == nil {
		return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	}
	out := make([]netip.Prefix, 0, len(raw))
	for i, s := range raw {
		if pfx, err := netip.ParsePrefix(s); err == nil {
			if pfx.Bits() == 0 {
				v.errorf("trusted_proxies[%d]: %q would trust every client; list your proxy addresses instead", i, s)
				continue
			}
			out = append(out, pfx.Masked())
			continue
		}
		if ip, err := netip.ParseAddr(s); err == nil {
			out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
			continue
		}
		v.errorf("trusted_proxies[%d]: %q is not an IP address or CIDR", i, s)
	}
	return out
}

func (v *validator) vexa(fv *fileVexa, baseDir string) Vexa {
	out := Vexa{TrackJobs: fv.TrackJobs}

	if fv.BaseURL == "" {
		v.errorf("vexa.base_url: required (example: \"http://127.0.0.1:8080\")")
	} else if u, err := url.Parse(fv.BaseURL); err != nil {
		v.errorf("vexa.base_url: %q is not a valid URL", fv.BaseURL)
	} else {
		switch {
		case u.Scheme != "http" && u.Scheme != "https":
			v.errorf("vexa.base_url: scheme must be http or https")
		case u.Host == "" || u.Hostname() == "":
			v.errorf("vexa.base_url: host is missing")
		case u.User != nil:
			v.errorf("vexa.base_url: must not contain credentials; use api_key_env or api_key_file")
		case u.RawQuery != "" || u.Fragment != "" || u.ForceQuery:
			v.errorf("vexa.base_url: must not contain a query or fragment")
		default:
			u.Path = strings.TrimSuffix(u.Path, "/")
			u.RawPath = ""
			if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
				if !fv.AllowInsecureHTTP {
					v.errorf("vexa.base_url: plain http to a non-loopback host would send the API key unencrypted; use https or set vexa.allow_insecure_http to true")
				} else {
					v.warnf("vexa.base_url uses plain http to %s; the API key is sent unencrypted", u.Host)
				}
			}
			out.BaseURL = u
		}
	}

	switch {
	case fv.APIKeyEnv != "" && fv.APIKeyFile != "":
		v.errorf("vexa: set only one of api_key_env and api_key_file")
	case fv.APIKeyFile != "":
		out.APIKeyFile = resolvePath(baseDir, fv.APIKeyFile)
	default:
		out.APIKeyEnv = fv.APIKeyEnv
		if out.APIKeyEnv == "" {
			out.APIKeyEnv = "VEXA_API_KEY"
		}
		if !envNamePattern.MatchString(out.APIKeyEnv) {
			v.errorf("vexa.api_key_env: %q is not a valid environment variable name", fv.APIKeyEnv)
		}
	}
	if fv.CAFile != "" {
		out.CAFile = resolvePath(baseDir, fv.CAFile)
	}

	out.Timeout = v.seconds("vexa.timeout_seconds", fv.TimeoutSeconds, 10, 1, 120)
	out.StatusCacheTTL = v.seconds("vexa.status_cache_seconds", fv.StatusCacheSeconds, 5, 1, 300)
	out.StartTimeout = v.seconds("vexa.start_timeout_seconds", fv.StartTimeoutSeconds, 180, 10, 3600)
	out.StopTimeout = v.seconds("vexa.stop_timeout_seconds", fv.StopTimeoutSeconds, 420, 10, 3600)
	if fv.StopTimeoutSeconds != nil && out.StopTimeout < 6*time.Minute {
		v.warnf("vexa.stop_timeout_seconds is below 360: Vexa-VM may take up to 5 minutes to notice that a guest has powered off")
	}
	return out
}

func (v *validator) seconds(field string, val *int, def, lo, hi int) time.Duration {
	n := intOr(val, def)
	if n < lo || n > hi {
		v.errorf("%s: must be between %d and %d", field, lo, hi)
		n = def
	}
	return time.Duration(n) * time.Second
}

func (v *validator) vms(cfg *Config, raw []fileVM) {
	if len(raw) == 0 {
		v.errorf("vms: at least one VM is required")
		return
	}
	folded := map[string]int{}
	for i, fvm := range raw {
		field := fmt.Sprintf("vms[%d]", i)
		vm := VM{
			ID:          strings.TrimSpace(fvm.ID),
			Name:        strings.TrimSpace(fvm.Name),
			Description: strings.TrimSpace(fvm.Description),
			AllowStop:   fvm.AllowStop,
			URL:         strings.TrimSpace(fvm.URL),
		}
		ok := true
		if vm.ID == "" {
			v.errorf("%s.id: required (the Vexa-VM UUID of the VM)", field)
			ok = false
		} else if !vmIDPattern.MatchString(vm.ID) {
			v.errorf("%s.id: %q may contain only letters, digits, '.', '_' and '-' (max 128)", field, vm.ID)
			ok = false
		} else if j, dup := folded[strings.ToLower(vm.ID)]; dup {
			v.errorf("%s.id: duplicate VM id %q (already used by vms[%d])", field, vm.ID, j)
			ok = false
		}
		if vm.Name == "" {
			v.errorf("%s.name: required", field)
		} else if !cleanText(vm.Name, 100) {
			v.errorf("%s.name: must be at most 100 characters without control characters", field)
		}
		if !cleanText(vm.Description, 300) {
			v.errorf("%s.description: must be at most 300 characters without control characters", field)
		}
		if vm.URL != "" && !validLink(vm.URL) {
			v.errorf("%s.url: %q must be an http(s) URL or a path starting with \"/\"", field, vm.URL)
		}
		if ok && !uuidPattern.MatchString(vm.ID) {
			v.warnf("vms[%d].id %q is not a UUID: Vexa-VM also accepts VM names, but a name can later refer to a different VM; prefer the UUID", i, vm.ID)
		}
		if ok {
			folded[strings.ToLower(vm.ID)] = i
			cfg.vmByID[vm.ID] = len(cfg.VMs)
			cfg.VMs = append(cfg.VMs, vm)
		}
	}
}

func (v *validator) singleUser(cfg *Config, fc *fileConfig) {
	if len(fc.Users) > 0 {
		v.warnf("mode is single-user: the \"users\" section is ignored")
	}
	if fc.SingleUser == nil {
		v.errorf("single_user: required in single-user mode (example: {\"username\": \"owner\"})")
		return
	}
	name := fc.SingleUser.Username
	if !usernamePattern.MatchString(name) {
		v.errorf("single_user.username: %q must be 1-64 lowercase letters, digits, '.', '_' or '-'", name)
		return
	}
	u := &user{name: name, perms: map[string]Permission{}}
	for _, vm := range cfg.VMs {
		if vm.AllowStop {
			u.perms[vm.ID] = PermStartStop
		} else {
			u.perms[vm.ID] = PermStart
		}
	}
	cfg.users[name] = u
}

func (v *validator) multiuser(cfg *Config, fc *fileConfig) {
	if fc.SingleUser != nil {
		v.warnf("mode is multiuser: the \"single_user\" section is ignored")
	}
	if len(fc.Users) == 0 {
		v.errorf("users: at least one user is required in multiuser mode")
		return
	}
	assigned := map[string]bool{}
	for i, fu := range fc.Users {
		field := fmt.Sprintf("users[%d]", i)
		if !usernamePattern.MatchString(fu.Username) {
			v.errorf("%s.username: %q must be 1-64 lowercase letters, digits, '.', '_' or '-'", field, fu.Username)
			continue
		}
		if _, dup := cfg.users[fu.Username]; dup {
			v.errorf("%s.username: duplicate user %q", field, fu.Username)
			continue
		}
		u := &user{name: fu.Username, disabled: fu.Disabled, perms: map[string]Permission{}}
		ids := make([]string, 0, len(fu.Permissions))
		for id := range fu.Permissions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			raw := fu.Permissions[id]
			perm, ok := parsePermission(raw)
			if !ok {
				v.errorf("%s.permissions[%q]: unknown permission %q; use \"none\", \"start\" or \"start-stop\"", field, id, raw)
				continue
			}
			idx, known := cfg.vmByID[id]
			if !known {
				hint := ""
				for _, vm := range cfg.VMs {
					if strings.EqualFold(vm.ID, id) {
						hint = fmt.Sprintf(" (did you mean %q?)", vm.ID)
					}
				}
				v.errorf("%s.permissions: VM %q is not defined in \"vms\"%s", field, id, hint)
				continue
			}
			vm := cfg.VMs[idx]
			if perm == PermStartStop && !vm.AllowStop {
				v.errorf("%s.permissions[%q]: \"start-stop\" contradicts allow_stop=false on VM %q; use \"start\" or enable allow_stop", field, id, vm.Name)
				continue
			}
			if perm != PermNone {
				u.perms[id] = perm
				if !fu.Disabled {
					assigned[id] = true
				}
			}
		}
		if len(u.perms) == 0 && !fu.Disabled {
			v.warnf("user %q has no VMs assigned", fu.Username)
		}
		cfg.users[fu.Username] = u
	}
	for _, vm := range cfg.VMs {
		if !assigned[vm.ID] {
			v.warnf("VM %q (%s) is not assigned to any enabled user", vm.Name, vm.ID)
		}
	}
}

// ---- policy queries --------------------------------------------------------

// VM returns the configured VM with the given exact ID.
func (c *Config) VM(id string) (VM, bool) {
	i, ok := c.vmByID[id]
	if !ok {
		return VM{}, false
	}
	return c.VMs[i], true
}

// UserActive reports whether username is configured and not disabled.
// Only active users may log in or keep a session.
func (c *Config) UserActive(username string) bool {
	u, ok := c.users[username]
	return ok && !u.disabled
}

// UserDefined reports whether username appears in the configuration of the
// current mode, and whether it is disabled.
func (c *Config) UserDefined(username string) (defined, disabled bool) {
	u, ok := c.users[username]
	if !ok {
		return false, false
	}
	return true, u.disabled
}

// Usernames returns all users of the current mode, sorted.
func (c *Config) Usernames() []string {
	out := make([]string, 0, len(c.users))
	for name := range c.users {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Permission returns what username may do with vmID. Unknown and disabled
// users, unknown VMs and unassigned VMs all yield PermNone. Stop is never
// granted for a VM whose AllowStop is false.
func (c *Config) Permission(username, vmID string) Permission {
	u, ok := c.users[username]
	if !ok || u.disabled {
		return PermNone
	}
	vm, ok := c.VM(vmID)
	if !ok {
		return PermNone
	}
	p := u.perms[vmID]
	if p == PermStartStop && !vm.AllowStop {
		p = PermStart
	}
	return p
}

// VisibleVM is a VM together with the viewer's permission on it.
type VisibleVM struct {
	VM         VM
	Permission Permission
}

// VisibleVMs returns the VMs username may see, in configuration order.
func (c *Config) VisibleVMs(username string) []VisibleVM {
	var out []VisibleVM
	for _, vm := range c.VMs {
		if p := c.Permission(username, vm.ID); p.CanView() {
			out = append(out, VisibleVM{VM: vm, Permission: p})
		}
	}
	return out
}

// ---- secrets ---------------------------------------------------------------

// LoadAPIKey reads the Vexa API key from the configured environment variable
// or file. The key is never part of the JSON configuration.
func (c *Config) LoadAPIKey() (string, error) {
	var raw, source string
	if c.Vexa.APIKeyFile != "" {
		source = c.Vexa.APIKeyFile
		f, err := os.Open(source)
		if err != nil {
			return "", fmt.Errorf("vexa API key file: %w", err)
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return "", fmt.Errorf("vexa API key file: %w", err)
		}
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("vexa API key file %s is not a regular file", source)
		}
		if st.Mode().Perm()&0o007 != 0 {
			return "", fmt.Errorf("vexa API key file %s is accessible by other users (mode %04o); run: chmod 0640 %s", source, st.Mode().Perm(), source)
		}
		data, err := io.ReadAll(io.LimitReader(f, 8192))
		if err != nil {
			return "", fmt.Errorf("vexa API key file: %w", err)
		}
		raw = string(data)
	} else {
		source = "environment variable " + c.Vexa.APIKeyEnv
		raw = os.Getenv(c.Vexa.APIKeyEnv)
	}
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", fmt.Errorf("vexa API key is missing: %s is empty or not set", source)
	}
	if len(key) > 4096 || strings.IndexFunc(key, func(r rune) bool { return r <= ' ' || r == 0x7f || r > 0x7e }) >= 0 {
		return "", fmt.Errorf("vexa API key from %s contains whitespace or non-printable characters", source)
	}
	return key, nil
}

// ---- helpers ---------------------------------------------------------------

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func resolvePath(baseDir, p string) string {
	if filepath.IsAbs(p) || baseDir == "" {
		return filepath.Clean(p)
	}
	return filepath.Join(baseDir, p)
}

func cleanText(s string, max int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	return strings.IndexFunc(s, unicode.IsControl) < 0
}

func validLink(s string) bool {
	if !cleanText(s, 2048) || strings.ContainsAny(s, " \\") {
		return false
	}
	if strings.HasPrefix(s, "/") {
		return !strings.HasPrefix(s, "//")
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

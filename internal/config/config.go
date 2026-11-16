// Package config loads, validates and persists the global application
// configuration (a JSON file) and resolves secrets from the environment or
// protected secret files.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Duration is a time.Duration that is (un)marshalled as a Go duration string
// such as "5s" or "10m".
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// Also accept a plain number of seconds.
		var n float64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return fmt.Errorf("duration must be a string like \"30s\"")
		}
		*d = Duration(time.Duration(n * float64(time.Second)))
		return nil
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

const (
	ModeProduction  = "production"
	ModeDevelopment = "development"
)

type Paths struct {
	DataDir    string `json:"data_dir"`
	Database   string `json:"database"`
	ServersDir string `json:"servers_dir"`
	LogDir     string `json:"log_dir"`
	SecretsDir string `json:"secrets_dir"`
	RuntimeDir string `json:"runtime_dir"`
}

type Argon2 struct {
	MemoryKiB   uint32 `json:"memory_kib"`
	Iterations  uint32 `json:"iterations"`
	Parallelism uint8  `json:"parallelism"`
}

type Auth struct {
	AccessTokenTTL    Duration `json:"access_token_ttl"`
	RefreshTokenTTL   Duration `json:"refresh_token_ttl"`
	InvitationTTL     Duration `json:"invitation_ttl"`
	EmailChangeTTL    Duration `json:"email_change_ttl"`
	MinPasswordLength int      `json:"min_password_length"`
	// CookieSecure: "auto" (secure when public_url is https or the request
	// arrived over TLS), "always" or "never".
	CookieSecure  string `json:"cookie_secure"`
	JWTSecretFile string `json:"jwt_secret_file"`
	Argon2        Argon2 `json:"argon2"`
}

type SMTP struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	// PasswordFile points at a file (mode 0600) holding the SMTP password.
	// The SM_SMTP_PASSWORD environment variable takes precedence.
	PasswordFile string `json:"password_file"`
	From         string `json:"from"`
	// TLS: "starttls" (default), "tls" (implicit TLS) or "none".
	TLS string `json:"tls"`
}

func (s SMTP) Configured() bool { return s.Host != "" && s.From != "" }

type CORS struct {
	AllowedOrigins []string `json:"allowed_origins"`
}

type Rate struct {
	Requests int      `json:"requests"`
	Window   Duration `json:"window"`
}

type RateLimits struct {
	Login          Rate     `json:"login"`
	Refresh        Rate     `json:"refresh"`
	API            Rate     `json:"api"`
	ServerStart    Rate     `json:"server_start"`
	ServerStop     Rate     `json:"server_stop"`
	ConsoleCommand Rate     `json:"console_command"`
	BackupCreate   Rate     `json:"backup_create"`
	ServerCooldown Duration `json:"server_cooldown"`
}

type Monitoring struct {
	HostMetricsInterval Duration `json:"host_metrics_interval"`
	HostMetricsForUsers bool     `json:"host_metrics_for_users"`
	StatusPollInterval  Duration `json:"status_poll_interval"`
}

type Console struct {
	BufferLines      int `json:"buffer_lines"`
	MaxLineBytes     int `json:"max_line_bytes"`
	SnapshotLines    int `json:"snapshot_lines"`
	MaxCommandLength int `json:"max_command_length"`
	// MaxLogFileBytes bounds the on-disk capture files kept for custom and
	// tmux servers; they are truncated when they grow past this size.
	MaxLogFileBytes int64 `json:"max_log_file_bytes"`
}

type Backups struct {
	MaxDownloadBytes       int64 `json:"max_download_bytes"`
	MaxConcurrentDownloads int   `json:"max_concurrent_downloads"`
}

type Logging struct {
	Level  string `json:"level"`
	Format string `json:"format"`
	// File is optional; when empty logs go to stderr only.
	File string `json:"file"`
}

type Audit struct {
	RetentionDays int `json:"retention_days"`
}

type Security struct {
	// ConfigPermissionFullAccess lets non-administrators holding the CONFIG
	// permission edit execution settings (commands, paths, environment). This
	// is equivalent to granting them command execution as the service user
	// and is disabled by default.
	ConfigPermissionFullAccess bool  `json:"config_permission_full_access"`
	MaxRequestBytes            int64 `json:"max_request_bytes"`
}

type Tools struct {
	Tmux         string `json:"tmux"`
	Systemctl    string `json:"systemctl"`
	Journalctl   string `json:"journalctl"`
	Sudo         string `json:"sudo"`
	DockerSocket string `json:"docker_socket"`
}

type Config struct {
	Mode           string     `json:"mode"`
	Listen         string     `json:"listen"`
	PublicURL      string     `json:"public_url"`
	TrustedProxies []string   `json:"trusted_proxies"`
	Paths          Paths      `json:"paths"`
	Auth           Auth       `json:"auth"`
	SMTP           SMTP       `json:"smtp"`
	CORS           CORS       `json:"cors"`
	RateLimits     RateLimits `json:"rate_limits"`
	Monitoring     Monitoring `json:"monitoring"`
	Console        Console    `json:"console"`
	Backups        Backups    `json:"backups"`
	Logging        Logging    `json:"logging"`
	Audit          Audit      `json:"audit"`
	Security       Security   `json:"security"`
	Tools          Tools      `json:"tools"`
}

func sec(n int) Duration { return Duration(time.Duration(n) * time.Second) }

// Default returns the built-in defaults. Paths are filled in by
// applyPathDefaults relative to the configuration file location.
func Default() *Config {
	return &Config{
		Mode:           ModeProduction,
		Listen:         "127.0.0.1:8080",
		PublicURL:      "",
		TrustedProxies: []string{"127.0.0.1/32", "::1/128"},
		Auth: Auth{
			AccessTokenTTL:    sec(600),
			RefreshTokenTTL:   Duration(30 * 24 * time.Hour),
			InvitationTTL:     Duration(72 * time.Hour),
			EmailChangeTTL:    Duration(24 * time.Hour),
			MinPasswordLength: 10,
			CookieSecure:      "auto",
			Argon2:            Argon2{MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 2},
		},
		SMTP: SMTP{Port: 587, TLS: "starttls"},
		RateLimits: RateLimits{
			Login:          Rate{Requests: 5, Window: sec(60)},
			Refresh:        Rate{Requests: 30, Window: sec(60)},
			API:            Rate{Requests: 600, Window: sec(60)},
			ServerStart:    Rate{Requests: 6, Window: sec(60)},
			ServerStop:     Rate{Requests: 6, Window: sec(60)},
			ConsoleCommand: Rate{Requests: 60, Window: sec(60)},
			BackupCreate:   Rate{Requests: 3, Window: sec(300)},
			ServerCooldown: sec(3),
		},
		Monitoring: Monitoring{
			HostMetricsInterval: sec(5),
			HostMetricsForUsers: true,
			StatusPollInterval:  sec(5),
		},
		Console: Console{
			BufferLines:      2000,
			MaxLineBytes:     4096,
			SnapshotLines:    300,
			MaxCommandLength: 1024,
			MaxLogFileBytes:  16 << 20,
		},
		Backups:  Backups{MaxDownloadBytes: 0, MaxConcurrentDownloads: 4},
		Logging:  Logging{Level: "info", Format: "text"},
		Audit:    Audit{RetentionDays: 0},
		Security: Security{MaxRequestBytes: 1 << 20},
		Tools: Tools{
			Tmux: "tmux", Systemctl: "systemctl", Journalctl: "journalctl", Sudo: "sudo",
			DockerSocket: "/var/run/docker.sock",
		},
	}
}

func (c *Config) Development() bool { return c.Mode == ModeDevelopment }

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	b, _ := json.Marshal(c)
	out := &Config{}
	_ = json.Unmarshal(b, out)
	return out
}

func (c *Config) applyPathDefaults(configDir string) {
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(configDir, p)
	}
	p := &c.Paths
	if p.DataDir == "" {
		p.DataDir = "/var/lib/server-manager"
	}
	p.DataDir = abs(p.DataDir)
	if p.Database == "" {
		p.Database = filepath.Join(p.DataDir, "server-manager.db")
	}
	if p.ServersDir == "" {
		p.ServersDir = filepath.Join(configDir, "servers")
	}
	if p.LogDir == "" {
		p.LogDir = "/var/log/server-manager"
	}
	if p.SecretsDir == "" {
		p.SecretsDir = filepath.Join(p.DataDir, "secrets")
	}
	if p.RuntimeDir == "" {
		p.RuntimeDir = filepath.Join(p.DataDir, "runtime")
	}
	p.Database, p.ServersDir, p.LogDir = abs(p.Database), abs(p.ServersDir), abs(p.LogDir)
	p.SecretsDir, p.RuntimeDir = abs(p.SecretsDir), abs(p.RuntimeDir)
	if c.Auth.JWTSecretFile == "" {
		c.Auth.JWTSecretFile = filepath.Join(p.SecretsDir, "jwt.key")
	}
	c.Auth.JWTSecretFile = abs(c.Auth.JWTSecretFile)
	c.SMTP.PasswordFile = abs(c.SMTP.PasswordFile)
	if c.Logging.File != "" && !filepath.IsAbs(c.Logging.File) {
		c.Logging.File = filepath.Join(p.LogDir, c.Logging.File)
	}
}

// ValidationError collects every problem found in a configuration.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid configuration: " + strings.Join(e.Problems, "; ")
}

func checkRate(problems *[]string, name string, r Rate) {
	if r.Requests < 1 || r.Requests > 1_000_000 {
		*problems = append(*problems, fmt.Sprintf("rate_limits.%s.requests must be between 1 and 1000000", name))
	}
	if r.Window.D() < time.Second || r.Window.D() > 24*time.Hour {
		*problems = append(*problems, fmt.Sprintf("rate_limits.%s.window must be between 1s and 24h", name))
	}
}

// Validate checks the configuration and returns a *ValidationError listing
// all problems, or nil.
func (c *Config) Validate() error {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }

	if c.Mode != ModeProduction && c.Mode != ModeDevelopment {
		add("mode must be %q or %q", ModeProduction, ModeDevelopment)
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		add("listen must be host:port (%v)", err)
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("public_url must be an absolute http(s) URL")
		} else if u.RawQuery != "" || u.Fragment != "" {
			add("public_url must not contain a query or fragment")
		}
	}
	for _, cidr := range c.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			if net.ParseIP(cidr) == nil {
				add("trusted_proxies entry %q is not an IP or CIDR", cidr)
			}
		}
	}
	for _, o := range c.CORS.AllowedOrigins {
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
			add("cors.allowed_origins entry %q must be an origin like https://example.com", o)
		}
	}
	a := c.Auth
	if a.AccessTokenTTL.D() < 30*time.Second || a.AccessTokenTTL.D() > time.Hour {
		add("auth.access_token_ttl must be between 30s and 1h")
	}
	if a.RefreshTokenTTL.D() < 5*time.Minute || a.RefreshTokenTTL.D() > 366*24*time.Hour {
		add("auth.refresh_token_ttl must be between 5m and 366 days")
	}
	if a.InvitationTTL.D() < time.Minute || a.InvitationTTL.D() > 90*24*time.Hour {
		add("auth.invitation_ttl must be between 1m and 90 days")
	}
	if a.EmailChangeTTL.D() < time.Minute || a.EmailChangeTTL.D() > 30*24*time.Hour {
		add("auth.email_change_ttl must be between 1m and 30 days")
	}
	if a.MinPasswordLength < 8 || a.MinPasswordLength > 128 {
		add("auth.min_password_length must be between 8 and 128")
	}
	switch a.CookieSecure {
	case "auto", "always", "never":
	default:
		add("auth.cookie_secure must be auto, always or never")
	}
	if a.Argon2.MemoryKiB < 8*1024 || a.Argon2.MemoryKiB > 4*1024*1024 {
		add("auth.argon2.memory_kib must be between 8192 and 4194304")
	}
	if a.Argon2.Iterations < 1 || a.Argon2.Iterations > 20 {
		add("auth.argon2.iterations must be between 1 and 20")
	}
	if a.Argon2.Parallelism < 1 || a.Argon2.Parallelism > 16 {
		add("auth.argon2.parallelism must be between 1 and 16")
	}
	s := c.SMTP
	if s.Host != "" || s.From != "" {
		if s.Host == "" {
			add("smtp.host is required when smtp.from is set")
		}
		if s.Port < 1 || s.Port > 65535 {
			add("smtp.port must be a valid port")
		}
		if _, err := mail.ParseAddress(s.From); err != nil {
			add("smtp.from must be a valid email address")
		}
	}
	switch s.TLS {
	case "starttls", "tls", "none":
	default:
		add("smtp.tls must be starttls, tls or none")
	}
	r := c.RateLimits
	checkRate(&p, "login", r.Login)
	checkRate(&p, "refresh", r.Refresh)
	checkRate(&p, "api", r.API)
	checkRate(&p, "server_start", r.ServerStart)
	checkRate(&p, "server_stop", r.ServerStop)
	checkRate(&p, "console_command", r.ConsoleCommand)
	checkRate(&p, "backup_create", r.BackupCreate)
	if r.ServerCooldown.D() < 0 || r.ServerCooldown.D() > time.Hour {
		add("rate_limits.server_cooldown must be between 0s and 1h")
	}
	m := c.Monitoring
	if m.HostMetricsInterval.D() < time.Second || m.HostMetricsInterval.D() > 5*time.Minute {
		add("monitoring.host_metrics_interval must be between 1s and 5m")
	}
	if m.StatusPollInterval.D() < time.Second || m.StatusPollInterval.D() > 10*time.Minute {
		add("monitoring.status_poll_interval must be between 1s and 10m")
	}
	cs := c.Console
	if cs.BufferLines < 100 || cs.BufferLines > 100_000 {
		add("console.buffer_lines must be between 100 and 100000")
	}
	if cs.MaxLineBytes < 256 || cs.MaxLineBytes > 65536 {
		add("console.max_line_bytes must be between 256 and 65536")
	}
	if cs.SnapshotLines < 10 || cs.SnapshotLines > cs.BufferLines {
		add("console.snapshot_lines must be between 10 and console.buffer_lines")
	}
	if cs.MaxCommandLength < 1 || cs.MaxCommandLength > 8192 {
		add("console.max_command_length must be between 1 and 8192")
	}
	if cs.MaxLogFileBytes < 64<<10 {
		add("console.max_log_file_bytes must be at least 65536")
	}
	if c.Backups.MaxDownloadBytes < 0 {
		add("backups.max_download_bytes must not be negative (0 = unlimited)")
	}
	if c.Backups.MaxConcurrentDownloads < 1 || c.Backups.MaxConcurrentDownloads > 1000 {
		add("backups.max_concurrent_downloads must be between 1 and 1000")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		add("logging.level must be debug, info, warn or error")
	}
	switch c.Logging.Format {
	case "text", "json":
	default:
		add("logging.format must be text or json")
	}
	if c.Audit.RetentionDays < 0 {
		add("audit.retention_days must not be negative (0 = keep forever)")
	}
	if c.Security.MaxRequestBytes < 4096 || c.Security.MaxRequestBytes > 64<<20 {
		add("security.max_request_bytes must be between 4096 and 67108864")
	}
	for name, v := range map[string]string{"tmux": c.Tools.Tmux, "systemctl": c.Tools.Systemctl, "journalctl": c.Tools.Journalctl, "sudo": c.Tools.Sudo} {
		if v == "" || strings.ContainsAny(v, "\x00\n") {
			add("tools.%s must be a command name or path", name)
		}
	}
	if c.Tools.DockerSocket == "" || !filepath.IsAbs(c.Tools.DockerSocket) {
		add("tools.docker_socket must be an absolute path")
	}
	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

// Parse decodes a configuration document on top of the defaults. Unknown
// fields are rejected so that typos do not silently fall back to defaults.
func Parse(data []byte, configDir string) (*Config, error) {
	c := Default()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("parse configuration: %w", err)
	}
	c.applyPathDefaults(configDir)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Holder owns the live configuration. Readers call Get (lock-free); writers
// call Update, which validates, persists atomically and swaps the pointer.
type Holder struct {
	path string
	mu   sync.Mutex
	cur  atomic.Pointer[Config]
	subs []func(*Config)
}

// Load reads the configuration file at path.
func Load(path string) (*Holder, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	c, err := Parse(data, filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	h := &Holder{path: abs}
	h.cur.Store(c)
	return h, nil
}

// NewHolder wraps an in-memory configuration (used by tests). When path is
// empty, updates are not persisted.
func NewHolder(c *Config, path string) *Holder {
	h := &Holder{path: path}
	h.cur.Store(c)
	return h
}

func (h *Holder) Get() *Config { return h.cur.Load() }
func (h *Holder) Path() string { return h.path }

// OnChange registers a callback invoked after each successful Update.
func (h *Holder) OnChange(fn func(*Config)) {
	h.mu.Lock()
	h.subs = append(h.subs, fn)
	h.mu.Unlock()
}

// Update applies fn to a copy of the configuration, validates the result,
// writes it to disk and makes it current.
func (h *Holder) Update(fn func(*Config) error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	next := h.cur.Load().Clone()
	if err := fn(next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if h.path != "" {
		data, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return err
		}
		if err := WriteFileAtomic(h.path, append(data, '\n'), 0o600); err != nil {
			return fmt.Errorf("save configuration: %w", err)
		}
	}
	h.cur.Store(next)
	for _, s := range h.subs {
		s(next)
	}
	return nil
}

// WriteFileAtomic writes data to a temporary file in the same directory and
// renames it over path so readers never observe a partial file.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { f.Close(); os.Remove(tmp) }
	if err := f.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// EnsureDirs creates the application-owned directories with restrictive
// permissions.
func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.Paths.DataDir, c.Paths.SecretsDir, c.Paths.RuntimeDir, filepath.Dir(c.Paths.Database)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	if err := os.MkdirAll(c.Paths.ServersDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", c.Paths.ServersDir, err)
	}
	if c.Logging.File != "" {
		if err := os.MkdirAll(filepath.Dir(c.Logging.File), 0o750); err != nil {
			return fmt.Errorf("create log directory: %w", err)
		}
	}
	return nil
}

// JWTSecret returns the signing key: SM_JWT_SECRET if set, otherwise the
// contents of auth.jwt_secret_file, which is generated on first use.
func (c *Config) JWTSecret() ([]byte, error) {
	if v := os.Getenv("SM_JWT_SECRET"); v != "" {
		if len(v) < 32 {
			return nil, errors.New("SM_JWT_SECRET must be at least 32 characters")
		}
		return []byte(v), nil
	}
	path := c.Auth.JWTSecretFile
	data, err := os.ReadFile(path)
	if err == nil {
		key := bytes.TrimSpace(data)
		if len(key) < 32 {
			return nil, fmt.Errorf("JWT secret file %s is too short (need at least 32 bytes)", path)
		}
		if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("JWT secret file %s must not be accessible by group or others (chmod 600)", path)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read JWT secret: %w", err)
	}
	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	key := []byte(fmt.Sprintf("%x", raw))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create JWT secret: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(key, '\n')); err != nil {
		return nil, err
	}
	return key, nil
}

// SMTPPassword resolves the SMTP password from SM_SMTP_PASSWORD or
// smtp.password_file. An empty result means "no authentication".
func (c *Config) SMTPPassword() (string, error) {
	if v := os.Getenv("SM_SMTP_PASSWORD"); v != "" {
		return v, nil
	}
	if c.SMTP.PasswordFile == "" {
		return "", nil
	}
	data, err := os.ReadFile(c.SMTP.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("read SMTP password file: %w", err)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

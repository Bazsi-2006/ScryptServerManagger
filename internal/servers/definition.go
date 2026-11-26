// Package servers defines the backend-independent server model, validates
// definitions and persists them as one JSON file per server.
package servers

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
)

type Duration = config.Duration

const (
	BackendCustom  = "custom"
	BackendSystemd = "systemd"
	BackendDocker  = "docker"
	BackendTmux    = "tmux"

	ModeForeground = "foreground"
	ModeForking    = "forking"

	ReadyNone    = "none"
	ReadyConsole = "console"
	ReadyPort    = "port"
	ReadyScript  = "script"

	InputNone       = "none"
	InputStdin      = "stdin"
	InputPTY        = "pty"
	InputTmux       = "tmux"
	InputCommand    = "command"
	InputDockerExec = "docker_exec"

	// CommandPlaceholder is replaced by the console line in input_command
	// arguments. The substituted text always stays inside one argv element.
	CommandPlaceholder = "{command}"

	// EnvMask replaces environment values when a definition is shown to
	// someone who may not read them; on update it means "keep current value".
	EnvMask = "********"

	// Backup and custom action execution methods.
	MethodCommand = "command" // run an executable with an argument list
	MethodTmux    = "tmux"    // type predefined text into the server's tmux pane

	// MaxActions bounds the custom actions of one server.
	MaxActions = 64
	// DefaultActionTimeout applies to command actions without a timeout.
	DefaultActionTimeout = 10 * time.Minute
)

// Command is an executable plus its argument vector. It is always executed
// directly (execve), never through a shell.
type Command struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

func (c *Command) Argv() []string {
	return append([]string{c.Command}, c.Args...)
}

type Custom struct {
	// Mode is "foreground" (the started process is the server; the manager
	// tracks it directly) or "forking" (the start command launches the server
	// in the background and exits).
	Mode  string   `json:"mode"`
	Start Command  `json:"start"`
	Stop  *Command `json:"stop,omitempty"`
	// StopInput is written to the server's stdin/PTY to request shutdown
	// (e.g. "stop") when no stop command is configured.
	StopInput string `json:"stop_input,omitempty"`
	// StopSignal is sent to the process group when neither a stop command nor
	// stop input is configured. Default SIGTERM.
	StopSignal string `json:"stop_signal,omitempty"`
	// KillOnTimeout sends SIGKILL when a graceful stop exceeds its timeout.
	// Default true.
	KillOnTimeout *bool `json:"kill_on_timeout,omitempty"`
	// PIDFile lets forking servers be tracked by process ID.
	PIDFile string `json:"pid_file,omitempty"`
	// LogFile is an additional file tailed into the console (useful for
	// forking servers that write their own log).
	LogFile string `json:"log_file,omitempty"`
	// OnManagerExit is "keep" (default: the server keeps running and is
	// re-adopted when the manager starts again) or "stop".
	OnManagerExit string `json:"on_manager_exit,omitempty"`
}

type Systemd struct {
	Unit string `json:"unit"`
	// Scope is "system" (default) or "user".
	Scope string `json:"scope,omitempty"`
	// UseSudo runs systemctl/journalctl through "sudo -n" (requires a
	// matching sudoers rule).
	UseSudo      bool  `json:"use_sudo,omitempty"`
	AllowRestart *bool `json:"allow_restart,omitempty"`
}

type Docker struct {
	Container    string `json:"container"`
	AllowRestart *bool  `json:"allow_restart,omitempty"`
}

type Tmux struct {
	Session string `json:"session"`
	// Window and Pane select the target pane; empty means the session's
	// active window / pane.
	Window     string `json:"window,omitempty"`
	Pane       string `json:"pane,omitempty"`
	SocketName string `json:"socket_name,omitempty"`
	SocketPath string `json:"socket_path,omitempty"`
	// Start is the command run in a new detached session. When omitted the
	// session is attach-only: it can be observed and stopped but not started.
	Start *Command `json:"start,omitempty"`
	// StopText is typed into the pane followed by Enter (e.g. "stop").
	StopText string `json:"stop_text,omitempty"`
	// StopKeys are tmux key names sent to the pane (e.g. ["C-c"]).
	StopKeys []string `json:"stop_keys,omitempty"`
	// KillOnTimeout kills the session when the graceful stop times out, or
	// immediately when no graceful method is configured. Default true.
	KillOnTimeout *bool `json:"kill_on_timeout,omitempty"`
	Width         int   `json:"width,omitempty"`
	Height        int   `json:"height,omitempty"`
}

type ConsoleMatch struct {
	Pattern string `json:"pattern"`
	Regex   bool   `json:"regex,omitempty"`
}

type PortCheck struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	// Timeout bounds a single probe.
	Timeout Duration `json:"timeout,omitempty"`
	// UDPPayloadHex is the datagram sent for UDP probes; the port counts as
	// ready when any reply datagram arrives.
	UDPPayloadHex string `json:"udp_payload_hex,omitempty"`
}

type ScriptCheck struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Timeout Duration `json:"timeout,omitempty"`
}

type Readiness struct {
	Method   string        `json:"method"`
	Console  *ConsoleMatch `json:"console,omitempty"`
	Port     *PortCheck    `json:"port,omitempty"`
	Script   *ScriptCheck  `json:"script,omitempty"`
	Interval Duration      `json:"interval,omitempty"`
}

type Console struct {
	// Input selects how console commands reach the server: none, stdin, pty,
	// tmux, command or docker_exec. Empty picks the backend default.
	Input        string   `json:"input,omitempty"`
	InputCommand *Command `json:"input_command,omitempty"`
}

// TmuxText is predefined text typed literally into the server's tmux pane
// (send-keys -l), optionally followed by the Enter key. It is data for the
// server's console and never interpreted by a shell on the host.
type TmuxText struct {
	Text string `json:"text"`
	// Enter presses Enter after the text. Default true.
	Enter *bool `json:"enter,omitempty"`
}

// PressEnter reports whether Enter follows the text.
func (t *TmuxText) PressEnter() bool { return boolOr(t.Enter, true) }

type Backup struct {
	// Directory holds the backup files that can be listed and downloaded.
	// Required for the command method (it is passed as SM_BACKUP_DIR);
	// optional for the tmux method.
	Directory string `json:"directory"`
	// Method is "command" (default: run Command) or "tmux" (type Tmux.Text
	// into the server's tmux pane; tmux backend only).
	Method  string    `json:"method,omitempty"`
	Command *Command  `json:"command,omitempty"`
	Tmux    *TmuxText `json:"tmux,omitempty"`
	// Completion recognises the end of a tmux backup in the console output.
	// Without it, a tmux backup is reported as "submitted": the command was
	// delivered, but whether the backup finished cannot be verified.
	Completion *ConsoleMatch `json:"completion,omitempty"`
	// AllowUserDelete lets holders of the BACKUP_DOWNLOAD permission delete
	// backup files; otherwise only administrators can.
	AllowUserDelete bool `json:"allow_user_delete,omitempty"`
}

// CanCreate reports whether backups can be started for this configuration.
func (b *Backup) CanCreate() bool {
	if b == nil {
		return false
	}
	if b.Method == MethodTmux {
		return b.Tmux != nil
	}
	return b.Command != nil
}

// Action is an administrator-defined command module attached to a server.
// Users run it by ID; they can never change what it executes.
type Action struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Icon is an optional short symbol (for example an emoji) shown on the
	// action's button.
	Icon string `json:"icon,omitempty"`
	// Method is "command" (run Command) or "tmux" (type Tmux.Text into the
	// server's tmux pane; tmux backend only).
	Method  string    `json:"method"`
	Command *Command  `json:"command,omitempty"`
	Tmux    *TmuxText `json:"tmux,omitempty"`
	// WorkingDir overrides the server's working directory for commands.
	WorkingDir string `json:"working_dir,omitempty"`
	// Timeout bounds a command action (default 10m); it is killed after.
	Timeout Duration `json:"timeout,omitempty"`
	// Confirm asks for confirmation before the action runs.
	Confirm bool `json:"confirm,omitempty"`
	// Disabled hides the action from users and refuses to run it.
	Disabled bool `json:"disabled,omitempty"`
}

// Action returns the custom action with the given ID, or nil.
func (d *Definition) Action(id string) *Action {
	for i := range d.Actions {
		if d.Actions[i].ID == id {
			return &d.Actions[i]
		}
	}
	return nil
}

// ActionIDs lists the IDs of the custom actions.
func (d *Definition) ActionIDs() []string {
	out := make([]string, 0, len(d.Actions))
	for _, a := range d.Actions {
		out = append(out, a.ID)
	}
	return out
}

type Timeouts struct {
	Start   Duration `json:"start,omitempty"`
	Stop    Duration `json:"stop,omitempty"`
	Backup  Duration `json:"backup,omitempty"`
	Command Duration `json:"command,omitempty"`
}

// Definition is a complete server definition.
type Definition struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Category    string            `json:"category,omitempty"`
	Backend     string            `json:"backend"`
	WorkingDir  string            `json:"working_dir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`

	Custom  *Custom  `json:"custom,omitempty"`
	Systemd *Systemd `json:"systemd,omitempty"`
	Docker  *Docker  `json:"docker,omitempty"`
	Tmux    *Tmux    `json:"tmux,omitempty"`

	Readiness Readiness `json:"readiness"`
	Console   Console   `json:"console"`
	Backup    *Backup   `json:"backup,omitempty"`
	// Actions are custom command modules users can be allowed to run.
	Actions  []Action `json:"actions,omitempty"`
	Timeouts Timeouts `json:"timeouts"`
	// Cooldown overrides rate_limits.server_cooldown for this server.
	Cooldown *Duration `json:"cooldown,omitempty"`
	// StatusPollInterval overrides monitoring.status_poll_interval.
	StatusPollInterval Duration `json:"status_poll_interval,omitempty"`
}

// Clone returns a deep copy.
func (d *Definition) Clone() *Definition {
	b, _ := json.Marshal(d)
	out := &Definition{}
	_ = json.Unmarshal(b, out)
	return out
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func (c *Custom) KillAfterTimeout() bool { return boolOr(c.KillOnTimeout, true) }
func (t *Tmux) KillAfterTimeout() bool   { return boolOr(t.KillOnTimeout, true) }
func (s *Systemd) RestartAllowed() bool  { return boolOr(s.AllowRestart, true) }
func (d *Docker) RestartAllowed() bool   { return boolOr(d.AllowRestart, true) }

// InputMethod returns the effective console input method.
func (d *Definition) InputMethod() string {
	if d.Console.Input != "" {
		return d.Console.Input
	}
	if d.Backend == BackendTmux {
		return InputTmux
	}
	return InputNone
}

var (
	idRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	envKeyRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	tmuxNameRe  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$`)
	tmuxPaneRe  = regexp.MustCompile(`^[0-9]{1,4}$`)
	tmuxKeyRe   = regexp.MustCompile(`^(?:[CMS]-){0,3}(?:[A-Za-z0-9]|Enter|Escape|Space|Tab|BSpace|Up|Down|Left|Right|Home|End|F[0-9]{1,2})$`)
	unitRe      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9:_.@\\-]{0,200}\.service$`)
	containerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	hexRe       = regexp.MustCompile(`^(?:[0-9a-fA-F]{2})+$`)
	signals     = map[string]bool{"SIGTERM": true, "SIGINT": true, "SIGQUIT": true, "SIGHUP": true, "SIGUSR1": true, "SIGUSR2": true, "SIGKILL": true}
)

// ValidID reports whether id is a well-formed server identifier.
func ValidID(id string) bool { return idRe.MatchString(id) }

// ValidationError lists every problem in a definition, keyed by field path.
type ValidationError struct {
	Problems []Problem
}

type Problem struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.Field + ": " + p.Message
	}
	return "invalid server definition: " + strings.Join(parts, "; ")
}

type validator struct{ problems []Problem }

func (v *validator) add(field, format string, a ...any) {
	v.problems = append(v.problems, Problem{Field: field, Message: fmt.Sprintf(format, a...)})
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func (v *validator) text(field, s string, max int, required bool) {
	if required && strings.TrimSpace(s) == "" {
		v.add(field, "is required")
		return
	}
	if len(s) > max {
		v.add(field, "must be at most %d bytes", max)
	}
	if hasControl(s) {
		v.add(field, "must not contain control characters")
	}
}

func (v *validator) absPath(field, p string, required bool) {
	if p == "" {
		if required {
			v.add(field, "is required")
		}
		return
	}
	if strings.ContainsRune(p, 0) || hasControl(p) {
		v.add(field, "must not contain control characters")
		return
	}
	if !filepath.IsAbs(p) {
		v.add(field, "must be an absolute path")
	}
	if len(p) > 4096 {
		v.add(field, "is too long")
	}
}

// command validates an executable reference: either an absolute path or a
// bare program name resolved through PATH. Relative paths are rejected so
// the result never depends on the working directory.
func (v *validator) command(field string, c *Command) {
	v.executable(field+".command", c.Command)
	v.args(field+".args", c.Args)
}

func (v *validator) executable(field, cmd string) {
	switch {
	case cmd == "":
		v.add(field, "is required")
	case strings.ContainsRune(cmd, 0) || hasControl(cmd):
		v.add(field, "must not contain control characters")
	case len(cmd) > 4096:
		v.add(field, "is too long")
	case strings.Contains(cmd, "/") && !filepath.IsAbs(cmd):
		v.add(field, "must be an absolute path or a bare program name found in PATH")
	case strings.HasPrefix(cmd, "-"):
		v.add(field, "must not start with '-'")
	}
}

func (v *validator) args(field string, args []string) {
	if len(args) > 256 {
		v.add(field, "must have at most 256 entries")
	}
	for i, a := range args {
		if strings.ContainsRune(a, 0) {
			v.add(fmt.Sprintf("%s[%d]", field, i), "must not contain NUL bytes")
		}
		if len(a) > 8192 {
			v.add(fmt.Sprintf("%s[%d]", field, i), "is too long")
		}
	}
}

// tmuxText validates predefined text for a tmux pane: one line without
// control characters (send-keys -l types it literally; a control character
// could otherwise act as a key press).
func (v *validator) tmuxText(field string, t *TmuxText) {
	switch {
	case strings.TrimSpace(t.Text) == "":
		v.add(field, "is required")
	case len(t.Text) > 1024:
		v.add(field, "must be at most 1024 bytes")
	case hasControl(t.Text):
		v.add(field, "must be a single line without control characters")
	}
}

func (v *validator) consoleMatch(field string, cm *ConsoleMatch) {
	if cm.Pattern == "" {
		v.add(field, "is required")
		return
	}
	if len(cm.Pattern) > 1000 {
		v.add(field, "is too long")
	}
	if cm.Regex {
		if _, err := regexp.Compile(cm.Pattern); err != nil {
			v.add(field, "is not a valid regular expression: %v", err)
		}
	}
}

func (d *Definition) validateActions(v *validator) {
	if len(d.Actions) > MaxActions {
		v.add("actions", "must have at most %d entries", MaxActions)
	}
	seen := map[string]bool{}
	for i := range d.Actions {
		a := &d.Actions[i]
		f := fmt.Sprintf("actions[%d]", i)
		if !idRe.MatchString(a.ID) {
			v.add(f+".id", "must be 1-63 characters of a-z, 0-9, '_' or '-' and start with a letter or digit")
		} else if seen[a.ID] {
			v.add(f+".id", "%q is used by more than one action", a.ID)
		}
		seen[a.ID] = true
		v.text(f+".name", a.Name, 100, true)
		v.text(f+".description", a.Description, 500, false)
		v.text(f+".icon", a.Icon, 16, false)
		switch a.Method {
		case MethodCommand:
			if a.Command == nil {
				v.add(f+".command.command", "is required")
			} else {
				v.command(f+".command", a.Command)
			}
			v.absPath(f+".working_dir", a.WorkingDir, false)
			v.duration(f+".timeout", a.Timeout, time.Second, 24*time.Hour)
		case MethodTmux:
			if d.Backend != BackendTmux {
				v.add(f+".method", "tmux is only available for servers that use the tmux backend")
			}
			if a.Tmux == nil {
				v.add(f+".tmux.text", "is required")
			} else {
				v.tmuxText(f+".tmux.text", a.Tmux)
			}
		default:
			v.add(f+".method", "must be command or tmux")
		}
	}
}

func (v *validator) duration(field string, d Duration, min, max time.Duration) {
	if d.D() < min || d.D() > max {
		v.add(field, "must be between %s and %s", min, max)
	}
}

// Normalize fills in defaults and cleans paths. It must be called before
// Validate.
func (d *Definition) Normalize() {
	d.ID = strings.TrimSpace(d.ID)
	d.Name = strings.TrimSpace(d.Name)
	d.Description = strings.TrimSpace(d.Description)
	d.Category = strings.TrimSpace(d.Category)
	clean := func(p *string) {
		if *p != "" && filepath.IsAbs(*p) {
			*p = filepath.Clean(*p)
		}
	}
	clean(&d.WorkingDir)
	if d.Backend != BackendCustom {
		d.Custom = nil
	}
	if d.Backend != BackendSystemd {
		d.Systemd = nil
	}
	if d.Backend != BackendDocker {
		d.Docker = nil
	}
	if d.Backend != BackendTmux {
		d.Tmux = nil
	}
	if c := d.Custom; c != nil {
		if c.Mode == "" {
			c.Mode = ModeForeground
		}
		if c.Stop != nil && c.Stop.Command == "" {
			c.Stop = nil
		}
		c.StopSignal = strings.ToUpper(c.StopSignal)
		if c.StopSignal == "" {
			c.StopSignal = "SIGTERM"
		}
		if c.OnManagerExit == "" {
			c.OnManagerExit = "keep"
		}
		if d.Console.Input == InputPTY {
			// A PTY cannot outlive the manager process.
			c.OnManagerExit = "stop"
		}
		clean(&c.PIDFile)
		clean(&c.LogFile)
	}
	if s := d.Systemd; s != nil {
		if s.Scope == "" {
			s.Scope = "system"
		}
		if s.Unit != "" && !strings.Contains(s.Unit, ".") {
			s.Unit += ".service"
		}
	}
	if t := d.Tmux; t != nil {
		if t.Start != nil && t.Start.Command == "" {
			t.Start = nil
		}
		if t.Width == 0 {
			t.Width = 200
		}
		if t.Height == 0 {
			t.Height = 50
		}
		clean(&t.SocketPath)
	}
	r := &d.Readiness
	if r.Method == "" {
		r.Method = ReadyNone
	}
	if r.Method != ReadyConsole {
		r.Console = nil
	}
	if r.Method != ReadyPort {
		r.Port = nil
	}
	if r.Method != ReadyScript {
		r.Script = nil
	}
	if r.Interval == 0 {
		r.Interval = Duration(2 * time.Second)
	}
	if p := r.Port; p != nil {
		p.Protocol = strings.ToLower(p.Protocol)
		if p.Protocol == "" {
			p.Protocol = "tcp"
		}
		if p.Host == "" {
			p.Host = "127.0.0.1"
		}
		if p.Timeout == 0 {
			p.Timeout = Duration(2 * time.Second)
		}
	}
	if s := r.Script; s != nil && s.Timeout == 0 {
		s.Timeout = Duration(10 * time.Second)
	}
	if d.Console.Input != InputCommand && d.Console.Input != InputDockerExec {
		d.Console.InputCommand = nil
	}
	if b := d.Backup; b != nil {
		clean(&b.Directory)
		if b.Method == "" {
			b.Method = MethodCommand
		}
		if b.Command != nil && b.Command.Command == "" {
			b.Command = nil
		}
		if b.Tmux != nil && b.Tmux.Text == "" {
			b.Tmux = nil
		}
		if b.Completion != nil && b.Completion.Pattern == "" {
			b.Completion = nil
		}
		// Only discard what a known method ignores: a misspelt method must
		// fail validation rather than silently drop the configuration.
		switch b.Method {
		case MethodTmux:
			b.Command = nil
		case MethodCommand:
			b.Tmux, b.Completion = nil, nil
		}
		if b.Directory == "" && b.Command == nil && b.Tmux == nil {
			d.Backup = nil
		}
	}
	for i := range d.Actions {
		a := &d.Actions[i]
		a.ID = strings.TrimSpace(a.ID)
		a.Name = strings.TrimSpace(a.Name)
		a.Description = strings.TrimSpace(a.Description)
		a.Icon = strings.TrimSpace(a.Icon)
		if a.Method == "" {
			a.Method = MethodCommand
		}
		clean(&a.WorkingDir)
		switch a.Method {
		case MethodTmux:
			a.Command, a.WorkingDir, a.Timeout = nil, "", 0
		case MethodCommand:
			a.Tmux = nil
			if a.Timeout == 0 {
				a.Timeout = Duration(DefaultActionTimeout)
			}
		}
	}
	if len(d.Actions) == 0 {
		d.Actions = nil
	}
	t := &d.Timeouts
	if t.Start == 0 {
		t.Start = Duration(120 * time.Second)
	}
	if t.Stop == 0 {
		t.Stop = Duration(60 * time.Second)
	}
	if t.Backup == 0 {
		t.Backup = Duration(time.Hour)
	}
	if t.Command == 0 {
		t.Command = Duration(15 * time.Second)
	}
	if len(d.Env) == 0 {
		d.Env = nil
	}
}

// Validate checks a normalized definition. It returns *ValidationError or nil.
func (d *Definition) Validate() error {
	v := &validator{}
	if !idRe.MatchString(d.ID) {
		v.add("id", "must be 1-63 characters of a-z, 0-9, '_' or '-' and start with a letter or digit")
	}
	v.text("name", d.Name, 100, true)
	v.text("description", d.Description, 1000, false)
	v.text("category", d.Category, 60, false)
	v.absPath("working_dir", d.WorkingDir, false)
	if len(d.Env) > 256 {
		v.add("env", "must have at most 256 entries")
	}
	for k, val := range d.Env {
		if !envKeyRe.MatchString(k) {
			v.add("env."+k, "is not a valid environment variable name")
		}
		if strings.ContainsRune(val, 0) || len(val) > 32768 {
			v.add("env."+k, "value is invalid or too long")
		}
	}

	switch d.Backend {
	case BackendCustom:
		d.validateCustom(v)
	case BackendSystemd:
		d.validateSystemd(v)
	case BackendDocker:
		d.validateDocker(v)
	case BackendTmux:
		d.validateTmux(v)
	default:
		v.add("backend", "must be one of custom, systemd, docker, tmux")
	}

	d.validateReadiness(v)
	d.validateConsole(v)

	if b := d.Backup; b != nil {
		v.absPath("backup.directory", b.Directory, b.Method != MethodTmux)
		if b.Directory == "/" {
			v.add("backup.directory", "must not be the filesystem root")
		}
		switch b.Method {
		case MethodCommand:
			if b.Command != nil {
				v.command("backup.command", b.Command)
			}
		case MethodTmux:
			if d.Backend != BackendTmux {
				v.add("backup.method", "tmux is only available for servers that use the tmux backend")
			}
			if b.Tmux == nil {
				v.add("backup.tmux.text", "is required for the tmux backup method")
			} else {
				v.tmuxText("backup.tmux.text", b.Tmux)
			}
			if b.Completion != nil {
				v.consoleMatch("backup.completion.pattern", b.Completion)
			}
		default:
			v.add("backup.method", "must be command or tmux")
		}
	}
	d.validateActions(v)
	v.duration("timeouts.start", d.Timeouts.Start, time.Second, 24*time.Hour)
	v.duration("timeouts.stop", d.Timeouts.Stop, time.Second, 24*time.Hour)
	v.duration("timeouts.backup", d.Timeouts.Backup, time.Second, 7*24*time.Hour)
	v.duration("timeouts.command", d.Timeouts.Command, time.Second, 10*time.Minute)
	if d.Cooldown != nil {
		v.duration("cooldown", *d.Cooldown, 0, time.Hour)
	}
	if d.StatusPollInterval != 0 {
		v.duration("status_poll_interval", d.StatusPollInterval, time.Second, 10*time.Minute)
	}
	if len(v.problems) > 0 {
		return &ValidationError{Problems: v.problems}
	}
	return nil
}

func (d *Definition) validateCustom(v *validator) {
	c := d.Custom
	if c == nil {
		v.add("custom", "is required for the custom backend")
		return
	}
	if c.Mode != ModeForeground && c.Mode != ModeForking {
		v.add("custom.mode", "must be foreground or forking")
	}
	v.command("custom.start", &c.Start)
	if c.Stop != nil {
		v.command("custom.stop", c.Stop)
	}
	v.text("custom.stop_input", c.StopInput, 512, false)
	if !signals[c.StopSignal] {
		v.add("custom.stop_signal", "must be one of SIGTERM, SIGINT, SIGQUIT, SIGHUP, SIGUSR1, SIGUSR2, SIGKILL")
	}
	v.absPath("custom.pid_file", c.PIDFile, false)
	v.absPath("custom.log_file", c.LogFile, false)
	if c.OnManagerExit != "keep" && c.OnManagerExit != "stop" {
		v.add("custom.on_manager_exit", "must be keep or stop")
	}
	if c.Mode == ModeForking {
		// A forking server needs something that tells us whether it is alive.
		ext := d.Readiness.Method == ReadyPort || d.Readiness.Method == ReadyScript
		if c.PIDFile == "" && !ext {
			v.add("custom.mode", "forking servers need custom.pid_file or a port/script readiness check to detect whether they are running")
		}
		if c.Stop == nil && c.PIDFile == "" {
			v.add("custom.stop", "forking servers need a stop command (or custom.pid_file so a signal can be sent)")
		}
		if c.StopInput != "" {
			v.add("custom.stop_input", "is only available in foreground mode")
		}
	}
	if c.StopInput != "" {
		in := d.InputMethod()
		if in != InputStdin && in != InputPTY {
			v.add("custom.stop_input", "requires console.input to be stdin or pty")
		}
	}
}

func (d *Definition) validateSystemd(v *validator) {
	s := d.Systemd
	if s == nil {
		v.add("systemd", "is required for the systemd backend")
		return
	}
	if !unitRe.MatchString(s.Unit) {
		v.add("systemd.unit", "must be a valid .service unit name")
	}
	if s.Scope != "system" && s.Scope != "user" {
		v.add("systemd.scope", "must be system or user")
	}
	if s.UseSudo && s.Scope == "user" {
		v.add("systemd.use_sudo", "cannot be combined with user scope")
	}
}

func (d *Definition) validateDocker(v *validator) {
	c := d.Docker
	if c == nil {
		v.add("docker", "is required for the docker backend")
		return
	}
	if !containerRe.MatchString(c.Container) {
		v.add("docker.container", "must be a container name or ID (letters, digits, '_', '.', '-')")
	}
}

func (d *Definition) validateTmux(v *validator) {
	t := d.Tmux
	if t == nil {
		v.add("tmux", "is required for the tmux backend")
		return
	}
	if !tmuxNameRe.MatchString(t.Session) {
		v.add("tmux.session", "must be 1-64 characters of letters, digits, '_' or '-'")
	}
	if t.Window != "" && !tmuxNameRe.MatchString(t.Window) {
		v.add("tmux.window", "must be a window index or a name of letters, digits, '_' or '-'")
	}
	if t.Pane != "" && !tmuxPaneRe.MatchString(t.Pane) {
		v.add("tmux.pane", "must be a pane index")
	}
	if t.SocketName != "" && !tmuxNameRe.MatchString(t.SocketName) {
		v.add("tmux.socket_name", "must be letters, digits, '_' or '-'")
	}
	v.absPath("tmux.socket_path", t.SocketPath, false)
	if t.SocketName != "" && t.SocketPath != "" {
		v.add("tmux.socket_path", "cannot be combined with tmux.socket_name")
	}
	if t.Start != nil {
		v.command("tmux.start", t.Start)
	}
	v.text("tmux.stop_text", t.StopText, 512, false)
	if len(t.StopKeys) > 16 {
		v.add("tmux.stop_keys", "must have at most 16 entries")
	}
	for i, k := range t.StopKeys {
		if !tmuxKeyRe.MatchString(k) {
			v.add(fmt.Sprintf("tmux.stop_keys[%d]", i), "is not an allowed key name (examples: C-c, Enter, q)")
		}
	}
	if t.StopText == "" && len(t.StopKeys) == 0 && !t.KillAfterTimeout() {
		v.add("tmux.kill_on_timeout", "cannot be false unless stop_text or stop_keys is configured (there would be no way to stop the server)")
	}
	if t.Width < 20 || t.Width > 1000 {
		v.add("tmux.width", "must be between 20 and 1000")
	}
	if t.Height < 5 || t.Height > 500 {
		v.add("tmux.height", "must be between 5 and 500")
	}
}

func (d *Definition) validateReadiness(v *validator) {
	r := d.Readiness
	v.duration("readiness.interval", r.Interval, 200*time.Millisecond, 10*time.Minute)
	switch r.Method {
	case ReadyNone:
	case ReadyConsole:
		if r.Console == nil {
			v.add("readiness.console.pattern", "is required")
			break
		}
		v.consoleMatch("readiness.console.pattern", r.Console)
	case ReadyPort:
		p := r.Port
		if p == nil {
			v.add("readiness.port", "is required")
			break
		}
		if p.Port < 1 || p.Port > 65535 {
			v.add("readiness.port.port", "must be between 1 and 65535")
		}
		if p.Host == "" || len(p.Host) > 253 || hasControl(p.Host) || strings.ContainsAny(p.Host, " /") {
			v.add("readiness.port.host", "must be a host name or IP address")
		} else if net.ParseIP(p.Host) == nil && strings.ContainsAny(p.Host, ":[]") {
			v.add("readiness.port.host", "must be a host name or IP address without a port")
		}
		v.duration("readiness.port.timeout", p.Timeout, 100*time.Millisecond, time.Minute)
		switch p.Protocol {
		case "tcp":
		case "udp":
			if !hexRe.MatchString(p.UDPPayloadHex) || len(p.UDPPayloadHex) > 2048 {
				v.add("readiness.port.udp_payload_hex", "is required for UDP: a hex-encoded probe datagram the server answers (an open UDP port alone proves nothing)")
			}
		default:
			v.add("readiness.port.protocol", "must be tcp or udp")
		}
	case ReadyScript:
		s := r.Script
		if s == nil {
			v.add("readiness.script", "is required")
			break
		}
		v.executable("readiness.script.command", s.Command)
		v.args("readiness.script.args", s.Args)
		v.duration("readiness.script.timeout", s.Timeout, 100*time.Millisecond, 10*time.Minute)
	default:
		v.add("readiness.method", "must be none, console, port or script")
	}
}

func (d *Definition) validateConsole(v *validator) {
	in := d.InputMethod()
	bad := func() {
		v.add("console.input", "%q is not available for the %s backend in this configuration", in, d.Backend)
	}
	switch in {
	case InputNone:
	case InputStdin, InputPTY:
		if d.Backend != BackendCustom || d.Custom == nil || d.Custom.Mode != ModeForeground {
			bad()
		}
	case InputTmux:
		if d.Backend != BackendTmux {
			bad()
		}
	case InputDockerExec:
		if d.Backend != BackendDocker {
			bad()
		}
		fallthrough
	case InputCommand:
		c := d.Console.InputCommand
		if c == nil || c.Command == "" {
			v.add("console.input_command", "is required for this input method")
			break
		}
		if in == InputDockerExec {
			// Runs inside the container, so host path rules do not apply.
			if strings.ContainsRune(c.Command, 0) || hasControl(c.Command) || strings.HasPrefix(c.Command, "-") {
				v.add("console.input_command.command", "is invalid")
			}
			v.args("console.input_command.args", c.Args)
		} else {
			v.command("console.input_command", c)
		}
	default:
		v.add("console.input", "must be none, stdin, pty, tmux, command or docker_exec")
	}
}

// ExecutionEqual reports whether two definitions are identical in every
// field that influences what gets executed on the host (custom actions
// included). Fields outside this set (name, description, category,
// timeouts, console readiness pattern, polling intervals, cooldown) are the
// "safe" ones a CONFIG holder may edit - except category, which decides the
// concurrency limit a server falls under and is therefore reserved for
// administrators separately (see the API layer).
func ExecutionEqual(a, b *Definition) bool {
	strip := func(d *Definition) *Definition {
		c := d.Clone()
		c.Name, c.Description, c.Category = "", "", ""
		c.Timeouts = Timeouts{}
		c.Cooldown = nil
		c.StatusPollInterval = 0
		c.Readiness.Interval = 0
		if c.Readiness.Method == ReadyConsole {
			c.Readiness.Console = nil
		}
		return c
	}
	x, _ := json.Marshal(strip(a))
	y, _ := json.Marshal(strip(b))
	return string(x) == string(y)
}

// RuntimeEqual reports whether two definitions control the server process
// identically. It ignores the backup configuration and the custom actions,
// which are executed on demand and never touch the running backend, so they
// can be changed while the server is running.
func RuntimeEqual(a, b *Definition) bool {
	x, y := a.Clone(), b.Clone()
	x.Backup, y.Backup = nil, nil
	x.Actions, y.Actions = nil, nil
	return ExecutionEqual(x, y)
}

// WithoutActions returns a copy without custom actions. Custom actions are
// managed by administrators only; other readers of a configuration do not
// see their commands.
func (d *Definition) WithoutActions() *Definition {
	c := d.Clone()
	c.Actions = nil
	return c
}

// MaskEnv returns a copy with every environment value hidden.
func (d *Definition) MaskEnv() *Definition {
	c := d.Clone()
	for k := range c.Env {
		c.Env[k] = EnvMask
	}
	return c
}

// RestoreMaskedEnv replaces masked values in d with the values from old.
func (d *Definition) RestoreMaskedEnv(old *Definition) {
	for k, v := range d.Env {
		if v == EnvMask {
			if old != nil {
				if cur, ok := old.Env[k]; ok {
					d.Env[k] = cur
					continue
				}
			}
			delete(d.Env, k)
		}
	}
}

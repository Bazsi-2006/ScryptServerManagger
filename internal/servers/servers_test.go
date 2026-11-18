package servers

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func custom() *Definition {
	return &Definition{ID: "mc", Name: "Minecraft", Backend: BackendCustom,
		Custom: &Custom{Start: Command{Command: "/opt/mc/start.sh", Args: []string{"--port", "25565"}}}}
}

func problems(t *testing.T, d *Definition) map[string]string {
	t.Helper()
	d.Normalize()
	err := d.Validate()
	out := map[string]string{}
	if err == nil {
		return out
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("unexpected error type %T", err)
	}
	for _, p := range ve.Problems {
		out[p.Field] = p.Message
	}
	return out
}

func TestMinimalDefinitionsOfEveryBackendAreValid(t *testing.T) {
	defs := []*Definition{
		custom(),
		{ID: "web", Name: "Web", Backend: BackendSystemd, Systemd: &Systemd{Unit: "nginx"}},
		{ID: "db", Name: "DB", Backend: BackendDocker, Docker: &Docker{Container: "postgres_1"}},
		{ID: "ts", Name: "TS", Backend: BackendTmux, Tmux: &Tmux{Session: "ts3"}},
	}
	for _, d := range defs {
		if p := problems(t, d); len(p) != 0 {
			t.Errorf("%s: unexpected problems %v", d.ID, p)
		}
	}
	// Normalisation fills defaults.
	if defs[1].Systemd.Unit != "nginx.service" || defs[1].Systemd.Scope != "system" {
		t.Errorf("systemd defaults: %+v", defs[1].Systemd)
	}
	if defs[0].Custom.Mode != ModeForeground || defs[0].Custom.StopSignal != "SIGTERM" || defs[0].Timeouts.Start == 0 {
		t.Errorf("custom defaults: %+v", defs[0].Custom)
	}
	if defs[3].InputMethod() != InputTmux || defs[0].InputMethod() != InputNone {
		t.Error("default console input methods")
	}
}

func TestIdentifierValidation(t *testing.T) {
	for _, id := range []string{"", "UPPER", "has space", "../etc", "a/b", "-lead", "x;rm", strings.Repeat("a", 64), "dot.name"} {
		d := custom()
		d.ID = id
		if _, bad := problems(t, d)["id"]; !bad {
			t.Errorf("id %q should be rejected", id)
		}
	}
	for _, name := range []string{"a;b", "$(x)", "a b", "a:b", "a.b", "-x", ""} {
		d := &Definition{ID: "t", Name: "T", Backend: BackendTmux, Tmux: &Tmux{Session: name}}
		if _, bad := problems(t, d)["tmux.session"]; !bad {
			t.Errorf("tmux session %q should be rejected", name)
		}
	}
	for _, unit := range []string{"a b.service", "x.timer", "-x.service", "a;b.service", "../x.service", "$(id).service"} {
		d := &Definition{ID: "s", Name: "S", Backend: BackendSystemd, Systemd: &Systemd{Unit: unit}}
		if _, bad := problems(t, d)["systemd.unit"]; !bad {
			t.Errorf("unit %q should be rejected", unit)
		}
	}
	if p := problems(t, &Definition{ID: "s", Name: "S", Backend: BackendSystemd, Systemd: &Systemd{Unit: "app@instance-1.service"}}); len(p) != 0 {
		t.Errorf("template instance unit should be valid: %v", p)
	}
	for _, c := range []string{"", "a b", "-rm", "a/b", "x;y", "$(id)"} {
		d := &Definition{ID: "d", Name: "D", Backend: BackendDocker, Docker: &Docker{Container: c}}
		if _, bad := problems(t, d)["docker.container"]; !bad {
			t.Errorf("container %q should be rejected", c)
		}
	}
}

func TestExecutableAndPathValidation(t *testing.T) {
	d := custom()
	d.Custom.Start.Command = "./relative/start.sh"
	if _, bad := problems(t, d)["custom.start.command"]; !bad {
		t.Error("relative paths must be rejected")
	}
	d = custom()
	d.Custom.Start.Command = "java" // bare name resolved through PATH is fine
	if p := problems(t, d); len(p) != 0 {
		t.Errorf("bare program name: %v", p)
	}
	d = custom()
	d.Custom.Start.Command = "-oProxyCommand=evil"
	if _, bad := problems(t, d)["custom.start.command"]; !bad {
		t.Error("a command starting with '-' must be rejected")
	}
	d = custom()
	d.Custom.Start.Args = []string{"ok", "nul\x00byte"}
	if _, bad := problems(t, d)["custom.start.args[1]"]; !bad {
		t.Error("NUL bytes in arguments must be rejected")
	}
	d = custom()
	d.WorkingDir = "relative/dir"
	d.Backup = &Backup{Directory: "/"}
	p := problems(t, d)
	if p["working_dir"] == "" || p["backup.directory"] == "" {
		t.Errorf("path checks: %v", p)
	}
	d = custom()
	d.Env = map[string]string{"GOOD_1": "v", "bad-name": "v", "1LEAD": "v"}
	p = problems(t, d)
	if p["env.bad-name"] == "" || p["env.1LEAD"] == "" || p["env.GOOD_1"] != "" {
		t.Errorf("env names: %v", p)
	}
	// Paths are cleaned, so traversal sequences do not survive.
	d = custom()
	d.WorkingDir = "/opt/mc/../mc/./data/"
	d.Normalize()
	if d.WorkingDir != "/opt/mc/data" {
		t.Errorf("working dir not cleaned: %s", d.WorkingDir)
	}
}

func TestForkingRequiresLifecycleSource(t *testing.T) {
	d := custom()
	d.Custom.Mode = ModeForking
	p := problems(t, d)
	if p["custom.mode"] == "" || p["custom.stop"] == "" {
		t.Errorf("forking without pid file/check/stop should fail: %v", p)
	}
	d = custom()
	d.Custom.Mode = ModeForking
	d.Custom.PIDFile = "/run/mc.pid"
	if p := problems(t, d); len(p) != 0 {
		t.Errorf("pid file is enough: %v", p)
	}
	d = custom()
	d.Custom.Mode = ModeForking
	d.Custom.Stop = &Command{Command: "/opt/mc/stop.sh"}
	d.Readiness = Readiness{Method: ReadyScript, Script: &ScriptCheck{Command: "/opt/mc/status.sh"}}
	if p := problems(t, d); len(p) != 0 {
		t.Errorf("status script + stop command is enough: %v", p)
	}
}

func TestReadinessValidation(t *testing.T) {
	d := custom()
	d.Readiness = Readiness{Method: ReadyConsole, Console: &ConsoleMatch{Pattern: "Done (", Regex: true}}
	if problems(t, d)["readiness.console.pattern"] == "" {
		t.Error("an invalid regular expression must be rejected")
	}
	d.Readiness = Readiness{Method: ReadyConsole, Console: &ConsoleMatch{Pattern: "Done ("}}
	if p := problems(t, d); len(p) != 0 {
		t.Errorf("literal text with regex characters is fine: %v", p)
	}
	d.Readiness = Readiness{Method: ReadyPort, Port: &PortCheck{Port: 70000}}
	if problems(t, d)["readiness.port.port"] == "" {
		t.Error("port out of range")
	}
	d.Readiness = Readiness{Method: ReadyPort, Port: &PortCheck{Port: 27015, Protocol: "UDP"}}
	if problems(t, d)["readiness.port.udp_payload_hex"] == "" {
		t.Error("UDP requires an explicit probe payload")
	}
	d.Readiness = Readiness{Method: ReadyPort, Port: &PortCheck{Port: 27015, Protocol: "udp", UDPPayloadHex: "ffffffff54"}}
	if p := problems(t, d); len(p) != 0 {
		t.Errorf("UDP with payload: %v", p)
	}
	d.Readiness = Readiness{Method: ReadyPort, Port: &PortCheck{Port: 80, Host: "host:81"}}
	if problems(t, d)["readiness.port.host"] == "" {
		t.Error("host with embedded port must be rejected")
	}
	d.Readiness = Readiness{Method: "telepathy"}
	if problems(t, d)["readiness.method"] == "" {
		t.Error("unknown method")
	}
}

func TestConsoleInputRules(t *testing.T) {
	d := custom()
	d.Console.Input = InputTmux
	if problems(t, d)["console.input"] == "" {
		t.Error("tmux input on a custom server must be rejected")
	}
	d = custom()
	d.Console.Input = InputPTY
	d.Normalize()
	if d.Custom.OnManagerExit != "stop" {
		t.Error("PTY servers cannot outlive the manager and must be stopped on exit")
	}
	d = custom()
	d.Custom.StopInput = "stop"
	if problems(t, d)["custom.stop_input"] == "" {
		t.Error("stop_input without stdin/pty input must be rejected")
	}
	d = &Definition{ID: "s", Name: "S", Backend: BackendSystemd, Systemd: &Systemd{Unit: "x.service"}, Console: Console{Input: InputStdin}}
	if problems(t, d)["console.input"] == "" {
		t.Error("stdin input on systemd must be rejected")
	}
	d.Console = Console{Input: InputCommand}
	if problems(t, d)["console.input_command"] == "" {
		t.Error("command input needs a helper")
	}
	d.Console = Console{Input: InputCommand, InputCommand: &Command{Command: "/usr/local/bin/rcon", Args: []string{CommandPlaceholder}}}
	if p := problems(t, d); len(p) != 0 {
		t.Errorf("helper command input: %v", p)
	}
	tm := &Definition{ID: "t", Name: "T", Backend: BackendTmux, Tmux: &Tmux{Session: "x", StopKeys: []string{"C-c", "Enter", "rm -rf"}}}
	if problems(t, tm)["tmux.stop_keys[2]"] == "" {
		t.Error("arbitrary text must not be accepted as a tmux key name")
	}
	no := false
	tm = &Definition{ID: "t", Name: "T", Backend: BackendTmux, Tmux: &Tmux{Session: "x", KillOnTimeout: &no}}
	if problems(t, tm)["tmux.kill_on_timeout"] == "" {
		t.Error("a tmux server must have some way to be stopped")
	}
}

func TestExecutionEqualSeparatesSafeFields(t *testing.T) {
	a := custom()
	a.Readiness = Readiness{Method: ReadyConsole, Console: &ConsoleMatch{Pattern: "Done"}}
	a.Normalize()
	b := a.Clone()
	b.Name, b.Description, b.Category = "Renamed", "desc", "Games"
	b.Timeouts.Start = Duration(5e9)
	b.Readiness.Console.Pattern = "Ready"
	if !ExecutionEqual(a, b) {
		t.Error("presentation, timeouts and the console pattern are safe fields")
	}
	mutations := map[string]func(*Definition){
		"start command":  func(d *Definition) { d.Custom.Start.Command = "/bin/sh" },
		"arguments":      func(d *Definition) { d.Custom.Start.Args = append(d.Custom.Start.Args, "-c", "id") },
		"stop command":   func(d *Definition) { d.Custom.Stop = &Command{Command: "/bin/x"} },
		"environment":    func(d *Definition) { d.Env = map[string]string{"LD_PRELOAD": "/tmp/x.so"} },
		"working dir":    func(d *Definition) { d.WorkingDir = "/tmp" },
		"backend":        func(d *Definition) { d.Backend = BackendTmux; d.Tmux = &Tmux{Session: "x"} },
		"backup command": func(d *Definition) { d.Backup = &Backup{Directory: "/b", Command: &Command{Command: "/bin/x"}} },
		"backup dir":     func(d *Definition) { d.Backup = &Backup{Directory: "/etc"} },
		"readiness kind": func(d *Definition) {
			d.Readiness = Readiness{Method: ReadyScript, Script: &ScriptCheck{Command: "/bin/x"}}
		},
		"console input": func(d *Definition) {
			d.Console = Console{Input: InputCommand, InputCommand: &Command{Command: "/bin/x"}}
		},
		"log file": func(d *Definition) { d.Custom.LogFile = "/etc/shadow" },
	}
	for name, mutate := range mutations {
		c := a.Clone()
		mutate(c)
		if ExecutionEqual(a, c) {
			t.Errorf("changing the %s must count as an execution change", name)
		}
	}
}

func TestEnvMasking(t *testing.T) {
	d := custom()
	d.Env = map[string]string{"TOKEN": "s3cret", "MODE": "prod"}
	masked := d.MaskEnv()
	data, _ := json.Marshal(masked)
	if strings.Contains(string(data), "s3cret") || masked.Env["TOKEN"] != EnvMask {
		t.Fatal("masked definition leaks values")
	}
	if d.Env["TOKEN"] != "s3cret" {
		t.Fatal("masking must not modify the original")
	}
	// Client sends back: one untouched, one changed, one new, one masked but unknown.
	edit := &Definition{Env: map[string]string{"TOKEN": EnvMask, "MODE": "dev", "NEW": "v", "GHOST": EnvMask}}
	edit.RestoreMaskedEnv(d)
	if edit.Env["TOKEN"] != "s3cret" || edit.Env["MODE"] != "dev" || edit.Env["NEW"] != "v" {
		t.Fatalf("restore: %v", edit.Env)
	}
	if _, ok := edit.Env["GHOST"]; ok {
		t.Fatal("a masked value with no original must be dropped, not stored as asterisks")
	}
}

func TestStorePersistsOneFilePerServer(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Create(custom())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mc.json")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("definition file: %v mode %v", err, st.Mode().Perm())
	}
	if _, err := s.Create(custom()); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate id: %v", err)
	}
	// Returned copies cannot corrupt stored state.
	saved.Name = "mutated"
	if got, _ := s.Get("mc"); got.Name != "Minecraft" {
		t.Fatal("store must hand out copies")
	}
	upd := custom()
	upd.Name = "Renamed"
	if _, err := s.Update(upd); err != nil {
		t.Fatal(err)
	}
	bad := custom()
	bad.Custom.Start.Command = ""
	if _, err := s.Update(bad); err == nil {
		t.Fatal("invalid update accepted")
	}
	// A fresh store reads the same thing back from disk.
	again := NewStore(dir)
	again.Load()
	if got, err := again.Get("mc"); err != nil || got.Name != "Renamed" || got.Custom.Start.Args[1] != "25565" {
		t.Fatalf("reload: %+v %v", got, err)
	}
	if err := s.Delete("mc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("definition file should be removed")
	}
	if err := s.Delete("mc"); !errors.Is(err, ErrNotFound) {
		t.Fatal("double delete")
	}
}

func TestStoreSkipsInvalidFilesAndReportsThem(t *testing.T) {
	dir := t.TempDir()
	good, _ := json.Marshal(custom())
	os.WriteFile(filepath.Join(dir, "mc.json"), good, 0o600)
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600)
	os.WriteFile(filepath.Join(dir, "wrongname.json"), good, 0o600) // id inside is "mc"
	os.WriteFile(filepath.Join(dir, "typo.json"), []byte(`{"id":"typo","name":"T","backend":"custom","custom":{"start":{"command":"/x"}},"startt":1}`), 0o600)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o600)
	s := NewStore(dir)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 1 {
		t.Fatalf("valid definitions: %d", len(s.List()))
	}
	inv := s.Invalid()
	for _, name := range []string{"broken.json", "wrongname.json", "typo.json"} {
		if inv[name] == "" {
			t.Errorf("%s should be reported as invalid", name)
		}
	}
	if _, ok := inv["notes.txt"]; ok {
		t.Error("non-JSON files are ignored, not reported")
	}
}

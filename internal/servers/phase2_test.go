package servers

import (
	"strings"
	"testing"
	"time"
)

func tmuxDef() *Definition {
	return &Definition{ID: "mc", Name: "Minecraft", Backend: BackendTmux, Tmux: &Tmux{Session: "mc"}}
}

func TestBackupMethods(t *testing.T) {
	// The command method is unchanged: directory required, command optional.
	d := custom()
	d.Backup = &Backup{Directory: "/srv/backups", Command: &Command{Command: "/opt/backup.sh"}}
	if p := problems(t, d); len(p) != 0 {
		t.Fatalf("command backup: %v", p)
	}
	if d.Backup.Method != MethodCommand || !d.Backup.CanCreate() {
		t.Fatalf("default method: %+v", d.Backup)
	}
	d = custom()
	d.Backup = &Backup{Command: &Command{Command: "/opt/backup.sh"}}
	if p := problems(t, d); p["backup.directory"] == "" {
		t.Fatal("the command method still needs a directory")
	}

	// tmux: text required, directory optional, Enter on by default.
	d = tmuxDef()
	d.Backup = &Backup{Method: MethodTmux, Tmux: &TmuxText{Text: "/backup start"}, Command: &Command{Command: "/ignored"}}
	if p := problems(t, d); len(p) != 0 {
		t.Fatalf("tmux backup: %v", p)
	}
	if d.Backup.Command != nil || !d.Backup.Tmux.PressEnter() || !d.Backup.CanCreate() {
		t.Fatalf("tmux normalisation: %+v", d.Backup)
	}
	no := false
	d.Backup.Tmux.Enter = &no
	if d.Backup.Tmux.PressEnter() {
		t.Fatal("enter=false must be honoured")
	}
	for name, mutate := range map[string]func(*Definition){
		"not a tmux server": func(d *Definition) {
			d.Backend, d.Tmux, d.Custom = BackendCustom, nil, custom().Custom
		},
		"no text":         func(d *Definition) { d.Backup.Tmux = &TmuxText{Text: "   "} },
		"control chars":   func(d *Definition) { d.Backup.Tmux = &TmuxText{Text: "save\x03now"} },
		"newline":         func(d *Definition) { d.Backup.Tmux = &TmuxText{Text: "a\nb"} },
		"too long":        func(d *Definition) { d.Backup.Tmux = &TmuxText{Text: strings.Repeat("x", 1025)} },
		"bad completion":  func(d *Definition) { d.Backup.Completion = &ConsoleMatch{Pattern: "(", Regex: true} },
		"unknown method":  func(d *Definition) { d.Backup.Method = "ftp" },
		"relative dir":    func(d *Definition) { d.Backup.Directory = "backups" },
		"filesystem root": func(d *Definition) { d.Backup.Directory = "/" },
	} {
		x := tmuxDef()
		x.Backup = &Backup{Method: MethodTmux, Tmux: &TmuxText{Text: "/backup start"}}
		mutate(x)
		if p := problems(t, x); len(p) == 0 {
			t.Errorf("%s: should be rejected", name)
		}
	}
	// An empty completion pattern means "no completion check".
	d = tmuxDef()
	d.Backup = &Backup{Method: MethodTmux, Tmux: &TmuxText{Text: "save"}, Completion: &ConsoleMatch{}}
	if p := problems(t, d); len(p) != 0 || d.Backup.Completion != nil {
		t.Fatalf("empty completion: %v %+v", p, d.Backup.Completion)
	}
	// Nothing configured at all removes the section.
	d = tmuxDef()
	d.Backup = &Backup{Method: MethodTmux}
	d.Normalize()
	if d.Backup != nil {
		t.Fatal("an empty backup section must disappear")
	}
}

func TestCustomActionValidation(t *testing.T) {
	d := tmuxDef()
	d.Actions = []Action{
		{ID: "save", Name: "Save World", Method: MethodTmux, Tmux: &TmuxText{Text: "save-all flush"}},
		{ID: "update", Name: "Update", Command: &Command{Command: "/opt/update.sh", Args: []string{"--force"}}, WorkingDir: "/opt/"},
	}
	if p := problems(t, d); len(p) != 0 {
		t.Fatalf("valid actions: %v", p)
	}
	up := d.Action("update")
	if up.Method != MethodCommand || up.Timeout.D() != DefaultActionTimeout || up.WorkingDir != "/opt" {
		t.Fatalf("command action defaults: %+v", up)
	}
	if save := d.Action("save"); save.Timeout != 0 || save.Command != nil || !save.Tmux.PressEnter() {
		t.Fatalf("tmux action normalisation: %+v", save)
	}
	if d.Action("missing") != nil || strings.Join(d.ActionIDs(), ",") != "save,update" {
		t.Fatal("lookup helpers")
	}

	cases := map[string]Action{
		"actions[1].id":               {ID: "save", Name: "Dup", Command: &Command{Command: "/x"}},
		"actions[1].name":             {ID: "noname", Command: &Command{Command: "/x"}},
		"actions[1].command.command":  {ID: "nocmd", Name: "No command"},
		"actions[1].method":           {ID: "m", Name: "M", Method: "shell", Command: &Command{Command: "/x"}},
		"actions[1].working_dir":      {ID: "wd", Name: "WD", Command: &Command{Command: "/x"}, WorkingDir: "relative"},
		"actions[1].timeout":          {ID: "to", Name: "TO", Command: &Command{Command: "/x"}, Timeout: Duration(48 * time.Hour)},
		"actions[1].tmux.text":        {ID: "tx", Name: "TX", Method: MethodTmux, Tmux: &TmuxText{Text: "x\x1b[A"}},
		"actions[1].icon":             {ID: "ic", Name: "IC", Command: &Command{Command: "/x"}, Icon: strings.Repeat("★", 10)},
		"actions[1].command.args[0]":  {ID: "nul", Name: "NUL", Command: &Command{Command: "/x", Args: []string{"a\x00b"}}},
		"actions[1].command.command ": {ID: "rel", Name: "Rel", Command: &Command{Command: "./run.sh"}},
	}
	for field, bad := range cases {
		x := tmuxDef()
		x.Actions = []Action{d.Actions[0], bad}
		p := problems(t, x)
		if p[strings.TrimSpace(field)] == "" {
			t.Errorf("expected a problem for %s, got %v", field, p)
		}
	}
	// tmux actions need a tmux server.
	c := custom()
	c.Actions = []Action{{ID: "save", Name: "Save", Method: MethodTmux, Tmux: &TmuxText{Text: "save"}}}
	if p := problems(t, c); p["actions[0].method"] == "" {
		t.Fatal("tmux actions must be refused for other backends")
	}
	// Bad IDs.
	for _, id := range []string{"", "Save", "a b", "../x", strings.Repeat("a", 64)} {
		x := tmuxDef()
		x.Actions = []Action{{ID: id, Name: "X", Command: &Command{Command: "/x"}}}
		if p := problems(t, x); p["actions[0].id"] == "" {
			t.Errorf("action id %q must be rejected", id)
		}
	}
	// Too many.
	x := tmuxDef()
	for i := 0; i <= MaxActions; i++ {
		x.Actions = append(x.Actions, Action{ID: "a" + strings.Repeat("x", i%10) + string(rune('a'+i%26)) + string(rune('a'+i/26)), Name: "A", Command: &Command{Command: "/x"}})
	}
	if p := problems(t, x); p["actions"] == "" {
		t.Fatal("the number of actions must be bounded")
	}
}

func TestComparisonsTreatActionsAndBackupsCorrectly(t *testing.T) {
	a := tmuxDef()
	a.Normalize()
	b := a.Clone()
	b.Actions = []Action{{ID: "save", Name: "Save", Method: MethodTmux, Tmux: &TmuxText{Text: "save-all"}}}
	b.Backup = &Backup{Method: MethodTmux, Tmux: &TmuxText{Text: "/backup start"}}
	b.Normalize()
	if ExecutionEqual(a, b) {
		t.Fatal("actions and backups decide what is executed: CONFIG holders must not change them")
	}
	if !RuntimeEqual(a, b) {
		t.Fatal("actions and backups do not affect the running process")
	}
	c := b.Clone()
	c.Tmux.Session = "other"
	if RuntimeEqual(b, c) {
		t.Fatal("the tmux session controls the process")
	}
	// Non-administrators never see actions.
	if v := b.WithoutActions(); v.Actions != nil || b.Actions == nil {
		t.Fatal("WithoutActions must return a stripped copy")
	}
}

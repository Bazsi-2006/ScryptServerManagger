package servers

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
)

// The shipped examples must always be valid for the current code.
func TestExampleDefinitionsAreValid(t *testing.T) {
	files, err := filepath.Glob("../../examples/servers/*.json")
	if err != nil || len(files) < 4 {
		t.Fatalf("expected example definitions, found %d (%v)", len(files), err)
	}
	backends := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		d, err := ParseDefinition(data)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		if d.ID+".json" != filepath.Base(f) {
			t.Errorf("%s: file name must match the id %q", filepath.Base(f), d.ID)
		}
		backends[d.Backend] = true
	}
	for _, b := range []string{BackendCustom, BackendTmux, BackendSystemd, BackendDocker} {
		if !backends[b] {
			t.Errorf("no example for the %s backend", b)
		}
	}
}

func TestExampleConfigurationIsValid(t *testing.T) {
	data, err := os.ReadFile("../../examples/config.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(data, "/etc/server-manager")
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != config.ModeProduction || c.Listen != "127.0.0.1:8080" {
		t.Errorf("the sample should be a production configuration bound to loopback: %s %s", c.Mode, c.Listen)
	}
}

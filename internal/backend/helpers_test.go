package backend

import (
	"sync"
	"testing"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/console"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/logging"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

type env struct {
	cfg  *config.Holder
	buf  *console.Buffer
	deps Deps
	mu   sync.Mutex
	rt   string
}

func newEnv(t *testing.T) *env {
	e := &env{cfg: testutil.Config(t), buf: console.NewBuffer(500)}
	e.deps = Deps{Config: e.cfg, Console: e.buf, Log: logging.Discard(),
		LoadRuntime: func() string { e.mu.Lock(); defer e.mu.Unlock(); return e.rt },
		SaveRuntime: func(s string) { e.mu.Lock(); e.rt = s; e.mu.Unlock() },
	}
	return e
}

func (e *env) lines() []string {
	ls, _ := e.buf.Tail(0)
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.Text
	}
	return out
}

func (e *env) has(text string) bool {
	for _, l := range e.lines() {
		if l == text {
			return true
		}
	}
	return false
}

func valid(t *testing.T, d *servers.Definition) *servers.Definition {
	t.Helper()
	d.Normalize()
	if err := d.Validate(); err != nil {
		t.Fatalf("test definition invalid: %v", err)
	}
	return d
}

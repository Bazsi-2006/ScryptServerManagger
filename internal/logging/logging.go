// Package logging configures the structured application log (log/slog).
// Application logs, the audit trail (SQLite) and server console output are
// three separate things; only the first is handled here.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
)

// reopenWriter is a file writer that can be reopened (logrotate + SIGHUP).
type reopenWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func (w *reopenWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	if w.f != nil {
		w.f.Close()
	}
	w.f = f
	return nil
}

func (w *reopenWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return len(p), nil
	}
	return w.f.Write(p)
}

// Logger bundles the slog logger with its runtime controls.
type Logger struct {
	*slog.Logger
	level *slog.LevelVar
	file  *reopenWriter
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// New builds the application logger: always stderr (journald under
// systemd), plus the optional log file.
func New(c config.Logging) (*Logger, error) {
	l := &Logger{level: new(slog.LevelVar)}
	l.level.Set(parseLevel(c.Level))
	var out io.Writer = os.Stderr
	if c.File != "" {
		l.file = &reopenWriter{path: c.File}
		if err := l.file.open(); err != nil {
			return nil, fmt.Errorf("open log file: %w", err)
		}
		out = io.MultiWriter(os.Stderr, l.file)
	}
	opts := &slog.HandlerOptions{Level: l.level}
	if c.Format == "json" {
		l.Logger = slog.New(slog.NewJSONHandler(out, opts))
	} else {
		l.Logger = slog.New(slog.NewTextHandler(out, opts))
	}
	return l, nil
}

// SetLevel changes the level at runtime.
func (l *Logger) SetLevel(level string) { l.level.Set(parseLevel(level)) }

// Reopen reopens the log file (after rotation).
func (l *Logger) Reopen() error {
	if l.file == nil {
		return nil
	}
	l.file.mu.Lock()
	defer l.file.mu.Unlock()
	return l.file.open()
}

// Discard returns a logger that drops everything (tests).
func Discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

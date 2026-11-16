// Package perm implements the per-server permission bitmask.
package perm

import "strings"

// Mask is a set of permission flags stored as an integer in SQLite.
type Mask int

const (
	Start        Mask = 1
	Stop         Mask = 2
	ConsoleRead  Mask = 4
	ConsoleWrite Mask = 8
	Backups      Mask = 16
	Config       Mask = 32

	// All is every defined flag; it is what administrators effectively hold.
	All Mask = Start | Stop | ConsoleRead | ConsoleWrite | Backups | Config
)

var names = []struct {
	Flag Mask
	Name string
}{
	{Start, "START"}, {Stop, "STOP"}, {ConsoleRead, "CONSOLE_READ"},
	{ConsoleWrite, "CONSOLE_WRITE"}, {Backups, "BACKUPS"}, {Config, "CONFIG"},
}

// Valid reports whether m only contains defined flags.
func (m Mask) Valid() bool { return m >= 0 && m&^All == 0 }

// Effective expands the hierarchical implications:
// STOP grants START and CONSOLE_WRITE grants CONSOLE_READ.
func (m Mask) Effective() Mask {
	m &= All
	if m&Stop != 0 {
		m |= Start
	}
	if m&ConsoleWrite != 0 {
		m |= ConsoleRead
	}
	return m
}

// Has reports whether m (after applying implications) grants every flag in p.
// Permissions default to denied: a zero mask grants nothing.
func (m Mask) Has(p Mask) bool {
	return p != 0 && m.Effective()&p == p
}

// Any reports whether m grants at least one permission, i.e. whether the
// holder may see the server at all.
func (m Mask) Any() bool { return m&All != 0 }

func (m Mask) String() string {
	var parts []string
	for _, n := range names {
		if m&n.Flag != 0 {
			parts = append(parts, n.Name)
		}
	}
	if len(parts) == 0 {
		return "NONE"
	}
	return strings.Join(parts, "|")
}

// Names returns the flag names contained in m (without implications).
func (m Mask) Names() []string {
	out := []string{}
	for _, n := range names {
		if m&n.Flag != 0 {
			out = append(out, n.Name)
		}
	}
	return out
}

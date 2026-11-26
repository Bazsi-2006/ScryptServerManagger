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
	// LegacyBackups is the former general BACKUPS permission (bit 16). It
	// was split into BackupCreate and BackupDownload; migration 0003
	// rewrote stored masks, and the bit is still accepted (and expanded to
	// both new flags) so older API clients and rows keep their meaning.
	LegacyBackups Mask = 16
	Config        Mask = 32
	// BackupCreate allows starting (and cancelling) a backup.
	BackupCreate Mask = 64
	// BackupDownload allows listing and downloading backup files.
	BackupDownload Mask = 128

	// All is every defined flag; it is what administrators effectively hold.
	All Mask = Start | Stop | ConsoleRead | ConsoleWrite | Config | BackupCreate | BackupDownload

	// AnyBackup matches either backup permission.
	AnyBackup = BackupCreate | BackupDownload
)

var names = []struct {
	Flag Mask
	Name string
}{
	{Start, "START"}, {Stop, "STOP"}, {ConsoleRead, "CONSOLE_READ"},
	{ConsoleWrite, "CONSOLE_WRITE"}, {Config, "CONFIG"},
	{BackupCreate, "BACKUP_CREATE"}, {BackupDownload, "BACKUP_DOWNLOAD"},
}

// Valid reports whether m only contains defined flags (the legacy BACKUPS
// bit included).
func (m Mask) Valid() bool { return m >= 0 && m&^(All|LegacyBackups) == 0 }

// Normalize replaces the legacy BACKUPS bit by the two flags it stands for
// and drops undefined bits. Masks are stored normalized.
func (m Mask) Normalize() Mask {
	if m < 0 {
		return 0
	}
	if m&LegacyBackups != 0 {
		m |= BackupCreate | BackupDownload
	}
	return m & All
}

// Effective expands the hierarchical implications:
// STOP grants START and CONSOLE_WRITE grants CONSOLE_READ. The two backup
// flags are independent of each other.
func (m Mask) Effective() Mask {
	m = m.Normalize()
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

// HasAny reports whether m grants at least one of the flags in p.
func (m Mask) HasAny(p Mask) bool {
	return p != 0 && m.Effective()&p != 0
}

// Any reports whether m grants at least one permission, i.e. whether the
// holder may see the server at all.
func (m Mask) Any() bool { return m.Normalize() != 0 }

func (m Mask) String() string {
	var parts []string
	for _, n := range names {
		if m.Normalize()&n.Flag != 0 {
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
		if m.Normalize()&n.Flag != 0 {
			out = append(out, n.Name)
		}
	}
	return out
}

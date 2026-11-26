package perm

import "testing"

func TestBitValues(t *testing.T) {
	// Stored values must never change meaning: they live in the database.
	want := map[Mask]int{Start: 1, Stop: 2, ConsoleRead: 4, ConsoleWrite: 8, LegacyBackups: 16, Config: 32, BackupCreate: 64, BackupDownload: 128}
	for m, v := range want {
		if int(m) != v {
			t.Errorf("flag %s = %d, want %d", m, int(m), v)
		}
	}
	if All != 239 {
		t.Errorf("All = %d, want 239 (every flag except the legacy BACKUPS bit)", All)
	}
}

func TestImplications(t *testing.T) {
	cases := []struct {
		name string
		mask Mask
		has  []Mask
		not  []Mask
	}{
		{"zero grants nothing", 0, nil, []Mask{Start, Stop, ConsoleRead, ConsoleWrite, BackupCreate, BackupDownload, Config}},
		{"start does not imply stop", Start, []Mask{Start}, []Mask{Stop, ConsoleRead}},
		{"stop implies start", Stop, []Mask{Start, Stop}, []Mask{ConsoleRead, Config}},
		{"read does not imply write", ConsoleRead, []Mask{ConsoleRead}, []Mask{ConsoleWrite, Start}},
		{"write implies read", ConsoleWrite, []Mask{ConsoleRead, ConsoleWrite}, []Mask{Start, BackupCreate}},
		{"backup create alone", BackupCreate, []Mask{BackupCreate}, []Mask{BackupDownload, Config, Start, ConsoleRead}},
		{"backup download alone", BackupDownload, []Mask{BackupDownload}, []Mask{BackupCreate, Config, Start}},
		{"legacy backups means both", LegacyBackups, []Mask{BackupCreate, BackupDownload, BackupCreate | BackupDownload}, []Mask{Config, Start}},
		{"config alone", Config, []Mask{Config}, []Mask{BackupCreate, BackupDownload, Stop}},
		{"combined", Stop | ConsoleWrite, []Mask{Start, Stop, ConsoleRead, ConsoleWrite, Start | ConsoleRead}, []Mask{BackupCreate, Config, Start | BackupDownload}},
	}
	for _, c := range cases {
		for _, p := range c.has {
			if !c.mask.Has(p) {
				t.Errorf("%s: %s should grant %s", c.name, c.mask, p)
			}
		}
		for _, p := range c.not {
			if c.mask.Has(p) {
				t.Errorf("%s: %s must not grant %s", c.name, c.mask, p)
			}
		}
	}
}

func TestHasAny(t *testing.T) {
	if !BackupCreate.HasAny(AnyBackup) || !BackupDownload.HasAny(AnyBackup) || !LegacyBackups.HasAny(AnyBackup) {
		t.Error("either backup flag satisfies HasAny(AnyBackup)")
	}
	if Start.HasAny(AnyBackup) || Mask(0).HasAny(All) || All.HasAny(0) {
		t.Error("HasAny without an overlapping flag must be false")
	}
}

func TestHasZeroIsAlwaysFalse(t *testing.T) {
	if All.Has(0) {
		t.Error("asking for no permission must not succeed")
	}
}

func TestValidNormalizeAndUndefinedBits(t *testing.T) {
	if !All.Valid() || !Mask(0).Valid() || !LegacyBackups.Valid() {
		t.Error("defined masks (and the legacy bit) must be valid")
	}
	for _, m := range []Mask{256, 512, -1, All | 256} {
		if m.Valid() {
			t.Errorf("mask %d must be invalid", m)
		}
	}
	// Undefined bits never grant anything, not even visibility.
	if Mask(256).Any() || Mask(256).Has(Start) {
		t.Error("undefined bits must not grant access")
	}
	if got := Mask(256 | 2).Effective(); got != Start|Stop {
		t.Errorf("Effective dropped/expanded wrongly: %d", got)
	}
	if got := (LegacyBackups | Config).Normalize(); got != Config|BackupCreate|BackupDownload {
		t.Errorf("legacy bit must normalize to both backup flags: %d", got)
	}
	if got := (Start | BackupCreate).Normalize(); got != Start|BackupCreate {
		t.Errorf("normalize must keep defined flags: %d", got)
	}
}

func TestString(t *testing.T) {
	if s := (Stop | ConsoleRead).String(); s != "STOP|CONSOLE_READ" {
		t.Errorf("got %q", s)
	}
	if s := LegacyBackups.String(); s != "BACKUP_CREATE|BACKUP_DOWNLOAD" {
		t.Errorf("legacy bit prints as %q", s)
	}
	if Mask(0).String() != "NONE" {
		t.Error("zero mask should print NONE")
	}
}

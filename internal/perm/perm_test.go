package perm

import "testing"

func TestBitValues(t *testing.T) {
	want := map[Mask]int{Start: 1, Stop: 2, ConsoleRead: 4, ConsoleWrite: 8, Backups: 16, Config: 32}
	for m, v := range want {
		if int(m) != v {
			t.Errorf("flag %s = %d, want %d", m, int(m), v)
		}
	}
	if All != 63 {
		t.Errorf("All = %d", All)
	}
}

func TestImplications(t *testing.T) {
	cases := []struct {
		name string
		mask Mask
		has  []Mask
		not  []Mask
	}{
		{"zero grants nothing", 0, nil, []Mask{Start, Stop, ConsoleRead, ConsoleWrite, Backups, Config}},
		{"start does not imply stop", Start, []Mask{Start}, []Mask{Stop, ConsoleRead}},
		{"stop implies start", Stop, []Mask{Start, Stop}, []Mask{ConsoleRead, Config}},
		{"read does not imply write", ConsoleRead, []Mask{ConsoleRead}, []Mask{ConsoleWrite, Start}},
		{"write implies read", ConsoleWrite, []Mask{ConsoleRead, ConsoleWrite}, []Mask{Start, Backups}},
		{"backups alone", Backups, []Mask{Backups}, []Mask{Config, Start, ConsoleRead}},
		{"config alone", Config, []Mask{Config}, []Mask{Backups, Stop}},
		{"combined", Stop | ConsoleWrite, []Mask{Start, Stop, ConsoleRead, ConsoleWrite, Start | ConsoleRead}, []Mask{Backups, Config, Start | Backups}},
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

func TestHasZeroIsAlwaysFalse(t *testing.T) {
	if All.Has(0) {
		t.Error("asking for no permission must not succeed")
	}
}

func TestValidAndUndefinedBits(t *testing.T) {
	if !All.Valid() || !Mask(0).Valid() {
		t.Error("defined masks must be valid")
	}
	for _, m := range []Mask{64, 128, -1, All + 1} {
		if m.Valid() {
			t.Errorf("mask %d must be invalid", m)
		}
	}
	// Undefined bits never grant anything, not even visibility.
	if Mask(64).Any() || Mask(64).Has(Start) {
		t.Error("undefined bits must not grant access")
	}
	if got := Mask(64 | 2).Effective(); got != Start|Stop {
		t.Errorf("Effective dropped/expanded wrongly: %d", got)
	}
}

func TestString(t *testing.T) {
	if s := (Stop | ConsoleRead).String(); s != "STOP|CONSOLE_READ" {
		t.Errorf("got %q", s)
	}
	if Mask(0).String() != "NONE" {
		t.Error("zero mask should print NONE")
	}
}

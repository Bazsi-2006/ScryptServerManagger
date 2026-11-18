package monitor

import (
	"os"
	"path/filepath"
	"testing"
)

const stat1 = `cpu  1000 0 500 8000 500 0 0 0 0 0
cpu0 500 0 250 4000 250 0 0 0 0 0
cpu1 500 0 250 4000 250 0 0 0 0 0
intr 1
`

const stat2 = `cpu  1300 0 600 8500 600 0 0 0 0 0
cpu0 650 0 300 4250 300 0 0 0 0 0
cpu1 650 0 300 4250 300 0 0 0 0 0
`

const meminfo = `MemTotal:       16000000 kB
MemFree:         2000000 kB
MemAvailable:   12000000 kB
Buffers:          500000 kB
Cached:          6000000 kB
`

func TestParseCPU(t *testing.T) {
	c, n, err := parseCPU([]byte(stat1))
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if c.total != 10000 || c.idle != 8500 {
		t.Fatalf("%+v", c)
	}
	if _, _, err := parseCPU([]byte("nothing here")); err == nil {
		t.Fatal("missing cpu line must be an error")
	}
}

func TestParseMeminfo(t *testing.T) {
	total, avail, err := parseMeminfo([]byte(meminfo))
	if err != nil || total != 16000000*1024 || avail != 12000000*1024 {
		t.Fatalf("total=%d avail=%d err=%v", total, avail, err)
	}
	// Old kernels without MemAvailable fall back to free+buffers+cached.
	_, avail, _ = parseMeminfo([]byte("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 250 kB\n"))
	if avail != 400*1024 {
		t.Fatalf("fallback avail=%d", avail)
	}
	if _, _, err := parseMeminfo([]byte("garbage")); err == nil {
		t.Fatal("missing MemTotal must be an error")
	}
}

func TestSampleComputesDeltas(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) { os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644) }
	write("stat", stat1)
	write("meminfo", meminfo)
	write("loadavg", "0.50 0.25 0.10 1/100 123\n")
	write("uptime", "3600.5 7000.0\n")
	m := New()
	m.procDir = dir
	first, err := m.sample()
	if err != nil {
		t.Fatal(err)
	}
	if first.CPUPercent != 0 {
		t.Fatal("the first sample has no baseline")
	}
	write("stat", stat2)
	s, err := m.sample()
	if err != nil {
		t.Fatal(err)
	}
	// total delta 1000, idle delta 600 -> 40% busy.
	if s.CPUPercent != 40 {
		t.Fatalf("cpu %.1f, want 40", s.CPUPercent)
	}
	if s.MemUsedPercent != 25 || s.MemUsedBytes != 4000000*1024 || s.CPUCount != 2 {
		t.Fatalf("%+v", s)
	}
	if s.Load1 != 0.5 || s.UptimeSeconds != 3600 {
		t.Fatalf("load/uptime: %+v", s)
	}
}

func TestRealProcIsReadable(t *testing.T) {
	m := New()
	s, err := m.sample()
	if err != nil {
		t.Skipf("no usable /proc here: %v", err)
	}
	if s.MemTotalBytes == 0 || s.MemAvailBytes > s.MemTotalBytes || s.CPUCount < 1 {
		t.Fatalf("implausible sample %+v", s)
	}
}

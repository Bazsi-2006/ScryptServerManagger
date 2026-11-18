// Package monitor samples host CPU and memory usage from /proc.
package monitor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sample is one measurement of the Linux host the application runs on.
type Sample struct {
	Time           time.Time `json:"time"`
	CPUPercent     float64   `json:"cpu_percent"`
	CPUCount       int       `json:"cpu_count"`
	MemTotalBytes  uint64    `json:"mem_total_bytes"`
	MemAvailBytes  uint64    `json:"mem_available_bytes"`
	MemUsedBytes   uint64    `json:"mem_used_bytes"`
	MemUsedPercent float64   `json:"mem_used_percent"`
	Load1          float64   `json:"load1"`
	Load5          float64   `json:"load5"`
	Load15         float64   `json:"load15"`
	UptimeSeconds  uint64    `json:"uptime_seconds"`
}

type cpuTimes struct{ idle, total uint64 }

// parseCPU extracts aggregate CPU time from /proc/stat content.
func parseCPU(data []byte) (cpuTimes, int, error) {
	var t cpuTimes
	count := 0
	found := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		f := strings.Fields(line)
		if f[0] != "cpu" {
			count++
			continue
		}
		// user nice system idle iowait irq softirq steal [guest guest_nice]
		// guest time is already included in user, so stop at steal.
		for i := 1; i < len(f) && i <= 8; i++ {
			v, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				return t, 0, err
			}
			t.total += v
			if i == 4 || i == 5 { // idle + iowait
				t.idle += v
			}
		}
		found = true
	}
	if !found {
		return t, 0, errors.New("no cpu line in /proc/stat")
	}
	return t, count, nil
}

// parseMeminfo returns MemTotal and MemAvailable in bytes.
func parseMeminfo(data []byte) (total, avail uint64, err error) {
	var free, buffers, cached uint64
	haveAvail := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		k, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, perr := strconv.ParseUint(f[0], 10, 64)
		if perr != nil {
			continue
		}
		v *= 1024
		switch k {
		case "MemTotal":
			total = v
		case "MemAvailable":
			avail, haveAvail = v, true
		case "MemFree":
			free = v
		case "Buffers":
			buffers = v
		case "Cached":
			cached = v
		}
	}
	if total == 0 {
		return 0, 0, errors.New("MemTotal missing from /proc/meminfo")
	}
	if !haveAvail {
		avail = free + buffers + cached // kernels older than 3.14
	}
	if avail > total {
		avail = total
	}
	return total, avail, nil
}

// Monitor samples periodically and keeps a short history.
type Monitor struct {
	procDir string

	mu       sync.RWMutex
	prev     cpuTimes
	havePrev bool
	history  []Sample
	maxHist  int
}

func New() *Monitor { return &Monitor{procDir: "/proc", maxHist: 120} }

func (m *Monitor) sample() (Sample, error) {
	s := Sample{Time: time.Now().UTC()}
	stat, err := os.ReadFile(m.procDir + "/stat")
	if err != nil {
		return s, err
	}
	cur, count, err := parseCPU(stat)
	if err != nil {
		return s, err
	}
	s.CPUCount = count
	mem, err := os.ReadFile(m.procDir + "/meminfo")
	if err != nil {
		return s, err
	}
	total, avail, err := parseMeminfo(mem)
	if err != nil {
		return s, err
	}
	s.MemTotalBytes, s.MemAvailBytes, s.MemUsedBytes = total, avail, total-avail
	s.MemUsedPercent = round1(float64(total-avail) / float64(total) * 100)
	if data, err := os.ReadFile(m.procDir + "/loadavg"); err == nil {
		f := strings.Fields(string(data))
		if len(f) >= 3 {
			s.Load1, _ = strconv.ParseFloat(f[0], 64)
			s.Load5, _ = strconv.ParseFloat(f[1], 64)
			s.Load15, _ = strconv.ParseFloat(f[2], 64)
		}
	}
	if data, err := os.ReadFile(m.procDir + "/uptime"); err == nil {
		if f := strings.Fields(string(data)); len(f) > 0 {
			up, _ := strconv.ParseFloat(f[0], 64)
			s.UptimeSeconds = uint64(up)
		}
	}
	m.mu.Lock()
	if m.havePrev && cur.total > m.prev.total {
		dt := float64(cur.total - m.prev.total)
		di := float64(cur.idle - m.prev.idle)
		pct := (dt - di) / dt * 100
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		s.CPUPercent = round1(pct)
	}
	m.prev, m.havePrev = cur, true
	m.mu.Unlock()
	return s, nil
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// Run samples at the interval returned by interval() until ctx is done,
// invoking onSample for each measurement.
func (m *Monitor) Run(ctx context.Context, interval func() time.Duration, onSample func(Sample), onError func(error)) {
	// Prime the CPU counters so the first published sample has a real value.
	_, _ = m.sample()
	first := time.NewTimer(time.Second)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	for {
		s, err := m.sample()
		if err != nil {
			if onError != nil {
				onError(err)
			}
		} else {
			m.mu.Lock()
			m.history = append(m.history, s)
			if len(m.history) > m.maxHist {
				m.history = append([]Sample(nil), m.history[len(m.history)-m.maxHist:]...)
			}
			m.mu.Unlock()
			if onSample != nil {
				onSample(s)
			}
		}
		t := time.NewTimer(interval())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Latest returns the most recent sample.
func (m *Monitor) Latest() (Sample, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.history) == 0 {
		return Sample{}, false
	}
	return m.history[len(m.history)-1], true
}

// History returns the retained samples, oldest first.
func (m *Monitor) History() []Sample {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Sample(nil), m.history...)
}

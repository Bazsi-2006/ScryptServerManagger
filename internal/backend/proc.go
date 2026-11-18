package backend

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// procStat returns the state character and start time (clock ticks since
// boot) of a process from /proc/<pid>/stat. The start time distinguishes a
// process from a later one that reuses the same PID.
func procStat(pid int) (state byte, startTicks uint64, err error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, err
	}
	// The command name (field 2) is parenthesised and may contain spaces or
	// parentheses, so parse from the last ')'.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return 0, 0, errors.New("malformed stat")
	}
	fields := strings.Fields(s[i+2:])
	// fields[0] is field 3 (state); starttime is field 22 -> index 19.
	if len(fields) < 20 {
		return 0, 0, errors.New("malformed stat")
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return fields[0][0], ticks, nil
}

// procAlive reports whether pid is still the process that had startTicks.
func procAlive(pid int, startTicks uint64) bool {
	if pid <= 0 {
		return false
	}
	state, ticks, err := procStat(pid)
	return err == nil && ticks == startTicks && state != 'Z' && state != 'X'
}

// pidAlive reports whether any live process has this PID.
func pidAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	if state, _, err := procStat(pid); err == nil {
		return state != 'Z' && state != 'X'
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// readPIDFile parses the first integer in a PID file.
func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, errors.New("PID file is empty")
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 {
		return 0, errors.New("PID file does not contain a valid process ID")
	}
	return pid, nil
}

func signalByName(name string) syscall.Signal {
	switch name {
	case "SIGINT":
		return syscall.SIGINT
	case "SIGQUIT":
		return syscall.SIGQUIT
	case "SIGHUP":
		return syscall.SIGHUP
	case "SIGUSR1":
		return syscall.SIGUSR1
	case "SIGUSR2":
		return syscall.SIGUSR2
	case "SIGKILL":
		return syscall.SIGKILL
	}
	return syscall.SIGTERM
}

// openPTY allocates a pseudo-terminal pair. The master is non-blocking so
// the Go runtime poller manages it (deadlines work, Close unblocks reads).
func openPTY(cols, rows int) (master, slave *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	fail := func(e error) (*os.File, *os.File, error) { unix.Close(fd); return nil, nil, e }
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return fail(fmt.Errorf("unlock pty: %w", err))
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		return fail(fmt.Errorf("query pty number: %w", err))
	}
	_ = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(cols), Row: uint16(rows)})
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return fail(fmt.Errorf("open pty slave: %w", err))
	}
	return os.NewFile(uintptr(fd), "/dev/ptmx"), slave, nil
}

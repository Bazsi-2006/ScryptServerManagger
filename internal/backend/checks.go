package backend

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// CheckPort runs one port probe.
//
// TCP: ready when a connection can be established.
// UDP: the configured payload is sent and the port counts as ready only if
// a reply datagram arrives, because "nothing refused the packet" proves
// nothing about a UDP service.
//
// Limitation (applies to every external check): a positive result shows
// that *something* answers on that address, not that it is the process the
// manager started.
func CheckPort(ctx context.Context, p *servers.PortCheck) (bool, error) {
	addr := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	timeout := p.Timeout.D()
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	switch p.Protocol {
	case "tcp":
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && !dnsErr.IsTimeout {
				return false, fmt.Errorf("cannot resolve host %q", p.Host)
			}
			return false, nil
		}
		conn.Close()
		return true, nil
	case "udp":
		payload, err := hex.DecodeString(p.UDPPayloadHex)
		if err != nil || len(payload) == 0 {
			return false, errors.New("invalid UDP probe payload")
		}
		conn, err := d.DialContext(ctx, "udp", addr)
		if err != nil {
			return false, nil
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(timeout))
		if _, err := conn.Write(payload); err != nil {
			return false, nil
		}
		buf := make([]byte, 2048)
		if _, err := conn.Read(buf); err != nil {
			return false, nil // timeout or ICMP port unreachable
		}
		return true, nil
	}
	return false, fmt.Errorf("unsupported protocol %q", p.Protocol)
}

// CheckScript runs the status script once.
//
// Convention: exit 0 = ready/running, exit 1 = not ready, anything else
// (other exit codes, signals, timeouts, exec failures) = check error, which
// the manager reports as an unknown state.
func CheckScript(ctx context.Context, def *servers.Definition, s *servers.ScriptCheck) (bool, error) {
	timeout := s.Timeout.D()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append([]string{s.Command}, s.Args...)
	res, err := Run(ctx, RunSpec{Argv: argv, Dir: def.WorkingDir, Env: ServerEnv(def), TailBytes: 512})
	if err != nil {
		if errors.Is(err, ErrTimeout) {
			return false, fmt.Errorf("status script timed out after %s", timeout)
		}
		return false, fmt.Errorf("status script failed: %v", err)
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	msg := fmt.Sprintf("status script exited with code %d", res.ExitCode)
	if res.Output != "" {
		msg += ": " + lastLine(res.Output)
	}
	return false, errors.New(msg)
}

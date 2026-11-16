package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/console"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// dockerBackend controls an existing container through the Docker Engine
// API on the local Unix socket. No docker CLI is needed.
//
// Access to the Docker socket is equivalent to root on the host: whoever
// can talk to it can start privileged containers. The manager only ever
// issues inspect/start/stop/restart/kill/logs (and, if an administrator
// configures it, exec of a fixed argv template) against the configured
// container, but the service user itself necessarily holds that power.
type dockerBackend struct {
	def  *servers.Definition
	d    *servers.Docker
	deps Deps
}

func newDocker(def *servers.Definition, deps Deps) *dockerBackend {
	return &dockerBackend{def: def, d: def.Docker, deps: deps}
}

func (b *dockerBackend) Caps() Caps {
	return Caps{Start: true, NativeRestart: b.d.RestartAllowed(), ForceKill: true, LiveOnDemand: true}
}

func (b *dockerBackend) client(timeout time.Duration) *http.Client {
	sock := b.deps.Config.Get().Tools.DockerSocket
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			DisableKeepAlives: true,
		},
	}
}

type dockerError struct {
	Status  int
	Message string
}

func (e *dockerError) Error() string { return e.Message }

// do performs a request. The caller must close the body on success.
func (b *dockerBackend) do(ctx context.Context, timeout time.Duration, method, path string, query url.Values, body any) (*http.Response, error) {
	u := "http://docker" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client(timeout).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: cannot reach the Docker daemon (is it running, and may the service user access its socket?)", ErrUnavailable)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var payload struct {
			Message string `json:"message"`
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = json.Unmarshal(data, &payload)
		msg := payload.Message
		if msg == "" {
			msg = "Docker returned HTTP " + strconv.Itoa(resp.StatusCode)
		}
		return nil, &dockerError{Status: resp.StatusCode, Message: msg}
	}
	return resp, nil
}

func (b *dockerBackend) containerPath(suffix string) string {
	return "/containers/" + url.PathEscape(b.d.Container) + suffix
}

type dockerInspect struct {
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Paused     bool   `json:"Paused"`
		Restarting bool   `json:"Restarting"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		OOMKilled  bool   `json:"OOMKilled"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Tty bool `json:"Tty"`
	} `json:"Config"`
}

func (b *dockerBackend) inspect(ctx context.Context) (*dockerInspect, error) {
	resp, err := b.do(ctx, 10*time.Second, http.MethodGet, b.containerPath("/json"), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var info dockerInspect
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode Docker response: %w", err)
	}
	return &info, nil
}

func (b *dockerBackend) Probe(ctx context.Context) Probe {
	info, err := b.inspect(ctx)
	if err != nil {
		var de *dockerError
		if errors.As(err, &de) && de.Status == http.StatusNotFound {
			return Probe{Life: LifeUnknown, Detail: fmt.Sprintf("Docker container %q does not exist", b.d.Container)}
		}
		return Probe{Life: LifeUnknown, Detail: err.Error()}
	}
	st := info.State
	switch {
	case st.Restarting:
		return Probe{Life: LifeAlive, Phase: PhaseStarting, Detail: "container is restarting"}
	case st.Running && st.Paused:
		return Probe{Life: LifeAlive, Detail: "container is paused"}
	case st.Running:
		if st.Health != nil {
			switch st.Health.Status {
			case "starting":
				return Probe{Life: LifeAlive, Phase: PhaseStarting, Detail: "health check: starting"}
			case "unhealthy":
				return Probe{Life: LifeAlive, Detail: "container health check reports unhealthy"}
			}
		}
		return Probe{Life: LifeAlive}
	}
	p := Probe{Life: LifeDead, Detail: "container is " + firstNonEmpty(st.Status, "stopped")}
	if st.Status == "exited" || st.Status == "dead" {
		p.Detail = fmt.Sprintf("container %s (exit code %d)", st.Status, st.ExitCode)
		if st.OOMKilled {
			p.Detail += ", out of memory"
		}
		if st.Error != "" {
			p.Phase = PhaseFailed
			p.Detail += ": " + st.Error
		}
	}
	return p
}

func (b *dockerBackend) post(ctx context.Context, timeout time.Duration, suffix string, q url.Values) error {
	resp, err := b.do(ctx, timeout, http.MethodPost, b.containerPath(suffix), q, nil)
	if err != nil {
		var de *dockerError
		if errors.As(err, &de) {
			if de.Status == http.StatusNotFound {
				return fmt.Errorf("Docker container %q does not exist", b.d.Container)
			}
			return fmt.Errorf("Docker: %s", de.Message)
		}
		return err
	}
	resp.Body.Close() // 204, or 304 when already in the requested state
	return nil
}

func graceQuery(grace time.Duration) url.Values {
	secs := int(grace.Seconds())
	if secs < 1 {
		secs = 1
	}
	return url.Values{"t": {strconv.Itoa(secs)}}
}

func (b *dockerBackend) Start(ctx context.Context) error {
	return b.post(ctx, 60*time.Second, "/start", nil)
}

// Stop asks Docker to stop the container. The API call blocks until the
// container has stopped (Docker escalates to SIGKILL after the grace
// period), so it runs in the background and the caller polls Probe.
func (b *dockerBackend) Stop(ctx context.Context, grace time.Duration) error {
	if _, err := b.inspect(ctx); err != nil {
		var de *dockerError
		if errors.As(err, &de) && de.Status == http.StatusNotFound {
			return fmt.Errorf("Docker container %q does not exist", b.d.Container)
		}
		return err
	}
	go func() {
		c, cancel := context.WithTimeout(context.Background(), grace+30*time.Second)
		defer cancel()
		if err := b.post(c, grace+30*time.Second, "/stop", graceQuery(grace)); err != nil {
			b.deps.Log.Warn("docker stop failed", "server", b.def.ID, "error", err)
		}
	}()
	return nil
}

func (b *dockerBackend) Kill(ctx context.Context) error {
	return b.post(ctx, 30*time.Second, "/kill", nil)
}

func (b *dockerBackend) Restart(ctx context.Context, grace time.Duration) error {
	if !b.d.RestartAllowed() {
		return ErrUnsupported
	}
	// Synchronous: Docker answers once the container has been restarted.
	return b.post(ctx, grace+60*time.Second, "/restart", graceQuery(grace))
}

// demux decodes Docker's multiplexed stream format (8-byte header: stream
// type, 3 zero bytes, big-endian payload length) into w.
func demux(r io.Reader, w io.Writer) error {
	br := bufio.NewReaderSize(r, 32<<10)
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if hdr[0] > 2 || hdr[1] != 0 || hdr[2] != 0 || hdr[3] != 0 {
			return errors.New("unexpected Docker stream framing")
		}
		n := int64(binary.BigEndian.Uint32(hdr[4:]))
		if _, err := io.CopyN(w, br, n); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// copyLogs copies a log/exec stream into w, demultiplexing unless the
// container has a TTY (in which case Docker sends the raw stream).
func copyLogs(body io.Reader, tty bool, w io.Writer) error {
	if tty {
		_, err := io.Copy(w, body)
		return err
	}
	return demux(body, w)
}

type lineCollector struct {
	max   int
	lines []string
	buf   []byte
	emit  func(string)
}

func (c *lineCollector) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	for {
		i := bytes.IndexByte(c.buf, '\n')
		if i < 0 {
			if len(c.buf) > c.max {
				c.flush(c.buf[:c.max])
				c.buf = c.buf[c.max:]
				continue
			}
			break
		}
		c.flush(c.buf[:i])
		c.buf = c.buf[i+1:]
	}
	return len(p), nil
}

func (c *lineCollector) flush(b []byte) {
	l := strings.TrimRight(strings.ToValidUTF8(string(b), "�"), "\r")
	if len(l) > c.max {
		l = l[:c.max]
	}
	l = stripControl(l)
	if c.emit != nil {
		c.emit(l)
	} else {
		c.lines = append(c.lines, l)
	}
}

func (c *lineCollector) finish() {
	if len(c.buf) > 0 {
		c.flush(c.buf)
		c.buf = nil
	}
}

// stripControl removes ANSI escape sequences and control characters.
func stripControl(s string) string {
	if !strings.ContainsAny(s, "\x1b\x00\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0c\x0e\x0f\x7f") {
		return s
	}
	return strings.Join(console.CleanLines([]byte(s), len(s)+16), "")
}

func (b *dockerBackend) Snapshot(ctx context.Context, n int) ([]string, error) {
	info, err := b.inspect(ctx)
	if err != nil {
		return nil, b.friendly(err)
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {strconv.Itoa(n)}}
	resp, err := b.do(ctx, 20*time.Second, http.MethodGet, b.containerPath("/logs"), q, nil)
	if err != nil {
		return nil, b.friendly(err)
	}
	defer resp.Body.Close()
	col := &lineCollector{max: b.deps.Config.Get().Console.MaxLineBytes}
	// Bound memory even if the daemon sends more than requested.
	if err := copyLogs(io.LimitReader(resp.Body, 16<<20), info.Config.Tty, col); err != nil {
		return nil, fmt.Errorf("read container logs: %w", err)
	}
	col.finish()
	return trimLines(col.lines, n), nil
}

func (b *dockerBackend) friendly(err error) error {
	var de *dockerError
	if errors.As(err, &de) {
		if de.Status == http.StatusNotFound {
			return fmt.Errorf("Docker container %q does not exist", b.d.Container)
		}
		return fmt.Errorf("Docker: %s", de.Message)
	}
	return err
}

// Follow streams container logs into the console buffer. Docker ends the
// stream when the container stops, so the follower reconnects (from the
// time of the last received data) until ctx ends.
func (b *dockerBackend) Follow(ctx context.Context, opts FollowOptions) error {
	since := opts.Since
	tail := opts.Tail
	first := true
	var lastErr error
	for ctx.Err() == nil {
		info, err := b.inspect(ctx)
		if err == nil {
			q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "follow": {"1"}}
			if !since.IsZero() {
				q.Set("since", fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()))
				q.Set("tail", "all")
			} else {
				q.Set("tail", strconv.Itoa(tail))
			}
			connected := time.Now()
			var resp *http.Response
			resp, err = b.do(ctx, 0, http.MethodGet, b.containerPath("/logs"), q, nil)
			if err == nil {
				col := &lineCollector{max: b.deps.Config.Get().Console.MaxLineBytes, emit: b.deps.Console.Append}
				go func() { <-ctx.Done(); resp.Body.Close() }()
				_ = copyLogs(resp.Body, info.Config.Tty, col)
				col.finish()
				resp.Body.Close()
				// Resume after reconnecting without replaying old output.
				since = time.Now()
				if since.Sub(connected) < time.Second {
					since = connected
				}
			}
		}
		if err != nil {
			lastErr = b.friendly(err)
			if first {
				return lastErr
			}
		}
		first = false
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}

// SendInput runs the administrator-configured exec template inside the
// container (console.input = docker_exec). The argv is passed to Docker as
// an array; no shell is involved unless the administrator configured one.
func (b *dockerBackend) SendInput(ctx context.Context, line string) error {
	if b.def.InputMethod() != servers.InputDockerExec || b.def.Console.InputCommand == nil {
		return ErrUnsupported
	}
	argv, used := SubstituteCommand(b.def.Console.InputCommand, line)
	if !used {
		argv = append(argv, line)
	}
	resp, err := b.do(ctx, 15*time.Second, http.MethodPost, b.containerPath("/exec"), nil, map[string]any{
		"AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": argv,
	})
	if err != nil {
		var de *dockerError
		if errors.As(err, &de) && de.Status == http.StatusConflict {
			return ErrNotRunning
		}
		return b.friendly(err)
	}
	var created struct {
		ID string `json:"Id"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&created)
	resp.Body.Close()
	if err != nil || created.ID == "" {
		return errors.New("Docker returned an invalid exec response")
	}
	timeout := b.def.Timeouts.Command.D()
	resp, err = b.do(ctx, timeout, http.MethodPost, "/exec/"+url.PathEscape(created.ID)+"/start", nil, map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return b.friendly(err)
	}
	col := &lineCollector{max: b.deps.Config.Get().Console.MaxLineBytes}
	_ = demux(io.LimitReader(resp.Body, 256<<10), col)
	col.finish()
	resp.Body.Close()
	for _, l := range trimLines(col.lines, 50) {
		b.deps.Console.Append(l)
	}
	resp, err = b.do(ctx, 10*time.Second, http.MethodGet, "/exec/"+url.PathEscape(created.ID)+"/json", nil, nil)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var st struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st) == nil && !st.Running && st.ExitCode != 0 {
		return fmt.Errorf("the command helper exited with code %d", st.ExitCode)
	}
	return nil
}

func (b *dockerBackend) Close() {}

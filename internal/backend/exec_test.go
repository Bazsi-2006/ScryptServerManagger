package backend

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

func TestRunPassesArgumentsVerbatimWithoutShell(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	// Prints each argument on its own line, bracketed.
	script := testutil.Script(t, dir, "args.sh", `for a in "$@"; do printf '[%s]\n' "$a"; done`)
	hostile := []string{
		"; touch " + marker, "$(touch " + marker + ")", "`touch " + marker + "`", "a b  c",
		"| touch " + marker, "&& touch " + marker, "*", "--flag=va lue", "quote'\"s", "",
	}
	res, err := Run(context.Background(), RunSpec{Argv: append([]string{script}, hostile...), TailBytes: 8192})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("run: %v %+v", err, res)
	}
	var got []string
	for _, l := range strings.Split(res.Output, "\n") {
		got = append(got, strings.TrimSuffix(strings.TrimPrefix(l, "["), "]"))
	}
	if !reflect.DeepEqual(got, hostile) {
		t.Fatalf("arguments were altered:\n got %q\nwant %q", got, hostile)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("shell metacharacters in an argument were executed")
	}
}

func TestSubstituteCommandKeepsUserTextInOneArgument(t *testing.T) {
	tmpl := &servers.Command{Command: "/usr/bin/rcon", Args: []string{"-H", "localhost", "{command}", "prefix:{command}"}}
	line := `say hi; rm -rf / && $(reboot) "x" --password=1`
	argv, used := SubstituteCommand(tmpl, line)
	if !used || len(argv) != 5 {
		t.Fatalf("argv %q used=%v", argv, used)
	}
	if argv[3] != line || argv[4] != "prefix:"+line {
		t.Fatalf("substitution changed the text: %q", argv)
	}
	argv, used = SubstituteCommand(&servers.Command{Command: "/bin/x", Args: []string{"fixed"}}, line)
	if used || len(argv) != 2 {
		t.Fatalf("no placeholder: %q used=%v", argv, used)
	}
}

func TestRunExitCodesTimeoutAndMissingExecutable(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(context.Background(), RunSpec{Argv: []string{testutil.Script(t, dir, "fail.sh", "echo oops >&2; exit 7")}})
	if err != nil || res.ExitCode != 7 || res.Output != "oops" {
		t.Fatalf("exit code: %v %+v", err, res)
	}
	// A hung script (and the child it spawned) is killed with its group.
	pidFile := filepath.Join(dir, "child.pid")
	hang := testutil.Script(t, dir, "hang.sh", "sleep 300 &\necho $! > "+pidFile+"\nwait")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = Run(ctx, RunSpec{Argv: []string{hang}})
	if !errors.Is(err, ErrTimeout) || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
	pid, perr := readPIDFile(pidFile)
	if perr != nil {
		t.Fatal(perr)
	}
	testutil.Eventually(t, 3*time.Second, "orphaned child to be killed", func() bool { return !pidAlive(pid) })

	_, err = Run(context.Background(), RunSpec{Argv: []string{filepath.Join(dir, "missing.sh")}})
	if err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("missing executable: %v", err)
	}
	noexec := filepath.Join(dir, "noexec.sh")
	os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644)
	if os.Geteuid() != 0 {
		if _, err = Run(context.Background(), RunSpec{Argv: []string{noexec}}); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("non-executable file: %v", err)
		}
	}
	if _, err := Run(context.Background(), RunSpec{}); err == nil {
		t.Fatal("empty argv must fail")
	}
}

func TestChildEnvironmentIsScrubbed(t *testing.T) {
	t.Setenv("SM_JWT_SECRET", "super-secret-signing-key-0123456789")
	t.Setenv("SM_SMTP_PASSWORD", "smtp-secret")
	t.Setenv("NOTIFY_SOCKET", "/run/x")
	t.Setenv("KEEP_ME", "yes")
	def := &servers.Definition{ID: "mc", Name: "MC", Env: map[string]string{"JAVA_OPTS": "-Xmx1G"}}
	res, err := Run(context.Background(), RunSpec{Argv: []string{"env"}, Env: ServerEnv(def), TailBytes: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"SM_JWT_SECRET", "super-secret", "SM_SMTP_PASSWORD", "smtp-secret", "NOTIFY_SOCKET"} {
		if strings.Contains(res.Output, leak) {
			t.Errorf("child environment leaks %s", leak)
		}
	}
	for _, want := range []string{"KEEP_ME=yes", "JAVA_OPTS=-Xmx1G", "SM_SERVER_ID=mc"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("child environment misses %s", want)
		}
	}
}

func TestCheckPortTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	check := &servers.PortCheck{Host: "127.0.0.1", Port: port, Protocol: "tcp", Timeout: servers.Duration(time.Second)}
	if ok, err := CheckPort(context.Background(), check); !ok || err != nil {
		t.Fatalf("open port: %v %v", ok, err)
	}
	ln.Close()
	if ok, err := CheckPort(context.Background(), check); ok || err != nil {
		t.Fatalf("closed port must be not-ready without error: %v %v", ok, err)
	}
}

func TestCheckPortUDPNeedsAReply(t *testing.T) {
	// A UDP socket that never answers is NOT ready, although it is "open".
	silent, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer silent.Close()
	check := &servers.PortCheck{Host: "127.0.0.1", Port: silent.LocalAddr().(*net.UDPAddr).Port, Protocol: "udp",
		Timeout: servers.Duration(300 * time.Millisecond), UDPPayloadHex: "ffffffff54"}
	if ok, _ := CheckPort(context.Background(), check); ok {
		t.Fatal("a silent UDP port must not count as ready")
	}
	echo, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		buf := make([]byte, 64)
		for {
			n, addr, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			if string(buf[:n]) == "\xff\xff\xff\xffT" {
				echo.WriteTo([]byte("pong"), addr)
			}
		}
	}()
	check.Port = echo.LocalAddr().(*net.UDPAddr).Port
	if ok, err := CheckPort(context.Background(), check); !ok || err != nil {
		t.Fatalf("answering UDP service: %v %v", ok, err)
	}
}

func TestCheckScriptExitConvention(t *testing.T) {
	dir := t.TempDir()
	def := &servers.Definition{ID: "x", Name: "X"}
	run := func(body string, timeout time.Duration) (bool, error) {
		s := &servers.ScriptCheck{Command: testutil.Script(t, dir, "st.sh", body), Timeout: servers.Duration(timeout)}
		return CheckScript(context.Background(), def, s)
	}
	if ok, err := run("exit 0", time.Second); !ok || err != nil {
		t.Fatalf("exit 0 = ready: %v %v", ok, err)
	}
	if ok, err := run("exit 1", time.Second); ok || err != nil {
		t.Fatalf("exit 1 = not ready, no error: %v %v", ok, err)
	}
	if ok, err := run("echo broken; exit 2", time.Second); ok || err == nil || !strings.Contains(err.Error(), "code 2") {
		t.Fatalf("exit 2 = check error: %v %v", ok, err)
	}
	if ok, err := run("sleep 30", 200*time.Millisecond); ok || err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout = check error: %v %v", ok, err)
	}
	s := &servers.ScriptCheck{Command: filepath.Join(dir, "gone.sh"), Timeout: servers.Duration(time.Second)}
	if ok, err := CheckScript(context.Background(), def, s); ok || err == nil {
		t.Fatalf("missing script = check error: %v %v", ok, err)
	}
}

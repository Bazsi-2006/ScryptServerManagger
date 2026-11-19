// Command vm-controller serves a small web interface that lets authenticated
// users start and gracefully stop specific Vexa-VM virtual machines.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/control"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/server"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/store"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/vexa"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `vm-controller - start and stop Vexa-VM virtual machines from a web page

Usage:
  vm-controller serve                [--config PATH]
  vm-controller check-config         [--config PATH] [--online]
  vm-controller user set-password    [--config PATH] [--stdin] USERNAME
  vm-controller user list            [--config PATH]
  vm-controller user revoke-sessions [--config PATH] USERNAME
  vm-controller user remove          [--config PATH] USERNAME
  vm-controller version

The configuration path defaults to $VM_CONTROLLER_CONFIG, then
` + config.DefaultPath + `.
`

// errUsage marks command-line mistakes (exit status 2).
var errUsage = errors.New("usage error")

func main() {
	err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, errUsage):
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("%w: no command given", errUsage)
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:], stderr)
	case "check-config":
		return cmdCheckConfig(args[1:], stdout, stderr)
	case "user":
		if len(args) < 2 {
			fmt.Fprint(stderr, usage)
			return fmt.Errorf("%w: 'user' needs a subcommand", errUsage)
		}
		switch args[1] {
		case "set-password":
			return cmdSetPassword(args[2:], stdin, stdout, stderr)
		case "list":
			return cmdUserList(args[2:], stdout, stderr)
		case "revoke-sessions":
			return cmdRevokeSessions(args[2:], stdout, stderr)
		case "remove":
			return cmdUserRemove(args[2:], stdout, stderr)
		}
		return fmt.Errorf("%w: unknown user command %q", errUsage, args[1])
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, "vm-controller", version)
		return nil
	case "help", "-h", "--help", "-help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	fmt.Fprint(stderr, usage)
	return fmt.Errorf("%w: unknown command %q", errUsage, args[0])
}

// ---- flag helpers ----------------------------------------------------------

type common struct {
	configPath string
}

func newFlags(name string, stderr io.Writer) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := &common{}
	def := os.Getenv("VM_CONTROLLER_CONFIG")
	if def == "" {
		def = config.DefaultPath
	}
	fs.StringVar(&c.configPath, "config", def, "path to the JSON configuration")
	return fs, c
}

// parseArgs parses flags that may appear before or after positional args.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%w: %v", errUsage, err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func oneUsername(fs *flag.FlagSet, args []string) (string, error) {
	pos, err := parseArgs(fs, args)
	if err != nil {
		return "", err
	}
	if len(pos) != 1 {
		return "", fmt.Errorf("%w: expected exactly one USERNAME", errUsage)
	}
	return strings.ToLower(strings.TrimSpace(pos[0])), nil
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// ---- serve -----------------------------------------------------------------

func cmdServe(args []string, stderr io.Writer) error {
	fs, c := newFlags("serve", stderr)
	if pos, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return fmt.Errorf("%w: serve takes no arguments", errUsage)
	}
	log := newLogger(stderr)
	slog.SetDefault(log)

	cfg, err := config.Load(c.configPath)
	if err != nil {
		return err
	}
	for _, w := range cfg.Warnings {
		log.Warn("configuration: " + w)
	}
	key, err := cfg.LoadAPIKey()
	if err != nil {
		return err
	}
	client, err := vexa.New(vexa.Options{
		BaseURL: cfg.Vexa.BaseURL, APIKey: key, CAFile: cfg.Vexa.CAFile,
		Timeout: cfg.Vexa.Timeout, UserAgent: "vm-controller/" + version,
	})
	if err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		log.Warn("running as root is not recommended; use the provided systemd unit with a dedicated user")
	}
	st, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer st.Close()

	ctl := control.New(client, cfg.VMs, control.Options{
		CacheTTL:     cfg.Vexa.StatusCacheTTL,
		StartTimeout: cfg.Vexa.StartTimeout,
		StopTimeout:  cfg.Vexa.StopTimeout,
		TrackJobs:    cfg.Vexa.TrackJobs,
		Logger:       log,
	})
	srv, err := server.New(cfg, st, ctl, server.Options{Logger: log})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Prepare(ctx); err != nil {
		return err
	}
	warnMissingPasswords(ctx, cfg, st, log)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Listen, err)
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cfg.Vexa.Timeout*3 + 15*time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	go srv.RunMaintenance(ctx)
	go reportVexa(ctx, client, cfg, log)

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	log.Info("vm-controller started", "version", version, "listen", ln.Addr().String(),
		"base_path", basePath(cfg), "mode", cfg.Mode, "vms", len(cfg.VMs), "vexa", cfg.Vexa.BaseURL.Redacted())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return nil
}

func basePath(cfg *config.Config) string {
	if cfg.BasePath == "" {
		return "/"
	}
	return cfg.BasePath + "/"
}

func warnMissingPasswords(ctx context.Context, cfg *config.Config, st *store.Store, log *slog.Logger) {
	for _, u := range cfg.Usernames() {
		if !cfg.UserActive(u) {
			continue
		}
		if _, err := st.PasswordHash(ctx, u); errors.Is(err, store.ErrNotFound) {
			log.Warn("user has no password yet and cannot sign in; run: vm-controller user set-password "+u, "user", u)
		}
	}
}

// reportVexa checks once at startup that every configured VM is visible to
// the API key, so configuration mistakes show up in the log immediately.
func reportVexa(ctx context.Context, client *vexa.Client, cfg *config.Config, log *slog.Logger) {
	ok := 0
	for _, vm := range cfg.VMs {
		cctx, cancel := context.WithTimeout(ctx, cfg.Vexa.Timeout)
		got, err := client.GetVM(cctx, vm.ID)
		cancel()
		switch {
		case err == nil:
			ok++
			log.Info("Vexa-VM: VM found", "vm", vm.ID, "name", vm.Name, "vexa_name", got.Name, "state", got.State)
		case vexa.KindOf(err) == vexa.KindNotFound:
			log.Error("Vexa-VM: VM not found; check vms[].id in the configuration", "vm", vm.ID, "name", vm.Name)
		case vexa.KindOf(err) == vexa.KindAuth:
			log.Error("Vexa-VM rejected the API key; it needs the vms:read and vms:power scopes", "error", err)
			return
		default:
			log.Warn("Vexa-VM: could not check VM", "vm", vm.ID, "error", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
	log.Info("Vexa-VM startup check finished", "found", ok, "configured", len(cfg.VMs))
}

// ---- check-config ----------------------------------------------------------

func cmdCheckConfig(args []string, stdout, stderr io.Writer) error {
	fs, c := newFlags("check-config", stderr)
	online := fs.Bool("online", false, "also contact Vexa-VM and check every configured VM")
	if pos, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return fmt.Errorf("%w: check-config takes no arguments", errUsage)
	}
	cfg, err := config.Load(c.configPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Configuration %s is valid.\n\n", cfg.Path)
	fmt.Fprintf(stdout, "Mode:      %s\nListen:    %s\nBase path: %s\nDatabase:  %s\nVexa-VM:   %s\n",
		cfg.Mode, cfg.Listen, basePath(cfg), cfg.Database, cfg.Vexa.BaseURL.Redacted())
	if cfg.Vexa.APIKeyFile != "" {
		fmt.Fprintf(stdout, "API key:   file %s\n", cfg.Vexa.APIKeyFile)
	} else {
		fmt.Fprintf(stdout, "API key:   environment variable %s\n", cfg.Vexa.APIKeyEnv)
	}

	fmt.Fprintln(stdout, "\nVMs:")
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ID\tNAME\tSTOP")
	for _, vm := range cfg.VMs {
		stop := "disabled"
		if vm.AllowStop {
			stop = "allowed"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", vm.ID, vm.Name, stop)
	}
	tw.Flush()

	fmt.Fprintln(stdout, "\nUsers:")
	tw = tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, u := range cfg.Usernames() {
		var parts []string
		for _, v := range cfg.VisibleVMs(u) {
			parts = append(parts, fmt.Sprintf("%s=%s", v.VM.Name, v.Permission))
		}
		status := ""
		if _, disabled := cfg.UserDefined(u); disabled {
			status = " (disabled)"
		}
		if len(parts) == 0 {
			parts = []string{"no VMs"}
		}
		fmt.Fprintf(tw, "  %s%s\t%s\n", u, status, strings.Join(parts, ", "))
	}
	tw.Flush()

	if len(cfg.Warnings) > 0 {
		fmt.Fprintln(stdout, "\nWarnings:")
		for _, w := range cfg.Warnings {
			fmt.Fprintf(stdout, "  - %s\n", w)
		}
	}
	if !*online {
		return nil
	}

	fmt.Fprintln(stdout, "\nChecking Vexa-VM...")
	key, err := cfg.LoadAPIKey()
	if err != nil {
		return err
	}
	client, err := vexa.New(vexa.Options{
		BaseURL: cfg.Vexa.BaseURL, APIKey: key, CAFile: cfg.Vexa.CAFile,
		Timeout: cfg.Vexa.Timeout, UserAgent: "vm-controller/" + version,
	})
	if err != nil {
		return err
	}
	failed := 0
	tw = tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  VM\tRESULT")
	for _, vm := range cfg.VMs {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Vexa.Timeout)
		got, err := client.GetVM(ctx, vm.ID)
		cancel()
		if err != nil {
			failed++
			fmt.Fprintf(tw, "  %s\t%s\n", vm.Name, describeVexaError(err))
			continue
		}
		fmt.Fprintf(tw, "  %s\tok: Vexa name %q, state %s\n", vm.Name, got.Name, got.State)
	}
	tw.Flush()
	if failed > 0 {
		return fmt.Errorf("%d of %d VMs could not be checked", failed, len(cfg.VMs))
	}
	fmt.Fprintln(stdout, "All VMs are reachable. (Power permissions are only exercised by real start/stop requests.)")
	return nil
}

func describeVexaError(err error) string {
	switch vexa.KindOf(err) {
	case vexa.KindNotFound:
		return "NOT FOUND in Vexa-VM (check the id)"
	case vexa.KindAuth:
		return "REJECTED: the API key is invalid or lacks vms:read (" + err.Error() + ")"
	case vexa.KindUnavailable:
		return "UNREACHABLE: " + err.Error()
	default:
		return "ERROR: " + err.Error()
	}
}

// ---- user commands ---------------------------------------------------------

// openStoreForCLI refuses to run as root against a database directory owned
// by another user: files SQLite creates would then belong to root and the
// service could no longer write to its own database.
func openStoreForCLI(cfg *config.Config) (*store.Store, error) {
	if os.Geteuid() == 0 {
		dir := filepath.Dir(cfg.Database)
		if uid, ok := ownerUID(dir); ok && uid != 0 {
			return nil, fmt.Errorf("refusing to modify %s as root because its directory belongs to uid %d; "+
				"run the command as that user instead, e.g.: sudo -u vm-controller vm-controller user ...", cfg.Database, uid)
		}
	}
	return store.Open(cfg.Database)
}

func cmdSetPassword(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs, c := newFlags("user set-password", stderr)
	fromStdin := fs.Bool("stdin", false, "read the password from standard input (one line) instead of prompting")
	username, err := oneUsername(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(c.configPath)
	if err != nil {
		return err
	}
	defined, disabled := cfg.UserDefined(username)
	if !defined {
		return fmt.Errorf("user %q is not defined in %s (mode %s); add the user to the configuration first", username, cfg.Path, cfg.Mode)
	}
	pw, err := readNewPassword(username, *fromStdin, stdin, stderr)
	if err != nil {
		return err
	}
	if err := auth.ValidateNewPassword(pw); err != nil {
		return err
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	st, err := openStoreForCLI(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.SetPassword(context.Background(), username, hash, time.Now()); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Password for %s updated. Existing sessions of %s were signed out.\n", username, username)
	if disabled {
		fmt.Fprintf(stdout, "Note: %s is disabled in the configuration and cannot sign in until enabled.\n", username)
	}
	return nil
}

func readNewPassword(username string, fromStdin bool, stdin io.Reader, stderr io.Writer) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(io.LimitReader(stdin, auth.MaxPasswordLen+2)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	f, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", errors.New("standard input is not a terminal; use --stdin to read the password from standard input")
	}
	fmt.Fprintf(stderr, "New password for %s (min. %d characters): ", username, auth.MinPasswordLen)
	first, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(stderr, "Repeat the password: ")
	second, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(stderr)
	if err != nil {
		return "", err
	}
	if string(first) != string(second) {
		return "", errors.New("the passwords do not match")
	}
	return string(first), nil
}

func cmdUserList(args []string, stdout, stderr io.Writer) error {
	fs, c := newFlags("user list", stderr)
	if pos, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return fmt.Errorf("%w: user list takes no arguments", errUsage)
	}
	cfg, err := config.Load(c.configPath)
	if err != nil {
		return err
	}
	st, err := openStoreForCLI(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	creds, err := st.ListCredentials(context.Background())
	if err != nil {
		return err
	}
	byName := map[string]store.Credential{}
	for _, cr := range creds {
		byName[cr.Username] = cr
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "USERNAME\tSTATUS\tPASSWORD\tSESSIONS\tVMS")
	for _, u := range cfg.Usernames() {
		status := "active"
		if _, disabled := cfg.UserDefined(u); disabled {
			status = "disabled"
		}
		pw, sessions := "not set", "0"
		if cr, ok := byName[u]; ok {
			pw = "set " + cr.UpdatedAt.Format("2006-01-02")
			sessions = fmt.Sprint(cr.Sessions)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", u, status, pw, sessions, len(cfg.VisibleVMs(u)))
	}
	var orphans []string
	for name := range byName {
		if ok, _ := cfg.UserDefined(name); !ok {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	for _, name := range orphans {
		fmt.Fprintf(tw, "%s\tnot in config\tset\t%d\t-\n", name, byName[name].Sessions)
	}
	tw.Flush()
	if len(orphans) > 0 {
		fmt.Fprintln(stdout, "\nUsers marked \"not in config\" cannot sign in. Delete their stored password with: vm-controller user remove USERNAME")
	}
	return nil
}

func cmdRevokeSessions(args []string, stdout, stderr io.Writer) error {
	fs, c := newFlags("user revoke-sessions", stderr)
	username, err := oneUsername(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(c.configPath)
	if err != nil {
		return err
	}
	st, err := openStoreForCLI(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.DeleteUserSessions(context.Background(), username)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Signed out %d session(s) of %s.\n", n, username)
	return nil
}

func cmdUserRemove(args []string, stdout, stderr io.Writer) error {
	fs, c := newFlags("user remove", stderr)
	username, err := oneUsername(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(c.configPath)
	if err != nil {
		return err
	}
	st, err := openStoreForCLI(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	ok, err := st.RemoveCredentials(context.Background(), username)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(stdout, "No stored password for %s.\n", username)
		return nil
	}
	fmt.Fprintf(stdout, "Removed the stored password and all sessions of %s.\n", username)
	if defined, _ := cfg.UserDefined(username); defined {
		fmt.Fprintf(stdout, "%s is still in the configuration but cannot sign in until a password is set again.\n", username)
	}
	return nil
}

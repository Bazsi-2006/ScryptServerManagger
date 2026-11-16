// Command server-manager is a self-hosted, universal Linux server manager.
//
// Usage: server-manager <command> [flags]
//
//	serve           run the web application (default)
//	init            write a default configuration and create directories
//	check-config    validate the configuration and server definitions
//	migrate         apply database migrations and exit
//	admin create    create an administrator account (first-run setup)
//	user list       list accounts
//	user set-password  reset an account's password
//	version         print the version
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/app"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/auth"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/db"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/servers"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const defaultConfig = "/etc/server-manager/config.json"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "init":
		err = cmdInit(args)
	case "check-config":
		err = cmdCheck(args)
	case "migrate":
		err = cmdMigrate(args)
	case "admin":
		err = cmdAdmin(args)
	case "user":
		err = cmdUser(args)
	case "version", "--version":
		fmt.Println("server-manager", version)
	case "help", "--help", "-h":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `server-manager - universal Linux server manager

Usage:
  server-manager [serve] [--config FILE]     run the web application
  server-manager init [--config FILE]        write a default configuration, create directories
  server-manager check-config [--config FILE]  validate configuration and server definitions
  server-manager migrate [--config FILE]     apply database migrations
  server-manager admin create --username NAME [--email ADDR] [--display-name NAME] [--password-stdin]
  server-manager user list
  server-manager user set-password --username NAME [--password-stdin]
  server-manager version

The configuration file defaults to $SM_CONFIG or `+defaultConfig+`.
Secrets are read from the environment: SM_JWT_SECRET, SM_SMTP_PASSWORD
(or from the files named in the configuration).
`)
}

func newFlags(name string) (*flag.FlagSet, *string) {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	def := os.Getenv("SM_CONFIG")
	if def == "" {
		def = defaultConfig
	}
	path := fl.String("config", def, "path to the configuration file")
	return fl, path
}

func cmdServe(args []string) error {
	fl, path := newFlags("serve")
	if err := fl.Parse(args); err != nil {
		return err
	}
	return app.Run(*path, version)
}

// cmdInit creates a ready-to-edit installation skeleton.
func cmdInit(args []string) error {
	fl, path := newFlags("init")
	dataDir := fl.String("data-dir", "", "data directory to write into the new configuration (default /var/lib/server-manager)")
	listen := fl.String("listen", "", "listen address to write into the new configuration (default 127.0.0.1:8080)")
	dev := fl.Bool("development", false, "create a development-mode configuration")
	if err := fl.Parse(args); err != nil {
		return err
	}
	abs, err := filepath.Abs(*path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err == nil {
		fmt.Println("configuration already exists:", abs)
	} else {
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			return err
		}
		c := config.Default()
		if *dataDir != "" {
			c.Paths.DataDir = *dataDir
		}
		if *listen != "" {
			c.Listen = *listen
		}
		if *dev {
			c.Mode = config.ModeDevelopment
		}
		data, _ := json.MarshalIndent(c, "", "  ")
		// Validate what we are about to write.
		if _, err := config.Parse(data, filepath.Dir(abs)); err != nil {
			return err
		}
		if err := config.WriteFileAtomic(abs, append(data, '\n'), 0o600); err != nil {
			return err
		}
		fmt.Println("wrote", abs)
	}
	h, err := config.Load(abs)
	if err != nil {
		return err
	}
	c := h.Get()
	if err := c.EnsureDirs(); err != nil {
		return err
	}
	if _, err := c.JWTSecret(); err != nil {
		return err
	}
	store, err := db.Open(c.Paths.Database)
	if err != nil {
		return err
	}
	store.Close()
	fmt.Println("data directory:    ", c.Paths.DataDir)
	fmt.Println("server definitions:", c.Paths.ServersDir)
	fmt.Println("database:          ", c.Paths.Database)
	fmt.Println("\nNext: create the first administrator with")
	fmt.Printf("  server-manager admin create --config %s --username <name>\n", abs)
	return nil
}

func cmdMigrate(args []string) error {
	fl, path := newFlags("migrate")
	if err := fl.Parse(args); err != nil {
		return err
	}
	h, err := config.Load(*path)
	if err != nil {
		return err
	}
	if err := h.Get().EnsureDirs(); err != nil {
		return err
	}
	store, err := db.Open(h.Get().Paths.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	v, err := store.SchemaVersion(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("database schema is at version %d\n", v)
	return nil
}

// cmdCheck validates everything that can be validated without starting.
func cmdCheck(args []string) error {
	fl, path := newFlags("check-config")
	if err := fl.Parse(args); err != nil {
		return err
	}
	problems, warnings := 0, 0
	warn := func(format string, a ...any) { warnings++; fmt.Printf("warning: "+format+"\n", a...) }
	fail := func(format string, a ...any) { problems++; fmt.Printf("ERROR:   "+format+"\n", a...) }

	h, err := config.Load(*path)
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			for _, p := range ve.Problems {
				fail("%s", p)
			}
			return fmt.Errorf("%d problem(s) in %s", problems, *path)
		}
		return err
	}
	c := h.Get()
	fmt.Println("configuration:", h.Path(), "- OK")
	if c.Development() {
		warn("mode is \"development\": emails are printed to the log; do not use in production")
	}
	if c.PublicURL == "" {
		warn("public_url is not set: invitation and verification links cannot be generated in production")
	} else if strings.HasPrefix(c.PublicURL, "http://") && !c.Development() {
		warn("public_url uses plain http: cookies will not be marked Secure")
	}
	if !c.SMTP.Configured() {
		warn("SMTP is not configured: invitations must be shared as links by an administrator")
	} else if _, err := c.SMTPPassword(); err != nil {
		fail("%v", err)
	}
	if host := c.Listen; !strings.HasPrefix(host, "127.0.0.1:") && !strings.HasPrefix(host, "localhost:") && !strings.HasPrefix(host, "[::1]:") {
		warn("listen address %s is not loopback: put a TLS-terminating reverse proxy in front or restrict access", host)
	}
	for _, d := range []struct{ name, path string }{{"data_dir", c.Paths.DataDir}, {"servers_dir", c.Paths.ServersDir}} {
		if st, err := os.Stat(d.path); err != nil {
			warn("paths.%s %s does not exist yet (it is created on start)", d.name, d.path)
		} else if !st.IsDir() {
			fail("paths.%s %s is not a directory", d.name, d.path)
		}
	}
	if st, err := os.Stat(c.Auth.JWTSecretFile); err == nil && st.Mode().Perm()&0o077 != 0 && os.Getenv("SM_JWT_SECRET") == "" {
		fail("JWT secret file %s is accessible by group/others (chmod 600)", c.Auth.JWTSecretFile)
	}
	if st, err := os.Stat(c.Paths.Database); err == nil && st.Mode().Perm()&0o077 != 0 {
		warn("database %s is accessible by group/others (chmod 600 recommended)", c.Paths.Database)
	}

	store := servers.NewStore(c.Paths.ServersDir)
	if err := store.Load(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Println("server definitions: none (directory does not exist yet)")
		} else {
			fail("%v", err)
		}
	}
	for name, msg := range store.Invalid() {
		fail("servers/%s: %s", name, msg)
	}
	used := map[string]bool{}
	for _, d := range store.List() {
		used[d.Backend] = true
		for _, w := range definitionWarnings(d) {
			warn("servers/%s.json: %s", d.ID, w)
		}
	}
	fmt.Printf("server definitions: %d valid, %d invalid\n", len(store.List()), len(store.Invalid()))
	tool := func(backend, name, bin string) {
		if used[backend] {
			if _, err := exec.LookPath(bin); err != nil {
				fail("%s servers are configured but %q (tools.%s) was not found", backend, bin, name)
			}
		}
	}
	tool(servers.BackendTmux, "tmux", c.Tools.Tmux)
	tool(servers.BackendSystemd, "systemctl", c.Tools.Systemctl)
	tool(servers.BackendSystemd, "journalctl", c.Tools.Journalctl)
	if used[servers.BackendDocker] {
		if _, err := os.Stat(c.Tools.DockerSocket); err != nil {
			warn("docker servers are configured but the socket %s is not accessible", c.Tools.DockerSocket)
		}
	}
	fmt.Printf("\n%d error(s), %d warning(s)\n", problems, warnings)
	if problems > 0 {
		return errors.New("configuration check failed")
	}
	return nil
}

func executableWarning(what, cmd string) string {
	if cmd == "" {
		return ""
	}
	if !strings.Contains(cmd, "/") {
		if _, err := exec.LookPath(cmd); err != nil {
			return fmt.Sprintf("%s %q was not found in PATH", what, cmd)
		}
		return ""
	}
	st, err := os.Stat(cmd)
	switch {
	case err != nil:
		return fmt.Sprintf("%s %s does not exist or is not accessible", what, cmd)
	case st.IsDir():
		return fmt.Sprintf("%s %s is a directory", what, cmd)
	case st.Mode().Perm()&0o111 == 0:
		return fmt.Sprintf("%s %s is not executable (chmod +x)", what, cmd)
	}
	return ""
}

func definitionWarnings(d *servers.Definition) []string {
	var out []string
	add := func(s string) {
		if s != "" {
			out = append(out, s)
		}
	}
	if d.WorkingDir != "" {
		if st, err := os.Stat(d.WorkingDir); err != nil || !st.IsDir() {
			add(fmt.Sprintf("working_dir %s does not exist or is not accessible", d.WorkingDir))
		}
	}
	if c := d.Custom; c != nil {
		add(executableWarning("start command", c.Start.Command))
		if c.Stop != nil {
			add(executableWarning("stop command", c.Stop.Command))
		}
	}
	if t := d.Tmux; t != nil && t.Start != nil {
		add(executableWarning("tmux start command", t.Start.Command))
	}
	if s := d.Readiness.Script; s != nil {
		add(executableWarning("status script", s.Command))
	}
	if d.Console.InputCommand != nil && d.Console.Input == servers.InputCommand {
		add(executableWarning("console input command", d.Console.InputCommand.Command))
	}
	if b := d.Backup; b != nil {
		if b.Command != nil {
			add(executableWarning("backup command", b.Command.Command))
		}
		if st, err := os.Stat(b.Directory); err != nil || !st.IsDir() {
			add(fmt.Sprintf("backup directory %s does not exist or is not accessible", b.Directory))
		}
	}
	return out
}

func openStore(path string) (*config.Config, *db.Store, error) {
	h, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	c := h.Get()
	if err := c.EnsureDirs(); err != nil {
		return nil, nil, err
	}
	store, err := db.Open(c.Paths.Database)
	if err != nil {
		return nil, nil, err
	}
	return c, store, nil
}

// readPassword obtains a password without echoing it. With --password-stdin
// it reads one line from standard input (for automation).
func readPassword(fromStdin bool, minLen int) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no password on standard input")
		}
		pw := strings.TrimRight(line, "\r\n")
		if len([]rune(pw)) < minLen {
			return "", fmt.Errorf("the password must be at least %d characters long", minLen)
		}
		return pw, nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("standard input is not a terminal; use --password-stdin to supply the password")
	}
	for {
		fmt.Fprintf(os.Stderr, "Password (min. %d characters): ", minLen)
		a, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if len([]rune(string(a))) < minLen {
			fmt.Fprintln(os.Stderr, "Too short, try again.")
			continue
		}
		fmt.Fprint(os.Stderr, "Repeat password: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			fmt.Fprintln(os.Stderr, "Passwords do not match, try again.")
			continue
		}
		return string(a), nil
	}
}

func params(c *config.Config) auth.Params {
	return auth.Params{MemoryKiB: c.Auth.Argon2.MemoryKiB, Iterations: c.Auth.Argon2.Iterations, Parallelism: c.Auth.Argon2.Parallelism}
}

// cmdAdmin implements the secure first-run bootstrap: it must be run by
// someone with shell access to the host, so there are never default
// credentials and no web-based setup that a stranger could reach first.
func cmdAdmin(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: server-manager admin create --username NAME [--email ADDR] [--display-name NAME] [--password-stdin]")
	}
	fl, path := newFlags("admin create")
	username := fl.String("username", "", "login name (required)")
	email := fl.String("email", "", "email address (optional)")
	display := fl.String("display-name", "", "display name (defaults to the username)")
	stdin := fl.Bool("password-stdin", false, "read the password from standard input")
	if err := fl.Parse(args[1:]); err != nil {
		return err
	}
	if !app.ValidUsername(*username) {
		return errors.New("--username must be 3-32 characters: letters, digits, '.', '_' or '-'")
	}
	c, store, err := openStore(*path)
	if err != nil {
		return err
	}
	defer store.Close()
	pw, err := readPassword(*stdin, c.Auth.MinPasswordLength)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(pw, params(c))
	if err != nil {
		return err
	}
	u := &db.User{Username: *username, DisplayName: *display, Email: strings.ToLower(strings.TrimSpace(*email)), PasswordHash: hash, IsAdmin: true}
	if u.DisplayName == "" {
		u.DisplayName = u.Username
	}
	if err := store.CreateUser(context.Background(), u); err != nil {
		if errors.Is(err, db.ErrConflict) {
			return errors.New("an account with that username or email already exists")
		}
		return err
	}
	_ = store.InsertAudit(context.Background(), &db.AuditEntry{Time: time.Now(), Username: "cli", Action: "USER_CREATE_CLI", Result: "SUCCESS",
		Details: map[string]any{"target_user": u.Username, "admin": true}})
	fmt.Printf("administrator %q created\n", u.Username)
	return nil
}

func cmdUser(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: server-manager user list | user set-password --username NAME [--password-stdin]")
	}
	switch args[0] {
	case "list":
		fl, path := newFlags("user list")
		if err := fl.Parse(args[1:]); err != nil {
			return err
		}
		_, store, err := openStore(*path)
		if err != nil {
			return err
		}
		defer store.Close()
		users, err := store.ListUsers(context.Background())
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tUSERNAME\tADMIN\tDISABLED\tEMAIL")
		for _, u := range users {
			fmt.Fprintf(tw, "%d\t%s\t%v\t%v\t%s\n", u.ID, u.Username, u.IsAdmin, u.Disabled, u.Email)
		}
		return tw.Flush()
	case "set-password":
		fl, path := newFlags("user set-password")
		username := fl.String("username", "", "login name (required)")
		stdin := fl.Bool("password-stdin", false, "read the password from standard input")
		if err := fl.Parse(args[1:]); err != nil {
			return err
		}
		c, store, err := openStore(*path)
		if err != nil {
			return err
		}
		defer store.Close()
		ctx := context.Background()
		u, err := store.UserByUsername(ctx, *username)
		if err != nil {
			return fmt.Errorf("no account named %q", *username)
		}
		pw, err := readPassword(*stdin, c.Auth.MinPasswordLength)
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw, params(c))
		if err != nil {
			return err
		}
		if _, err := store.UpdateUser(ctx, u.ID, db.UserUpdate{PasswordHash: &hash}); err != nil {
			return err
		}
		// Existing sessions were established with the old password.
		n, _ := store.RevokeUserSessions(ctx, u.ID, "", time.Now())
		_ = store.InsertAudit(ctx, &db.AuditEntry{Time: time.Now(), Username: "cli", Action: "PASSWORD_RESET_CLI", Result: "SUCCESS",
			Details: map[string]any{"target_user": u.Username}})
		fmt.Printf("password updated for %q (%d session(s) revoked)\n", u.Username, len(n))
		return nil
	}
	return fmt.Errorf("unknown user command %q", args[0])
}

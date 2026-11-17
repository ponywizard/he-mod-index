package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/term"
)

type userOptions struct {
	name, role, defaultRole string
	wizard                  bool
}

const cliUsage = `Usage:
  he-mod-index                         Start the server
  he-mod-index ingest --help           Archive import and thread polling
  he-mod-index admin                   Admin account wizard
  he-mod-index user                    Account wizard
  he-mod-index user --name NAME --role ROLE

Roles: user, reviewer, admin.
Without ACCOUNT_PASSWORD, account commands prompt in a terminal.
For scripts, supply --name and set ACCOUNT_PASSWORD in the environment.
All commands use DATA_DIR (default: data).
`

// A nil command means serve. Parse help and invalid arguments before opening data.
func parseCLI(args []string, out io.Writer) (*userOptions, error) {
	if len(args) == 0 {
		return nil, nil
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(out, cliUsage)
		return nil, flag.ErrHelp
	}
	if args[0] != "user" && args[0] != "admin" {
		return nil, fmt.Errorf("unknown command %q; use --help", args[0])
	}
	options := &userOptions{wizard: len(args) == 1}
	if args[0] == "admin" {
		options.defaultRole = "admin"
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&options.name, "name", "", "username (prompted when omitted)")
	fs.StringVar(&options.role, "role", "", "user, reviewer, or admin (default: existing role)")
	fs.Usage = func() { fmt.Fprint(out, cliUsage); fmt.Fprintln(out, "\nAccount flags:"); fs.PrintDefaults() }
	if err := fs.Parse(args[1:]); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, errors.New("unexpected arguments; use user --help")
	}
	if options.name != "" {
		if err := validateUsername(options.name); err != nil {
			return nil, err
		}
	}
	if options.role != "" && options.role != "user" && options.role != "reviewer" && options.role != "admin" {
		return nil, errors.New("role must be user, reviewer, or admin")
	}
	return options, nil
}

func defaultAccountRole(name, fallback string) string {
	if fallback != "" {
		return fallback
	}
	switch strings.ToLower(name) {
	case "devanon":
		return "reviewer"
	default:
		return "user"
	}
}

func (a *app) accountRole(name string) (string, bool, error) {
	var role string
	err := a.db.QueryRow("SELECT role FROM users WHERE username=?", name).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return role, err == nil, err
}

func (a *app) runUserCommand(options userOptions, password string, in *os.File, out io.Writer) error {
	interactive := options.wizard || options.name == "" || password == ""
	if !interactive {
		role, exists, err := a.accountRole(options.name)
		if err != nil {
			return err
		}
		if options.role != "" {
			role = options.role
		} else if !exists {
			role = defaultAccountRole(options.name, options.defaultRole)
		}
		if err = a.provisionUser(options.name, password, role); err != nil {
			return err
		}
		fmt.Fprintf(out, "Updated user %s (%s). Existing sessions and tokens revoked.\n", options.name, role)
		return nil
	}
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("account wizard requires a terminal; for scripts, supply --name and set ACCOUNT_PASSWORD")
	}
	state, err := term.GetState(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, state)
	interrupts := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupts)
	defer close(done)
	go func() {
		select {
		case <-interrupts:
			_ = term.Restore(fd, state)
			fmt.Fprintln(out, "\nCancelled.")
			os.Exit(130)
		case <-done:
		}
	}()
	// Terminal.ReadPassword handles Ctrl+D/Ctrl+C explicitly. Keep one reader
	// across password/confirmation prompts so pasted lines are not lost.
	passwordTerminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{in, io.Discard}, "")
	prompt := accountPrompt{input: in, out: out, secret: func() ([]byte, error) {
		state, err := term.MakeRaw(fd)
		if err != nil {
			return nil, err
		}
		defer term.Restore(fd, state)
		value, err := passwordTerminal.ReadPassword("")
		return []byte(value), err
	}}
	err = a.userWizard(options, &prompt)
	if errors.Is(err, io.EOF) {
		return errors.New("account setup cancelled")
	}
	return err
}

type accountPrompt struct {
	input  io.Reader
	out    io.Writer
	secret func() ([]byte, error)
}

func (p *accountPrompt) line(label, fallback string) (string, error) {
	if fallback != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", label, fallback)
	} else {
		fmt.Fprintf(p.out, "%s: ", label)
	}
	// Do not buffer ahead: a buffered reader could consume the following password
	// before ReadPassword disables terminal echo.
	var value []byte
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(p.input, one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			break
		}
		value = append(value, one[0])
		if len(value) > 4096 {
			return "", errors.New("input too long")
		}
	}
	text := strings.TrimSpace(string(value))
	if text == "" {
		text = fallback
	}
	return text, nil
}
func (p *accountPrompt) password(label string) (string, error) {
	fmt.Fprintf(p.out, "%s: ", label)
	value, err := p.secret()
	fmt.Fprintln(p.out)
	if err != nil {
		return "", err
	}
	defer clear(value)
	return string(value), nil
}

func (a *app) userWizard(options userOptions, p *accountPrompt) error {
	fmt.Fprintf(p.out, "Database: %s\n", filepath.Join(a.cfg.DataDir, "index.db"))
	name := options.name
	defaultName := ""
	if options.defaultRole == "admin" {
		defaultName = "admin"
	}
	for name == "" {
		value, err := p.line("Username", defaultName)
		if err != nil {
			return err
		}
		if err = validateUsername(value); err != nil {
			fmt.Fprintln(p.out, err)
			continue
		}
		name = value
	}
	role, exists, err := a.accountRole(name)
	if err != nil {
		return err
	}
	if exists {
		fmt.Fprintf(p.out, "Existing user: %s (%s)\n", name, role)
	} else {
		role = defaultAccountRole(name, options.defaultRole)
	}
	if options.role != "" {
		role = options.role
	} else {
		for {
			value, err := p.line("Role (user/reviewer/admin)", role)
			if err != nil {
				return err
			}
			if value != "user" && value != "reviewer" && value != "admin" {
				fmt.Fprintln(p.out, "Choose user, reviewer, or admin.")
				continue
			}
			role = value
			break
		}
	}
	label := "Password (10–128 bytes)"
	if exists {
		label = "New password (Enter to keep current)"
	}
	var password string
	for {
		password, err = p.password(label)
		if err != nil {
			return err
		}
		if password == "" && exists {
			break
		}
		if err = validateAccount(name, password); err != nil {
			fmt.Fprintln(p.out, err)
			continue
		}
		confirm, err := p.password("Confirm password")
		if err != nil {
			return err
		}
		if password != confirm {
			fmt.Fprintln(p.out, "Passwords do not match.")
			continue
		}
		break
	}
	if password == "" {
		changed, err := a.changeUserRole(name, role)
		if err != nil {
			return err
		}
		if !changed {
			fmt.Fprintln(p.out, "No changes.")
			return nil
		}
		fmt.Fprintf(p.out, "Updated user %s (%s). Password kept; sessions and tokens revoked.\n", name, role)
		return nil
	}
	if err = a.provisionUser(name, password, role); err != nil {
		return err
	}
	action := "Created"
	if exists {
		action = "Updated"
	}
	fmt.Fprintf(p.out, "%s user %s (%s).", action, name, role)
	if exists {
		fmt.Fprint(p.out, " Existing sessions and tokens revoked.")
	}
	fmt.Fprintln(p.out)
	return nil
}

package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
)

func runWizard(a *app, options userOptions, lines string, passwords ...string) (string, error) {
	var output bytes.Buffer
	next := 0
	prompt := &accountPrompt{input: strings.NewReader(lines), out: &output, secret: func() ([]byte, error) {
		if next >= len(passwords) {
			return nil, io.EOF
		}
		value := []byte(passwords[next])
		next++
		return value, nil
	}}
	err := a.userWizard(options, prompt)
	return output.String(), err
}

func TestWizardCreatesAccountsAndRetriesInvalidInput(t *testing.T) {
	a := testApp(t)
	password := "wizard-test-password"
	output, err := runWizard(a, userOptions{defaultRole: "admin"}, "invalid name\n\nroot\n\n", "", "short", password, "wrong-confirmation", password, password)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Username must be", "Choose user, reviewer, or admin.", "Password must be", "Passwords do not match.", "Created user ponywizard (admin).", a.cfg.DataDir} {
		if !strings.Contains(output, text) {
			t.Fatalf("missing wizard output %q: %s", text, output)
		}
	}
	if strings.Contains(output, password) || strings.Contains(output, "wrong-confirmation") {
		t.Fatal("wizard printed a password")
	}
	var role, hash string
	if err = a.db.QueryRow("SELECT role,password FROM users WHERE username='ponywizard'").Scan(&role, &hash); err != nil {
		t.Fatal(err)
	}
	if role != "admin" {
		t.Fatalf("default admin role=%s", role)
	}
	if ok, err := a.checkPassword(password, hash); err != nil || !ok {
		t.Fatal("wizard password was not saved", err)
	}
	output, err = runWizard(a, userOptions{}, "devanon\n\n", password, password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "Created user devanon (reviewer).") {
		t.Fatalf("reviewer default missing: %s", output)
	}
	if _, err = runWizard(a, userOptions{}, "ordinary_user\n\n", password, password); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT role FROM users WHERE username='ordinary_user'").Scan(&role); err != nil || role != "user" {
		t.Fatal("ordinary account defaulted to privileged role", role, err)
	}
}

func TestWizardPreservesPasswordAndRevokesOnRoleChange(t *testing.T) {
	a := testApp(t)
	c := client(a, "192.0.2.1")
	account := c.provision(t, "existing_admin", "admin")
	m := c.addMod(t, "Keep ownership")
	w := c.request("POST", "/api/tokens", map[string]string{"name": "Keep until change"}, nil)
	status(t, w, 201)
	if _, err := a.db.Exec("UPDATE users SET anonymous=1 WHERE id=?", account.ID); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := a.db.QueryRow("SELECT password FROM users WHERE id=?", account.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	output, err := runWizard(a, userOptions{name: account.Username}, "\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "No changes.") {
		t.Fatalf("expected no-op: %s", output)
	}
	status(t, c.request("GET", "/api/tokens", nil, nil), 200)
	var tokens int
	a.db.QueryRow("SELECT COUNT(*) FROM tokens WHERE user_id=?", account.ID).Scan(&tokens)
	if tokens != 1 {
		t.Fatal("no-op revoked token")
	}
	output, err = runWizard(a, userOptions{name: account.Username}, "reviewer\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "Password kept; sessions and tokens revoked.") {
		t.Fatalf("missing role-only result: %s", output)
	}
	var after, role string
	var anonymous bool
	if err = a.db.QueryRow("SELECT password,role,anonymous FROM users WHERE id=?", account.ID).Scan(&after, &role, &anonymous); err != nil {
		t.Fatal(err)
	}
	if after != before || role != "reviewer" || !anonymous {
		t.Fatal("role-only change altered other account properties")
	}
	status(t, c.request("GET", "/api/tokens", nil, nil), 401)
	a.db.QueryRow("SELECT COUNT(*) FROM tokens WHERE user_id=?", account.ID).Scan(&tokens)
	if tokens != 0 {
		t.Fatal("role change did not revoke token")
	}
	got, err := a.readMod(m.ID, 0)
	if err != nil || got.UserID != account.ID {
		t.Fatal("role change altered mod ownership", err)
	}
	c.login(t, account.Username)
	password := "wizard-changed-password"
	output, err = runWizard(a, userOptions{name: account.Username}, "\n", password, password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "(reviewer)") {
		t.Fatal("password reset defaulted to wrong role")
	}
	status(t, c.request("GET", "/api/tokens", nil, nil), 401)
	if err = a.db.QueryRow("SELECT password,role FROM users WHERE id=?", account.ID).Scan(&after, &role); err != nil {
		t.Fatal(err)
	}
	if role != "reviewer" {
		t.Fatal("password reset changed existing role")
	}
	if ok, err := a.checkPassword(password, after); err != nil || !ok {
		t.Fatal("new password was not saved", err)
	}
}

func TestWizardCancellationDoesNotWrite(t *testing.T) {
	for _, test := range []struct {
		name, lines string
		options     userOptions
		passwords   []string
	}{
		{"username EOF", "", userOptions{}, nil},
		{"role EOF", "new_user\n", userOptions{}, nil},
		{"password EOF", "new_user\n\n", userOptions{}, nil},
		{"confirmation EOF", "new_user\n\n", userOptions{}, []string{"wizard-test-password"}},
		{"mismatch then EOF", "new_user\n\n", userOptions{}, []string{"wizard-test-password", "other-password"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := testApp(t)
			if _, err := runWizard(a, test.options, test.lines, test.passwords...); !errors.Is(err, io.EOF) {
				t.Fatal("expected cancellation", err)
			}
			var count int
			if err := a.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil || count != 0 {
				t.Fatal("cancelled wizard created user", err, count)
			}
		})
	}
	a := testApp(t)
	c := client(a, "192.0.2.1")
	c.provision(t, "existing_admin", "admin")
	if _, err := runWizard(a, userOptions{name: "existing_admin"}, "user\n"); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	role, _, err := a.accountRole("existing_admin")
	if err != nil || role != "admin" {
		t.Fatal("cancelled password prompt changed role", err)
	}
	status(t, c.request("GET", "/api/tokens", nil, nil), 200)
}

func TestCLIFlagsHelpAndNoninteractiveMode(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"user", "--help"}, {"admin", "--help"}} {
		var out bytes.Buffer
		if _, err := parseCLI(args, &out); !errors.Is(err, flag.ErrHelp) || !strings.Contains(out.String(), "Admin account wizard") {
			t.Fatalf("help %v: %v %s", args, err, out.String())
		}
	}
	for _, args := range [][]string{{"unknown"}, {"user", "extra"}, {"user", "--role", "root"}, {"user", "--name", "bad name"}, {"admin", "--unknown"}} {
		if _, err := parseCLI(args, io.Discard); err == nil {
			t.Fatalf("accepted CLI arguments %v", args)
		}
	}
	if options, err := parseCLI(nil, io.Discard); err != nil || options != nil {
		t.Fatal("no-argument command should serve", err)
	}
	options, err := parseCLI([]string{"user", "--name", "script_admin", "--role", "admin"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	a := testApp(t)
	in, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var out bytes.Buffer
	if err = a.runUserCommand(*options, "script-test-password", in, &out); err != nil {
		t.Fatal(err)
	}
	if err = a.runUserCommand(userOptions{name: "script_admin"}, "script-reset-password", in, &out); err != nil {
		t.Fatal(err)
	}
	role, _, err := a.accountRole("script_admin")
	if err != nil || role != "admin" {
		t.Fatal("script reset changed omitted role", role, err)
	}
	for _, option := range []userOptions{{wizard: true}, {name: "script_admin"}} {
		if err = a.runUserCommand(option, "", in, &out); err == nil || !strings.Contains(err.Error(), "requires a terminal") {
			t.Fatal("accepted an insecure non-terminal wizard", err)
		}
	}
	if strings.Contains(out.String(), "script-test-password") || strings.Contains(out.String(), "script-reset-password") {
		t.Fatal("script mode printed a password")
	}
}

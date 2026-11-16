package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func deployFixture(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Ubuntu installer checks require Linux")
	}
	source := filepath.Join(t.TempDir(), "release files")
	destination := filepath.Join(t.TempDir(), "installation root")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		from, to string
		mode     os.FileMode
	}{{"deploy.sh", "deploy.sh", 0755}, {"deploy/he-mod-index.service", "he-mod-index.service", 0644}} {
		data, err := os.ReadFile(file.from)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(source, file.to), data, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "he-mod-index"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return source, destination
}
func runDeploy(t *testing.T, source, destination string, args ...string) (string, error) {
	t.Helper()
	command := exec.Command("bash", append([]string{filepath.Join(source, "deploy.sh")}, args...)...)
	command.Env = append(os.Environ(), "DESTDIR="+destination)
	command.Dir = t.TempDir()
	output, err := command.CombinedOutput()
	return string(output), err
}
func TestDeploymentStagesPackageAndPreservesConfiguration(t *testing.T) {
	source, destination := deployFixture(t)
	output, err := runDeploy(t, source, destination, "mods.example.org")
	if err != nil {
		t.Fatal(err, output)
	}
	for _, text := range []string{"No users or services were changed.", "https://caddyserver.com/docs/install#debian-ubuntu-raspbian", "mods.example.org {", "reverse_proxy 127.0.0.1:8080", "sudo systemctl reload caddy"} {
		if !strings.Contains(output, text) {
			t.Fatalf("missing installer output %q: %s", text, output)
		}
	}
	for path, mode := range map[string]os.FileMode{"srv/he-mod-index": 0755, "srv/he-mod-index/data": 0700, "srv/he-mod-index/he-mod-index": 0755, "etc/he-mod-index.env": 0600, "etc/systemd/system/he-mod-index.service": 0644} {
		info, err := os.Stat(filepath.Join(destination, path))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("incorrect installed path/mode", path, err)
		}
	}
	config := filepath.Join(destination, "etc/he-mod-index.env")
	contents, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "PUBLIC_URL=https://mods.example.org\n") || !strings.Contains(string(contents), "TRUSTED_PROXIES=127.0.0.1/32,::1/128\n") {
		t.Fatal("missing deployment environment", string(contents))
	}
	marker := filepath.Join(t.TempDir(), "must-not-run")
	contents = bytes.ReplaceAll(contents, []byte("PUBLIC_URL=https://mods.example.org"), []byte("PUBLIC_URL=\"https://mods.example.org\""))
	contents = append(contents, []byte("REGISTRATION_COOLDOWN=2h\nIGNORED=$(touch "+marker+")\n")...)
	if err = os.WriteFile(config, contents, 0600); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(destination, "srv/he-mod-index/data/index.db")
	saved := []byte("existing database must not change")
	if err = os.WriteFile(database, saved, 0600); err != nil {
		t.Fatal(err)
	}
	updated := []byte("#!/bin/sh\n# new release\nexit 0\n")
	if err = os.WriteFile(filepath.Join(source, "he-mod-index"), updated, 0755); err != nil {
		t.Fatal(err)
	}
	output, err = runDeploy(t, source, destination)
	if err != nil {
		t.Fatal(err, output)
	}
	for path, want := range map[string][]byte{config: contents, database: saved, filepath.Join(destination, "srv/he-mod-index/he-mod-index"): updated} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("upgrade changed preserved content or failed to replace binary", path, err)
		}
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("installer executed configuration as shell code", err)
	}
}

func TestDeploymentRejectsInvalidDomainsAndConflictingConfig(t *testing.T) {
	source, _ := deployFixture(t)
	for _, domain := range []string{"", "localhost", "mods.example.org/path", "mods.example.org:8080", "*.example.org", "-bad.example.org", "bad-.example.org", "bad..example.org", "mods.example.org.", "mods.example.org\nother.example.org", "mods.example.org { respond injected }", strings.Repeat("a", 64) + ".example.org"} {
		t.Run(domain, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "root")
			if output, err := runDeploy(t, source, destination, domain); err == nil {
				t.Fatal("accepted invalid domain", domain, output)
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatal("invalid input changed installation paths", err)
			}
		})
	}
	destination := filepath.Join(t.TempDir(), "root")
	if output, err := runDeploy(t, source, destination, "mods.example.org"); err != nil {
		t.Fatal(err, output)
	}
	before, err := os.ReadFile(filepath.Join(destination, "srv/he-mod-index/he-mod-index"))
	if err != nil {
		t.Fatal(err)
	}
	if output, err := runDeploy(t, source, destination, "different.example.org"); err == nil || !strings.Contains(output, "Existing PUBLIC_URL does not match") {
		t.Fatal("silently changed domain", err, output)
	}
	after, err := os.ReadFile(filepath.Join(destination, "srv/he-mod-index/he-mod-index"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("domain conflict changed installed binary", err)
	}
}

func TestDeploymentRejectsSymlinkedDataDirectory(t *testing.T) {
	source, destination := deployFixture(t)
	app := filepath.Join(destination, "srv/he-mod-index")
	if err := os.MkdirAll(app, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(app, "data")); err != nil {
		t.Fatal(err)
	}
	if output, err := runDeploy(t, source, destination, "mods.example.org"); err == nil || !strings.Contains(output, "Refusing a symlink") {
		t.Fatal("accepted symlinked data directory", err, output)
	}
}

package profile_test

import (
	"strings"
	"testing"

	"github.com/josegonzalez/ccvm/internal/profile"
)

const goProfile = `
description = "Go 1.24 + delve"
extends     = "base"

[backend.docker]
image = "ccvm/go:latest"

[backend.proxmox]
lxc_template = 9002
vm_template  = 9102

[defaults]
backend   = "docker"
code_mode = "mount"
ttl       = "12h"

[resources]
cpus = 4
memory = "8G"
disk = "32G"

[provision]
packages = ["delve", "golangci-lint"]

[env]
GOFLAGS = "-mod=mod"
`

func TestParseFullProfile(t *testing.T) {
	c, err := profile.Parse("profile.toml", []byte(goProfile), profile.ScopeOwned)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Extends != "base" {
		t.Errorf("Extends = %q", c.Extends)
	}
	if c.Backend["docker"].Image != "ccvm/go:latest" {
		t.Errorf("docker image = %q", c.Backend["docker"].Image)
	}
	// Typed, not string-split: a vmid is an int and packages are a list.
	if c.Backend["proxmox"].LXCTemplate != 9002 {
		t.Errorf("lxc_template = %d", c.Backend["proxmox"].LXCTemplate)
	}
	if c.Resources.CPUs != 4 {
		t.Errorf("cpus = %d", c.Resources.CPUs)
	}
	if len(c.Provision.Packages) != 2 {
		t.Errorf("packages = %v", c.Provision.Packages)
	}
	if c.Env["GOFLAGS"] != "-mod=mod" {
		t.Errorf("GOFLAGS = %q", c.Env["GOFLAGS"])
	}
}

// Malformed input is a hard error naming file and line — never a silent skip.
func TestParseMalformedNamesFileAndLine(t *testing.T) {
	src := "description = \"ok\"\nthis is not toml\n"
	_, err := profile.Parse("profiles/go/profile.toml", []byte(src), profile.ScopeOwned)
	if err == nil {
		t.Fatal("expected an error")
	}
	var pe *profile.ParseError
	if !asParseError(err, &pe) {
		t.Fatalf("err = %T (%v), want *ParseError", err, err)
	}
	if pe.File != "profiles/go/profile.toml" {
		t.Errorf("File = %q", pe.File)
	}
	if pe.Row != 2 {
		t.Errorf("Row = %d, want 2", pe.Row)
	}
	if !strings.Contains(err.Error(), "profiles/go/profile.toml:2") {
		t.Errorf("Error() = %q, want it to name file and line", err)
	}
}

// The same parser serves first-party and project configs, so a malformed
// ccvm-owned profile must fail exactly the same way.
func TestParseMalformedFailsForOwnedProfilesToo(t *testing.T) {
	src := "[resources\ncpus = 2\n"
	if _, err := profile.Parse("profiles/base/profile.toml", []byte(src), profile.ScopeOwned); err == nil {
		t.Fatal("expected an error for a malformed first-party profile")
	}
}

// A typo'd key that silently does nothing is the failure this format exists to
// avoid.
func TestParseRejectsUnknownKey(t *testing.T) {
	src := "[resources]\ncpus = 2\nmemroy = \"8G\"\n"
	_, err := profile.Parse("p.toml", []byte(src), profile.ScopeOwned)
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "memroy") {
		t.Errorf("Error() = %q, want it to name the offending key", err)
	}
}

func TestParseRejectsUnknownTable(t *testing.T) {
	src := "[resauces]\ncpus = 2\n"
	if _, err := profile.Parse("p.toml", []byte(src), profile.ScopeOwned); err == nil {
		t.Fatal("expected an error for an unknown table")
	}
}

// Authority: a repo may size a machine but may not choose what runs on it.
func TestParseProjectScopeRefusesBackendTable(t *testing.T) {
	src := "[resources]\nmemory = \"16G\"\n\n[backend.docker]\nimage = \"evil/image\"\n"
	_, err := profile.Parse(".ccvm/profile.toml", []byte(src), profile.ScopeProject)
	if err == nil {
		t.Fatal("expected a project config setting [backend.docker] to be refused")
	}
	if !strings.Contains(err.Error(), "[backend]") {
		t.Errorf("Error() = %q, want it to name the offending table", err)
	}
	if !strings.Contains(err.Error(), ".ccvm/profile.toml") {
		t.Errorf("Error() = %q, want it to name the file", err)
	}
}

func TestParseProjectScopeRefusesEnvTable(t *testing.T) {
	src := "[env]\nLD_PRELOAD = \"/tmp/evil.so\"\n"
	if _, err := profile.Parse(".ccvm/profile.toml", []byte(src), profile.ScopeProject); err == nil {
		t.Fatal("expected a project config setting [env] to be refused")
	}
}

// The same tables are fine in the user's own global config.
func TestParseOwnedScopeAllowsBackendAndEnv(t *testing.T) {
	src := "[backend.docker]\nimage = \"mine/base\"\n\n[env]\nFOO = \"bar\"\n"
	c, err := profile.Parse("~/.config/ccvm/profile.toml", []byte(src), profile.ScopeOwned)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Backend["docker"].Image != "mine/base" {
		t.Errorf("image = %q", c.Backend["docker"].Image)
	}
}

// What a project IS allowed to say must still work.
func TestParseProjectScopeAllowsResourcesAndProvision(t *testing.T) {
	src := "[resources]\nmemory = \"16G\"\n\n[provision]\npackages = [\"jq\"]\n\n[defaults]\ncode_mode = \"rsync\"\n"
	c, err := profile.Parse(".ccvm/profile.toml", []byte(src), profile.ScopeProject)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Resources.Memory != "16G" {
		t.Errorf("memory = %q", c.Resources.Memory)
	}
	if c.Defaults.CodeMode != "rsync" {
		t.Errorf("code_mode = %q", c.Defaults.CodeMode)
	}
}

// TOML is data. There is no expansion or substitution to smuggle anything
// through — the value arrives as the literal text it was written as.
func TestParseTreatsShellSyntaxAsLiteralText(t *testing.T) {
	src := "[resources]\nmemory = \"16G\"\n\n[provision]\npackages = [\"$(touch /tmp/pwned)\"]\n"
	c, err := profile.Parse(".ccvm/profile.toml", []byte(src), profile.ScopeProject)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Resources.Memory != "16G" {
		t.Errorf("memory = %q, want the legitimate value to still apply", c.Resources.Memory)
	}
	if got := c.Provision.Packages[0]; got != "$(touch /tmp/pwned)" {
		t.Errorf("package = %q, want the literal string with no expansion", got)
	}
}

func asParseError(err error, target **profile.ParseError) bool {
	pe, ok := err.(*profile.ParseError)
	if ok {
		*target = pe
	}
	return ok
}

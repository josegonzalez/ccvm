package profile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/josegonzalez/ccvm/internal/profile"
)

func TestLayeredPrefersEarlierSource(t *testing.T) {
	user := profile.FSSource{FS: fstest.MapFS{
		"base/profile.toml": &fstest.MapFile{Data: []byte("description = \"mine\"\n")},
	}}
	builtin := profile.FSSource{FS: fstest.MapFS{
		"base/profile.toml": &fstest.MapFile{Data: []byte("description = \"shipped\"\n")},
		"go/profile.toml":   &fstest.MapFile{Data: []byte("description = \"shipped go\"\n")},
	}}
	l := profile.Layered{Sources: []profile.Source{user, builtin}}

	c, err := l.Load("base")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Description != "mine" {
		t.Errorf("Description = %q, want the user's profile to shadow the built-in", c.Description)
	}

	// A name only the built-ins have still resolves.
	c, err = l.Load("go")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Description != "shipped go" {
		t.Errorf("Description = %q", c.Description)
	}
}

func TestLayeredUnknownProfile(t *testing.T) {
	l := profile.Layered{Sources: []profile.Source{
		profile.FSSource{FS: fstest.MapFS{}},
	}}
	if _, err := l.Load("ghost"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLayeredNoSources(t *testing.T) {
	if _, err := (profile.Layered{}).Load("base"); err == nil {
		t.Fatal("expected an error")
	}
}

// Provisioning runs parents before children, so a child's script can rely on
// what its parent installed.
func TestNamesReturnsTheChainRootFirst(t *testing.T) {
	src := profile.FSSource{FS: fstest.MapFS{
		"base/profile.toml": &fstest.MapFile{Data: []byte("")},
		"lang/profile.toml": &fstest.MapFile{Data: []byte("extends = \"base\"\n")},
		"go/profile.toml":   &fstest.MapFile{Data: []byte("extends = \"lang\"\n")},
	}}

	got, err := profile.Names("go", src)
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	want := []string{"base", "lang", "go"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("Names = %v, want %v", got, want)
	}
}

func TestNamesDetectsACycle(t *testing.T) {
	src := profile.FSSource{FS: fstest.MapFS{
		"a/profile.toml": &fstest.MapFile{Data: []byte("extends = \"b\"\n")},
		"b/profile.toml": &fstest.MapFile{Data: []byte("extends = \"a\"\n")},
	}}
	if _, err := profile.Names("a", src); err == nil {
		t.Fatal("expected a cycle error")
	}
}

func TestNamesUnknownProfile(t *testing.T) {
	src := profile.FSSource{FS: fstest.MapFS{}}
	if _, err := profile.Names("ghost", src); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFSSourceScriptReadsAProfileFile(t *testing.T) {
	src := profile.FSSource{FS: fstest.MapFS{
		"base/provision.sh": &fstest.MapFile{Data: []byte("echo hi\n")},
	}}
	got, err := src.Script("base", "provision.sh")
	if err != nil {
		t.Fatalf("Script: %v", err)
	}
	if string(got) != "echo hi\n" {
		t.Errorf("Script = %q", got)
	}
}

// A profile without a hook contributes nothing, so the absence has to be
// reported as such rather than as a failure.
func TestFSSourceScriptMissingIsNotExist(t *testing.T) {
	src := profile.FSSource{FS: fstest.MapFS{}}
	_, err := src.Script("base", "provision.sh")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

// A user profile's hook shadows a built-in's the same way its config does.
func TestLayeredScriptPrefersTheEarlierSource(t *testing.T) {
	l := profile.Layered{Sources: []profile.Source{
		profile.FSSource{FS: fstest.MapFS{
			"base/provision.sh": &fstest.MapFile{Data: []byte("mine\n")},
		}},
		profile.FSSource{FS: fstest.MapFS{
			"base/provision.sh": &fstest.MapFile{Data: []byte("shipped\n")},
			"go/provision.sh":   &fstest.MapFile{Data: []byte("shipped go\n")},
		}},
	}}

	got, err := l.Script("base", "provision.sh")
	if err != nil {
		t.Fatalf("Script: %v", err)
	}
	if string(got) != "mine\n" {
		t.Errorf("Script = %q, want the user's to win", got)
	}

	got, err = l.Script("go", "provision.sh")
	if err != nil {
		t.Fatalf("Script: %v", err)
	}
	if string(got) != "shipped go\n" {
		t.Errorf("Script = %q", got)
	}
}

func TestLayeredScriptMissingEverywhere(t *testing.T) {
	l := profile.Layered{Sources: []profile.Source{profile.FSSource{FS: fstest.MapFS{}}}}
	if _, err := l.Script("base", "provision.sh"); err == nil {
		t.Fatal("expected an error")
	}
}

// With no user directory the built-ins still resolve, which is what makes ccvm
// work on a machine with no configuration.
func TestDefaultSourceFallsBackToTheBuiltins(t *testing.T) {
	builtin := fstest.MapFS{
		"base/profile.toml": &fstest.MapFile{Data: []byte("description = \"shipped\"\n")},
	}
	src := profile.DefaultSource(t.TempDir(), builtin)

	c, err := src.Load("base")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Description != "shipped" {
		t.Errorf("Description = %q", c.Description)
	}
}

// A profile in the user's directory shadows the shipped one of the same name,
// which is how base is customized without forking the tool.
func TestDefaultSourcePrefersTheUserDirectory(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "ccvm", "profiles", "base")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.toml"),
		[]byte("description = \"mine\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	builtin := fstest.MapFS{
		"base/profile.toml": &fstest.MapFile{Data: []byte("description = \"shipped\"\n")},
	}
	c, err := profile.DefaultSource(home, builtin).Load("base")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Description != "mine" {
		t.Errorf("Description = %q, want the user's profile", c.Description)
	}
}

func TestScopeString(t *testing.T) {
	if got := profile.ScopeProject.String(); got != "project" {
		t.Errorf("ScopeProject = %q", got)
	}
	if got := profile.ScopeOwned.String(); got != "owned" {
		t.Errorf("ScopeOwned = %q", got)
	}
}

// Shadowing a built-in profile is the documented way to customize it, so a typo
// in the shadow must be reported. Falling through to the built-in hands you a
// session built from a config you did not write, and says nothing.
func TestLayeredReportsAMalformedUserProfile(t *testing.T) {
	broken := fstest.MapFS{
		"base/profile.toml": &fstest.MapFile{Data: []byte("this is not = valid = toml\n")},
	}
	builtin := fstest.MapFS{
		"base/profile.toml": &fstest.MapFile{Data: []byte("description = \"the shipped one\"\n")},
	}
	l := profile.Layered{Sources: []profile.Source{
		profile.FSSource{FS: broken},
		profile.FSSource{FS: builtin},
	}}

	c, err := l.Load("base")
	if err == nil {
		t.Fatalf("a malformed user profile was masked; got %+v", c)
	}
	if errors.Is(err, profile.ErrNotFound) {
		t.Errorf("err = %v, want a parse error rather than not-found", err)
	}
}

// A profile the user simply does not have must still fall through, or shadowing
// would become mandatory.
func TestLayeredStillFallsThroughForAMissingProfile(t *testing.T) {
	l := profile.Layered{Sources: []profile.Source{
		profile.FSSource{FS: fstest.MapFS{}},
		profile.FSSource{FS: fstest.MapFS{
			"base/profile.toml": &fstest.MapFile{Data: []byte("description = \"shipped\"\n")},
		}},
	}}

	c, err := l.Load("base")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Description != "shipped" {
		t.Errorf("Description = %q, want the built-in", c.Description)
	}
}

package profile_test

import (
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

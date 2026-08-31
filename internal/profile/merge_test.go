package profile_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/josegonzalez/ccvm/internal/profile"
)

func TestMergeScalarsOverride(t *testing.T) {
	dst := &profile.Config{
		Defaults:  profile.Defaults{Backend: "docker", CodeMode: "mount", TTL: "12h"},
		Resources: profile.Resources{CPUs: 2, Memory: "4G", Disk: "16G"},
	}
	profile.Merge(dst, &profile.Config{
		Resources: profile.Resources{Memory: "16G"},
	})

	if dst.Resources.Memory != "16G" {
		t.Errorf("Memory = %q, want 16G", dst.Resources.Memory)
	}
	// Unset fields in the overlay must not clobber what is already there.
	if dst.Resources.CPUs != 2 {
		t.Errorf("CPUs = %d, want 2 (untouched)", dst.Resources.CPUs)
	}
	if dst.Defaults.Backend != "docker" {
		t.Errorf("Backend = %q, want docker (untouched)", dst.Defaults.Backend)
	}
}

// Packages append down the chain. A naive implementation replaces the list and
// silently drops everything the parent installed.
func TestMergePackagesAppend(t *testing.T) {
	dst := &profile.Config{Provision: profile.Provision{Packages: []string{"git", "tmux"}}}
	profile.Merge(dst, &profile.Config{Provision: profile.Provision{Packages: []string{"delve"}}})

	want := []string{"git", "tmux", "delve"}
	if !reflect.DeepEqual(dst.Provision.Packages, want) {
		t.Errorf("Packages = %v, want %v", dst.Provision.Packages, want)
	}
}

func TestMergePackagesAppendDeduplicates(t *testing.T) {
	dst := &profile.Config{Provision: profile.Provision{Packages: []string{"git", "tmux"}}}
	profile.Merge(dst, &profile.Config{Provision: profile.Provision{Packages: []string{"tmux", "delve"}}})

	want := []string{"git", "tmux", "delve"}
	if !reflect.DeepEqual(dst.Provision.Packages, want) {
		t.Errorf("Packages = %v, want %v (restating a parent's package must not duplicate it)",
			dst.Provision.Packages, want)
	}
}

func TestMergePackagesReplaceOptsOut(t *testing.T) {
	dst := &profile.Config{Provision: profile.Provision{Packages: []string{"git", "tmux"}}}
	profile.Merge(dst, &profile.Config{Provision: profile.Provision{
		Packages:        []string{"delve"},
		PackagesReplace: true,
	}})

	want := []string{"delve"}
	if !reflect.DeepEqual(dst.Provision.Packages, want) {
		t.Errorf("Packages = %v, want %v", dst.Provision.Packages, want)
	}
}

// [env] and [backend.*] merge per key rather than replacing the whole table.
func TestMergeEnvPerKey(t *testing.T) {
	dst := &profile.Config{Env: map[string]string{"GOFLAGS": "-mod=mod", "CGO_ENABLED": "0"}}
	profile.Merge(dst, &profile.Config{Env: map[string]string{"CGO_ENABLED": "1"}})

	want := map[string]string{"GOFLAGS": "-mod=mod", "CGO_ENABLED": "1"}
	if !reflect.DeepEqual(dst.Env, want) {
		t.Errorf("Env = %v, want %v", dst.Env, want)
	}
}

func TestMergeBackendPerKeyAndPerField(t *testing.T) {
	dst := &profile.Config{Backend: map[string]profile.Backend{
		"docker":  {Image: "ccvm/base:latest"},
		"proxmox": {LXCTemplate: 9000, VMTemplate: 9100},
	}}
	profile.Merge(dst, &profile.Config{Backend: map[string]profile.Backend{
		"docker":  {Image: "ccvm/go:latest"},
		"proxmox": {LXCTemplate: 9002},
	}})

	if got := dst.Backend["docker"].Image; got != "ccvm/go:latest" {
		t.Errorf("docker.Image = %q", got)
	}
	if got := dst.Backend["proxmox"].LXCTemplate; got != 9002 {
		t.Errorf("proxmox.LXCTemplate = %d, want 9002", got)
	}
	// Overriding the LXC template must not clear the VM template beside it.
	if got := dst.Backend["proxmox"].VMTemplate; got != 9100 {
		t.Errorf("proxmox.VMTemplate = %d, want 9100 (untouched)", got)
	}
}

func TestMergeNilSourceIsNoop(t *testing.T) {
	dst := &profile.Config{Resources: profile.Resources{CPUs: 4}}
	profile.Merge(dst, nil)
	if dst.Resources.CPUs != 4 {
		t.Error("merging nil changed dst")
	}
}

// Clone must deep-copy, or one resolution can corrupt another through a shared
// map — parents are reused across profiles.
func TestCloneIsDeep(t *testing.T) {
	orig := &profile.Config{
		Backend:   map[string]profile.Backend{"docker": {Image: "a"}},
		Env:       map[string]string{"K": "v"},
		Provision: profile.Provision{Packages: []string{"git"}},
	}
	c := orig.Clone()
	c.Backend["docker"] = profile.Backend{Image: "b"}
	c.Env["K"] = "changed"
	c.Provision.Packages[0] = "changed"

	if orig.Backend["docker"].Image != "a" {
		t.Error("Backend map is shared")
	}
	if orig.Env["K"] != "v" {
		t.Error("Env map is shared")
	}
	if orig.Provision.Packages[0] != "git" {
		t.Error("Packages slice is shared")
	}
}

// Commands accumulate down the chain like packages, so a child adds to what its
// parent asked for rather than silently discarding it.
func TestMergeAccumulatesCommands(t *testing.T) {
	dst := &profile.Config{Provision: profile.Provision{
		Pre:   []string{"parent pre"},
		Post:  []string{"parent post"},
		Setup: []string{"parent setup"},
	}}
	src := &profile.Config{Provision: profile.Provision{
		Pre:   []string{"child pre"},
		Post:  []string{"child post"},
		Setup: []string{"child setup"},
	}}
	profile.Merge(dst, src)

	for _, tc := range []struct {
		name string
		got  []string
		want string
	}{
		{"pre", dst.Provision.Pre, "parent pre,child pre"},
		{"post", dst.Provision.Post, "parent post,child post"},
		{"setup", dst.Provision.Setup, "parent setup,child setup"},
	} {
		if strings.Join(tc.got, ",") != tc.want {
			t.Errorf("%s = %v, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// Unlike packages, commands are a sequence rather than a set. Two layers that
// both ask for the same command both meant it, so it must not be deduped.
func TestMergeKeepsDuplicateCommands(t *testing.T) {
	dst := &profile.Config{Provision: profile.Provision{Pre: []string{"make"}}}
	src := &profile.Config{Provision: profile.Provision{Pre: []string{"make"}}}
	profile.Merge(dst, src)

	if got := dst.Provision.Pre; len(got) != 2 {
		t.Errorf("pre = %v, want the command kept twice", got)
	}
}

// A layer that genuinely needs to drop what a parent contributed says so.
func TestMergeCommandReplace(t *testing.T) {
	dst := &profile.Config{Provision: profile.Provision{
		Pre:   []string{"parent"},
		Setup: []string{"parent"},
	}}
	src := &profile.Config{Provision: profile.Provision{
		Pre:          []string{"only"},
		PreReplace:   true,
		Setup:        nil,
		SetupReplace: true,
	}}
	profile.Merge(dst, src)

	if strings.Join(dst.Provision.Pre, ",") != "only" {
		t.Errorf("pre = %v, want the parent's dropped", dst.Provision.Pre)
	}
	// Replace with an empty list clears, rather than being ignored as "unset".
	if len(dst.Provision.Setup) != 0 {
		t.Errorf("setup = %v, want cleared", dst.Provision.Setup)
	}
}

// Clone must copy every slice. Sharing a backing array lets one resolution's
// commands appear in another the first time append reuses spare capacity.
func TestCloneDoesNotAliasCommands(t *testing.T) {
	orig := &profile.Config{Provision: profile.Provision{
		Pre:   []string{"a"},
		Post:  []string{"b"},
		Setup: []string{"c"},
	}}
	c := orig.Clone()
	c.Provision.Pre[0] = "mutated"
	c.Provision.Post[0] = "mutated"
	c.Provision.Setup[0] = "mutated"

	if orig.Provision.Pre[0] != "a" || orig.Provision.Post[0] != "b" || orig.Provision.Setup[0] != "c" {
		t.Errorf("clone aliased the original: %+v", orig.Provision)
	}
}

package profile_test

import (
	"reflect"
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

package profiles_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/josegonzalez/ccvm/internal/code"
	"github.com/josegonzalez/ccvm/internal/profile"
	"github.com/josegonzalez/ccvm/profiles"
)

// The shipped profiles must actually parse and resolve. A typo here breaks
// every session on a fresh install, and nothing else would catch it.
func TestBuiltinProfilesResolve(t *testing.T) {
	src := profile.FSSource{FS: profiles.FS()}

	for _, name := range []string{"base", "go", "node"} {
		t.Run(name, func(t *testing.T) {
			c, err := profile.Resolve(name, src)
			if err != nil {
				t.Fatalf("Resolve(%s): %v", name, err)
			}
			if c.Description == "" {
				t.Error("no description")
			}
			// Every shipped profile must be usable on every backend, or
			// --backend silently stops working for some of them.
			for _, b := range []string{"docker", "k8s", "orbstack", "proxmox"} {
				if cfg, ok := c.Backend[b]; !ok || cfg.Empty() {
					t.Errorf("profile %q has no usable [backend.%s]", name, b)
				}
			}
		})
	}
}

// Inheritance is only useful if the shipped profiles actually exercise it.
func TestBuiltinGoInheritsFromBase(t *testing.T) {
	src := profile.FSSource{FS: profiles.FS()}
	c, err := profile.Resolve("go", src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Defaults.TTL != "12h" {
		t.Errorf("TTL = %q, want it inherited from base", c.Defaults.TTL)
	}
	// Deliberately unset all the way down, so code.DefaultFor picks per backend.
	// A value here would shadow it and hand proxmox and k8s a mode neither can
	// serve.
	if c.Defaults.CodeMode != "" {
		t.Errorf("CodeMode = %q, want it left to the backend", c.Defaults.CodeMode)
	}
	if c.Resources.Memory != "8G" {
		t.Errorf("Memory = %q, want the go profile's override", c.Resources.Memory)
	}
	if c.Backend["docker"].Image != "ccvm/go:latest" {
		t.Errorf("image = %q, want the go override", c.Backend["docker"].Image)
	}
	// Proxmox templates come from go's own table, not base's.
	if c.Backend["proxmox"].LXCTemplate != 9002 {
		t.Errorf("lxc_template = %d, want 9002", c.Backend["proxmox"].LXCTemplate)
	}
}

// node overrides only memory, so cpus must survive from base.
func TestBuiltinNodeInheritsUnsetResources(t *testing.T) {
	src := profile.FSSource{FS: profiles.FS()}
	c, err := profile.Resolve("node", src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Resources.CPUs != 2 {
		t.Errorf("CPUs = %d, want 2 inherited from base", c.Resources.CPUs)
	}
	if c.Resources.Memory != "8G" {
		t.Errorf("Memory = %q, want node's override", c.Resources.Memory)
	}
}

// The guide is composed and written at spawn, on every backend. Baking a copy
// into the image as well would give the same path two sources of truth, and the
// baked one would go stale the moment a layer changed. This is a regression
// guard: the COPY was removed deliberately.
func TestBaseImageDoesNotBakeAGuide(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("base", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "COPY") && strings.Contains(line, "CLAUDE.md") {
			t.Errorf("Dockerfile bakes a CLAUDE.md again: %q", strings.TrimSpace(line))
		}
	}
}

// Every shipped profile must resolve to a code mode its backend can actually
// serve. This is the regression that shipped: base pinned code_mode = "mount",
// which shadowed code.DefaultFor and handed proxmox a mode that silently
// produced an empty /work and k8s one it refuses outright.
func TestShippedProfilesLeaveTheCodeModeToTheBackend(t *testing.T) {
	src := profile.FSSource{FS: profiles.FS()}

	for _, name := range []string{"base", "go", "node"} {
		c, err := profile.Resolve(name, src)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", name, err)
		}
		for _, b := range []string{"docker", "orbstack", "proxmox", "k8s"} {
			// The resolution ccvm up performs: an explicit flag, then the
			// profile chain, then the per-backend default.
			mode := c.Defaults.CodeMode
			if mode == "" {
				mode = code.DefaultFor(b)
			}
			if err := code.Check(mode, b); err != nil {
				t.Errorf("profile %q on %s resolves to %q, which it cannot serve: %v", name, b, mode, err)
			}
		}
	}
}

// A profile that names an image must ship something that can build it.
// `go` and `node` declared images and shipped only a profile.toml, so
// `ccvm up --profile go` failed preflight on a missing image and the fix it
// suggested could not succeed either.
func TestShippedProfilesCanBeBuilt(t *testing.T) {
	for _, name := range []string{"base", "go", "node"} {
		entries, err := profiles.FS().(interface {
			ReadDir(string) ([]os.DirEntry, error)
		}).ReadDir(name)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", name, err)
		}
		var hasDockerfile bool
		for _, e := range entries {
			if e.Name() == "Dockerfile" {
				hasDockerfile = true
			}
		}
		if !hasDockerfile {
			t.Errorf("profile %q declares a docker image but ships no Dockerfile", name)
		}
	}
}

// apt cannot install a Go tool or an npm package. Naming one in
// [provision].packages aborted every spawn of that profile, because a failing
// provisioning layer destroys the machine by design.
func TestShippedProfilesDoNotAskAptForNonPackages(t *testing.T) {
	notDebian := map[string]bool{
		"delve": true, "golangci-lint": true, "pnpm": true, "typescript": true,
	}
	src := profile.FSSource{FS: profiles.FS()}

	for _, name := range []string{"base", "go", "node"} {
		c, err := profile.Resolve(name, src)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", name, err)
		}
		for _, pkg := range c.Provision.Packages {
			if notDebian[pkg] {
				t.Errorf("profile %q asks apt for %q, which is not a Debian package", name, pkg)
			}
		}
	}
}

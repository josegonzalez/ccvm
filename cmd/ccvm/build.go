package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/josegonzalez/ccvm/internal/profile"
	"github.com/josegonzalez/ccvm/profiles"
)

// profilesBuild bakes a profile's image for a backend.
//
// Error messages elsewhere tell users to run this, so it has to exist for the
// paths those messages describe to be followable.
func (a *app) profilesBuild(name, backendName string, args []string) error {
	cfg, err := profile.Resolve(name, a.profiles)
	if err != nil {
		return err
	}
	if backendName == "" {
		backendName = firstNonEmpty(cfg.Defaults.Backend, "docker")
	}

	switch backendName {
	case "docker", "k8s":
		return a.buildDockerImage(name, backendName, cfg)
	case "orbstack":
		return a.buildOrbstackTemplate(name, cfg)
	case "proxmox":
		return fmt.Errorf("building proxmox templates is not implemented yet; " +
			"build one by hand with packer, convert it with `pct template <vmid>`, " +
			"and point [backend.proxmox].lxc_template at it")
	default:
		return fmt.Errorf("unknown backend %q", backendName)
	}
}

func (a *app) buildDockerImage(name, backendName string, cfg *profile.Config) error {
	image := cfg.Backend[backendName].Image
	if image == "" {
		return fmt.Errorf("profile %q has no [backend.%s].image to build", name, backendName)
	}

	dir, cleanup, err := a.stageProfile(name)
	if err != nil {
		return err
	}
	defer cleanup()

	dockerfile := filepath.Join(dir, "Dockerfile")
	if _, err := os.Stat(dockerfile); err != nil {
		return fmt.Errorf("profile %q has no Dockerfile", name)
	}

	// The build context is the working directory because the Dockerfile copies
	// the guest binaries out of dist/. That ties this command to the repo, so
	// say it plainly rather than letting docker fail on a missing COPY source.
	if _, err := os.Stat("dist"); err != nil {
		return fmt.Errorf("no dist/ in the working directory: run `make build` from the ccvm repo first, "+
			"since the image copies the guest binaries from there (%w)", err)
	}

	fmt.Fprintf(a.out, "building %s\n", image)
	out, err := a.runner.Run(a.ctx, "docker", "build", "-f", dockerfile, "-t", image, ".")
	if err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	if a.verbose {
		fmt.Fprintln(a.out, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(a.out, "built %s\n", image)
	return nil
}

// buildOrbstackTemplate creates the machine sessions are cloned from.
//
// It provisions with the same provision.sh that spawn-time hooks run, so a
// baked template and a hook-provisioned machine cannot drift apart.
func (a *app) buildOrbstackTemplate(name string, cfg *profile.Config) error {
	template := cfg.Backend["orbstack"].Template
	if template == "" {
		return fmt.Errorf("profile %q has no [backend.orbstack].template to build", name)
	}

	dir, cleanup, err := a.stageProfile(name)
	if err != nil {
		return err
	}
	defer cleanup()

	script := filepath.Join(dir, "build.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("profile %q has no build.sh, so there is nothing to bake into a template", name)
	}

	// A leftover template from a failed build would be reused silently.
	if _, err := a.runner.Run(a.ctx, "orbctl", "delete", "-f", template); err == nil {
		fmt.Fprintf(a.out, "replaced the existing %s template\n", template)
	}

	fmt.Fprintf(a.out, "creating %s\n", template)
	if _, err := a.runner.Run(a.ctx, "orbctl", "create",
		"--isolated", "--forward-ssh-agent", "debian", template); err != nil {
		return fmt.Errorf("create template machine: %w", err)
	}

	fmt.Fprintln(a.out, "installing claude, git, tmux, and sshd (a few minutes)")

	// Staged under /etc/ccvm rather than /tmp. A guest's /tmp is a per-boot
	// tmpfs that orbctl push writes into a different view of: the push reports
	// success and the file is not there, which fails later and confusingly.
	const staged = "/etc/ccvm/build.sh"
	if _, err := a.runner.Run(a.ctx, "orbctl", "run", "-m", template, "-u", "root",
		"mkdir", "-p", "/etc/ccvm"); err != nil {
		return err
	}
	if _, err := a.runner.Run(a.ctx, "orbctl", "push", "-m", template, script, staged); err != nil {
		return fmt.Errorf("copy provision.sh: %w", err)
	}
	if _, err := a.runner.Run(a.ctx, "orbctl", "run", "-m", template, "-u", "root",
		"sh", staged); err != nil {
		return fmt.Errorf("provision the template: %w", err)
	}

	// The guest binaries come from this repo rather than a download.
	for _, bin := range []string{"ccvm-done", "ccvm-init"} {
		src := filepath.Join("dist", bin+"-linux-arm64")
		if _, err := os.Stat(src); err != nil {
			fmt.Fprintf(a.err, "ccvm: %s not built; run `make build` and rebuild this profile\n", src)
			continue
		}
		dst := "/usr/local/bin/" + bin
		if _, err := a.runner.Run(a.ctx, "orbctl", "push", "-m", template, src, dst); err != nil {
			return fmt.Errorf("install %s: %w", bin, err)
		}
		if _, err := a.runner.Run(a.ctx, "orbctl", "run", "-m", template, "-u", "root",
			"chmod", "0755", dst); err != nil {
			return err
		}
	}

	// Seeded so Claude discovers ccvm-done without being told the command name.
	if err := a.pushGuestGuide(template); err != nil {
		fmt.Fprintf(a.err, "ccvm: could not seed the guest CLAUDE.md: %v\n", err)
	}

	// Clones start from a stopped template, and leaving it running wastes
	// memory on the Mac for a machine nobody uses directly.
	if _, err := a.runner.Run(a.ctx, "orbctl", "stop", template); err != nil {
		fmt.Fprintf(a.err, "ccvm: could not stop %s: %v\n", template, err)
	}

	fmt.Fprintf(a.out, "built %s; sessions clone from it\n", template)
	return nil
}

func (a *app) pushGuestGuide(template string) error {
	data, err := os.ReadFile("guest/CLAUDE.md")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "ccvm-guide-*.md")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	if _, err := a.runner.Run(a.ctx, "orbctl", "run", "-m", template, "-u", "root",
		"mkdir", "-p", "/root/.claude"); err != nil {
		return err
	}
	_, err = a.runner.Run(a.ctx, "orbctl", "push", "-m", template, path, "/root/.claude/CLAUDE.md")
	return err
}

// stageProfile materializes a profile's build inputs, whether they come from
// the embedded copy or a user directory.
func (a *app) stageProfile(name string) (dir string, cleanup func(), err error) {
	// A working tree wins, so editing a profile and rebuilding does what you
	// expect rather than rebuilding the version compiled into the binary.
	local := filepath.Join("profiles", name)
	if st, err := os.Stat(local); err == nil && st.IsDir() {
		return local, func() {}, nil
	}

	tmp, err := os.MkdirTemp("", "ccvm-profile-"+name+"-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { os.RemoveAll(tmp) }

	entries, err := profiles.FS().(interface {
		ReadDir(string) ([]os.DirEntry, error)
	}).ReadDir(name)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("profile %q has no build inputs: %w", name, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := readEmbedded(name + "/" + e.Name())
		if err != nil {
			cleanup()
			return "", nil, err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(e.Name(), ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(tmp, e.Name()), data, mode); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return tmp, cleanup, nil
}

func readEmbedded(path string) ([]byte, error) {
	f, err := profiles.FS().Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	data := make([]byte, st.Size())
	if _, err := f.Read(data); err != nil {
		return nil, err
	}
	return data, nil
}

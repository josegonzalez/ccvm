package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/josegonzalez/ccvm/internal/backend"
)

// guestBinaries are the programs a session needs inside the machine.
//
// ccvm-done is what makes "end the session from inside Claude" work at all, so
// a guest without it is a session you can only end from the host.
var guestBinaries = []string{"ccvm-done", "ccvm-init"}

// installGuestBinaries puts the guest programs in place when the image did not
// already carry them.
//
// The docker and kubernetes images bake them, and the orbstack template build
// pushes them, but a proxmox guest is cloned from a template built by hand -
// the README's own recipe installs claude, git, tmux, and sshd and stops there.
// Checking rather than always pushing keeps the common case free.
func (a *app) installGuestBinaries(b backend.Backend, h backend.Handle) error {
	missing := make([]string, 0, len(guestBinaries))
	for _, name := range guestBinaries {
		if _, err := b.Exec(a.ctx, h, "test", "-x", "/usr/local/bin/"+name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	arch, err := guestArch(a, b, h)
	if err != nil {
		return err
	}
	for _, name := range missing {
		src, err := guestBinaryPath(name, arch)
		if err != nil {
			return err
		}
		dst := "/usr/local/bin/" + name
		if err := b.Push(a.ctx, h, src, dst); err != nil {
			return fmt.Errorf("install %s: %w", name, err)
		}
		// Transfer carries the host's uid, and a binary the guest cannot
		// execute fails in a way that looks like the binary is wrong.
		if _, err := b.Exec(a.ctx, h, "chown", "root:root", dst); err != nil {
			return err
		}
		if _, err := b.Exec(a.ctx, h, "chmod", "0755", dst); err != nil {
			return err
		}
	}
	return nil
}

// guestArch asks the guest rather than assuming the host's architecture. A Mac
// on Apple silicon routinely talks to an amd64 cluster, and a binary for the
// wrong one installs cleanly and then fails with "Exec format error".
func guestArch(a *app, b backend.Backend, h backend.Handle) (string, error) {
	out, err := b.Exec(a.ctx, h, "uname", "-m")
	if err != nil {
		return "", fmt.Errorf("determine the guest architecture: %w", err)
	}
	switch m := strings.TrimSpace(string(out)); m {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported guest architecture %q", m)
	}
}

// guestBinaryPath finds a guest binary next to the running ccvm first, so a
// release that ships them together works, then in ./dist for a repo checkout.
func guestBinaryPath(name, arch string) (string, error) {
	file := fmt.Sprintf("%s-linux-%s", name, arch)

	var tried []string
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), file)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		tried = append(tried, p)
	}
	p := filepath.Join("dist", file)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	tried = append(tried, p)

	return "", fmt.Errorf("no %s to install in the guest (looked in %s); "+
		"run `make build` from the ccvm repo, or bake it into the template",
		file, strings.Join(tried, ", "))
}

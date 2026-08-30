// Package code puts a project's source into a session machine.
//
// Which mechanism is right depends on the backend, so the mode is part of the
// spec rather than a global setting: a bind mount is free on docker and
// impossible on kubernetes, and a git clone is the only thing that works
// everywhere.
package code

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/run"
)

// Modes.
const (
	Mount = "mount"
	Rsync = "rsync"
	Git   = "git"
)

// Supported maps each mode to the backends that can serve it.
//
// kubernetes has no host filesystem to reach, so mount is not merely
// unimplemented there; it cannot exist.
var Supported = map[string][]string{
	Mount: {"docker", "orbstack", "proxmox"},
	Rsync: {"docker", "orbstack", "proxmox", "k8s"},
	Git:   {"docker", "orbstack", "proxmox", "k8s"},
}

// defaultExcludes are paths never worth copying into a session.
//
// Honouring .gitignore is not enough on its own: a large .git or a vendored
// dependency tree is tracked, and copying it turns a two-second spawn into a
// minute.
var defaultExcludes = []string{
	"node_modules", ".venv", "venv", "__pycache__",
	"target", "dist", "build", ".next", ".cache",
	".DS_Store", "*.log",
}

// Check reports whether a mode can be served by a backend.
func Check(mode, backendName string) error {
	backends, ok := Supported[mode]
	if !ok {
		return fmt.Errorf("unknown code mode %q; try %s", mode, strings.Join(Modes(), ", "))
	}
	for _, b := range backends {
		if b == backendName {
			return nil
		}
	}
	return fmt.Errorf("--code %s is not available on the %s backend; it supports %s",
		mode, backendName, strings.Join(ModesFor(backendName), ", "))
}

// Modes lists every mode, in a stable order.
func Modes() []string { return []string{Mount, Rsync, Git} }

// ModesFor lists the modes a backend can serve.
func ModesFor(backendName string) []string {
	var out []string
	for _, mode := range Modes() {
		for _, b := range Supported[mode] {
			if b == backendName {
				out = append(out, mode)
				break
			}
		}
	}
	return out
}

// DefaultFor is the mode a backend should use when none is asked for: the
// cheapest one that carries uncommitted work.
func DefaultFor(backendName string) string {
	switch backendName {
	case "docker", "orbstack":
		return Mount
	case "proxmox":
		return Rsync
	default:
		// k8s has no host to reach, so a clone is the only option.
		return Git
	}
}

// Options describe one materialization.
type Options struct {
	Mode    string
	Project string // host path
	WorkDir string // path inside the machine
	Backend backend.Backend
	Handle  backend.Handle
	Runner  run.Execer
	// SSHTarget and IdentityFile are how rsync reaches the machine.
	SSHTarget    string
	IdentityFile string
}

// Materialize puts the code in place.
func Materialize(ctx context.Context, o Options) error {
	switch o.Mode {
	case Mount:
		// The backend attached the host directory at create time; there is
		// nothing to copy.
		return nil
	case Git:
		return cloneRepo(ctx, o)
	case Rsync:
		return syncIn(ctx, o)
	default:
		return fmt.Errorf("unknown code mode %q", o.Mode)
	}
}

// SyncBack returns changes to the host. Only rsync has anything to do: a mount
// is already live, and a clone's work belongs in a commit.
func SyncBack(ctx context.Context, o Options) error {
	if o.Mode != Rsync {
		return nil
	}
	args := rsyncBase(o)
	args = append(args,
		o.SSHTarget+":"+strings.TrimSuffix(o.WorkDir, "/")+"/",
		strings.TrimSuffix(o.Project, "/")+"/",
	)
	if _, err := o.Runner.Run(ctx, args...); err != nil {
		return fmt.Errorf("sync %s back to %s: %w%s", o.WorkDir, o.Project, err, rsyncHint(err))
	}
	return nil
}

func syncIn(ctx context.Context, o Options) error {
	if _, err := o.Backend.Exec(ctx, o.Handle, "mkdir", "-p", o.WorkDir); err != nil {
		return err
	}
	args := rsyncBase(o)
	args = append(args,
		strings.TrimSuffix(o.Project, "/")+"/",
		o.SSHTarget+":"+strings.TrimSuffix(o.WorkDir, "/")+"/",
	)
	if _, err := o.Runner.Run(ctx, args...); err != nil {
		return fmt.Errorf("sync %s into the machine: %w%s", o.Project, err, rsyncHint(err))
	}
	return nil
}

// rsyncHint names the fix for the one failure that is about the image rather
// than the transfer. rsync has to exist on both ends, and its absence in the
// guest reads as a bare "command not found".
func rsyncHint(err error) string {
	if strings.Contains(err.Error(), "rsync: command not found") ||
		strings.Contains(err.Error(), "rsync: not found") {
		return "\nThe machine's image has no rsync. Add it to the profile and rebuild " +
			"with `ccvm profiles build <name>`, or use --code mount or --code git"
	}
	return ""
}

func rsyncBase(o Options) []string {
	ssh := "ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
	if o.IdentityFile != "" {
		ssh += " -i " + o.IdentityFile
	}

	// --delete so a file removed on one side disappears on the other. Without
	// it a deletion silently reverts on the next sync, which is worse than not
	// syncing at all.
	args := []string{"rsync", "--archive", "--compress", "--delete", "-e", ssh}
	for _, ex := range defaultExcludes {
		args = append(args, "--exclude", ex)
	}
	// .gitignore on top of the fixed list, so a project's own ignores apply
	// without having to restate them.
	if _, err := os.Stat(filepath.Join(o.Project, ".gitignore")); err == nil {
		args = append(args, "--filter", ":- .gitignore")
	}
	return args
}

// cloneRepo clones the project's origin at the branch checked out on the host.
//
// The guest gets committed work only. Uncommitted changes stay behind, which is
// why ccvm-done refuses to destroy a machine with a dirty tree in this mode.
func cloneRepo(ctx context.Context, o Options) error {
	origin, err := gitValue(ctx, o, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("%s has no origin remote, so there is nothing to clone; "+
			"use --code rsync to copy the working tree instead", o.Project)
	}
	branch, err := gitValue(ctx, o, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "HEAD" {
		branch = ""
	}

	argv := []string{"git", "clone", "--depth", "1"}
	if branch != "" {
		argv = append(argv, "--branch", branch)
	}
	argv = append(argv, origin, o.WorkDir)

	if _, err := o.Backend.Exec(ctx, o.Handle, argv...); err != nil {
		return fmt.Errorf("clone %s into the machine: %w\n"+
			"The machine needs credentials for that remote; --forward-ssh-agent covers an ssh remote", origin, err)
	}
	return nil
}

func gitValue(ctx context.Context, o Options, args ...string) (string, error) {
	full := append([]string{"git", "-C", o.Project}, args...)
	out, err := o.Runner.Run(ctx, full...)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("git %s produced nothing", strings.Join(args, " "))
	}
	return v, nil
}

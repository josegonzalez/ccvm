// Package backend provisions the disposable machines a Claude Code session runs
// in, across docker, orbstack, proxmox, and kubernetes.
package backend

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Backend creates, inspects, and destroys session machines.
//
// Implementations never call os/exec directly; they take a run.Execer so their
// command construction is testable without the underlying tool present.
type Backend interface {
	// Name is the backend's identifier as written in config and --backend.
	Name() string

	// Preflight reports whether a machine could be created right now, without
	// creating one. It must not mutate anything.
	Preflight(ctx context.Context, s Spec) error

	// Create provisions the machine. It may leave it stopped.
	Create(ctx context.Context, s Spec) (Handle, error)

	// Start runs a created machine. Implementations whose Create already starts
	// the machine make this a no-op.
	Start(ctx context.Context, h Handle) error

	// Wait blocks until the machine is actually usable, not merely created.
	// The distinction matters: a kubernetes pod can be created successfully and
	// never run.
	Wait(ctx context.Context, h Handle) error

	// SSHTarget returns a destination usable with ssh(1).
	SSHTarget(h Handle) string

	// Exec runs a command inside the machine.
	Exec(ctx context.Context, h Handle, argv ...string) ([]byte, error)

	// Push copies a host file into the machine. Implementations set mode on the
	// destination when the source carries one.
	Push(ctx context.Context, h Handle, src, dst string) error

	// Pull copies a file out of the machine. It must work on a stopped machine
	// where the backend allows it: the reaper reads TTL from machines that are
	// no longer running.
	Pull(ctx context.Context, h Handle, src, dst string) error

	// List returns every ccvm-owned machine this backend knows about.
	List(ctx context.Context) ([]Machine, error)

	// Destroy removes the machine and its storage.
	Destroy(ctx context.Context, h Handle) error
}

// Spec is what a session needs from a machine.
type Spec struct {
	Name     string // machine name, e.g. cc-terminal-code
	Profile  string // resolved profile name
	Image    string // image or template, resolved from the profile's backend table
	Project  string // host path to the project
	WorkDir  string // where the code lands inside the machine
	CodeMode string // mount | rsync | git | sshfs

	CPUs   int
	Memory string
	Disk   string

	Env map[string]string

	// Keep exempts the machine from teardown and from the reaper. On docker it
	// also suppresses --rm, or the container would be deleted the moment it
	// stopped and there would be nothing left to inspect.
	Keep bool
	TTL  string

	// SSHPort is the loopback port forwarded to the machine's sshd, for
	// backends that need one. Zero means the backend does not use ports.
	SSHPort int

	CreatedAt time.Time
}

// Handle identifies a provisioned machine.
type Handle struct {
	Backend string
	Name    string
	ID      string // container id, vmid, pod/job name
	Node    string // proxmox node; empty elsewhere
	SSHPort int
}

// Machine is a normalized listing row.
type Machine struct {
	Name    string
	Backend string
	ID      string
	State   string // running | stopped | pending | unknown
	Node    string
	Profile string
	Project string
	TTL     string // duration, or "keep"
	Created time.Time
	SSH     string
}

// Handle builds the Handle addressing this machine.
func (m Machine) Handle() Handle {
	return Handle{Backend: m.Backend, Name: m.Name, ID: m.ID, Node: m.Node}
}

// KeepTTL marks a machine the reaper must not destroy.
const KeepTTL = "keep"

// Kept reports whether the machine is exempt from reaping.
func (m Machine) Kept() bool { return m.TTL == KeepTTL }

// Metadata keys.
//
// The split between these two groups is forced by docker: labels cannot be
// changed on a running container, so anything `ccvm keep` has to move at
// runtime cannot live in one. Immutable creation facts go in labels; mutable
// TTL goes in SessionFile, which is readable from a stopped machine.
const (
	LabelOwner    = "ccvm"
	LabelProject  = "ccvm.project"
	LabelProfile  = "ccvm.profile"
	LabelCreated  = "ccvm.created"
	LabelSSHPort  = "ccvm.ssh-port"
	LabelCodeMode = "ccvm.code-mode"

	// SessionFile holds the mutable half, and is what ccvm-done reads to decide
	// whether ending the session would lose uncommitted work.
	SessionFile = "/etc/ccvm/session.toml"

	// DoneSentinel is touched by ccvm-done. On docker and kubernetes the
	// machine's PID 1 watches for it; elsewhere the reaper does.
	DoneSentinel = "/run/ccvm/destroy"
)

// States.
const (
	StateRunning = "running"
	StateStopped = "stopped"
	StatePending = "pending"
	StateUnknown = "unknown"
)

// EphemeralReporter is implemented by backends whose machines can be created in
// a mode that deletes them the moment they stop.
//
// It exists because `ccvm keep` can move a TTL at runtime but cannot undo that
// mode: docker's AutoRemove is fixed at creation. Without this, `ccvm keep`
// would report success while docker still stood ready to delete the machine —
// an exemption from the reaper is not an exemption from the daemon.
type EphemeralReporter interface {
	// AutoRemoves reports whether the machine will be deleted on stop.
	AutoRemoves(ctx context.Context, h Handle) (bool, error)
}

// ErrNotConfigured marks a backend that has not been set up on this machine, as
// opposed to one that is set up but unreachable.
//
// The difference matters for tone: `ccvm ls` should say nothing about a backend
// you never configured, and should complain loudly about one you did.
var ErrNotConfigured = errors.New("not configured")

// IsToolMissing reports whether an error means the backend's command-line tool
// is not installed at all.
//
// A backend whose tool is absent is not broken, it is simply not present on
// this machine — a Proxmox node has no orbctl, and a Mac without kubernetes has
// no kubectl. Reporting those as failures fills the reaper's log with noise on
// every run, which is how a real failure stops being noticed.
func IsToolMissing(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "executable file not found")
}

// Stopper is implemented by backends that can halt a machine without destroying
// it. The reaper needs this to exist for `ccvm keep` to mean anything: a
// machine that cannot be stopped and resumed is not durable.
type Stopper interface {
	Stop(ctx context.Context, h Handle) error
}

// NamePrefix marks a session machine. Backends with no label or metadata field
// — orbstack — rely on it entirely, so it must not be changed casually.
const NamePrefix = "cc-"

// TemplatePrefix marks the images sessions are cloned from. Templates live
// alongside sessions on backends like orbstack, and listing one as a session
// would offer the user a machine whose destruction breaks every future spawn.
const TemplatePrefix = "ccvm-"

// IsSessionName reports whether a machine name is one ccvm created for a
// session, as opposed to a template or a machine the user owns.
func IsSessionName(name string) bool {
	return strings.HasPrefix(name, NamePrefix)
}

// IsTemplateName reports whether a machine is a profile template.
func IsTemplateName(name string) bool {
	return strings.HasPrefix(name, TemplatePrefix)
}

package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/josegonzalez/ccvm/internal/run"
)

// Orbstack runs sessions as OrbStack Linux machines on the Mac.
//
// It sits between docker and proxmox: a real machine boundary rather than a
// shared kernel, but still local and gone when the Mac sleeps. Machines are
// created with --isolated, which disables OrbStack's file sharing and host
// integration — the point being that a session gets no ambient access to the
// Mac.
type Orbstack struct {
	Runner run.Execer
	Bin    string
	// User is the account commands run as inside the machine. Sessions need
	// root to write /etc/ccvm and manage sshd.
	User string
}

var (
	_ Backend = (*Orbstack)(nil)
	_ Stopper = (*Orbstack)(nil)
)

func NewOrbstack(e run.Execer) *Orbstack {
	return &Orbstack{Runner: e, Bin: "orbctl", User: "root"}
}

func (o *Orbstack) Name() string { return "orbstack" }

func (o *Orbstack) bin() string {
	if o.Bin == "" {
		return "orbctl"
	}
	return o.Bin
}

func (o *Orbstack) user() string {
	if o.User == "" {
		return "root"
	}
	return o.User
}

func (o *Orbstack) Preflight(ctx context.Context, s Spec) error {
	out, err := o.Runner.Run(ctx, o.bin(), "status")
	if err != nil {
		return fmt.Errorf("OrbStack is not running: %w", err)
	}
	if !strings.Contains(strings.ToLower(string(out)), "running") {
		return fmt.Errorf("OrbStack reports status %q, want running", strings.TrimSpace(string(out)))
	}

	if s.Image == "" {
		return fmt.Errorf("profile %q has no [backend.orbstack].template", s.Profile)
	}

	// The template is a machine, not a registry reference, so it either exists
	// on this Mac or it does not.
	machines, err := o.machines(ctx)
	if err != nil {
		return err
	}
	for _, m := range machines {
		if m.Name == s.Image {
			return nil
		}
	}
	return fmt.Errorf("template machine %q does not exist; create it with `ccvm profiles build %s --backend orbstack`",
		s.Image, s.Profile)
}

// Create clones the profile's template machine.
//
// Cloning is copy-on-write, so a session costs little disk. The clone lands
// stopped, and OrbStack pauses the template while it runs — concurrent creates
// from one template serialize on it.
func (o *Orbstack) Create(ctx context.Context, s Spec) (Handle, error) {
	if _, err := o.Runner.Run(ctx, o.bin(), "clone", s.Image, s.Name); err != nil {
		return Handle{}, fmt.Errorf("clone %s to %s: %w", s.Image, s.Name, err)
	}
	return Handle{Backend: "orbstack", Name: s.Name, ID: s.Name}, nil
}

func (o *Orbstack) Start(ctx context.Context, h Handle) error {
	_, err := o.Runner.Run(ctx, o.bin(), "start", h.Name)
	return err
}

// Wait polls until the machine reports running and accepts a command. Reporting
// running is not the same as being able to serve one.
func (o *Orbstack) Wait(ctx context.Context, h Handle) error {
	deadline := time.Now().Add(2 * time.Minute)
	var last error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := o.Exec(ctx, h, "true"); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("machine %s did not become usable: %w", h.Name, last)
}

// SSHTarget uses OrbStack's built-in multiplexing ssh server, so nothing has to
// be written to ~/.ssh/config and no port is allocated.
//
// The user is explicit. OrbStack connects as the machine's default account
// otherwise, which cannot read /root — where claude is installed and where
// ccvm puts the session's key and credential. The failure looks like
// "claude: command not found" rather than a permission error, so it is worth
// being unambiguous here.
func (o *Orbstack) SSHTarget(h Handle) string {
	return o.user() + "@" + h.Name + "@orb"
}

func (o *Orbstack) Exec(ctx context.Context, h Handle, argv ...string) ([]byte, error) {
	// orbctl run takes the command directly; a "--" separator is rejected.
	full := append([]string{o.bin(), "run", "-m", h.Name, "-u", o.user()}, argv...)
	return o.Runner.Run(ctx, full...)
}

func (o *Orbstack) Push(ctx context.Context, h Handle, src, dst string) error {
	_, err := o.Runner.Run(ctx, o.bin(), "push", "-m", h.Name, src, dst)
	return err
}

// Pull works on a stopped machine, which is what lets the reaper read a
// stopped machine's TTL and keeps orbstack on the same metadata rule as every
// other backend.
func (o *Orbstack) Pull(ctx context.Context, h Handle, src, dst string) error {
	_, err := o.Runner.Run(ctx, o.bin(), "pull", "-m", h.Name, src, dst)
	return err
}

// orbMachine is the subset of `orbctl list -f json` this backend reads.
type orbMachine struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	Image struct {
		Distro string `json:"distro"`
	} `json:"image"`
	Config struct {
		Isolated bool `json:"isolated"`
	} `json:"config"`
}

func (o *Orbstack) machines(ctx context.Context) ([]orbMachine, error) {
	out, err := o.Runner.Run(ctx, o.bin(), "list", "-f", "json")
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var machines []orbMachine
	if err := json.Unmarshal([]byte(trimmed), &machines); err != nil {
		return nil, fmt.Errorf("parse orbctl list output: %w", err)
	}
	return machines, nil
}

// List reports ccvm's machines.
//
// OrbStack has no label or metadata field, so ownership is inferred from the
// name prefix and the session record inside the machine supplies the rest.
func (o *Orbstack) List(ctx context.Context) ([]Machine, error) {
	machines, err := o.machines(ctx)
	if err != nil {
		return nil, err
	}
	var out []Machine
	for _, m := range machines {
		// Templates share the machine list with sessions here, and destroying
		// one would break every future spawn from that profile.
		if !IsSessionName(m.Name) {
			continue
		}
		out = append(out, Machine{
			Name:    m.Name,
			Backend: "orbstack",
			ID:      m.ID,
			State:   normalizeOrbState(m.State),
			SSH:     o.user() + "@" + m.Name + "@orb",
		})
	}
	return out, nil
}

func normalizeOrbState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "running":
		return StateRunning
	case "stopped":
		return StateStopped
	case "starting", "stopping":
		return StatePending
	default:
		return StateUnknown
	}
}

func (o *Orbstack) Stop(ctx context.Context, h Handle) error {
	_, err := o.Runner.Run(ctx, o.bin(), "stop", h.Name)
	return err
}

func (o *Orbstack) Destroy(ctx context.Context, h Handle) error {
	_, err := o.Runner.Run(ctx, o.bin(), "delete", "-f", h.Name)
	return err
}

func init() {
	Register("orbstack", func(e run.Execer, _ Config) (Backend, error) {
		return NewOrbstack(e), nil
	})
}

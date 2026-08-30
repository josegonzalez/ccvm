package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/josegonzalez/ccvm/internal/run"
)

// Docker runs sessions as containers on the local daemon. It is the fastest
// backend and the weakest isolation: a shared kernel on the same host as your
// real work.
type Docker struct {
	Runner run.Execer
	// Bin is the docker binary, overridable for testing against a shim.
	Bin string
}

var _ Backend = (*Docker)(nil)

func NewDocker(e run.Execer) *Docker { return &Docker{Runner: e, Bin: "docker"} }

func (d *Docker) Name() string { return "docker" }

func (d *Docker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

func (d *Docker) Preflight(ctx context.Context, s Spec) error {
	if _, err := d.Runner.Run(ctx, d.bin(), "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("docker daemon is not reachable: %w", err)
	}
	if s.Image == "" {
		return fmt.Errorf("profile %q has no [backend.docker].image", s.Profile)
	}
	if _, err := d.Runner.Run(ctx, d.bin(), "image", "inspect", s.Image); err != nil {
		// Not fatal on its own — the image may be pullable — but the caller
		// gets to decide, so report it distinctly.
		return fmt.Errorf("image %q is not present locally: %w", s.Image, err)
	}
	return nil
}

func (d *Docker) Create(ctx context.Context, s Spec) (Handle, error) {
	argv := []string{d.bin(), "run", "--detach", "--name", s.Name, "--hostname", s.Name}

	// --rm is conditional on Keep. A kept container must survive stopping, or
	// there is nothing left to inspect and `ccvm keep` is meaningless.
	if !s.Keep {
		argv = append(argv, "--rm")
	}

	// Bind to loopback. A session container must never be reachable from the
	// network.
	if s.SSHPort > 0 {
		argv = append(argv, "--publish", fmt.Sprintf("127.0.0.1:%d:22", s.SSHPort))
	}

	for _, kv := range labelArgs(s) {
		argv = append(argv, "--label", kv)
	}

	if s.CodeMode == "mount" && s.Project != "" {
		argv = append(argv, "--volume", fmt.Sprintf("%s:%s", filepath.Clean(s.Project), s.WorkDir))
	}

	if s.CPUs > 0 {
		argv = append(argv, "--cpus", strconv.Itoa(s.CPUs))
	}
	if s.Memory != "" {
		argv = append(argv, "--memory", s.Memory)
	}

	for _, k := range sortedKeys(s.Env) {
		argv = append(argv, "--env", k+"="+s.Env[k])
	}

	argv = append(argv, s.Image)

	out, err := d.Runner.Run(ctx, argv...)
	if err != nil {
		return Handle{}, fmt.Errorf("create container %s: %w", s.Name, err)
	}
	return Handle{
		Backend: "docker",
		Name:    s.Name,
		ID:      strings.TrimSpace(string(out)),
		SSHPort: s.SSHPort,
	}, nil
}

// Start is a no-op: docker run already started the container. It exists so the
// lifecycle reads the same across backends.
func (d *Docker) Start(ctx context.Context, h Handle) error { return nil }

func (d *Docker) Wait(ctx context.Context, h Handle) error {
	out, err := d.Runner.Run(ctx, d.bin(), "inspect", "--format", "{{.State.Running}}", h.Name)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", h.Name, err)
	}
	if strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("container %s is not running", h.Name)
	}
	return nil
}

func (d *Docker) SSHTarget(h Handle) string { return h.Name }

func (d *Docker) Exec(ctx context.Context, h Handle, argv ...string) ([]byte, error) {
	return d.Runner.Run(ctx, append([]string{d.bin(), "exec", h.Name}, argv...)...)
}

func (d *Docker) Push(ctx context.Context, h Handle, src, dst string) error {
	_, err := d.Runner.Run(ctx, d.bin(), "cp", src, h.Name+":"+dst)
	return err
}

// Pull uses docker cp, which works on a stopped container. That is the whole
// reason mutable metadata lives in a file rather than a label: the reaper has
// to read TTL from machines that are no longer running.
func (d *Docker) Pull(ctx context.Context, h Handle, src, dst string) error {
	_, err := d.Runner.Run(ctx, d.bin(), "cp", h.Name+":"+src, dst)
	return err
}

// dockerPS is the subset of `docker ps --format json` this backend reads.
type dockerPS struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	State  string `json:"State"`
	Labels string `json:"Labels"`
}

func (d *Docker) List(ctx context.Context) ([]Machine, error) {
	out, err := d.Runner.Run(ctx, d.bin(), "ps", "--all",
		"--filter", "label="+LabelOwner, "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	var machines []Machine
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p dockerPS
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("parse docker ps output: %w", err)
		}
		machines = append(machines, machineFromPS(p))
	}
	return machines, nil
}

func machineFromPS(p dockerPS) Machine {
	labels := parseLabels(p.Labels)
	m := Machine{
		Name:    firstName(p.Names),
		Backend: "docker",
		ID:      p.ID,
		State:   normalizeDockerState(p.State),
		Profile: labels[LabelProfile],
		Project: labels[LabelProject],
	}
	if ts, err := time.Parse(time.RFC3339, labels[LabelCreated]); err == nil {
		m.Created = ts
	}
	m.SSH = m.Name
	return m
}

func normalizeDockerState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "running":
		return StateRunning
	case "created", "restarting", "paused":
		return StatePending
	case "exited", "dead", "removing":
		return StateStopped
	case "":
		return StateUnknown
	default:
		return StateUnknown
	}
}

func (d *Docker) Destroy(ctx context.Context, h Handle) error {
	_, err := d.Runner.Run(ctx, d.bin(), "rm", "--force", "--volumes", h.Name)
	return err
}

func labelArgs(s Spec) []string {
	created := s.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	// Only immutable creation facts. TTL is deliberately absent: docker labels
	// cannot be changed after creation, so a TTL stored here could never be
	// moved by `ccvm keep`.
	return []string{
		LabelOwner + "=1",
		LabelProject + "=" + s.Project,
		LabelProfile + "=" + s.Profile,
		LabelCodeMode + "=" + s.CodeMode,
		LabelCreated + "=" + created.UTC().Format(time.RFC3339),
		LabelSSHPort + "=" + strconv.Itoa(s.SSHPort),
	}
}

// parseLabels reads docker's comma-separated key=value label rendering.
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[strings.TrimSpace(k)] = v
		}
	}
	return out
}

func firstName(s string) string {
	if n, _, ok := strings.Cut(s, ","); ok {
		return n
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// AutoRemoves reports whether the daemon will delete this container when it
// stops. The flag is fixed at creation, so a container made without --keep
// cannot be fully protected by a later `ccvm keep`.
func (d *Docker) AutoRemoves(ctx context.Context, h Handle) (bool, error) {
	out, err := d.Runner.Run(ctx, d.bin(), "inspect", "--format", "{{.HostConfig.AutoRemove}}", h.Name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

var _ EphemeralReporter = (*Docker)(nil)

// Package backendtest provides a Backend implementation for testing the layers
// above it — the reaper's decisions, the listing, the rollback stack — without
// any infrastructure.
//
// It lives outside the backend package so test scaffolding does not ship in the
// production one, or count against its coverage.
package backendtest

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/josegonzalez/ccvm/internal/backend"
)

// Fake is a backend.Backend implementation for testing the layers above it — the reaper's decisions,
// the listing, the rollback stack — without any infrastructure.
//
// It is strict about the same things the real backends are: reading a session
// file that was never written fails, and destroying an unknown machine fails.
type Fake struct {
	BackendName string

	mu        sync.Mutex
	machines  map[string]*backend.Machine
	files     map[string]map[string][]byte // machine -> path -> contents
	execCalls [][]string

	// Failures let a test script a specific operation failing.
	CreateErr  error
	StartErr   error
	WaitErr    error
	ListErr    error
	DestroyErr error
	PushErr    error
	// PushErrAfter fails the Nth push onward (1 = the second push), for
	// testing a failure partway through a sequence of them.
	PushErrAfter int
	pushes       int
	// ExecErrOn makes any Exec whose argv contains this string fail, for
	// testing what happens partway through a sequence.
	ExecErrOn string
	// ListDelay makes List slow, for testing that callers bound it.
	ListDelay time.Duration
	// PreflightErr makes Preflight fail, for testing how callers report it.
	PreflightErr error
	// BareGuest models a machine cloned from a template that never had the
	// guest binaries installed, which is the proxmox case.
	BareGuest   bool
	AutoRemove  bool
	Destroyed   []string
	CreateCalls int
}

var (
	_ backend.Backend           = (*Fake)(nil)
	_ backend.EphemeralReporter = (*Fake)(nil)
)

func NewFake(name string) *Fake {
	return &Fake{
		BackendName: name,
		machines:    map[string]*backend.Machine{},
		files:       map[string]map[string][]byte{},
	}
}

func (f *Fake) Name() string { return f.BackendName }

func (f *Fake) Preflight(ctx context.Context, s backend.Spec) error { return f.PreflightErr }

func (f *Fake) Create(ctx context.Context, s backend.Spec) (backend.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreateCalls++
	if f.CreateErr != nil {
		return backend.Handle{}, f.CreateErr
	}
	f.machines[s.Name] = &backend.Machine{
		Name:    s.Name,
		Backend: f.BackendName,
		ID:      "fake-" + s.Name,
		State:   backend.StateRunning,
		Profile: s.Profile,
		Project: s.Project,
		Created: s.CreatedAt,
		SSH:     s.Name,
	}
	// A real session image carries the guest binaries: docker and kubernetes
	// bake them, and the orbstack template build pushes them. Modelling a guest
	// without them by default would make every test exercise the install path
	// that only proxmox actually needs. BareGuest opts into that case.
	f.files[s.Name] = map[string][]byte{}
	if !f.BareGuest {
		f.files[s.Name]["/usr/local/bin/ccvm-done"] = []byte("#!/bin/sh\n")
		f.files[s.Name]["/usr/local/bin/ccvm-init"] = []byte("#!/bin/sh\n")
	}
	return backend.Handle{Backend: f.BackendName, Name: s.Name, ID: "fake-" + s.Name}, nil
}

func (f *Fake) Start(ctx context.Context, h backend.Handle) error { return f.StartErr }
func (f *Fake) Wait(ctx context.Context, h backend.Handle) error  { return f.WaitErr }

func (f *Fake) SSHTarget(h backend.Handle) string { return h.Name }

// ExecCalls returns every command run inside a machine, for assertions.
func (f *Fake) ExecCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.execCalls))
	copy(out, f.execCalls)
	return out
}

// Ran reports whether argv was run inside a machine.
func (f *Fake) Ran(argv ...string) bool {
	for _, c := range f.ExecCalls() {
		if len(c) != len(argv) {
			continue
		}
		same := true
		for i := range c {
			if c[i] != argv[i] {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// Exec supports only what the layers above actually run: the sentinel check.
func (f *Fake) Exec(ctx context.Context, h backend.Handle, argv ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCalls = append(f.execCalls, append([]string(nil), argv...))
	if f.ExecErrOn != "" {
		for _, a := range argv {
			if strings.Contains(a, f.ExecErrOn) {
				return nil, fmt.Errorf("exec %v: scripted failure", argv)
			}
		}
	}
	// -x as well as -f: callers check for an executable before installing one,
	// and answering success for a file that was never pushed would let that
	// check pass over a guest that has nothing.
	if len(argv) == 3 && argv[0] == "test" && (argv[1] == "-f" || argv[1] == "-x") {
		if _, ok := f.files[h.Name][argv[2]]; ok {
			return nil, nil
		}
		return nil, fmt.Errorf("test %s %s: no such file", argv[1], argv[2])
	}
	// The guest architecture, so callers can pick a binary for it.
	if len(argv) == 2 && argv[0] == "uname" && argv[1] == "-m" {
		return []byte("x86_64\n"), nil
	}
	if len(argv) > 0 && (argv[0] == "mkdir" || argv[0] == "chmod" || argv[0] == "chown") {
		return nil, nil
	}
	return nil, nil
}

func (f *Fake) Push(ctx context.Context, h backend.Handle, src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes++
	if f.PushErr != nil {
		return f.PushErr
	}
	if f.PushErrAfter > 0 && f.pushes > f.PushErrAfter {
		return fmt.Errorf("push %d: scripted failure", f.pushes)
	}
	data, err := readFile(src)
	if err != nil {
		return err
	}
	if f.files[h.Name] == nil {
		f.files[h.Name] = map[string][]byte{}
	}
	f.files[h.Name][dst] = data
	return nil
}

func (f *Fake) Pull(ctx context.Context, h backend.Handle, src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[h.Name][src]
	if !ok {
		return fmt.Errorf("no such file in %s: %s", h.Name, src)
	}
	return writeFile(dst, data)
}

func (f *Fake) List(ctx context.Context) ([]backend.Machine, error) {
	if f.ListDelay > 0 {
		select {
		case <-time.After(f.ListDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	out := make([]backend.Machine, 0, len(f.machines))
	for _, m := range f.machines {
		out = append(out, *m)
	}
	return out, nil
}

func (f *Fake) Destroy(ctx context.Context, h backend.Handle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.DestroyErr != nil {
		return f.DestroyErr
	}
	if _, ok := f.machines[h.Name]; !ok {
		return fmt.Errorf("no such machine: %s", h.Name)
	}
	delete(f.machines, h.Name)
	delete(f.files, h.Name)
	f.Destroyed = append(f.Destroyed, h.Name)
	return nil
}

func (f *Fake) AutoRemoves(ctx context.Context, h backend.Handle) (bool, error) {
	return f.AutoRemove, nil
}

// Seed adds a machine directly, for tests that need one to already exist.
func (f *Fake) Seed(m backend.Machine, files map[string][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m.Backend = f.BackendName
	f.machines[m.Name] = &m
	if files == nil {
		files = map[string][]byte{}
	}
	f.files[m.Name] = files
}

// SetState changes a seeded machine's state, for stopped-machine cases.
func (f *Fake) SetState(name, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.machines[name]; ok {
		m.State = state
	}
}

// FilesIn returns every file written into a machine, for assertions that do
// not know the exact path.
func (f *Fake) FilesIn(name string) map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]byte{}
	maps.Copy(out, f.files[name])
	return out
}

// FileIn returns a file's contents from inside a machine.
func (f *Fake) FileIn(name, path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[name][path]
	return b, ok
}

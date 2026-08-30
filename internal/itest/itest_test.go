package itest_test

import (
	"errors"
	"testing"

	"github.com/josegonzalez/ccvm/internal/itest"
)

func TestRequestedParsesList(t *testing.T) {
	tests := []struct {
		env  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"docker", []string{"docker"}},
		{"docker,k8s", []string{"docker", "k8s"}},
		{" docker , k8s ", []string{"docker", "k8s"}},
		{"docker,,k8s", []string{"docker", "k8s"}},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv(itest.EnvBackends, tt.env)
			got := itest.Requested()
			if len(got) != len(tt.want) {
				t.Fatalf("Requested() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Requested()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestIsRequested(t *testing.T) {
	t.Setenv(itest.EnvBackends, "docker,proxmox")
	if !itest.IsRequested("proxmox") {
		t.Error("proxmox should be requested")
	}
	if itest.IsRequested("k8s") {
		t.Error("k8s should not be requested")
	}
}

func TestImageDefaultsAndOverrides(t *testing.T) {
	t.Setenv(itest.EnvImage, "")
	if got := itest.Image(); got != itest.DefaultImage {
		t.Errorf("Image() = %q, want the default", got)
	}
	t.Setenv(itest.EnvImage, "registry/ccvm:ci")
	if got := itest.Image(); got != "registry/ccvm:ci" {
		t.Errorf("Image() = %q", got)
	}
}

// The gate's whole purpose: a backend the operator asked for must not quietly
// skip when it turns out to be unavailable.
func TestGateDecisions(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		backend     string
		probe       error
		wantFailed  bool
		wantSkipped bool
	}{
		{
			name:       "requested but unavailable fails",
			env:        "docker",
			backend:    "docker",
			probe:      errors.New("daemon not running"),
			wantFailed: true,
		},
		{
			name:        "not requested skips",
			env:         "docker",
			backend:     "proxmox",
			probe:       errors.New("no cluster"),
			wantSkipped: true,
		},
		{
			name:    "requested and healthy runs",
			env:     "docker",
			backend: "docker",
			probe:   nil,
		},
		{
			name:        "nothing requested skips everything",
			env:         "",
			backend:     "docker",
			wantSkipped: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(itest.EnvBackends, tt.env)
			rec := runGate(tt.backend, func() error { return tt.probe })

			if rec.failed != tt.wantFailed {
				t.Errorf("failed = %v, want %v", rec.failed, tt.wantFailed)
			}
			if rec.skipped != tt.wantSkipped {
				t.Errorf("skipped = %v, want %v", rec.skipped, tt.wantSkipped)
			}
		})
	}
}

// runGate captures Gate's decision. Fatalf and Skipf unwind in the real
// testing.T, so the recorder does too and the panic is caught here.
func runGate(backend string, probe func() error) *recorder {
	rec := &recorder{}
	func() {
		defer func() { _ = recover() }()
		itest.Gate(rec, backend, probe)
	}()
	return rec
}

type recorder struct {
	failed  bool
	skipped bool
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(string, ...any) {
	r.failed = true
	panic(gateStop{})
}

func (r *recorder) Skipf(string, ...any) {
	r.skipped = true
	panic(gateStop{})
}

type gateStop struct{}

// `go test ./...` without the env var should stay quiet rather than reporting
// a wall of skips.
func TestSkipUnlessAnyRequested(t *testing.T) {
	t.Setenv(itest.EnvBackends, "")
	rec := &recorder{}
	func() {
		defer func() { _ = recover() }()
		itest.SkipUnlessAnyRequested(rec)
	}()
	if !rec.skipped {
		t.Error("did not skip with nothing requested")
	}

	t.Setenv(itest.EnvBackends, "docker")
	rec2 := &recorder{}
	func() {
		defer func() { _ = recover() }()
		itest.SkipUnlessAnyRequested(rec2)
	}()
	if rec2.skipped {
		t.Error("skipped even though a backend was requested")
	}
}

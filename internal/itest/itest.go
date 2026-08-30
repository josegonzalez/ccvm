// Package itest is the opt-in gate for integration tests.
//
// The rule it enforces: naming a backend in CCVM_ITEST_BACKENDS makes it
// required, and a required backend that is unavailable FAILS rather than skips.
// A suite that silently skips when infrastructure is missing reports green
// while testing nothing, which is worse than having no suite at all.
//
// Omitting a backend from the list is a deliberate choice a human makes. The
// test never decides for itself.
package itest

import (
	"os"
	"strings"
)

// TB is the slice of *testing.T that Gate needs. Narrowing it this way lets the
// gate's own decisions be tested, which matters: a gate that skips when it
// should fail defeats the point of having one.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// EnvBackends names the backends a run is required to exercise.
const EnvBackends = "CCVM_ITEST_BACKENDS"

// EnvImage overrides the image integration tests build machines from.
const EnvImage = "CCVM_ITEST_IMAGE"

// DefaultImage is the base session image built by `make image`.
const DefaultImage = "ccvm/base:latest"

// Requested returns the backends named in CCVM_ITEST_BACKENDS, in order.
func Requested() []string {
	raw := os.Getenv(EnvBackends)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, f := range strings.Split(raw, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// IsRequested reports whether name was named in CCVM_ITEST_BACKENDS.
func IsRequested(name string) bool {
	for _, n := range Requested() {
		if n == name {
			return true
		}
	}
	return false
}

// Gate decides whether a backend's integration tests run.
//
// Not requested: skip, because the operator chose not to test it here.
// Requested but broken: fail, because the operator said it should work.
func Gate(t TB, name string, probe func() error) {
	t.Helper()

	if !IsRequested(name) {
		t.Skipf("%s not in %s=%q", name, EnvBackends, os.Getenv(EnvBackends))
	}

	if probe == nil {
		return
	}
	if err := probe(); err != nil {
		t.Fatalf("%s was required by %s but is unavailable: %v\n\n"+
			"Remove it from %s if you did not mean to test it here.",
			name, EnvBackends, err, EnvBackends)
	}
}

// Image returns the image integration tests should build machines from.
func Image() string {
	if v := strings.TrimSpace(os.Getenv(EnvImage)); v != "" {
		return v
	}
	return DefaultImage
}

// SkipUnlessAnyRequested stops a whole suite early when nothing was requested,
// so `go test ./...` without the env var stays quiet rather than reporting a
// wall of skips.
func SkipUnlessAnyRequested(t TB) {
	t.Helper()
	if len(Requested()) == 0 {
		t.Skipf("no backends requested; set %s to run integration tests", EnvBackends)
	}
}

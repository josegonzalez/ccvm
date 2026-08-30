package main

import (
	"errors"
	"strings"
	"testing"
)

// The error taxonomy is a claim about exact output, so assert on it.
func TestFaultRendersStepCauseAndFix(t *testing.T) {
	f := &Fault{
		Backend: "docker",
		Step:    "create container cc-foo",
		Cause:   errors.New("Cannot connect to the Docker daemon"),
		Fix:     "start Docker Desktop, or switch with --backend orbstack",
	}
	got := f.Error()
	for _, want := range []string{
		"backend: docker",
		"step:    create container cc-foo",
		"cause:   Cannot connect to the Docker daemon",
		"fix:     start Docker Desktop",
		"no machine was created; nothing to clean up.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Multi-line tool output must stay inside the block rather than breaking it.
func TestFaultIndentsMultilineCause(t *testing.T) {
	f := &Fault{
		Backend: "k8s",
		Step:    "wait for pod cc-foo to become Ready",
		Cause:   errors.New("ImagePullBackOff\npull access denied"),
	}
	got := f.Error()
	if !strings.Contains(got, "cause:   ImagePullBackOff\n           pull access denied") {
		t.Errorf("continuation line not aligned:\n%s", got)
	}
}

// When something was created and then torn down, say so — the reader needs to
// know whether to go tidy up.
func TestFaultReportsCleanup(t *testing.T) {
	f := &Fault{Step: "start", Cleanup: "pod deleted; nothing left running."}
	if !strings.Contains(f.Error(), "pod deleted") {
		t.Errorf("cleanup not reported:\n%s", f.Error())
	}
}

func TestFaultUnwraps(t *testing.T) {
	cause := errors.New("boom")
	f := &Fault{Cause: cause}
	if !errors.Is(f, cause) {
		t.Error("Fault does not unwrap to its cause")
	}
}

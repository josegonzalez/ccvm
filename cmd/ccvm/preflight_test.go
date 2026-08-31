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

// The taxonomy exists so a failure says what to do about it. These are the
// failures people actually hit, and each produced either nothing or advice
// about a local daemon that does not exist in that backend's world.
func TestFixForNamesTheActualRemedy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend string
		err     error
		want    string
	}{
		{"orbstack down", "orbstack", errors.New("OrbStack is not running: exit 1"), "start OrbStack"},
		{"proxmox bad token", "proxmox", errors.New(`Proxmox rejected token "root@pam!x": 401`), "CCVM_PROXMOX_TOKEN_ID"},
		{"proxmox thin token", "proxmox", errors.New(`token "x" lacks the needed privileges: 403`), "VM.Clone"},
		{"proxmox unreachable", "proxmox", errors.New("Proxmox API at https://x is not reachable: timeout"), "CCVM_PROXMOX_URL"},
		{"k8s no context", "k8s", errors.New("not configured: no kubectl context is selected"), "use-context"},
		{"k8s missing namespace", "k8s", errors.New(`namespace "ccvm" is not reachable`), "create namespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fixFor(tc.backend, tc.err)
			if got == "" {
				t.Fatalf("no fix offered for %v", tc.err)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("fix = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// A global -v is ccvm's, but everything after `--` is the guest's. Stripping it
// there rewrote the user's command: `grep -v foo` became `grep foo`, which
// matches the opposite lines and says nothing about having done so.
func TestSplitVerboseLeavesGuestArgumentsAlone(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		wantArgs    []string
		wantVerbose bool
	}{
		{"before the separator", []string{"-v", "box"}, []string{"box"}, true},
		{"long form", []string{"--verbose", "box"}, []string{"box"}, true},
		{"after the separator", []string{"box", "--", "grep", "-v", "foo"},
			[]string{"box", "--", "grep", "-v", "foo"}, false},
		{"both sides", []string{"-v", "box", "--", "grep", "-v", "foo"},
			[]string{"box", "--", "grep", "-v", "foo"}, true},
		{"none", []string{"box"}, []string{"box"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, verbose := splitVerbose(append([]string(nil), tc.args...))
			if strings.Join(got, " ") != strings.Join(tc.wantArgs, " ") {
				t.Errorf("args = %v, want %v", got, tc.wantArgs)
			}
			if verbose != tc.wantVerbose {
				t.Errorf("verbose = %v, want %v", verbose, tc.wantVerbose)
			}
		})
	}
}

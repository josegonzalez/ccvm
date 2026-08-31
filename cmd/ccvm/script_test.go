package main

import (
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// TestScript runs the golden files in testdata/script.
//
// These cover what a user sees before a backend is ever touched: argument
// handling, contradictory flags, a missing credential, and the ordering between
// those checks. They run the real binary, so a regression in layout shows up
// here rather than in a mock's idea of it.
//
// They deliberately do not cover a failed Create - reaching that needs a
// backend that fails on demand, which the golden runner has no way to inject.
// That case is asserted against the fake backend in TestUpReportsACreateFailure
// instead.
func TestMain(m *testing.M) {
	// Main rather than the deprecated RunMain: Go collects coverage from the
	// subprocesses itself now, so the commands no longer return exit codes.
	testscript.Main(m, map[string]func(){
		// The scripts drive a real ccvm, so a regression in argument handling
		// or error formatting shows up here rather than in a mock's idea of it.
		"ccvm": main,
	})
}

func TestScript(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata/script",
		Setup: func(e *testscript.Env) error {
			// A hermetic HOME: these must not read the developer's real
			// credentials, ssh config, or profiles, and must not write to them.
			e.Setenv("HOME", e.WorkDir)
			e.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
			e.Setenv("CCVM_CREDENTIALS_FILE", "")
			e.Setenv("CCVM_PROXMOX_URL", "")
			e.Setenv("CCVM_KUBE_NAMESPACE", "")
			e.Setenv("CCVM_KUBE_CONTEXT", "")
			return nil
		},
	})
}

package main

import (
	"fmt"
	"strings"
)

// Fault is a failure that stopped ccvm before it created anything.
//
// The shape is deliberate: naming the step, the underlying cause, and the fix
// is the difference between an error you can act on and one you have to
// investigate. The taxonomy requirement is really a claim about exact output.
type Fault struct {
	Backend string
	Step    string
	Cause   error
	Fix     string
	// Cleanup describes what was left behind, so the reader knows whether they
	// have to go tidy up.
	Cleanup string
	// Retryable marks faults worth another attempt with different inputs, such
	// as a name or port collision. Auth and missing-image faults are not.
	Retryable bool
}

func (f *Fault) Error() string {
	var b strings.Builder
	b.WriteString("failed to create session machine\n\n")
	if f.Backend != "" {
		fmt.Fprintf(&b, "  backend: %s\n", f.Backend)
	}
	if f.Step != "" {
		fmt.Fprintf(&b, "  step:    %s\n", f.Step)
	}
	if f.Cause != nil {
		fmt.Fprintf(&b, "  cause:   %s\n", indent(f.Cause.Error(), "           "))
	}
	if f.Fix != "" {
		fmt.Fprintf(&b, "  fix:     %s\n", indent(f.Fix, "           "))
	}
	cleanup := f.Cleanup
	if cleanup == "" {
		cleanup = "no machine was created; nothing to clean up."
	}
	fmt.Fprintf(&b, "\n  %s", cleanup)
	return b.String()
}

func (f *Fault) Unwrap() error { return f.Cause }

// indent aligns continuation lines under the first, so multi-line tool output
// stays inside the block rather than breaking its shape.
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

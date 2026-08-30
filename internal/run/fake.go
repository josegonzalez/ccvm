package run

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Fake is an Execer for tests. It records every invocation and replays scripted
// results, so a backend's command construction can be asserted with no daemon,
// cluster, or hypervisor present.
//
// It is strict by design: an unmatched command fails rather than returning a
// zero value. A test that silently tolerates unexpected commands stops noticing
// when a backend starts issuing them.
type Fake struct {
	mu    sync.Mutex
	rules []*Rule
	calls [][]string
}

// Rule is a scripted response for commands matching a prefix.
type Rule struct {
	prefix []string
	stdout []byte
	err    error
	limit  int // 0 means unlimited
	used   int
}

func NewFake() *Fake { return &Fake{} }

// On scripts a response for the next command whose argv starts with prefix.
// Rules are matched in declaration order; the first live match wins.
func (f *Fake) On(prefix ...string) *Rule {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := &Rule{prefix: prefix}
	f.rules = append(f.rules, r)
	return r
}

// Stdout sets what the matched command prints.
func (r *Rule) Stdout(s string) *Rule { r.stdout = []byte(s); return r }

// Fail makes the matched command exit non-zero with the given stderr, as a
// real tool would.
func (r *Rule) Fail(code int, stderr string) *Rule {
	r.err = &CmdError{Argv: r.prefix, Code: code, Stderr: stderr}
	return r
}

// ErrorWith makes the matched command fail with an arbitrary error, for
// modelling "binary not found" rather than "command exited non-zero".
func (r *Rule) ErrorWith(err error) *Rule { r.err = err; return r }

// Once limits the rule to a single match, so a test can script a failure
// followed by a success — the retry paths depend on it.
func (r *Rule) Once() *Rule { r.limit = 1; return r }

// Times limits the rule to n matches.
func (r *Rule) Times(n int) *Rule { r.limit = n; return r }

func (f *Fake) Run(ctx context.Context, argv ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), argv...))

	for _, r := range f.rules {
		if r.limit > 0 && r.used >= r.limit {
			continue
		}
		if hasPrefix(argv, r.prefix) {
			r.used++
			if r.err != nil {
				if ce, ok := r.err.(*CmdError); ok {
					// Report the argv actually run, not the matching prefix.
					return r.stdout, &CmdError{Argv: argv, Code: ce.Code, Stderr: ce.Stderr}
				}
				return r.stdout, r.err
			}
			return r.stdout, nil
		}
	}
	return nil, fmt.Errorf("fake: unscripted command: %s", ShellQuote(argv))
}

// Calls returns every command run so far, in order.
func (f *Fake) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

// Lines renders the calls as shell-quoted strings, which makes assertion
// failures readable.
func (f *Fake) Lines() []string {
	calls := f.Calls()
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = ShellQuote(c)
	}
	return out
}

// Ran reports whether any recorded call started with prefix.
func (f *Fake) Ran(prefix ...string) bool {
	for _, c := range f.Calls() {
		if hasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// Find returns the first recorded call starting with prefix, or nil.
func (f *Fake) Find(prefix ...string) []string {
	for _, c := range f.Calls() {
		if hasPrefix(c, prefix) {
			return c
		}
	}
	return nil
}

// ArgAfter returns the argument following flag in the first call matching
// prefix. Reports ok=false when the flag is absent, which distinguishes "not
// passed" from "passed empty".
func (f *Fake) ArgAfter(flag string, prefix ...string) (string, bool) {
	call := f.Find(prefix...)
	for i, a := range call {
		if a == flag && i+1 < len(call) {
			return call[i+1], true
		}
	}
	return "", false
}

// HasArg reports whether the first call matching prefix contains arg. Use it
// for boolean flags such as --rm, where presence is the whole assertion.
func (f *Fake) HasArg(arg string, prefix ...string) bool {
	for _, a := range f.Find(prefix...) {
		if a == arg {
			return true
		}
	}
	return false
}

// Reset clears recorded calls and rule usage, for reuse across subtests.
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.rules = nil
}

func (f *Fake) String() string { return strings.Join(f.Lines(), "\n") }

func hasPrefix(argv, prefix []string) bool {
	if len(prefix) > len(argv) {
		return false
	}
	for i, p := range prefix {
		if argv[i] != p {
			return false
		}
	}
	return true
}

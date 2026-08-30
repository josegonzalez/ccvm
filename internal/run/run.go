// Package run is the seam between ccvm and the external commands its backends
// drive (docker, orbctl, pct, kubectl).
//
// Backends never call os/exec directly. Routing every invocation through an
// Execer does three jobs at once: it makes backends unit-testable with no
// infrastructure running, it gives --verbose a single choke point that logs
// commands verbatim, and it applies context cancellation uniformly.
package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Execer runs an external command and returns its stdout.
//
// A non-zero exit must be reported as a *CmdError so callers can surface the
// underlying tool's own stderr rather than inventing a message for it.
type Execer interface {
	Run(ctx context.Context, argv ...string) ([]byte, error)
}

// CmdError is a command that ran and failed. It carries the pieces the error
// taxonomy needs: what was run, how it failed, and what the tool said.
type CmdError struct {
	Argv   []string
	Code   int
	Stderr string
	Err    error
}

func (e *CmdError) Error() string {
	cmd := ShellQuote(e.Argv)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		return fmt.Sprintf("%s: exit %d: %s", cmd, e.Code, s)
	}
	return fmt.Sprintf("%s: exit %d", cmd, e.Code)
}

func (e *CmdError) Unwrap() error { return e.Err }

// ExitCode reports the exit status of a failed command, or -1 when err is not
// a command failure.
func ExitCode(err error) int {
	if ce, ok := errors.AsType[*CmdError](err); ok {
		return ce.Code
	}
	return -1
}

// Stderr returns the failed command's stderr, or "" when err is not a command
// failure.
func Stderr(err error) string {
	if ce, ok := errors.AsType[*CmdError](err); ok {
		return ce.Stderr
	}
	return ""
}

// Exec is the real Execer, backed by os/exec.
type Exec struct {
	// Log, when non-nil, receives each command before it runs. This is what
	// --verbose writes to.
	Log io.Writer
}

// New returns an Exec that logs to w. A nil w disables logging.
func New(w io.Writer) *Exec { return &Exec{Log: w} }

func (e *Exec) Run(ctx context.Context, argv ...string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("run: empty argv")
	}
	if e.Log != nil {
		fmt.Fprintf(e.Log, "+ %s\n", ShellQuote(argv))
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}

	// A cancelled or timed-out context is the real story; report it as such
	// rather than as whatever exit code the signal produced.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout.Bytes(), fmt.Errorf("%s: %w", ShellQuote(argv), ctxErr)
	}

	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return stdout.Bytes(), &CmdError{
			Argv:   argv,
			Code:   ee.ExitCode(),
			Stderr: stderr.String(),
			Err:    err,
		}
	}
	// Command could not be started at all: not found, not executable.
	return nil, fmt.Errorf("%s: %w", ShellQuote(argv), err)
}

// ShellQuote renders argv as a line you can paste back into a shell. It exists
// so --verbose output is directly re-runnable, which is the whole point of
// logging it.
func ShellQuote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = quote(a)
	}
	return strings.Join(parts, " ")
}

func quote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`&|;<>()*?[]#~=%{}!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

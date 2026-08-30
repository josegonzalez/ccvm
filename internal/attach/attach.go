// Package attach builds the command that puts you inside a session.
//
// Claude runs under tmux rather than directly under ssh, so a dropped
// connection detaches instead of killing the session. That matters most on k8s,
// where the port-forward will drop, and it is what makes `ccvm attach` able to
// return you to work in progress.
package attach

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/josegonzalez/ccvm/internal/creds"
	"github.com/josegonzalez/ccvm/internal/run"
)

// TmuxSession is the tmux session name inside every guest. ccvm-done kills it
// by this name, so the two must agree.
const TmuxSession = "cc"

// Options describe the session to enter.
type Options struct {
	// Target is an ssh destination: a config alias, or user@host.
	Target string
	// WorkDir is where Claude starts.
	WorkDir string
	// SessionName names the Remote Control session, so it is findable at
	// claude.ai rather than appearing as an anonymous entry.
	SessionName string
	// RemoteControl enables driving the session from claude.ai or a phone. It
	// only works on the login credential path.
	RemoteControl bool
	// Yolo passes --dangerously-skip-permissions.
	Yolo bool
	// IdentityFile is ccvm's private key.
	IdentityFile string
	// ExtraSSHArgs are appended to the ssh invocation, for backends that need
	// a port or a jump host.
	ExtraSSHArgs []string
}

// ClaudeCommand renders the claude invocation that runs inside tmux.
func ClaudeCommand(o Options) string {
	argv := []string{"claude"}
	if o.RemoteControl {
		argv = append(argv, "--remote-control")
		if o.SessionName != "" {
			argv = append(argv, o.SessionName)
		}
	}
	if o.Yolo {
		argv = append(argv, "--dangerously-skip-permissions")
	}
	return run.ShellQuote(argv)
}

// RemoteScript renders what runs on the guest.
//
// The credential is sourced from a file rather than passed as an argument: an
// ssh argument is visible in the process list on both ends and lands in shell
// history. Sourcing failures are tolerated so a guest without a credential
// still opens a shell you can debug in, rather than refusing to start.
func RemoteScript(o Options) string {
	workDir := o.WorkDir
	if workDir == "" {
		workDir = "/work"
	}

	var b strings.Builder
	b.WriteString("set -a; . " + run.ShellQuote([]string{creds.GuestEnvFile}) + " 2>/dev/null || true; set +a; ")
	// tmux new -A attaches to an existing session and creates one otherwise, so
	// reconnecting after a dropped link returns to the same work.
	b.WriteString("exec tmux new -A -s " + TmuxSession +
		" -c " + run.ShellQuote([]string{workDir}) +
		" " + run.ShellQuote([]string{ClaudeCommand(o)}))
	return b.String()
}

// SSHArgs renders the full ssh invocation.
func SSHArgs(o Options) []string {
	argv := []string{"ssh", "-t",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
	if o.IdentityFile != "" {
		argv = append(argv, "-i", o.IdentityFile)
	}
	argv = append(argv, o.ExtraSSHArgs...)
	return append(argv, o.Target, RemoteScript(o))
}

// runInteractive runs a command with this process's terminal attached.
//
// A variable rather than a direct call so tests can observe what would be run:
// Claude is a full-screen program, so these paths cannot be exercised any other
// way, and they are where --yolo and Remote Control actually take effect.
var runInteractive = func(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Run enters the session with the terminal attached.
//
// Exiting the session is a normal outcome, not a failure: ssh reports the
// remote command's status, and a user who quits Claude with a non-zero code
// should not see ccvm report an error.
func Run(o Options) error {
	if o.Target == "" {
		return fmt.Errorf("no ssh target for the session")
	}
	if err := runInteractive(SSHArgs(o)); err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			// Quitting Claude with a non-zero status is a normal way to end a
			// session, not a ccvm failure.
			return nil
		}
		return fmt.Errorf("open the session: %w", err)
	}
	return nil
}

// Shell opens a plain shell rather than a Claude session, for `ccvm ssh`.
func Shell(o Options, command ...string) error {
	argv := []string{"ssh", "-t",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
	if o.IdentityFile != "" {
		argv = append(argv, "-i", o.IdentityFile)
	}
	argv = append(argv, o.ExtraSSHArgs...)
	argv = append(argv, o.Target)
	argv = append(argv, command...)

	if err := runInteractive(argv); err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			// A command that exits non-zero inside the machine is the user's
			// result, not a ccvm error.
			return nil
		}
		return err
	}
	return nil
}

// SetRunnerForTest swaps the interactive runner and returns a restore func.
func SetRunnerForTest(f func(argv []string) error) func() {
	prev := runInteractive
	runInteractive = f
	return func() { runInteractive = prev }
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

package attach_test

import (
	"strings"
	"testing"

	"github.com/josegonzalez/ccvm/internal/attach"
)

func TestClaudeCommandPlain(t *testing.T) {
	got := attach.ClaudeCommand(attach.Options{})
	if got != "claude" {
		t.Errorf("ClaudeCommand = %q, want plain claude", got)
	}
}

// The Remote Control session is named so it is findable at claude.ai rather
// than appearing as an anonymous entry.
func TestClaudeCommandRemoteControlIsNamed(t *testing.T) {
	got := attach.ClaudeCommand(attach.Options{RemoteControl: true, SessionName: "cc-foo"})
	if got != "claude --remote-control cc-foo" {
		t.Errorf("ClaudeCommand = %q", got)
	}
}

func TestClaudeCommandYolo(t *testing.T) {
	got := attach.ClaudeCommand(attach.Options{Yolo: true})
	if !strings.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("ClaudeCommand = %q, want the flag actually passed", got)
	}
}

func TestClaudeCommandBoth(t *testing.T) {
	got := attach.ClaudeCommand(attach.Options{RemoteControl: true, SessionName: "cc-foo", Yolo: true})
	for _, want := range []string{"--remote-control cc-foo", "--dangerously-skip-permissions"} {
		if !strings.Contains(got, want) {
			t.Errorf("ClaudeCommand = %q, missing %q", got, want)
		}
	}
}

// tmux new -A attaches to an existing session rather than creating a second
// one, which is what makes reconnecting return to work in progress.
func TestRemoteScriptAttachesRatherThanDuplicating(t *testing.T) {
	got := attach.RemoteScript(attach.Options{WorkDir: "/work"})
	if !strings.Contains(got, "tmux new -A -s cc") {
		t.Errorf("RemoteScript = %q, want tmux new -A", got)
	}
	if !strings.Contains(got, "-c /work") {
		t.Errorf("RemoteScript = %q, want the working directory", got)
	}
}

// The credential is sourced from a file, never passed as an argument.
func TestRemoteScriptSourcesTheCredentialFile(t *testing.T) {
	got := attach.RemoteScript(attach.Options{})
	if !strings.Contains(got, "/etc/ccvm/env") {
		t.Errorf("RemoteScript = %q, want it to source the credential file", got)
	}
	if !strings.Contains(got, "set -a") {
		t.Errorf("RemoteScript = %q, want the sourced values exported", got)
	}
}

// A guest with no credential should still open a shell you can debug in.
func TestRemoteScriptToleratesAMissingCredentialFile(t *testing.T) {
	got := attach.RemoteScript(attach.Options{})
	if !strings.Contains(got, "|| true") {
		t.Errorf("RemoteScript = %q, want sourcing failures tolerated", got)
	}
}

func TestRemoteScriptDefaultsWorkDir(t *testing.T) {
	got := attach.RemoteScript(attach.Options{})
	if !strings.Contains(got, "-c /work") {
		t.Errorf("RemoteScript = %q, want a default working directory", got)
	}
}

// A directory with a space must survive into tmux as one argument.
func TestRemoteScriptQuotesAwkwardPaths(t *testing.T) {
	got := attach.RemoteScript(attach.Options{WorkDir: "/work dir"})
	if !strings.Contains(got, "'/work dir'") {
		t.Errorf("RemoteScript = %q, want the path quoted", got)
	}
}

// The claude invocation reaches tmux as a single argument, or tmux treats the
// flags as its own.
func TestRemoteScriptPassesClaudeAsOneArgument(t *testing.T) {
	got := attach.RemoteScript(attach.Options{RemoteControl: true, SessionName: "cc-foo"})
	if !strings.Contains(got, "'claude --remote-control cc-foo'") {
		t.Errorf("RemoteScript = %q, want the command quoted as one argument", got)
	}
}

func TestSSHArgsUsesTheIdentityAndPTY(t *testing.T) {
	got := attach.SSHArgs(attach.Options{
		Target:       "cc-foo",
		IdentityFile: "/home/j/.ssh/ccvm_ed25519",
	})
	joined := strings.Join(got, " ")
	// -t: Claude is a full-screen program and needs a terminal.
	if !strings.Contains(joined, "ssh -t") {
		t.Errorf("SSHArgs = %q, want a pty allocated", joined)
	}
	if !strings.Contains(joined, "-i /home/j/.ssh/ccvm_ed25519") {
		t.Errorf("SSHArgs = %q, want ccvm's key", joined)
	}
	if got[len(got)-2] != "cc-foo" {
		t.Errorf("target = %q, want it immediately before the remote command", got[len(got)-2])
	}
}

// Disposable machines reuse names and addresses, so their host keys change.
func TestSSHArgsRelaxesHostKeyChecking(t *testing.T) {
	joined := strings.Join(attach.SSHArgs(attach.Options{Target: "cc-foo"}), " ")
	for _, want := range []string{"StrictHostKeyChecking=no", "UserKnownHostsFile=/dev/null"} {
		if !strings.Contains(joined, want) {
			t.Errorf("SSHArgs missing %q", want)
		}
	}
}

func TestSSHArgsCarriesExtraArgs(t *testing.T) {
	got := attach.SSHArgs(attach.Options{Target: "cc-foo", ExtraSSHArgs: []string{"-p", "2231"}})
	if !strings.Contains(strings.Join(got, " "), "-p 2231") {
		t.Errorf("SSHArgs = %v, want the extra args", got)
	}
}

func TestRunRequiresATarget(t *testing.T) {
	if err := attach.Run(attach.Options{}); err == nil {
		t.Fatal("expected an error with no target")
	}
}

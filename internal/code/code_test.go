package code_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/backendtest"
	"github.com/josegonzalez/ccvm/internal/code"
	"github.com/josegonzalez/ccvm/internal/run"
)

// kubernetes has no host filesystem to reach, so mount is not merely
// unimplemented there; it cannot exist.
func TestCheck(t *testing.T) {
	tests := []struct {
		mode, backend string
		wantErr       bool
	}{
		{code.Mount, "docker", false},
		{code.Mount, "orbstack", false},
		// Refused, not silently empty: the project is on this machine and the
		// guest is on a remote cluster, with no filesystem in common.
		{code.Mount, "proxmox", true},
		{code.Mount, "k8s", true},
		{code.Git, "k8s", false},
		{code.Rsync, "k8s", false},
		// sshfs is the live-mount story for a remote guest, which is what
		// proxmox is. It was advertised in the flag help and unimplemented,
		// and this case pinned that mismatch as correct.
		{code.Sshfs, "proxmox", false},
		// Not on k8s: the ssh path there is itself a port-forward, and nesting
		// a reverse tunnel inside it is more fragile than a clone.
		{code.Sshfs, "k8s", true},
		{"nonsense", "docker", true},
	}
	for _, tt := range tests {
		t.Run(tt.mode+"/"+tt.backend, func(t *testing.T) {
			err := code.Check(tt.mode, tt.backend)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Check(%q, %q) = %v, wantErr %v", tt.mode, tt.backend, err, tt.wantErr)
			}
		})
	}
}

// A refusal has to say what the backend does support, or the user is left
// guessing which of four modes to try.
func TestCheckNamesTheAlternatives(t *testing.T) {
	err := code.Check(code.Mount, "k8s")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"git", "rsync"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to offer %q", err, want)
		}
	}
}

// The default is the cheapest mode that still carries uncommitted work.
func TestDefaultFor(t *testing.T) {
	tests := map[string]string{
		"docker":   code.Mount,
		"orbstack": code.Mount,
		"proxmox":  code.Rsync,
		"k8s":      code.Git,
	}
	for backendName, want := range tests {
		if got := code.DefaultFor(backendName); got != want {
			t.Errorf("DefaultFor(%q) = %q, want %q", backendName, got, want)
		}
	}
}

func newOpts(t *testing.T, mode string) (code.Options, *run.Fake, *backendtest.Fake) {
	t.Helper()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := run.NewFake()
	b := backendtest.NewFake("docker")
	return code.Options{
		Mode:         mode,
		Project:      project,
		WorkDir:      "/work",
		Backend:      b,
		Handle:       backend.Handle{Name: "cc-demo"},
		Runner:       f,
		SSHTarget:    "cc-demo",
		IdentityFile: "/home/j/.ssh/ccvm_ed25519",
	}, f, b
}

// The backend attached the directory at create time, so there is nothing to do.
func TestMaterializeMountCopiesNothing(t *testing.T) {
	o, f, _ := newOpts(t, code.Mount)
	if err := code.Materialize(context.Background(), o); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("mount mode ran commands: %v", f.Lines())
	}
}

func TestMaterializeRsyncPushesTheWorkingTree(t *testing.T) {
	o, f, _ := newOpts(t, code.Rsync)
	f.On("rsync").Stdout("")

	if err := code.Materialize(context.Background(), o); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	call := f.Find("rsync")
	joined := strings.Join(call, " ")

	// A trailing slash on the source copies the contents rather than nesting
	// the directory inside itself.
	if !strings.Contains(joined, o.Project+"/ cc-demo:/work/") {
		t.Errorf("rsync = %q, want contents copied into /work", joined)
	}
	if !strings.Contains(joined, "--delete") {
		t.Error("no --delete: a file removed on one side would silently reappear")
	}
	if !strings.Contains(joined, "-i /home/j/.ssh/ccvm_ed25519") {
		t.Errorf("rsync = %q, want ccvm's key", joined)
	}
}

// Honouring .gitignore alone is not enough: a vendored dependency tree is
// tracked, and copying it turns a two-second spawn into a minute.
func TestRsyncExcludesHeavyDirectoriesEvenWhenTracked(t *testing.T) {
	o, f, _ := newOpts(t, code.Rsync)
	f.On("rsync").Stdout("")

	if err := code.Materialize(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.Find("rsync"), " ")
	for _, want := range []string{"node_modules", ".venv", "__pycache__"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rsync = %q, missing exclude %q", joined, want)
		}
	}
}

func TestRsyncAppliesGitignoreWhenPresent(t *testing.T) {
	o, f, _ := newOpts(t, code.Rsync)
	f.On("rsync").Stdout("")
	if err := os.WriteFile(filepath.Join(o.Project, ".gitignore"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := code.Materialize(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.Find("rsync"), " "), ":- .gitignore") {
		t.Error("a project's own ignores were not applied")
	}
}

func TestRsyncSkipsGitignoreFilterWhenAbsent(t *testing.T) {
	o, f, _ := newOpts(t, code.Rsync)
	f.On("rsync").Stdout("")

	if err := code.Materialize(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.Find("rsync"), " "), ":- .gitignore") {
		t.Error("applied a .gitignore filter with no .gitignore present")
	}
}

// Only rsync has anything to return: a mount is already live and a clone's work
// belongs in a commit.
func TestSyncBackOnlyAppliesToRsync(t *testing.T) {
	for _, mode := range []string{code.Mount, code.Git} {
		t.Run(mode, func(t *testing.T) {
			o, f, _ := newOpts(t, mode)
			if err := code.SyncBack(context.Background(), o); err != nil {
				t.Fatalf("SyncBack: %v", err)
			}
			if len(f.Calls()) != 0 {
				t.Errorf("%s mode synced back: %v", mode, f.Lines())
			}
		})
	}
}

func TestSyncBackReversesTheDirection(t *testing.T) {
	o, f, _ := newOpts(t, code.Rsync)
	f.On("rsync").Stdout("")

	if err := code.SyncBack(context.Background(), o); err != nil {
		t.Fatalf("SyncBack: %v", err)
	}
	joined := strings.Join(f.Find("rsync"), " ")
	if !strings.Contains(joined, "cc-demo:/work/ "+o.Project+"/") {
		t.Errorf("rsync = %q, want the machine as the source", joined)
	}
}

func TestMaterializeGitClonesTheCheckedOutBranch(t *testing.T) {
	o, f, b := newOpts(t, code.Git)
	f.On("git", "-C", o.Project, "remote").Stdout("git@github.com:j/foo.git\n")
	f.On("git", "-C", o.Project, "rev-parse").Stdout("feature-branch\n")

	if err := code.Materialize(context.Background(), o); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	var clone []string
	for _, c := range b.ExecCalls() {
		if len(c) > 1 && c[0] == "git" && c[1] == "clone" {
			clone = c
		}
	}
	if clone == nil {
		t.Fatalf("no clone ran: %v", b.ExecCalls())
	}
	joined := strings.Join(clone, " ")
	if !strings.Contains(joined, "--branch feature-branch") {
		t.Errorf("clone = %q, want the branch checked out on the host", joined)
	}
	if !strings.Contains(joined, "git@github.com:j/foo.git /work") {
		t.Errorf("clone = %q", joined)
	}
}

// A detached HEAD has no branch to ask for, and passing "HEAD" as one fails.
func TestMaterializeGitToleratesDetachedHead(t *testing.T) {
	o, f, b := newOpts(t, code.Git)
	f.On("git", "-C", o.Project, "remote").Stdout("git@github.com:j/foo.git\n")
	f.On("git", "-C", o.Project, "rev-parse").Stdout("HEAD\n")

	if err := code.Materialize(context.Background(), o); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	for _, c := range b.ExecCalls() {
		if strings.Contains(strings.Join(c, " "), "--branch HEAD") {
			t.Errorf("passed HEAD as a branch name: %v", c)
		}
	}
}

// Without a remote there is nothing to clone, and the message has to offer the
// mode that does carry a working tree.
func TestMaterializeGitWithoutARemoteOffersRsync(t *testing.T) {
	o, f, _ := newOpts(t, code.Git)
	f.On("git", "-C", o.Project, "remote").Fail(128, "error: No such remote 'origin'")

	err := code.Materialize(context.Background(), o)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--code rsync") {
		t.Errorf("err = %v, want it to offer the alternative", err)
	}
}

func TestMaterializeUnknownMode(t *testing.T) {
	o, _, _ := newOpts(t, "nonsense")
	if err := code.Materialize(context.Background(), o); err == nil {
		t.Fatal("expected an error")
	}
}

// A missing rsync in the guest is an image problem, not a transfer problem, and
// it surfaces as a bare "command not found".
func TestRsyncMissingInGuestNamesTheFix(t *testing.T) {
	o, f, _ := newOpts(t, code.Rsync)
	f.On("rsync").Fail(127, "bash: line 1: rsync: command not found")

	err := code.Materialize(context.Background(), o)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "profiles build") {
		t.Errorf("err = %v, want it to name the fix", err)
	}
}

// The mount runs inside the guest and reaches back to this machine, so the
// command has to name this machine's user and the host-side project path, and
// point at the tunnel's port rather than a real sshd port.
func TestSSHFSMountCommand(t *testing.T) {
	b := backendtest.NewFake("proxmox")
	b.Seed(backend.Machine{Name: "cc-demo"}, nil)
	h := backend.Handle{Backend: "proxmox", Name: "cc-demo"}

	err := code.Materialize(context.Background(), code.Options{
		Mode:    code.Sshfs,
		Project: "/Users/me/src/demo",
		WorkDir: "/work",
		Backend: b,
		Handle:  h,
	})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	var call string
	for _, argv := range b.ExecCalls() {
		if len(argv) > 0 && argv[0] == "sshfs" {
			call = strings.Join(argv, " ")
		}
	}
	if call == "" {
		t.Fatalf("no sshfs command ran in the guest; calls were %v", b.ExecCalls())
	}
	for _, want := range []string{"/Users/me/src/demo", "/work", "127.0.0.1", "2222", "reconnect"} {
		if !strings.Contains(call, want) {
			t.Errorf("sshfs call = %q, missing %q", call, want)
		}
	}
}

// A guest without sshfs must say so, and say what to do, rather than failing
// inside a mount command nobody can read.
func TestSSHFSRefusesAGuestWithoutIt(t *testing.T) {
	b := backendtest.NewFake("proxmox")
	b.Seed(backend.Machine{Name: "cc-demo"}, nil)
	b.ExecErrOn = "command -v sshfs"

	err := code.Materialize(context.Background(), code.Options{
		Mode: code.Sshfs, Project: "/p", WorkDir: "/work",
		Backend: b, Handle: backend.Handle{Name: "cc-demo"},
	})
	if err == nil {
		t.Fatal("expected a refusal when the guest has no sshfs")
	}
	for _, want := range []string{"sshfs", "rsync"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

// The tunnel binds a port in the guest that reaches this machine's sshd, and
// must fail rather than run without the forward - otherwise the mount would be
// attempted through a tunnel that is not there.
func TestSSHFSTunnelArgs(t *testing.T) {
	got := strings.Join(code.SSHFSTunnelArgs("root@10.0.0.5", "/keys/id"), " ")
	for _, want := range []string{"-N", "-R 2222:localhost:22", "ExitOnForwardFailure=yes", "-i /keys/id", "root@10.0.0.5"} {
		if !strings.Contains(got, want) {
			t.Errorf("tunnel args = %q, missing %q", got, want)
		}
	}
}

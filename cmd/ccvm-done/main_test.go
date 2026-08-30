package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/josegonzalez/ccvm/internal/session"
)

// A clean git checkout has nothing to lose, so git mode should not refuse.
func TestWouldLoseWorkCleanGitCheckout(t *testing.T) {
	dir := newGitRepo(t, false)
	s := session.Session{CodeMode: "git", WorkDir: dir}

	if reason, lost := wouldLoseWork(s); lost {
		t.Errorf("clean checkout reported as losing work: %s", reason)
	}
}

// A dirty git checkout must refuse, and must name the files.
func TestWouldLoseWorkDirtyGitCheckoutNamesFiles(t *testing.T) {
	dir := newGitRepo(t, true)
	s := session.Session{CodeMode: "git", WorkDir: dir}

	reason, lost := wouldLoseWork(s)
	if !lost {
		t.Fatal("dirty checkout in git mode must report losing work")
	}
	if !strings.Contains(reason, "scratch.txt") {
		t.Errorf("reason = %q, want it to name the dirty file", reason)
	}
}

// rsync is synced back by the wrapper and mount is already on the host, so a
// dirty tree is not a reason to refuse under either.
func TestWouldLoseWorkSafeCodeModes(t *testing.T) {
	dir := newGitRepo(t, true)
	for _, mode := range []string{"rsync", "mount", "sshfs"} {
		t.Run(mode, func(t *testing.T) {
			s := session.Session{CodeMode: mode, WorkDir: dir}
			if reason, lost := wouldLoseWork(s); lost {
				t.Errorf("%s reported as losing work: %s", mode, reason)
			}
		})
	}
}

// Under a mode that does not sync back, being unable to check is itself unsafe.
func TestWouldLoseWorkUncheckableGitDirIsUnsafe(t *testing.T) {
	s := session.Session{CodeMode: "git", WorkDir: t.TempDir()} // not a repo
	reason, lost := wouldLoseWork(s)
	if !lost {
		t.Fatal("an unverifiable work dir in git mode must be treated as unsafe")
	}
	if !strings.Contains(reason, "cannot verify") {
		t.Errorf("reason = %q, want it to say the check could not be made", reason)
	}
}

func TestTouchSentinelCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "ccvm", "destroy")
	if err := touchSentinel(path); err != nil {
		t.Fatalf("touchSentinel: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("sentinel not created: %v", err)
	}
}

func TestTouchSentinelIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "destroy")
	for i := range 2 {
		if err := touchSentinel(path); err != nil {
			t.Fatalf("touchSentinel #%d: %v", i+1, err)
		}
	}
}

func TestSessionRoundTripThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.toml")
	want := session.Session{Name: "cc-foo", CodeMode: "mount", TTL: "12h", WorkDir: "/work"}
	if err := writeSession(path, want); err != nil {
		t.Fatalf("writeSession: %v", err)
	}
	got, err := readSession(path)
	if err != nil {
		t.Fatalf("readSession: %v", err)
	}
	if got.Name != want.Name || got.TTL != want.TTL {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// Running outside a ccvm machine should say so, rather than failing obscurely.
func TestReadSessionMissingFileExplainsItself(t *testing.T) {
	_, err := readSession(filepath.Join(t.TempDir(), "nope.toml"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ccvm machine") {
		t.Errorf("err = %v, want it to explain the likely cause", err)
	}
}

func newGitRepo(t *testing.T, dirty bool) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-qm", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if dirty {
		if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("wip"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newDoneCmd(t *testing.T, s session.Session) (*doneCmd, *strings.Builder, *bool) {
	t.Helper()
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.toml")
	if err := writeSession(sessionPath, s); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	detached := false
	return &doneCmd{
		sessionPath:  sessionPath,
		sentinelPath: filepath.Join(dir, "run", "ccvm", "destroy"),
		detach:       func() { detached = true },
		out:          &out,
	}, &out, &detached
}

// The ordinary path: write the sentinel, say so, then detach.
func TestRunWritesSentinelAndDetaches(t *testing.T) {
	c, out, detached := newDoneCmd(t, session.Session{Name: "cc-foo", CodeMode: "mount"})

	if err := c.run(false, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(c.sentinelPath); err != nil {
		t.Errorf("sentinel not written: %v", err)
	}
	if !*detached {
		t.Error("terminal was not detached")
	}
	if !strings.Contains(out.String(), "cc-foo will be destroyed") {
		t.Errorf("output = %q", out.String())
	}
}

// --keep records the exemption in the session file, which is what the wrapper
// re-reads before deciding whether to tear down.
func TestRunKeepMarksSessionAndSkipsSentinel(t *testing.T) {
	c, out, detached := newDoneCmd(t, session.Session{Name: "cc-foo", CodeMode: "mount", TTL: "12h"})

	if err := c.run(true, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := readSession(c.sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Kept() {
		t.Errorf("TTL = %q, want the machine marked kept", got.TTL)
	}
	if _, err := os.Stat(c.sentinelPath); err == nil {
		t.Error("--keep must not write the destroy sentinel")
	}
	if !*detached {
		t.Error("--keep should still detach the terminal")
	}
	if !strings.Contains(out.String(), "keep running") {
		t.Errorf("output = %q", out.String())
	}
}

// The data-loss guard must refuse before writing the sentinel, or the refusal
// is cosmetic and the reaper collects the machine anyway.
func TestRunRefusesDirtyGitCheckoutWithoutForce(t *testing.T) {
	dir := newGitRepo(t, true)
	c, _, detached := newDoneCmd(t, session.Session{Name: "cc-foo", CodeMode: "git", WorkDir: dir})

	err := c.run(false, false)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "scratch.txt") {
		t.Errorf("err = %v, want it to name the dirty file", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("err = %v, want it to name the override", err)
	}
	if _, statErr := os.Stat(c.sentinelPath); statErr == nil {
		t.Error("sentinel was written despite refusing")
	}
	if *detached {
		t.Error("terminal was detached despite refusing")
	}
}

func TestRunForceOverridesDirtyCheckout(t *testing.T) {
	dir := newGitRepo(t, true)
	c, _, detached := newDoneCmd(t, session.Session{Name: "cc-foo", CodeMode: "git", WorkDir: dir})

	if err := c.run(false, true); err != nil {
		t.Fatalf("run with --force: %v", err)
	}
	if _, err := os.Stat(c.sentinelPath); err != nil {
		t.Errorf("sentinel not written under --force: %v", err)
	}
	if !*detached {
		t.Error("terminal was not detached")
	}
}

// --keep is not destruction, so the data-loss guard does not apply to it.
func TestRunKeepIgnoresDirtyCheckout(t *testing.T) {
	dir := newGitRepo(t, true)
	c, _, _ := newDoneCmd(t, session.Session{Name: "cc-foo", CodeMode: "git", WorkDir: dir})

	if err := c.run(true, false); err != nil {
		t.Fatalf("run --keep on a dirty tree should not refuse: %v", err)
	}
}

// A sentinel that cannot be written means the session would appear to end and
// then quietly not, so the failure has to surface.
func TestTouchSentinelReportsAnUnwritablePath(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "run")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := touchSentinel(filepath.Join(blocker, "ccvm", "destroy")); err == nil {
		t.Fatal("expected an error when the sentinel directory cannot be created")
	}
}

func TestWriteSessionReportsAnUnwritablePath(t *testing.T) {
	if err := writeSession(filepath.Join(t.TempDir(), "nope", "session.toml"),
		session.Session{Name: "cc-foo"}); err == nil {
		t.Fatal("expected an error writing into a missing directory")
	}
}

func TestReadSessionRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.toml")
	if err := os.WriteFile(path, []byte("not = = toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSession(path); err == nil {
		t.Fatal("expected a parse error")
	}
}

// The whole point of the detached child is that this returns immediately, so
// the calling tool call can report what happened before the terminal dies.
func TestDetachTerminalReturnsImmediately(t *testing.T) {
	done := make(chan struct{})
	go func() {
		detachTerminal()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("detachTerminal blocked; the calling tool call would die mid-flight")
	}
}

// Running outside a ccvm machine is the most likely mistake, and it should
// exit non-zero with an explanation rather than appearing to work.
func TestRealMainOutsideAMachine(t *testing.T) {
	var out, errOut strings.Builder
	if code := realMain(nil, &out, &errOut); code == 0 {
		t.Error("exit code = 0 outside a ccvm machine")
	}
	if !strings.Contains(errOut.String(), "ccvm-done:") {
		t.Errorf("stderr = %q, want the command named", errOut.String())
	}
}

func TestRealMainRejectsUnknownFlags(t *testing.T) {
	var out, errOut strings.Builder
	if code := realMain([]string{"--frobnicate"}, &out, &errOut); code != 2 {
		t.Errorf("exit code = %d, want 2 for a usage error", code)
	}
}

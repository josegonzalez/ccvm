package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchSentinelFiresWhenFileAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "destroy")
	done := make(chan struct{})
	go watchSentinel(path, 10*time.Millisecond, done)

	select {
	case <-done:
		t.Fatal("fired before the sentinel existed")
	case <-time.After(50 * time.Millisecond):
	}

	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not fire after the sentinel appeared")
	}
}

// The machine must outlive its session. If PID 1 exited when the supervised
// service or the session ended, closing an ssh connection would destroy the
// machine, `--keep` would be impossible, and `ccvm attach` would have nothing
// to return to.
func TestInitOutlivesASessionThatEnds(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "destroy")

	exit := make(chan int, 1)
	go func() {
		// A service that finishes quickly stands in for a session ending.
		exit <- runInit(sentinel, 10*time.Millisecond, []string{"sh", "-c", "sleep 0.05"})
	}()

	select {
	case code := <-exit:
		// It exits non-zero: the service dying on its own is an image failure,
		// not a clean teardown, and must not be reported as one.
		if code == 0 {
			t.Fatal("a service exiting on its own must not look like a clean session end")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runInit hung")
	}
}

// The sentinel is the only clean way out, and it must exit 0.
func TestInitExitsCleanlyOnSentinel(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "destroy")

	exit := make(chan int, 1)
	go func() {
		exit <- runInit(sentinel, 10*time.Millisecond, []string{"sleep", "30"})
	}()

	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case code := <-exit:
		if code != 0 {
			t.Errorf("exit code = %d, want 0 for a sentinel shutdown", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runInit did not shut down after the sentinel appeared")
	}
}

func TestInitReportsUnstartableService(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "destroy")
	code := runInit(sentinel, 10*time.Millisecond, []string{"ccvm-no-such-binary"})
	if code == 0 {
		t.Error("exit code = 0 for a service that could not start")
	}
}

func TestRealMainRejectsUnknownFlags(t *testing.T) {
	if code := realMain([]string{"--frobnicate"}); code != 2 {
		t.Errorf("exit code = %d, want 2 for a usage error", code)
	}
}

// The supervised service is whatever follows the flags, so a caller can run
// something other than sshd.
func TestRealMainSupervisesTheGivenService(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "destroy")
	done := make(chan int, 1)
	go func() {
		done <- realMain([]string{"-sentinel", sentinel, "-poll", "10ms", "sleep", "30"})
	}()

	time.Sleep(150 * time.Millisecond)
	if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0 for a sentinel shutdown", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("realMain did not shut down on the sentinel")
	}
}

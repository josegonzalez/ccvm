// Command ccvm-done ends a ccvm session from inside the guest.
//
// It never destroys the machine itself. A guest holding infrastructure
// credentials — a Proxmox token, a mounted docker socket, a ServiceAccount that
// can delete pods — could destroy machines that are not its own, so this
// signals outward instead and something already holding credentials acts.
//
// Claude runs this from inside a session, so the ordering matters: the process
// must return before anything kills the terminal it was invoked from, or the
// tool call dies mid-flight and Claude cannot report what happened.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/josegonzalez/ccvm/internal/session"
)

const (
	sessionFile  = "/etc/ccvm/session.toml"
	doneSentinel = "/run/ccvm/destroy"
	tmuxSession  = "cc"

	// detachDelay is how long to wait before killing the terminal, so the
	// calling tool call returns first.
	detachDelay = 2 * time.Second
)

func main() {
	var (
		keep  = flag.Bool("keep", false, "leave the machine running and just detach")
		force = flag.Bool("force", false, "destroy even if uncommitted work would be lost")
	)
	flag.Parse()

	cmd := &doneCmd{
		sessionPath:  sessionFile,
		sentinelPath: doneSentinel,
		detach:       detachTerminal,
		out:          os.Stdout,
	}
	if err := cmd.run(*keep, *force); err != nil {
		fmt.Fprintln(os.Stderr, "ccvm-done:", err)
		os.Exit(1)
	}
}

// doneCmd holds what run touches, so the paths and the terminal kill can be
// swapped out under test. Without this, testing --keep would mean writing to
// /etc and killing the developer's tmux.
type doneCmd struct {
	sessionPath  string
	sentinelPath string
	detach       func()
	out          io.Writer
}

func (c *doneCmd) run(keep, force bool) error {
	s, err := readSession(c.sessionPath)
	if err != nil {
		return err
	}

	// --keep records the exemption in the session file rather than telling the
	// wrapper directly: the wrapper re-reads this file before tearing down, so
	// the in-machine record stays the single authority on TTL.
	if keep {
		s.TTL = session.Keep
		if err := writeSession(c.sessionPath, s); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "%s will keep running; reattach with `ccvm attach %s`\n", s.Name, s.Name)
		c.detach()
		return nil
	}

	if reason, lost := wouldLoseWork(s); lost && !force {
		return fmt.Errorf("%s\n\nre-run with --force to destroy anyway, or commit and push first", reason)
	}

	if err := touchSentinel(c.sentinelPath); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "session ending; %s will be destroyed\n", s.Name)
	c.detach()
	return nil
}

// wouldLoseWork reports whether destroying now discards uncommitted changes,
// naming the files so the warning is actionable rather than just alarming.
func wouldLoseWork(s session.Session) (string, bool) {
	if !s.LosesWorkOnDestroy() {
		return "", false
	}
	dirty, err := dirtyFiles(s.WorkDir)
	if err != nil {
		// Not a git checkout, or git is unavailable. Under a code mode that
		// does not sync back, that is still unsafe to assume away.
		return fmt.Sprintf("cannot verify whether %s has uncommitted work (%v), and code mode %q does not sync back to the host",
			s.WorkDir, err, s.CodeMode), true
	}
	if len(dirty) == 0 {
		return "", false
	}
	return fmt.Sprintf("code mode %q keeps changes only inside this machine, and %s has uncommitted work:\n  %s",
		s.CodeMode, s.WorkDir, strings.Join(dirty, "\n  ")), true
}

func dirtyFiles(dir string) ([]string, error) {
	if dir == "" {
		return nil, fmt.Errorf("no work directory recorded")
	}
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func touchSentinel(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create sentinel directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("write sentinel: %w", err)
	}
	return f.Close()
}

// detachTerminal schedules the tmux session's death in a detached child, so
// this process can exit first and its caller can read the result.
func detachTerminal() {
	script := fmt.Sprintf("sleep %d; tmux kill-session -t %s",
		int(detachDelay.Seconds()), tmuxSession)
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = nil, nil
	// A failure here is not worth failing the command over: the sentinel is
	// already written, so the reaper will collect the machine regardless.
	_ = cmd.Start()
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
}

func readSession(path string) (session.Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return session.Session{}, fmt.Errorf("read %s: %w (is this a ccvm machine?)", path, err)
	}
	return session.Unmarshal(data)
}

func writeSession(path string, s session.Session) error {
	data, err := session.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

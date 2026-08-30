// Command ccvm-init is PID 1 in the docker and kubernetes session images.
//
// It exists for two reasons. PID 1 has real duties a shell gets wrong by
// default: reaping orphaned children, and forwarding signals to them. And it
// decides when the machine's life ends.
//
// That decision is the subtle one. ccvm-init waits on the done sentinel and
// nothing else. It must NOT exit when the Claude session or its tmux server
// exits, or closing an ssh connection would destroy the machine — `--keep`
// would be impossible and `ccvm attach` would have nothing to return to.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

const (
	doneSentinel = "/run/ccvm/destroy"
	pollInterval = time.Second

	// stopGrace is how long children get to exit after SIGTERM before the
	// process gives up and lets the container die.
	stopGrace = 10 * time.Second
)

func main() {
	var (
		sentinel = flag.String("sentinel", doneSentinel, "path whose appearance ends the machine")
		poll     = flag.Duration("poll", pollInterval, "how often to check for the sentinel")
	)
	flag.Parse()

	// argv after the flags is the service to supervise, defaulting to sshd in
	// the foreground.
	service := flag.Args()
	if len(service) == 0 {
		service = []string{"/usr/sbin/sshd", "-D", "-e"}
	}

	os.Exit(runInit(*sentinel, *poll, service))
}

func runInit(sentinel string, poll time.Duration, service []string) int {
	// Reap orphans for as long as this process is PID 1. Without this, every
	// process whose parent exits inside the container becomes a permanent
	// zombie.
	go reapOrphans()

	cmd := exec.Command(service[0], service[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "ccvm-init: start %v: %v\n", service, err)
		return 1
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	serviceExit := make(chan error, 1)
	go func() { serviceExit <- cmd.Wait() }()

	done := make(chan struct{})
	go watchSentinel(sentinel, poll, done)

	select {
	case <-done:
		fmt.Fprintln(os.Stderr, "ccvm-init: session ended; shutting down")
		stop(cmd)
		return 0

	case sig := <-signals:
		fmt.Fprintf(os.Stderr, "ccvm-init: received %v; shutting down\n", sig)
		stop(cmd)
		return 0

	case err := <-serviceExit:
		// The supervised service died on its own. That is a failure of the
		// image, not a session ending, so report it rather than exiting 0 and
		// letting it look like a clean teardown.
		fmt.Fprintf(os.Stderr, "ccvm-init: %v exited unexpectedly: %v\n", service[0], err)
		return 1
	}
}

// watchSentinel polls rather than using inotify: the file appears once, the
// latency budget is a second, and polling behaves the same on every filesystem
// a guest might use.
func watchSentinel(path string, every time.Duration, done chan<- struct{}) {
	if every <= 0 {
		every = pollInterval
	}
	for {
		if _, err := os.Stat(path); err == nil {
			close(done)
			return
		}
		time.Sleep(every)
	}
}

func stop(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)

	exited := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(exited)
	}()

	select {
	case <-exited:
	case <-time.After(stopGrace):
		_ = cmd.Process.Kill()
	}
}

// reapOrphans collects children re-parented to PID 1. A container init that
// skips this accumulates zombies until the process table fills.
func reapOrphans() {
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)
	for range sigchld {
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
		}
	}
}

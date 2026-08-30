// Package kubefwd keeps a kubectl port-forward alive.
//
// A kubernetes session is only reachable over ssh through a forwarded local
// port, and `kubectl port-forward` is a foreground process that dies on a
// network blip, an API server restart, or the pod being rescheduled. Left
// unsupervised it makes k8s sessions feel broken in a way that looks like ccvm
// losing the machine.
//
// This is the weakest link in the design and is documented as such: a forward
// only exists while something holds it, so a detached k8s session is not
// reachable until a ccvm command re-establishes one.
package kubefwd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"
)

// Launcher starts the forwarding process. A variable so tests can simulate a
// forward that dies, which is the behaviour this package exists to handle and
// cannot be provoked from a real kubectl on demand.
type Launcher func(ctx context.Context, argv []string) (*exec.Cmd, error)

func defaultLauncher(ctx context.Context, argv []string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// Forwarder supervises one port-forward.
type Forwarder struct {
	Bin        string
	Namespace  string
	Context    string
	Pod        string
	LocalPort  int
	RemotePort int
	// Log receives supervision events, so a forward that keeps dying is
	// visible rather than merely slow.
	Log io.Writer
	// Launch is how the process is started.
	Launch Launcher
	// ReadyTimeout bounds how long Start waits for the port to accept.
	ReadyTimeout time.Duration

	mu      sync.Mutex
	cmd     *exec.Cmd
	stopped bool
	// drops counts how many times the forward died; restarts counts how many
	// of those were successfully re-established. They differ when a pod is
	// gone for good, which is the case worth surfacing.
	drops    int
	restarts int
	cancel   context.CancelFunc
}

// New returns a Forwarder with sensible defaults.
func New(namespace, kubeContext, pod string, localPort int) *Forwarder {
	return &Forwarder{
		Bin:          "kubectl",
		Namespace:    namespace,
		Context:      kubeContext,
		Pod:          pod,
		LocalPort:    localPort,
		RemotePort:   22,
		Launch:       defaultLauncher,
		ReadyTimeout: 30 * time.Second,
	}
}

// Args renders the kubectl invocation.
func (f *Forwarder) Args() []string {
	argv := []string{f.Bin, "--namespace", f.Namespace}
	if f.Context != "" {
		argv = append(argv, "--context", f.Context)
	}
	return append(argv, "port-forward", f.Pod,
		fmt.Sprintf("%d:%d", f.LocalPort, f.RemotePort))
}

// Start establishes the forward and keeps it alive until Stop.
//
// It returns once the port accepts a connection, rather than once kubectl has
// been launched: a forward that is starting is not one you can ssh through, and
// returning early makes the first connection fail for no visible reason.
func (f *Forwarder) Start(ctx context.Context) error {
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return fmt.Errorf("forwarder already stopped")
	}
	ctx, cancel := context.WithCancel(ctx)
	f.cancel = cancel
	f.mu.Unlock()

	if err := f.spawn(ctx); err != nil {
		cancel()
		return err
	}
	if err := f.waitReady(ctx); err != nil {
		cancel()
		return err
	}
	go f.supervise(ctx)
	return nil
}

func (f *Forwarder) spawn(ctx context.Context) error {
	launch := f.Launch
	if launch == nil {
		launch = defaultLauncher
	}
	cmd, err := launch(ctx, f.Args())
	if err != nil {
		return fmt.Errorf("start port-forward for %s: %w", f.Pod, err)
	}
	f.mu.Lock()
	f.cmd = cmd
	f.mu.Unlock()
	return nil
}

// supervise restarts the forward whenever it exits on its own.
func (f *Forwarder) supervise(ctx context.Context) {
	for {
		f.mu.Lock()
		cmd := f.cmd
		f.mu.Unlock()
		if cmd == nil {
			return
		}
		_ = cmd.Wait()

		f.mu.Lock()
		stopped := f.stopped
		f.drops++
		n := f.drops
		f.mu.Unlock()

		if stopped || ctx.Err() != nil {
			return
		}
		f.logf("port-forward for %s exited; restarting (%d)", f.Pod, n)

		// A short pause so a pod that is gone for good does not become a spin.
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff(n)):
		}
		if err := f.spawn(ctx); err != nil {
			f.logf("could not restart port-forward for %s: %v", f.Pod, err)
			return
		}
		f.mu.Lock()
		f.restarts++
		f.mu.Unlock()
	}
}

// backoff grows to a ceiling. A pod that will never come back should not be
// retried every few milliseconds, and one that is merely restarting should be
// picked up quickly.
func backoff(attempt int) time.Duration {
	d := time.Duration(attempt) * 500 * time.Millisecond
	if d > 5*time.Second {
		return 5 * time.Second
	}
	return d
}

// waitReady blocks until the local port accepts a connection.
func (f *Forwarder) waitReady(ctx context.Context) error {
	timeout := f.ReadyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", f.LocalPort)

	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("port-forward for %s did not start listening on %s within %s",
		f.Pod, addr, timeout)
}

// Healthy reports whether the local port currently accepts a connection.
//
// Checked rather than remembered: the supervising goroutine learns a forward
// died only when the process exits, and a half-open forward accepts nothing
// while still looking alive.
func (f *Forwarder) Healthy() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", f.LocalPort), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Restarts reports how many times the forward was successfully re-established.
func (f *Forwarder) Restarts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restarts
}

// Drops reports how many times the forward died.
//
// It exceeds Restarts when a pod is gone for good, which is the difference
// between a connection that is flaky and one that is over.
func (f *Forwarder) Drops() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drops
}

// Stop ends the forward and its supervision.
func (f *Forwarder) Stop() error {
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return nil
	}
	f.stopped = true
	cmd := f.cmd
	cancel := f.cancel
	f.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return nil
}

func (f *Forwarder) logf(format string, args ...any) {
	if f.Log == nil {
		return
	}
	fmt.Fprintf(f.Log, "ccvm: "+format+"\n", args...)
}

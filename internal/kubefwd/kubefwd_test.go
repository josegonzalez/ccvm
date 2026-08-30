package kubefwd_test

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/josegonzalez/ccvm/internal/kubefwd"
)

// listener stands in for the forwarded port: something that actually accepts
// connections, since readiness is checked by dialing rather than by trusting
// that kubectl started.
func listener(t *testing.T) (int, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, func() { l.Close() }
}

func TestArgsScopeToNamespaceAndContext(t *testing.T) {
	f := kubefwd.New("ccvm", "kind-test", "cc-demo-abc", 2231)
	got := strings.Join(f.Args(), " ")

	for _, want := range []string{"--namespace ccvm", "--context kind-test",
		"port-forward cc-demo-abc", "2231:22"} {
		if !strings.Contains(got, want) {
			t.Errorf("Args = %q, missing %q", got, want)
		}
	}
}

func TestArgsOmitContextWhenUnset(t *testing.T) {
	f := kubefwd.New("ccvm", "", "cc-demo-abc", 2231)
	if strings.Contains(strings.Join(f.Args(), " "), "--context") {
		t.Errorf("Args = %v, want no --context when none is set", f.Args())
	}
}

// A forward that is starting is not one you can ssh through. Returning before
// the port accepts makes the first connection fail for no visible reason.
func TestStartWaitsForThePortToAccept(t *testing.T) {
	port, closeL := listener(t)
	defer closeL()

	f := kubefwd.New("ccvm", "", "cc-demo", port)
	f.Launch = fakeLaunch(t, nil)

	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer f.Stop()

	if !f.Healthy() {
		t.Error("Start returned before the forward was usable")
	}
}

// A port that never opens has to be reported, not waited on forever.
func TestStartGivesUpWhenThePortNeverOpens(t *testing.T) {
	// A port nothing is listening on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	f := kubefwd.New("ccvm", "", "cc-demo", port)
	f.Launch = fakeLaunch(t, nil)
	f.ReadyTimeout = 500 * time.Millisecond

	err = f.Start(context.Background())
	if err == nil {
		t.Fatal("Start succeeded with nothing listening")
	}
	if !strings.Contains(err.Error(), "did not start listening") {
		t.Errorf("err = %v", err)
	}
}

// The behaviour this package exists for: kubectl dies, and the forward comes
// back without the user doing anything.
func TestForwardIsRestartedWhenItDies(t *testing.T) {
	port, closeL := listener(t)
	defer closeL()

	var mu sync.Mutex
	launches := 0
	f := kubefwd.New("ccvm", "", "cc-demo", port)
	f.Launch = func(ctx context.Context, argv []string) (*exec.Cmd, error) {
		mu.Lock()
		launches++
		n := launches
		mu.Unlock()
		// The first process exits on its own, as a dropped forward would.
		if n == 1 {
			return startCmd(t, ctx, "true")
		}
		return startCmd(t, ctx, "sleep", "30")
	}

	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer f.Stop()

	// Wait on the observable, not the counter: the drop is recorded before the
	// backoff, so a wait on Drops would race the respawn it is meant to prove.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := launches
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if launches < 2 {
		t.Errorf("launches = %d, want the forward re-established", launches)
	}
	if f.Restarts() == 0 {
		t.Error("a successful re-establishment was not counted")
	}
}

// A pod that will never come back should not become a spin.
func TestRestartsBackOff(t *testing.T) {
	port, closeL := listener(t)
	defer closeL()

	var mu sync.Mutex
	var times []time.Time
	f := kubefwd.New("ccvm", "", "cc-demo", port)
	f.Launch = func(ctx context.Context, argv []string) (*exec.Cmd, error) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		return startCmd(t, ctx, "true")
	}

	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(2500 * time.Millisecond)
	f.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(times) < 2 {
		t.Fatalf("only %d launches; expected several restarts", len(times))
	}
	// Without backoff a process that exits immediately would relaunch hundreds
	// of times in this window.
	if len(times) > 12 {
		t.Errorf("%d launches in 2.5s; restarts are not backing off", len(times))
	}
}

func TestStopEndsSupervision(t *testing.T) {
	port, closeL := listener(t)
	defer closeL()

	var mu sync.Mutex
	launches := 0
	f := kubefwd.New("ccvm", "", "cc-demo", port)
	f.Launch = func(ctx context.Context, argv []string) (*exec.Cmd, error) {
		mu.Lock()
		launches++
		mu.Unlock()
		return startCmd(t, ctx, "sleep", "30")
	}

	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := f.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mu.Lock()
	before := launches
	mu.Unlock()

	time.Sleep(1500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if launches != before {
		t.Errorf("launches went from %d to %d after Stop; supervision kept running",
			before, launches)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	port, closeL := listener(t)
	defer closeL()

	f := kubefwd.New("ccvm", "", "cc-demo", port)
	f.Launch = fakeLaunch(t, nil)
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

// Health is dialed rather than remembered: the supervisor learns a forward died
// only when the process exits, and a half-open forward accepts nothing while
// still looking alive.
func TestHealthyReflectsTheActualPort(t *testing.T) {
	port, closeL := listener(t)

	f := kubefwd.New("ccvm", "", "cc-demo", port)
	f.Launch = fakeLaunch(t, nil)
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer f.Stop()

	if !f.Healthy() {
		t.Fatal("Healthy = false with the port open")
	}
	closeL()
	if f.Healthy() {
		t.Error("Healthy = true after the port stopped accepting")
	}
}

func TestStartAfterStopFails(t *testing.T) {
	port, closeL := listener(t)
	defer closeL()

	f := kubefwd.New("ccvm", "", "cc-demo", port)
	f.Launch = fakeLaunch(t, nil)
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Stop()
	if err := f.Start(context.Background()); err == nil {
		t.Error("Start succeeded on a stopped forwarder")
	}
}

func TestStartReportsALaunchFailure(t *testing.T) {
	f := kubefwd.New("ccvm", "", "cc-demo", 1)
	f.Launch = func(context.Context, []string) (*exec.Cmd, error) {
		return nil, fmt.Errorf("kubectl not found")
	}
	err := f.Start(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "cc-demo") {
		t.Errorf("err = %v, want it to name the pod", err)
	}
}

func fakeLaunch(t *testing.T, _ error) kubefwd.Launcher {
	return func(ctx context.Context, argv []string) (*exec.Cmd, error) {
		return startCmd(t, ctx, "sleep", "30")
	}
}

func startCmd(t *testing.T, ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

//go:build integration

// Integration tests exercise real backends. They are opt-in: name the backends
// in CCVM_ITEST_BACKENDS, and a named backend that turns out to be unavailable
// fails rather than skips.
//
// The suite is deliberately backend-agnostic. Everything here is a claim about
// the Backend contract, so a new backend inherits the whole suite by
// registering itself.
package backend_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/itest"
	"github.com/josegonzalez/ccvm/internal/run"
	"github.com/josegonzalez/ccvm/internal/session"
)

// imageFor resolves what a backend builds a machine from. The word "image"
// means different things per backend: a registry reference for docker and k8s,
// a template machine for orbstack, a template vmid for proxmox.
func imageFor(name string) string {
	switch name {
	case "orbstack":
		return envOr("CCVM_ITEST_ORBSTACK_TEMPLATE", "ccvm-base")
	case "proxmox":
		return envOr("CCVM_ITEST_PROXMOX_TEMPLATE", "9000")
	default:
		return itest.Image()
	}
}

// probeFor is how the suite decides a requested backend is actually usable.
// It must not create anything.
func probeFor(name string, b backend.Backend) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// In the control-plane tier there is no template to check against, so
		// the probe asks only whether the API answers and authenticates.
		// Demanding a template here would fail every run for the one condition
		// this tier cannot satisfy.
		if controlPlaneOnly(name) {
			p, ok := b.(*backend.Proxmox)
			if !ok {
				return fmt.Errorf("control-plane mode is only defined for proxmox")
			}
			_, err := p.PickNodeForTest(ctx)
			return err
		}
		return b.Preflight(ctx, backend.Spec{Profile: "base", Image: imageFor(name)})
	}
}

func configFromEnv() backend.Config {
	return backend.Config{
		ProxmoxURL:      os.Getenv("CCVM_ITEST_PROXMOX_URL"),
		ProxmoxNode:     os.Getenv("CCVM_ITEST_PROXMOX_NODE"),
		ProxmoxTokenID:  os.Getenv("CCVM_ITEST_PROXMOX_TOKEN_ID"),
		ProxmoxSecret:   os.Getenv("CCVM_ITEST_PROXMOX_SECRET"),
		ProxmoxStorage:  os.Getenv("CCVM_ITEST_PROXMOX_STORAGE"),
		ProxmoxInsecure: os.Getenv("CCVM_ITEST_PROXMOX_INSECURE") != "",
		ProxmoxBridge:   os.Getenv("CCVM_PROXMOX_BRIDGE"),
		ProxmoxSubnet:   os.Getenv("CCVM_PROXMOX_SUBNET"),
		ProxmoxGateway:  os.Getenv("CCVM_PROXMOX_GATEWAY"),
		ProxmoxSSHKey:   os.Getenv("CCVM_PROXMOX_SSH_KEY"),
		KubeNamespace:   envOr("CCVM_ITEST_KUBE_NAMESPACE", "default"),
		KubeContext:     os.Getenv("CCVM_ITEST_KUBE_CONTEXT"),
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// controlPlaneOnly reports whether a backend can create machines here.
// Containerized Proxmox in CI has a real API but no hypervisor: guest boot is
// out of reach, and pretending otherwise would fail for the wrong reason.
func controlPlaneOnly(name string) bool {
	return name == "proxmox" && os.Getenv("CCVM_ITEST_PROXMOX_CONTROL_PLANE_ONLY") != ""
}

// eachBackend runs fn against every requested backend, gating each one.
func eachBackend(t *testing.T, fn func(t *testing.T, name string, b backend.Backend)) {
	t.Helper()
	itest.SkipUnlessAnyRequested(t)

	for _, name := range itest.Requested() {
		name := name
		t.Run(name, func(t *testing.T) {
			b, err := backend.New(name, run.New(testLogger{t}), configFromEnv())
			if err != nil {
				t.Fatalf("backend %q was required but could not be built: %v", name, err)
			}
			itest.Gate(t, name, probeFor(name, b))
			fn(t, name, b)
		})
	}
}

// testLogger routes --verbose output into the test log, so a failure shows the
// exact commands that produced it.
type testLogger struct{ t *testing.T }

func (l testLogger) Write(p []byte) (int, error) {
	l.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func specFor(t *testing.T, name string) backend.Spec {
	t.Helper()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return backend.Spec{
		Name:      uniqueName(t),
		Profile:   "base",
		Image:     imageFor(name),
		Project:   project,
		WorkDir:   "/work",
		CodeMode:  "mount",
		CPUs:      1,
		Memory:    "1G",
		TTL:       "1h",
		CreatedAt: time.Now().UTC(),
	}
}

// uniqueName keeps concurrent runs and leftovers from colliding.
func uniqueName(t *testing.T) string {
	base := strings.ToLower(t.Name())
	base = strings.NewReplacer("/", "-", "_", "-", " ", "-").Replace(base)
	if len(base) > 24 {
		base = base[len(base)-24:]
	}
	// The prefix is what marks the machine as a session on backends with no
	// metadata field, so it has to survive into the name.
	return backend.NamePrefix + "it-" + strings.Trim(base, "-")
}

// mustCreate creates a machine and registers its destruction, so a failing
// test does not leave infrastructure behind.
func mustCreate(t *testing.T, b backend.Backend, spec backend.Spec) backend.Handle {
	t.Helper()
	ctx := testCtx(t)

	h, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		// Best effort: the test may already have destroyed it.
		_ = b.Destroy(context.Background(), h)
	})

	if err := b.Start(ctx, h); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Wait(ctx, h); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return h
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// TestLifecycle is the core claim: a machine can be created, becomes usable,
// runs commands, and goes away.
func TestLifecycle(t *testing.T) {
	eachBackend(t, func(t *testing.T, name string, b backend.Backend) {
		if controlPlaneOnly(name) {
			t.Skip("guest boot needs a hypervisor; this tier covers the control plane only")
		}
		ctx := testCtx(t)
		spec := specFor(t, name)
		h := mustCreate(t, b, spec)

		out, err := b.Exec(ctx, h, "sh", "-c", "echo alive")
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !strings.Contains(string(out), "alive") {
			t.Errorf("Exec output = %q", out)
		}

		if target := b.SSHTarget(h); target == "" {
			t.Error("SSHTarget is empty; the machine is unreachable")
		}

		found := false
		machines, err := b.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, m := range machines {
			if m.Name == spec.Name {
				found = true
				if m.State != backend.StateRunning {
					t.Errorf("State = %q, want running", m.State)
				}
				if m.Backend != name {
					t.Errorf("Backend = %q, want %q", m.Backend, name)
				}
			}
		}
		if !found {
			t.Errorf("machine %q missing from List", spec.Name)
		}

		if err := b.Destroy(ctx, h); err != nil {
			t.Fatalf("Destroy: %v", err)
		}

		machines, err = b.List(ctx)
		if err != nil {
			t.Fatalf("List after destroy: %v", err)
		}
		for _, m := range machines {
			if m.Name == spec.Name {
				t.Errorf("machine %q survived Destroy", spec.Name)
			}
		}
	})
}

// The session record is what ccvm-done, ls, and the reaper all read, so it has
// to survive a round trip through the machine.
func TestSessionRecordRoundTrip(t *testing.T) {
	eachBackend(t, func(t *testing.T, name string, b backend.Backend) {
		if controlPlaneOnly(name) {
			t.Skip("guest boot needs a hypervisor")
		}
		ctx := testCtx(t)
		spec := specFor(t, name)
		h := mustCreate(t, b, spec)

		want := session.Session{
			Name: spec.Name, Backend: name, Profile: "base",
			Project: spec.Project, WorkDir: "/work", CodeMode: "mount",
			Created: spec.CreatedAt, TTL: "1h",
		}
		data, err := session.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}

		local := filepath.Join(t.TempDir(), "session.toml")
		if err := os.WriteFile(local, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Exec(ctx, h, "mkdir", "-p", filepath.Dir(backend.SessionFile)); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := b.Push(ctx, h, local, backend.SessionFile); err != nil {
			t.Fatalf("Push: %v", err)
		}

		back := filepath.Join(t.TempDir(), "back.toml")
		if err := b.Pull(ctx, h, backend.SessionFile, back); err != nil {
			t.Fatalf("Pull: %v", err)
		}
		raw, err := os.ReadFile(back)
		if err != nil {
			t.Fatal(err)
		}
		got, err := session.Unmarshal(raw)
		if err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.Name != want.Name || got.TTL != want.TTL || got.CodeMode != want.CodeMode {
			t.Errorf("round trip = %+v, want %+v", got, want)
		}
	})
}

// The reaper reads TTL from machines that are no longer running. This is the
// case an in-machine file read over exec cannot serve, and the reason Pull
// exists as a distinct operation.
func TestReadSessionRecordFromStoppedMachine(t *testing.T) {
	eachBackend(t, func(t *testing.T, name string, b backend.Backend) {
		if controlPlaneOnly(name) {
			t.Skip("guest boot needs a hypervisor")
		}
		// k8s used to be exempt here, because stopping a session deletes its
		// pod and takes the filesystem with it. The record is now mirrored onto
		// the Job, which outlives the pod, so the contract holds on every
		// backend and this is worth asserting rather than skipping.
		stopper, ok := b.(interface {
			Stop(context.Context, backend.Handle) error
		})
		if !ok {
			t.Skipf("%s cannot stop a machine without destroying it", name)
		}

		ctx := testCtx(t)
		spec := specFor(t, name)
		// Keep, or the backend may delete the machine the moment it stops and
		// there is nothing left to read from.
		spec.Keep = true
		h := mustCreate(t, b, spec)

		data, err := session.Marshal(session.Session{
			Name: spec.Name, TTL: session.Keep, Created: spec.CreatedAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		local := filepath.Join(t.TempDir(), "session.toml")
		if err := os.WriteFile(local, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Exec(ctx, h, "mkdir", "-p", filepath.Dir(backend.SessionFile)); err != nil {
			t.Fatal(err)
		}
		if err := b.Push(ctx, h, local, backend.SessionFile); err != nil {
			t.Fatal(err)
		}

		if err := stopper.Stop(ctx, h); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		// The machine must still be listed, as stopped rather than absent.
		//
		// Polled rather than read once. `docker stop` returns when the
		// container's process exits, but `docker ps` can still report it
		// running for a moment afterwards. That window is far shorter than the
		// gap between two ccvm invocations, so it is an artifact of asserting
		// in-process rather than anything a user can observe. An absent machine
		// never reaches the wanted state, so the case this assertion exists for
		// still fails.
		var state string
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			machines, err := b.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state = ""
			for _, m := range machines {
				if m.Name == spec.Name {
					state = m.State
				}
			}
			if state == backend.StateStopped {
				break
			}
			time.Sleep(time.Second)
		}
		if state != backend.StateStopped {
			t.Errorf("stopped machine listed as %q, want %q", state, backend.StateStopped)
		}

		back := filepath.Join(t.TempDir(), "back.toml")
		if err := b.Pull(ctx, h, backend.SessionFile, back); err != nil {
			t.Fatalf("Pull from a stopped machine: %v\n\n"+
				"The reaper decides whether to destroy machines that are no longer "+
				"running, so it must be able to read their TTL.", err)
		}
		raw, _ := os.ReadFile(back)
		got, err := session.Unmarshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Kept() {
			t.Errorf("TTL = %q, want keep", got.TTL)
		}
	})
}

// The sentinel is how a session ends itself, and PID 1 must act on it.
func TestDoneSentinelEndsTheMachine(t *testing.T) {
	eachBackend(t, func(t *testing.T, name string, b backend.Backend) {
		if controlPlaneOnly(name) {
			t.Skip("guest boot needs a hypervisor")
		}
		if name != "docker" && name != "k8s" {
			t.Skipf("%s relies on the reaper rather than PID 1 for this", name)
		}

		ctx := testCtx(t)
		spec := specFor(t, name)
		h := mustCreate(t, b, spec)

		if _, err := b.Exec(ctx, h, "mkdir", "-p", filepath.Dir(backend.DoneSentinel)); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Exec(ctx, h, "touch", backend.DoneSentinel); err != nil {
			t.Fatalf("touch sentinel: %v", err)
		}

		// PID 1 polls, so give it a little longer than its interval.
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			machines, err := b.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			gone := true
			for _, m := range machines {
				if m.Name == spec.Name && m.State == backend.StateRunning {
					gone = false
				}
			}
			if gone {
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("machine %q still running 60s after the sentinel appeared", spec.Name)
	})
}

// A machine created without Keep must be ephemeral, and one created with it
// must be durable. `ccvm keep` depends on the difference.
func TestKeepControlsDurability(t *testing.T) {
	eachBackend(t, func(t *testing.T, name string, b backend.Backend) {
		if controlPlaneOnly(name) {
			t.Skip("guest boot needs a hypervisor")
		}
		reporter, ok := b.(backend.EphemeralReporter)
		if !ok {
			t.Skipf("%s machines are always durable", name)
		}
		ctx := testCtx(t)

		for _, keep := range []bool{false, true} {
			keep := keep
			t.Run(map[bool]string{true: "keep", false: "ephemeral"}[keep], func(t *testing.T) {
				spec := specFor(t, name)
				spec.Keep = keep
				h := mustCreate(t, b, spec)

				auto, err := reporter.AutoRemoves(ctx, h)
				if err != nil {
					t.Fatalf("AutoRemoves: %v", err)
				}
				if auto == keep {
					t.Errorf("Keep=%v produced auto-remove=%v; they must be opposites", keep, auto)
				}
			})
		}
	})
}

// Preflight must not create anything, or `ccvm doctor` has side effects.
func TestPreflightCreatesNothing(t *testing.T) {
	eachBackend(t, func(t *testing.T, name string, b backend.Backend) {
		if controlPlaneOnly(name) {
			t.Skip("no template exists in the control-plane tier")
		}
		ctx := testCtx(t)
		before, err := b.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if err := b.Preflight(ctx, backend.Spec{
			Name: uniqueName(t), Profile: "base", Image: imageFor(name),
		}); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		after, err := b.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(after) != len(before) {
			t.Errorf("Preflight changed the machine count from %d to %d", len(before), len(after))
		}
	})
}

// A missing image must fail with the backend's own message, not a hang.
//
// k8s is exempt by design: whether an image pulls is the cluster's business,
// not the client's, so it surfaces a pull failure from the pod's events during
// Wait instead. TestK8sUnpullableImageFailsFast covers that path.
func TestPreflightRejectsMissingImage(t *testing.T) {
	eachBackend(t, func(t *testing.T, name string, b backend.Backend) {
		if name == "k8s" {
			t.Skip("k8s reports pull failures from the pod's events in Wait, not from Preflight")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		err := b.Preflight(ctx, backend.Spec{
			Name:    uniqueName(t),
			Profile: "base",
			Image:   "ccvm-nonexistent/image:v0",
		})
		if err == nil {
			t.Fatal("Preflight accepted an image that does not exist")
		}
		if strings.TrimSpace(err.Error()) == "" {
			t.Error("Preflight failed with an empty message")
		}
	})
}

// --------------------------------------------------------------- proxmox

// Proxmox is the one backend whose control plane can be exercised in CI
// without a hypervisor. These assertions are what frozen httptest fixtures
// cannot make: fixtures keep passing against a Proxmox release that changed a
// response shape, and only a real pvedaemon notices.
func TestProxmoxControlPlane(t *testing.T) {
	if !itest.IsRequested("proxmox") {
		t.Skip("proxmox not requested")
	}
	b, err := backend.New("proxmox", run.New(testLogger{t}), configFromEnv())
	if err != nil {
		t.Fatalf("build proxmox backend: %v", err)
	}
	itest.Gate(t, "proxmox", probeFor("proxmox", b))

	p, ok := b.(*backend.Proxmox)
	if !ok {
		t.Fatalf("expected *backend.Proxmox, got %T", b)
	}
	ctx := testCtx(t)

	t.Run("nextid returns a usable vmid", func(t *testing.T) {
		id, err := p.NextIDForTest(ctx)
		if err != nil {
			t.Fatalf("nextid: %v", err)
		}
		if id <= 0 {
			t.Errorf("nextid = %d", id)
		}
	})

	t.Run("cluster reports an online node", func(t *testing.T) {
		node, err := p.PickNodeForTest(ctx)
		if err != nil {
			t.Fatalf("pick node: %v", err)
		}
		if node == "" {
			t.Error("no node chosen")
		}
	})

	// List must not error against a real cluster even when nothing is tagged;
	// an empty result and a failure are very different things.
	t.Run("list succeeds with no ccvm guests", func(t *testing.T) {
		if _, err := b.List(ctx); err != nil {
			t.Fatalf("List: %v", err)
		}
	})

	// A missing template is the most common misconfiguration, and the message
	// has to name it rather than failing somewhere downstream.
	t.Run("missing template is named", func(t *testing.T) {
		err := b.Preflight(ctx, backend.Spec{Profile: "base", Image: "999999"})
		if err == nil {
			t.Fatal("Preflight accepted a template vmid that does not exist")
		}
		if !strings.Contains(err.Error(), "999999") {
			t.Errorf("err = %v, want it to name the missing template", err)
		}
	})
}

// --------------------------------------------------------------- k8s

// The defining kubernetes failure: a Job applies successfully and its pod never
// runs. This is the case a naive implementation hangs on forever, so it is
// asserted against a real API server rather than only against a fake.
func TestK8sUnpullableImageFailsFast(t *testing.T) {
	if !itest.IsRequested("k8s") {
		t.Skip("k8s not requested")
	}
	b, err := backend.New("k8s", run.New(testLogger{t}), configFromEnv())
	if err != nil {
		t.Fatalf("build k8s backend: %v", err)
	}
	itest.Gate(t, "k8s", probeFor("k8s", b))

	ctx := testCtx(t)
	spec := specFor(t, "k8s")
	spec.Image = "ccvm-nonexistent.invalid/base:v0"

	h, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(context.Background(), h) })

	// Creating succeeds — that is the whole point. Readiness is where it fails.
	start := time.Now()
	err = b.Wait(ctx, h)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Wait reported a pod ready when its image cannot be pulled")
	}
	if elapsed > 3*time.Minute {
		t.Errorf("Wait took %s; a pull failure must be reported, not waited out", elapsed)
	}
	// The reason has to come from the pod, or the message is useless.
	if !strings.Contains(err.Error(), "ImagePull") && !strings.Contains(err.Error(), "ErrImagePull") {
		t.Errorf("err = %v\n\nwant the pod's own pull failure reason", err)
	}
	t.Logf("failed in %s with: %v", elapsed.Round(time.Second), err)
}

// A finished Job must clean itself up through the cluster's own TTL, without
// ccvm running a reaper.
func TestK8sJobCarriesNativeTTL(t *testing.T) {
	if !itest.IsRequested("k8s") {
		t.Skip("k8s not requested")
	}
	b, err := backend.New("k8s", run.New(testLogger{t}), configFromEnv())
	if err != nil {
		t.Fatalf("build k8s backend: %v", err)
	}
	itest.Gate(t, "k8s", probeFor("k8s", b))

	k, ok := b.(*backend.K8s)
	if !ok {
		t.Fatalf("expected *backend.K8s, got %T", b)
	}
	spec := specFor(t, "k8s")
	spec.TTL = "1h"

	manifest, err := k.ManifestForTest(spec)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(manifest, &job); err != nil {
		t.Fatal(err)
	}
	jobSpec := job["spec"].(map[string]any)
	if jobSpec["activeDeadlineSeconds"] != float64(3600) {
		t.Errorf("activeDeadlineSeconds = %v, want the TTL expressed natively", jobSpec["activeDeadlineSeconds"])
	}

	// And the cluster must actually accept it.
	ctx := testCtx(t)
	h, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(context.Background(), h) })
}

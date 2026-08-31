package backend_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/run"
)

func baseSpec() backend.Spec {
	return backend.Spec{
		Name:      "cc-foo",
		Profile:   "go",
		Image:     "ccvm/go:latest",
		Project:   "/Users/j/src/foo",
		WorkDir:   "/work",
		CodeMode:  "mount",
		CPUs:      4,
		Memory:    "8G",
		SSHPort:   2231,
		CreatedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	}
}

func newDocker(t *testing.T) (*backend.Docker, *run.Fake) {
	t.Helper()
	f := run.NewFake()
	return backend.NewDocker(f), f
}

// --rm must be present for an ordinary session: the sentinel-aware entrypoint
// exits and the container should clean itself up.
func TestDockerCreateUsesRmForEphemeralSession(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "run").Stdout("abc123\n")

	h, err := d.Create(context.Background(), baseSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !f.HasArg("--rm", "docker", "run") {
		t.Errorf("expected --rm for a non-keep session\ngot: %s", f)
	}
	if h.ID != "abc123" {
		t.Errorf("ID = %q", h.ID)
	}
}

// ...and must be absent under --keep, or the container is deleted the instant
// it stops and there is nothing left to inspect.
func TestDockerCreateOmitsRmWhenKeep(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "run").Stdout("abc123\n")

	s := baseSpec()
	s.Keep = true
	if _, err := d.Create(context.Background(), s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.HasArg("--rm", "docker", "run") {
		t.Errorf("--rm must not be set for a kept container\ngot: %s", f)
	}
}

// A session container must never be reachable from the network.
func TestDockerCreatePublishesOnLoopbackOnly(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "run").Stdout("x\n")

	if _, err := d.Create(context.Background(), baseSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, ok := f.ArgAfter("--publish", "docker", "run")
	if !ok {
		t.Fatalf("no --publish\ngot: %s", f)
	}
	if got != "127.0.0.1:2231:22" {
		t.Errorf("--publish = %q, want it bound to loopback", got)
	}
}

func TestDockerCreateSkipsPublishWhenNoPort(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "run").Stdout("x\n")

	s := baseSpec()
	s.SSHPort = 0
	if _, err := d.Create(context.Background(), s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := f.ArgAfter("--publish", "docker", "run"); ok {
		t.Error("published a port when none was allocated")
	}
}

// Labels carry immutable creation facts only. TTL must NOT be among them:
// docker labels cannot be changed after creation, so a TTL stored here could
// never be moved by `ccvm keep`.
func TestDockerCreateLabelsExcludeTTL(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "run").Stdout("x\n")

	s := baseSpec()
	s.TTL = "12h"
	if _, err := d.Create(context.Background(), s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	call := strings.Join(f.Find("docker", "run"), " ")
	for _, want := range []string{
		"ccvm=1",
		"ccvm.project=/Users/j/src/foo",
		"ccvm.profile=go",
		"ccvm.created=2026-08-30T12:00:00Z",
		"ccvm.ssh-port=2231",
	} {
		if !strings.Contains(call, want) {
			t.Errorf("missing label %q in: %s", want, call)
		}
	}
	if strings.Contains(call, "ccvm.ttl") {
		t.Errorf("TTL must not be a label — it is mutable and labels are not:\n%s", call)
	}
}

func TestDockerCreateMountsProjectOnlyInMountMode(t *testing.T) {
	tests := []struct {
		mode      string
		wantMount bool
	}{
		{"mount", true},
		{"rsync", false},
		{"git", false},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			d, f := newDocker(t)
			f.On("docker", "run").Stdout("x\n")

			s := baseSpec()
			s.CodeMode = tt.mode
			if _, err := d.Create(context.Background(), s); err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, ok := f.ArgAfter("--volume", "docker", "run")
			if ok != tt.wantMount {
				t.Fatalf("--volume present = %v, want %v", ok, tt.wantMount)
			}
			if tt.wantMount && got != "/Users/j/src/foo:/work" {
				t.Errorf("--volume = %q", got)
			}
		})
	}
}

func TestDockerCreatePassesResourcesAndEnv(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "run").Stdout("x\n")

	s := baseSpec()
	s.Env = map[string]string{"GOFLAGS": "-mod=mod", "CGO_ENABLED": "0"}
	if _, err := d.Create(context.Background(), s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if v, _ := f.ArgAfter("--cpus", "docker", "run"); v != "4" {
		t.Errorf("--cpus = %q", v)
	}
	if v, _ := f.ArgAfter("--memory", "docker", "run"); v != "8G" {
		t.Errorf("--memory = %q", v)
	}
	call := strings.Join(f.Find("docker", "run"), " ")
	for _, want := range []string{"GOFLAGS=-mod=mod", "CGO_ENABLED=0"} {
		if !strings.Contains(call, want) {
			t.Errorf("missing env %q", want)
		}
	}
	// The image is the final argument, after every flag.
	call2 := f.Find("docker", "run")
	if got := call2[len(call2)-1]; got != "ccvm/go:latest" {
		t.Errorf("last arg = %q, want the image", got)
	}
}

// Env ordering must be deterministic or the --verbose log and any golden test
// become flaky.
func TestDockerCreateEnvOrderIsStable(t *testing.T) {
	var first string
	for i := range 5 {
		d, f := newDocker(t)
		f.On("docker", "run").Stdout("x\n")
		s := baseSpec()
		s.Env = map[string]string{"C": "3", "A": "1", "B": "2"}
		if _, err := d.Create(context.Background(), s); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got := strings.Join(f.Find("docker", "run"), " ")
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("argv order varies between runs:\n%s\n%s", first, got)
		}
	}
}

func TestDockerCreateSurfacesDaemonError(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "run").Fail(125, "docker: Error response from daemon: no such image")

	_, err := d.Create(context.Background(), baseSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no such image") {
		t.Errorf("err = %v, want the daemon's own message", err)
	}
}

func TestDockerPreflightReportsUnreachableDaemon(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "info").Fail(1, "Cannot connect to the Docker daemon")

	err := d.Preflight(context.Background(), baseSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("err = %v", err)
	}
}

func TestDockerPreflightRequiresImageInProfile(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "info").Stdout("27.0.0\n")

	s := baseSpec()
	s.Image = ""
	err := d.Preflight(context.Background(), s)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "[backend.docker].image") {
		t.Errorf("err = %v, want it to name the missing profile table", err)
	}
}

// Pull must go through docker cp, which works on a stopped container — the
// reaper reads TTL from machines that are no longer running.
func TestDockerPullUsesCpSoItWorksOnStoppedContainers(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "cp").Stdout("")

	h := backend.Handle{Backend: "docker", Name: "cc-foo"}
	if err := d.Pull(context.Background(), h, backend.SessionFile, "/tmp/session.toml"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	call := f.Find("docker", "cp")
	if len(call) < 4 || call[2] != "cc-foo:"+backend.SessionFile {
		t.Errorf("call = %v, want docker cp from the container", call)
	}
	if f.Ran("docker", "exec") {
		t.Error("Pull used docker exec, which cannot read from a stopped container")
	}
}

func TestDockerListParsesAndNormalizes(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "ps").Stdout(
		`{"ID":"abc","Names":"cc-foo","State":"running","Labels":"ccvm=1,ccvm.profile=go,ccvm.project=/src/foo,ccvm.created=2026-08-30T12:00:00Z"}
{"ID":"def","Names":"cc-bar","State":"exited","Labels":"ccvm=1,ccvm.profile=base,ccvm.project=/src/bar"}
`)

	got, err := d.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d machines, want 2", len(got))
	}
	if got[0].Name != "cc-foo" || got[0].State != backend.StateRunning || got[0].Profile != "go" {
		t.Errorf("machine[0] = %+v", got[0])
	}
	if got[0].Created.IsZero() {
		t.Error("created timestamp not parsed from label")
	}
	// A stopped container must appear as stopped, not vanish from the listing.
	if got[1].State != backend.StateStopped {
		t.Errorf("machine[1].State = %q, want stopped", got[1].State)
	}
}

func TestDockerListFiltersOnOwnerLabel(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "ps").Stdout("")

	if _, err := d.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if v, _ := f.ArgAfter("--filter", "docker", "ps"); v != "label=ccvm" {
		t.Errorf("--filter = %q, want label=ccvm", v)
	}
	// --all, or stopped machines are invisible to the reaper.
	if !f.HasArg("--all", "docker", "ps") {
		t.Error("expected --all so stopped containers are listed")
	}
}

func TestDockerListEmptyIsNotAnError(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "ps").Stdout("\n")

	got, err := d.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d machines, want 0", len(got))
	}
}

func TestDockerWaitRejectsNonRunningContainer(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "inspect").Stdout("false\n")

	err := d.Wait(context.Background(), backend.Handle{Name: "cc-foo"})
	if err == nil {
		t.Fatal("expected an error for a container that is not running")
	}
}

func TestDockerDestroyForcesAndRemovesVolumes(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "rm").Stdout("")

	if err := d.Destroy(context.Background(), backend.Handle{Name: "cc-foo"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !f.HasArg("--force", "docker", "rm") || !f.HasArg("--volumes", "docker", "rm") {
		t.Errorf("call = %v, want --force and --volumes", f.Find("docker", "rm"))
	}
}

// `ccvm keep` can move a TTL but cannot undo docker's AutoRemove, which is
// fixed at creation. The backend has to be able to report that so the CLI can
// avoid promising an exemption it cannot deliver.
func TestDockerAutoRemoves(t *testing.T) {
	tests := []struct {
		out  string
		want bool
	}{
		{"true\n", true},
		{"false\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.out, func(t *testing.T) {
			d, f := newDocker(t)
			f.On("docker", "inspect").Stdout(tt.out)

			got, err := d.AutoRemoves(context.Background(), backend.Handle{Name: "cc-foo"})
			if err != nil {
				t.Fatalf("AutoRemoves: %v", err)
			}
			if got != tt.want {
				t.Errorf("AutoRemoves = %v, want %v", got, tt.want)
			}
		})
	}
}

// A container created with --keep must not carry AutoRemove, or `ccvm keep`
// is a promise the daemon will break.
func TestDockerKeepAndAutoRemoveAreConsistent(t *testing.T) {
	for _, keep := range []bool{true, false} {
		d, f := newDocker(t)
		f.On("docker", "run").Stdout("x\n")

		s := baseSpec()
		s.Keep = keep
		if _, err := d.Create(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		hasRm := f.HasArg("--rm", "docker", "run")
		if hasRm == keep {
			t.Errorf("Keep=%v produced --rm=%v; they must be opposites", keep, hasRm)
		}
	}
}

func TestRegistryBuildsDocker(t *testing.T) {
	b, err := backend.New("docker", run.NewFake(), backend.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.Name() != "docker" {
		t.Errorf("Name() = %q", b.Name())
	}
}

func TestRegistryUnknownBackendListsAvailable(t *testing.T) {
	_, err := backend.New("frobnicate", run.NewFake(), backend.Config{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Errorf("err = %v, want it to list what is available", err)
	}
}

func TestDockerStopDoesNotRemove(t *testing.T) {
	d, f := newDocker(t)
	f.On("docker", "stop").Stdout("cc-foo\n")

	if err := d.Stop(context.Background(), backend.Handle{Name: "cc-foo"}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if f.Ran("docker", "rm") {
		t.Error("Stop removed the container; the reaper must be able to read a stopped machine")
	}
}

// A container that lost ccvm's label is invisible to every command and will
// never be reaped. --all is how you find it again, and it must not drag in
// unrelated containers while doing so.
func TestDockerListUnownedFindsOnlyLostSessions(t *testing.T) {
	f := run.NewFake()
	f.On("docker", "ps").Stdout(strings.Join([]string{
		`{"ID":"1","Names":"cc-lost","State":"running","Labels":"com.example=1"}`,
		`{"ID":"2","Names":"cc-owned","State":"running","Labels":"ccvm=1"}`,
		`{"ID":"3","Names":"postgres","State":"running","Labels":""}`,
	}, "\n"))
	d := backend.NewDocker(f)

	got, err := d.ListUnowned(context.Background())
	if err != nil {
		t.Fatalf("ListUnowned: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("machines = %+v, want only the unlabelled session", got)
	}
	if got[0].Name != "cc-lost" {
		t.Errorf("Name = %q, want cc-lost", got[0].Name)
	}
}

package backend_test

import (
	"context"
	"strings"
	"testing"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/run"
)

func newOrbstack(t *testing.T) (*backend.Orbstack, *run.Fake) {
	t.Helper()
	f := run.NewFake()
	return backend.NewOrbstack(f), f
}

func orbSpec() backend.Spec {
	s := baseSpec()
	s.Image = "ccvm-base" // a template machine, not a registry reference
	// Mount mode adds a config step; tests that care set it explicitly.
	s.CodeMode = "git"
	return s
}

// Create clones the template rather than building from scratch: copy-on-write
// is what makes a session cheap.
func TestOrbstackCreateClonesTemplate(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "clone").Stdout("")

	h, err := o.Create(context.Background(), orbSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	call := f.Find("orbctl", "clone")
	if len(call) != 4 || call[2] != "ccvm-base" || call[3] != "cc-foo" {
		t.Errorf("call = %v, want `orbctl clone ccvm-base cc-foo`", call)
	}
	if h.Name != "cc-foo" {
		t.Errorf("Name = %q", h.Name)
	}
}

// OrbStack multiplexes ssh itself, so no port is allocated and nothing is
// written to ~/.ssh/config.
// The user must be explicit: OrbStack connects as the machine's default account
// otherwise, which cannot read /root — where claude lives and where ccvm puts
// the session's key and credential. It surfaces as "claude: command not found".
func TestOrbstackSSHTargetNamesTheUser(t *testing.T) {
	o, _ := newOrbstack(t)
	if got := o.SSHTarget(backend.Handle{Name: "cc-foo"}); got != "root@cc-foo@orb" {
		t.Errorf("SSHTarget = %q, want root@cc-foo@orb", got)
	}
}

// orbctl run rejects a "--" separator, so the command must follow the flags
// directly.
func TestOrbstackExecPassesCommandWithoutSeparator(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "run").Stdout("alive\n")

	if _, err := o.Exec(context.Background(), backend.Handle{Name: "cc-foo"}, "echo", "alive"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	call := f.Find("orbctl", "run")
	for _, a := range call {
		if a == "--" {
			t.Fatalf("argv contains a -- separator, which orbctl rejects: %v", call)
		}
	}
	want := "orbctl run -m cc-foo -u root echo alive"
	if got := run.ShellQuote(call); got != want {
		t.Errorf("call = %q, want %q", got, want)
	}
}

func TestOrbstackExecRunsAsRoot(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "run").Stdout("")

	if _, err := o.Exec(context.Background(), backend.Handle{Name: "cc-foo"}, "true"); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.ArgAfter("-u", "orbctl", "run"); v != "root" {
		t.Errorf("-u = %q, want root (sessions write /etc/ccvm)", v)
	}
}

// Push and pull take absolute paths, which is what the session record needs.
func TestOrbstackPushAndPullUseAbsolutePaths(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "push").Stdout("")
	f.On("orbctl", "pull").Stdout("")
	h := backend.Handle{Name: "cc-foo"}

	if err := o.Push(context.Background(), h, "/tmp/local.toml", backend.SessionFile); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := run.ShellQuote(f.Find("orbctl", "push")); got != "orbctl push -m cc-foo /tmp/local.toml /etc/ccvm/session.toml" {
		t.Errorf("push call = %q", got)
	}

	if err := o.Pull(context.Background(), h, backend.SessionFile, "/tmp/back.toml"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if got := run.ShellQuote(f.Find("orbctl", "pull")); got != "orbctl pull -m cc-foo /etc/ccvm/session.toml /tmp/back.toml" {
		t.Errorf("pull call = %q", got)
	}
}

// OrbStack has no metadata field, so ownership is inferred from the name. A
// machine the user created themselves must not be listed as ccvm's.
func TestOrbstackListFiltersByNamePrefix(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "list").Stdout(`[
      {"id":"a1","name":"cc-foo","state":"running","image":{"distro":"debian"},"config":{"isolated":true}},
      {"id":"b2","name":"my-dev-box","state":"running","image":{"distro":"ubuntu"},"config":{"isolated":false}},
      {"id":"c3","name":"cc-bar","state":"stopped","image":{"distro":"debian"},"config":{"isolated":true}}
    ]`)

	got, err := o.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d machines, want 2 (the user's own machine must be left alone)", len(got))
	}
	if got[0].Name != "cc-foo" || got[0].State != backend.StateRunning {
		t.Errorf("machines[0] = %+v", got[0])
	}
	// A stopped machine must appear as stopped, not vanish: the reaper needs it.
	if got[1].Name != "cc-bar" || got[1].State != backend.StateStopped {
		t.Errorf("machines[1] = %+v", got[1])
	}
	if got[0].SSH != "root@cc-foo@orb" {
		t.Errorf("SSH = %q", got[0].SSH)
	}
}

func TestOrbstackListEmpty(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "list").Stdout("[]")

	got, err := o.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d machines, want 0", len(got))
	}
}

func TestOrbstackPreflightReportsStoppedOrbstack(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "status").Stdout("Stopped\n")

	err := o.Preflight(context.Background(), orbSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "status") {
		t.Errorf("err = %v", err)
	}
}

// The template is a machine on this Mac, so a missing one is a local, fixable
// condition and the message should say how.
func TestOrbstackPreflightNamesMissingTemplate(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "status").Stdout("Running\n")
	f.On("orbctl", "list").Stdout(`[{"id":"a","name":"something-else","state":"running","image":{"distro":"debian"},"config":{"isolated":false}}]`)

	err := o.Preflight(context.Background(), orbSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ccvm-base") {
		t.Errorf("err = %v, want it to name the missing template", err)
	}
	if !strings.Contains(err.Error(), "profiles build") {
		t.Errorf("err = %v, want it to say how to create it", err)
	}
}

func TestOrbstackPreflightPassesWhenTemplateExists(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "status").Stdout("Running\n")
	f.On("orbctl", "list").Stdout(`[{"id":"a","name":"ccvm-base","state":"stopped","image":{"distro":"debian"},"config":{"isolated":true}}]`)

	if err := o.Preflight(context.Background(), orbSpec()); err != nil {
		t.Errorf("Preflight: %v", err)
	}
}

func TestOrbstackPreflightRequiresTemplateInProfile(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "status").Stdout("Running\n")

	s := orbSpec()
	s.Image = ""
	err := o.Preflight(context.Background(), s)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "[backend.orbstack].template") {
		t.Errorf("err = %v, want it to name the missing profile key", err)
	}
}

func TestOrbstackStopDoesNotDelete(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "stop").Stdout("")

	if err := o.Stop(context.Background(), backend.Handle{Name: "cc-foo"}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if f.Ran("orbctl", "delete") {
		t.Error("Stop deleted the machine")
	}
}

func TestOrbstackDestroyForces(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "delete").Stdout("")

	if err := o.Destroy(context.Background(), backend.Handle{Name: "cc-foo"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !f.HasArg("-f", "orbctl", "delete") {
		t.Errorf("call = %v, want -f so teardown is not interactive", f.Find("orbctl", "delete"))
	}
}

func TestOrbstackRegistered(t *testing.T) {
	b, err := backend.New("orbstack", run.NewFake(), backend.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.Name() != "orbstack" {
		t.Errorf("Name() = %q", b.Name())
	}
	// Registering is what earns a backend the integration suite.
	if _, ok := b.(backend.Stopper); !ok {
		t.Error("orbstack must implement Stopper, or the stopped-machine tests skip silently")
	}
}

// A template is not a session. Listing one would offer the user a machine whose
// destruction breaks every future spawn from that profile.
func TestSessionAndTemplateNamesAreDistinct(t *testing.T) {
	tests := []struct {
		name       string
		isSession  bool
		isTemplate bool
	}{
		{"cc-foo", true, false},
		{"cc-it-lifecycle", true, false},
		{"ccvm-base", false, true},
		{"ccvm-go", false, true},
		{"my-dev-box", false, false},
		{"", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backend.IsSessionName(tt.name); got != tt.isSession {
				t.Errorf("IsSessionName(%q) = %v, want %v", tt.name, got, tt.isSession)
			}
			if got := backend.IsTemplateName(tt.name); got != tt.isTemplate {
				t.Errorf("IsTemplateName(%q) = %v, want %v", tt.name, got, tt.isTemplate)
			}
		})
	}
}

func TestOrbstackListExcludesTemplates(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "list").Stdout(`[
      {"id":"a","name":"cc-foo","state":"running","image":{"distro":"debian"},"config":{"isolated":true}},
      {"id":"b","name":"ccvm-base","state":"running","image":{"distro":"debian"},"config":{"isolated":true}}
    ]`)

	got, err := o.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Name != "cc-foo" {
		t.Errorf("List = %v, want only the session; a template must not be offered for destruction", got)
	}
}

// An isolated machine has no view of the Mac, so a mount must be granted
// explicitly — and a clone cannot take --mount, so it is configured between
// cloning and starting. Without this, --code mount silently produced an empty
// /work.
func TestOrbstackCreateAttachesTheProjectForMountMode(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "clone").Stdout("")
	f.On("orbctl", "config", "add").Stdout("")

	s := orbSpec()
	s.CodeMode = "mount"
	if _, err := o.Create(context.Background(), s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	call := strings.Join(f.Find("orbctl", "config", "add"), " ")
	if !strings.Contains(call, "machine.cc-foo.mounts") {
		t.Errorf("call = %q, want the machine's mount list", call)
	}
	if !strings.Contains(call, "/Users/j/src/foo:/work") {
		t.Errorf("call = %q, want the project attached at the work directory", call)
	}
}

// The mount has to be configured before the machine first starts.
func TestOrbstackMountIsConfiguredBeforeStart(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "clone").Stdout("")
	f.On("orbctl", "config", "add").Stdout("")
	f.On("orbctl", "start").Stdout("")

	s := orbSpec()
	s.CodeMode = "mount"
	h, err := o.Create(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}

	var configAt, startAt = -1, -1
	for i, c := range f.Calls() {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "config add") && configAt == -1 {
			configAt = i
		}
		if strings.Contains(joined, "orbctl start") && startAt == -1 {
			startAt = i
		}
	}
	if configAt == -1 || startAt == -1 || configAt > startAt {
		t.Errorf("mount configured after start: %v", f.Lines())
	}
}

func TestOrbstackCreateSkipsMountForOtherModes(t *testing.T) {
	for _, mode := range []string{"git", "rsync"} {
		t.Run(mode, func(t *testing.T) {
			o, f := newOrbstack(t)
			f.On("orbctl", "clone").Stdout("")

			s := orbSpec()
			s.CodeMode = mode
			if _, err := o.Create(context.Background(), s); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if f.Ran("orbctl", "config", "add") {
				t.Errorf("%s mode attached a host directory", mode)
			}
		})
	}
}

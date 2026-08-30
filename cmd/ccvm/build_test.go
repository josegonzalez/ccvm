package main

import (
	"os"
	"strings"
	"testing"

	"github.com/josegonzalez/ccvm/internal/backendtest"
	"github.com/josegonzalez/ccvm/internal/run"
)

func newBuildApp(t *testing.T) (*app, *run.Fake) {
	t.Helper()
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	f := run.NewFake()
	a.runner = f
	return a, f
}

func TestProfilesBuildDockerUsesTheProfileDockerfile(t *testing.T) {
	a, f := newBuildApp(t)
	f.On("docker", "build").Stdout("")

	// The image copies guest binaries out of dist/, so the command requires it.
	if err := os.MkdirAll("dist", 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll("dist") })

	if err := a.profilesBuild("base", "docker", nil); err != nil {
		t.Fatalf("profilesBuild: %v", err)
	}
	call := strings.Join(f.Find("docker", "build"), " ")
	if !strings.HasSuffix(strings.Split(call, " -t ")[0], "Dockerfile") {
		t.Errorf("call = %q, want a Dockerfile", call)
	}
	if !strings.Contains(call, "-t ccvm/base:latest") {
		t.Errorf("call = %q, want the image from the profile", call)
	}
	// The context is the working directory: the Dockerfile copies guest
	// binaries out of dist/, which is not under the profile directory.
	if !strings.HasSuffix(strings.TrimSpace(call), " .") {
		t.Errorf("call = %q, want the working directory as the build context", call)
	}
}

// Building outside the repo cannot work, so it must say so rather than letting
// docker fail on a missing COPY source.
func TestProfilesBuildDockerRequiresDist(t *testing.T) {
	a, f := newBuildApp(t)
	f.On("docker", "build").Stdout("")

	err := a.profilesBuild("base", "docker", nil)
	if err == nil {
		t.Fatal("expected an error with no dist/ present")
	}
	if !strings.Contains(err.Error(), "make build") {
		t.Errorf("err = %v, want it to name the fix", err)
	}
	if f.Ran("docker", "build") {
		t.Error("ran a build that was going to fail on a missing COPY source")
	}
}

func TestProfilesBuildOrbstackReplacesAndProvisions(t *testing.T) {
	a, f := newBuildApp(t)
	f.On("orbctl", "delete").Stdout("")
	f.On("orbctl", "create").Stdout("")
	f.On("orbctl", "run").Stdout("")
	f.On("orbctl", "push").Stdout("")
	f.On("orbctl", "stop").Stdout("")

	if err := a.profilesBuild("base", "orbstack", nil); err != nil {
		t.Fatalf("profilesBuild: %v", err)
	}

	// Isolated, or the machine gets ambient access to the Mac.
	create := strings.Join(f.Find("orbctl", "create"), " ")
	if !strings.Contains(create, "--isolated") {
		t.Errorf("create = %q, want --isolated", create)
	}
	if !strings.Contains(create, "ccvm-base") {
		t.Errorf("create = %q, want the template name from the profile", create)
	}

	// A leftover template from a failed build would otherwise be reused.
	if !f.Ran("orbctl", "delete") {
		t.Error("an existing template was not replaced")
	}

	// Staged under /etc/ccvm, never /tmp: a guest's /tmp is a per-boot tmpfs
	// that orbctl push writes into a different view of, reporting success and
	// leaving nothing behind.
	push := strings.Join(f.Find("orbctl", "push"), " ")
	if strings.Contains(push, "/tmp/") {
		t.Errorf("push = %q, want staging away from /tmp", push)
	}

	// Clones start from a stopped template.
	if !f.Ran("orbctl", "stop") {
		t.Error("the template was left running")
	}
}

func TestProfilesBuildOrbstackFailsLoudlyOnProvisionError(t *testing.T) {
	a, f := newBuildApp(t)
	f.On("orbctl", "delete").Stdout("")
	f.On("orbctl", "create").Stdout("")
	f.On("orbctl", "run", "-m", "ccvm-base", "-u", "root", "mkdir").Stdout("")
	f.On("orbctl", "push").Stdout("")
	f.On("orbctl", "run").Fail(1, "E: Unable to locate package ripgrep")

	err := a.profilesBuild("base", "orbstack", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ripgrep") {
		t.Errorf("err = %v, want the underlying failure", err)
	}
}

func TestProfilesBuildUnknownBackend(t *testing.T) {
	a, _ := newBuildApp(t)
	if err := a.profilesBuild("base", "frobnicate", nil); err == nil {
		t.Fatal("expected an error")
	}
}

// Proxmox templates are not buildable yet, and the message has to say what to
// do instead rather than failing obscurely.
func TestProfilesBuildProxmoxExplainsTheManualPath(t *testing.T) {
	a, _ := newBuildApp(t)
	err := a.profilesBuild("base", "proxmox", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"packer", "pct template"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

func TestProfilesBuildUnknownProfile(t *testing.T) {
	a, _ := newBuildApp(t)
	if err := a.profilesBuild("ghost", "docker", nil); err == nil {
		t.Fatal("expected an error")
	}
}

// The default backend comes from the profile when none is named.
func TestProfilesBuildDefaultsToTheProfileBackend(t *testing.T) {
	a, f := newBuildApp(t)
	f.On("docker", "build").Stdout("")
	if err := os.MkdirAll("dist", 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll("dist") })

	if err := a.profilesBuild("base", "", nil); err != nil {
		t.Fatalf("profilesBuild: %v", err)
	}
	if !f.Ran("docker", "build") {
		t.Errorf("did not fall back to the profile's default backend: %s", f)
	}
}

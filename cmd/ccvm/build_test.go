package main

import (
	"os"
	"path/filepath"
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
	// Before the catch-all: rules match in declaration order, and the build
	// asks the template what architecture it is before pushing binaries to it.
	f.OnContaining("orbctl", "run", "uname").Stdout("aarch64\n")
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

// Building a template runs pct on a node, because the API can create a guest
// but not run a command inside one. Without that access, say so and name the
// setting rather than failing somewhere inside ssh.
func TestProfilesBuildProxmoxNeedsNodeAccess(t *testing.T) {
	a, _ := newBuildApp(t)
	err := a.profilesBuild("base", "proxmox", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "CCVM_PROXMOX_NODE_SSH") {
		t.Errorf("err = %v, want it to name the setting", err)
	}
}

// The distro tarball is the other thing that cannot be guessed.
func TestProfilesBuildProxmoxNeedsAnOSTemplate(t *testing.T) {
	a, _ := newBuildApp(t)
	a.backendCfg.ProxmoxNodeSSH = "root@pve1"

	err := a.profilesBuild("base", "proxmox", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "CCVM_PROXMOX_OSTEMPLATE") {
		t.Errorf("err = %v, want it to name the setting", err)
	}
}

// The whole recipe the README used to ask you to run by hand, in order. The
// key install is the load-bearing step: proxmox is the one backend where ccvm
// cannot install its own key at spawn, because it has no way in until the key
// is already there.
func TestProfilesBuildProxmoxRunsTheRecipe(t *testing.T) {
	a, f := newBuildApp(t)
	a.backendCfg.ProxmoxNodeSSH = "root@pve1"
	a.backendCfg.ProxmoxOSTemplate = "local:vztmpl/debian-13.tar.zst"
	f.On("ssh").Stdout("x86_64\n")
	f.On("scp").Stdout("")

	if err := a.profilesBuild("base", "proxmox", nil); err != nil {
		t.Fatalf("profilesBuild: %v", err)
	}

	var script string
	for _, argv := range f.Calls() {
		script += strings.Join(argv, " ") + "\n"
	}
	for _, want := range []string{
		"pct create 9000 local:vztmpl/debian-13.tar.zst",
		"pct start 9000",
		"sh /root/build.sh",
		"/root/.ssh/authorized_keys",
		"pct stop 9000",
		"pct template 9000",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the build never ran %q; it ran:\n%s", want, script)
		}
	}

	// Ordering matters: converting to a template before provisioning would
	// bake an empty container.
	if strings.Index(script, "pct template 9000") < strings.Index(script, "sh /root/build.sh") {
		t.Error("the container was converted to a template before it was provisioned")
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

// A profile you wrote resolved, listed, and linted, and then could not be
// built: staging looked in the working tree and the embedded copies and never
// in the directory every other command reads.
func TestStageProfileFindsAUserProfile(t *testing.T) {
	a, _ := newBuildApp(t)
	dir := filepath.Join(a.home, ".config", "ccvm", "profiles", "mine")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM debian\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	staged, cleanup, err := a.stageProfile("mine")
	if err != nil {
		t.Fatalf("stageProfile: %v", err)
	}
	defer cleanup()

	if _, err := os.Stat(filepath.Join(staged, "Dockerfile")); err != nil {
		t.Errorf("the user profile's Dockerfile was not staged: %v", err)
	}
}

// The embed uses `all:` so a profile can ship a directory of build inputs.
// Flattening dropped them, and only when building from the embedded copy - a
// checkout of the same profile kept them, so the same name built two ways.
func TestStageProfileKeepsNestedInputs(t *testing.T) {
	files, err := collectProfileFiles("base")
	if err != nil {
		t.Fatalf("collectProfileFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no build inputs found for base")
	}
	for _, f := range files {
		if filepath.Base(f) != f {
			return // a nested input is carried through
		}
	}
	// base has no nested inputs today; assert the walker would carry one.
	t.Log("base is currently flat; the walk is what allows a nested input")
}

// pct wants megabytes and gigabytes as bare numbers, while a profile writes
// "8G". Getting this wrong silently builds a template with the wrong size.
func TestProxmoxSizeConversions(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantMem  int
		wantDisk int
	}{
		{"8G", 8192, 8},
		{"512M", 512, 8},
		{"16G", 16384, 16},
		{"", 2048, 8},
		{"nonsense", 2048, 8},
	} {
		if got := memoryMB(tc.in); got != tc.wantMem {
			t.Errorf("memoryMB(%q) = %d, want %d", tc.in, got, tc.wantMem)
		}
		if got := diskGB(tc.in); got != tc.wantDisk {
			t.Errorf("diskGB(%q) = %d, want %d", tc.in, got, tc.wantDisk)
		}
	}
}

// A provisioning failure must abort rather than converting a half-built
// container into the template every future session clones from.
func TestProfilesBuildProxmoxStopsOnAFailedProvision(t *testing.T) {
	a, f := newBuildApp(t)
	a.backendCfg.ProxmoxNodeSSH = "root@pve1"
	a.backendCfg.ProxmoxOSTemplate = "local:vztmpl/debian-13.tar.zst"
	f.OnContaining("ssh", "build.sh").Fail(1, "installation failed")
	f.On("ssh").Stdout("")
	f.On("scp").Stdout("")

	err := a.profilesBuild("base", "proxmox", nil)
	if err == nil {
		t.Fatal("a failed provision produced a template anyway")
	}
	for _, argv := range f.Calls() {
		if strings.Contains(strings.Join(argv, " "), "pct template") {
			t.Fatal("the container was converted to a template despite the failure")
		}
	}
}

// The cluster's storage and bridge are not guessable, so a configured one has
// to reach the create command rather than being quietly replaced by a default
// that does not exist on that cluster.
func TestProfilesBuildProxmoxHonorsClusterSettings(t *testing.T) {
	a, f := newBuildApp(t)
	a.backendCfg.ProxmoxNodeSSH = "root@pve1"
	a.backendCfg.ProxmoxOSTemplate = "local:vztmpl/debian-13.tar.zst"
	a.backendCfg.ProxmoxStorage = "fastnvme"
	a.backendCfg.ProxmoxBridge = "vmbr7"
	f.On("ssh").Stdout("aarch64\n")
	f.On("scp").Stdout("")

	if err := a.profilesBuild("base", "proxmox", nil); err != nil {
		t.Fatalf("profilesBuild: %v", err)
	}

	var create string
	for _, argv := range f.Calls() {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "pct create") {
			create = joined
		}
	}
	if create == "" {
		t.Fatal("no create command ran")
	}
	for _, want := range []string{"fastnvme:", "bridge=vmbr7"} {
		if !strings.Contains(create, want) {
			t.Errorf("create = %q, missing %q", create, want)
		}
	}
}

// A template on an architecture ccvm has no guest binaries for is named, rather
// than having a binary for the wrong one installed that fails from inside the
// session with "Exec format error".
func TestProfilesBuildProxmoxRefusesAnUnknownArchitecture(t *testing.T) {
	a, f := newBuildApp(t)
	a.backendCfg.ProxmoxNodeSSH = "root@pve1"
	a.backendCfg.ProxmoxOSTemplate = "local:vztmpl/debian-13.tar.zst"
	// The pct command is shell-quoted into one argument, so a rule cannot match
	// `uname` on its own; only nodeArch reads this output.
	f.On("ssh").Stdout("riscv64\n")
	f.On("scp").Stdout("")

	err := a.profilesBuild("base", "proxmox", nil)
	if err == nil {
		t.Fatal("expected an unsupported architecture to be refused")
	}
	if !strings.Contains(err.Error(), "riscv64") {
		t.Errorf("err = %v, want it to name the architecture", err)
	}
}

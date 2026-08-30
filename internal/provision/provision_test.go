package provision_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/backendtest"
	"github.com/josegonzalez/ccvm/internal/profile"
	"github.com/josegonzalez/ccvm/internal/provision"
)

func src(files map[string]string) profile.FSSource {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return profile.FSSource{FS: m}
}

func names(layers []provision.Layer) []string {
	out := make([]string, len(layers))
	for i, l := range layers {
		out[i] = l.Name
	}
	return out
}

// Parents run before children, so a child's script can rely on what its parent
// installed.
func TestPlanRunsProfileChainRootFirst(t *testing.T) {
	s := src(map[string]string{
		"base/profile.toml": "",
		"base/provision.sh": "echo base\n",
		"go/profile.toml":   "extends = \"base\"\n",
		"go/provision.sh":   "echo go\n",
	})
	cfg, err := profile.Resolve("go", s)
	if err != nil {
		t.Fatal(err)
	}

	layers, err := provision.Plan(provision.Options{Profile: "go", Config: cfg, Source: s})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := names(layers)
	if len(got) < 2 || got[0] != "profile:base" || got[1] != "profile:go" {
		t.Errorf("layers = %v, want base before go", got)
	}
}

// A profile without a hook contributes nothing rather than failing.
func TestPlanSkipsProfilesWithoutAHook(t *testing.T) {
	s := src(map[string]string{
		"base/profile.toml": "",
		"go/profile.toml":   "extends = \"base\"\n",
		"go/provision.sh":   "echo go\n",
	})
	cfg, _ := profile.Resolve("go", s)

	layers, err := provision.Plan(provision.Options{Profile: "go", Config: cfg, Source: s})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := names(layers); len(got) != 1 || got[0] != "profile:go" {
		t.Errorf("layers = %v, want only the profile that has a hook", got)
	}
}

// The project's hook runs late so it can rely on everything else, and
// --install runs last so a one-off always wins.
func TestPlanOrdering(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	writeHook(t, filepath.Join(home, ".config", "ccvm"), "echo global\n")
	writeHook(t, filepath.Join(project, ".ccvm"), "echo project\n")

	s := src(map[string]string{
		"base/profile.toml": "[provision]\npackages = [\"jq\"]\n",
		"base/provision.sh": "echo base\n",
	})
	cfg, _ := profile.Resolve("base", s)

	layers, err := provision.Plan(provision.Options{
		Profile: "base", Config: cfg, Source: s,
		Home: home, Project: project, Install: "ripgrep",
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []string{"profile:base", "packages", "global", "project", "--install"}
	got := names(layers)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("layers = %v, want %v", got, want)
	}
}

func TestPlanEmptyWhenNothingToDo(t *testing.T) {
	s := src(map[string]string{"base/profile.toml": ""})
	cfg, _ := profile.Resolve("base", s)

	layers, err := provision.Plan(provision.Options{Profile: "base", Config: cfg, Source: s})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(layers) != 0 {
		t.Errorf("layers = %v, want none", names(layers))
	}
}

// An empty hook file is not a layer; running it would only add latency.
func TestPlanIgnoresEmptyHooks(t *testing.T) {
	s := src(map[string]string{
		"base/profile.toml": "",
		"base/provision.sh": "   \n\n",
	})
	cfg, _ := profile.Resolve("base", s)

	layers, _ := provision.Plan(provision.Options{Profile: "base", Config: cfg, Source: s})
	if len(layers) != 0 {
		t.Errorf("layers = %v, want an empty hook skipped", names(layers))
	}
}

func TestPlanInstallAcceptsCommasAndSpaces(t *testing.T) {
	s := src(map[string]string{"base/profile.toml": ""})
	cfg, _ := profile.Resolve("base", s)

	for _, spec := range []string{"jq,ripgrep", "jq ripgrep", " jq , ripgrep "} {
		layers, err := provision.Plan(provision.Options{
			Profile: "base", Config: cfg, Source: s, Install: spec,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(layers) != 1 {
			t.Fatalf("%q: layers = %v", spec, names(layers))
		}
		for _, pkg := range []string{"jq", "ripgrep"} {
			if !strings.Contains(layers[0].Script, pkg) {
				t.Errorf("%q: script missing %q", spec, pkg)
			}
		}
	}
}

// A package name is interpolated into a shell script, so it has to be quoted.
func TestInstallScriptQuotesPackageNames(t *testing.T) {
	s := src(map[string]string{"base/profile.toml": ""})
	cfg, _ := profile.Resolve("base", s)

	layers, err := provision.Plan(provision.Options{
		Profile: "base", Config: cfg, Source: s, Install: "jq;rm -rf /",
	})
	if err != nil {
		t.Fatal(err)
	}
	script := layers[0].Script
	if strings.Contains(script, "install -y -qq --no-install-recommends 'jq;rm' '-rf' '/'") {
		return // quoted as separate literal arguments, which is correct
	}
	if !strings.Contains(script, "'jq;rm'") {
		t.Errorf("script = %q, want package names quoted", script)
	}
}

// An image without a known package manager should say so rather than appearing
// to succeed.
func TestInstallScriptFailsLoudlyOnAnUnknownImage(t *testing.T) {
	s := src(map[string]string{"base/profile.toml": ""})
	cfg, _ := profile.Resolve("base", s)
	layers, _ := provision.Plan(provision.Options{
		Profile: "base", Config: cfg, Source: s, Install: "jq",
	})
	if !strings.Contains(layers[0].Script, "exit 1") {
		t.Errorf("script = %q, want a failure when no package manager is found", layers[0].Script)
	}
}

func TestRunExecutesLayersInOrder(t *testing.T) {
	b := backendtest.NewFake("docker")
	h := backend.Handle{Name: "cc-demo"}
	b.Seed(backend.Machine{Name: "cc-demo"}, nil)

	layers := []provision.Layer{
		{Name: "profile:base", Script: "echo base"},
		{Name: "--install", Script: "echo install"},
	}
	var progress []string
	if err := provision.Run(context.Background(), b, h, layers, func(n string) {
		progress = append(progress, n)
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if strings.Join(progress, ",") != "profile:base,--install" {
		t.Errorf("progress = %v, want declaration order", progress)
	}

	// Each layer is numbered, so the order is visible in the machine too.
	if _, ok := b.FileIn("cc-demo", "/etc/ccvm/provision.d/00-profile-base.sh"); !ok {
		t.Errorf("first layer not staged: %v", b.ExecCalls())
	}
	if _, ok := b.FileIn("cc-demo", "/etc/ccvm/provision.d/01-install.sh"); !ok {
		t.Error("second layer not staged")
	}
}

// A script the guest cannot execute fails in a way that looks like the script
// itself is wrong.
func TestRunMakesLayersExecutableAndRootOwned(t *testing.T) {
	b := backendtest.NewFake("docker")
	h := backend.Handle{Name: "cc-demo"}
	b.Seed(backend.Machine{Name: "cc-demo"}, nil)

	if err := provision.Run(context.Background(), b, h,
		[]provision.Layer{{Name: "one", Script: "true"}}, nil); err != nil {
		t.Fatal(err)
	}
	if !b.Ran("chmod", "0755", "/etc/ccvm/provision.d/00-one.sh") {
		t.Errorf("layer not made executable: %v", b.ExecCalls())
	}
	if !b.Ran("chown", "root:root", "/etc/ccvm/provision.d/00-one.sh") {
		t.Errorf("layer left owned by the host uid: %v", b.ExecCalls())
	}
}

// Dropping the user into a half-provisioned machine is worse than not starting:
// the failure is silent from inside and surfaces later, pointing nowhere.
func TestRunStopsAtTheFirstFailure(t *testing.T) {
	b := backendtest.NewFake("docker")
	h := backend.Handle{Name: "cc-demo"}
	b.Seed(backend.Machine{Name: "cc-demo"}, nil)
	b.ExecErrOn = "/etc/ccvm/provision.d/00-first.sh"

	var ran []string
	err := provision.Run(context.Background(), b, h, []provision.Layer{
		{Name: "first", Script: "false"},
		{Name: "second", Script: "true"},
	}, func(n string) { ran = append(ran, n) })

	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "first") {
		t.Errorf("err = %v, want it to name the failing layer", err)
	}
	if len(ran) != 1 {
		t.Errorf("ran = %v, want it to stop after the failure", ran)
	}
}

func TestRunNoLayersIsANoop(t *testing.T) {
	b := backendtest.NewFake("docker")
	h := backend.Handle{Name: "cc-demo"}
	b.Seed(backend.Machine{Name: "cc-demo"}, nil)

	if err := provision.Run(context.Background(), b, h, nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(b.ExecCalls()) != 0 {
		t.Errorf("ran commands with no layers: %v", b.ExecCalls())
	}
}

func writeHook(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provision.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

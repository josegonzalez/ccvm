package profile_test

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/josegonzalez/ccvm/internal/profile"
)

func srcFS(files map[string]string) profile.FSSource {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name+"/profile.toml"] = &fstest.MapFile{Data: []byte(body)}
	}
	return profile.FSSource{FS: m}
}

func TestResolveAppliesBuiltinDefaults(t *testing.T) {
	src := srcFS(map[string]string{
		"base": "[backend.docker]\nimage = \"ccvm/base\"\n",
	})
	c, err := profile.Resolve("base", src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Defaults.Backend != "docker" {
		t.Errorf("Backend = %q, want the built-in default", c.Defaults.Backend)
	}
	if c.Resources.CPUs != 2 || c.Resources.Memory != "4G" {
		t.Errorf("Resources = %+v, want built-in defaults", c.Resources)
	}
}

func TestResolveChildOverridesParent(t *testing.T) {
	src := srcFS(map[string]string{
		"base": `
[backend.docker]
image = "ccvm/base"
[resources]
cpus = 2
memory = "4G"
`,
		"go": `
extends = "base"
[resources]
memory = "8G"
`,
	})
	c, err := profile.Resolve("go", src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Resources.Memory != "8G" {
		t.Errorf("Memory = %q, want the child's 8G", c.Resources.Memory)
	}
	// Inherited, not reset to the built-in.
	if c.Resources.CPUs != 2 {
		t.Errorf("CPUs = %d, want 2 inherited from base", c.Resources.CPUs)
	}
	if c.Backend["docker"].Image != "ccvm/base" {
		t.Errorf("image = %q, want it inherited", c.Backend["docker"].Image)
	}
}

// Packages accumulate root-first down the chain.
func TestResolveAccumulatesPackagesInInheritanceOrder(t *testing.T) {
	src := srcFS(map[string]string{
		"base": "[provision]\npackages = [\"git\", \"tmux\"]\n",
		"lang": "extends = \"base\"\n[provision]\npackages = [\"make\"]\n",
		"go":   "extends = \"lang\"\n[provision]\npackages = [\"delve\"]\n",
	})
	c, err := profile.Resolve("go", src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"git", "tmux", "make", "delve"}
	if !reflect.DeepEqual(c.Provision.Packages, want) {
		t.Errorf("Packages = %v, want %v", c.Provision.Packages, want)
	}
}

func TestResolveDetectsCycle(t *testing.T) {
	src := srcFS(map[string]string{
		"a": "extends = \"b\"\n",
		"b": "extends = \"c\"\n",
		"c": "extends = \"a\"\n",
	})
	_, err := profile.Resolve("a", src)
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("err = %v, want it to say cycle", err)
	}
	// The chain makes the cycle actionable rather than just reported.
	if !strings.Contains(err.Error(), "a -> b -> c -> a") {
		t.Errorf("err = %v, want it to show the chain", err)
	}
}

func TestResolveDetectsSelfCycle(t *testing.T) {
	src := srcFS(map[string]string{"a": "extends = \"a\"\n"})
	if _, err := profile.Resolve("a", src); err == nil {
		t.Fatal("expected a cycle error for a self-extending profile")
	}
}

func TestResolveMissingParentNamesBothProfiles(t *testing.T) {
	src := srcFS(map[string]string{"go": "extends = \"nope\"\n"})
	_, err := profile.Resolve("go", src)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "go") || !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v, want it to name the child and the missing parent", err)
	}
}

func TestResolveMissingProfileIsNotFound(t *testing.T) {
	src := srcFS(map[string]string{"base": ""})
	_, err := profile.Resolve("ghost", src)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("err = %v, want it to name the profile", err)
	}
}

func TestResolveClearsExtendsOnResult(t *testing.T) {
	src := srcFS(map[string]string{
		"base": "",
		"go":   "extends = \"base\"\n",
	})
	c, err := profile.Resolve("go", src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Extends != "" {
		t.Errorf("Extends = %q, want cleared on a fully resolved config", c.Extends)
	}
}

// Layers 4 and 5: the user's global config, then the project's.
func TestApplyOverlaysInOrder(t *testing.T) {
	base := &profile.Config{
		Resources: profile.Resources{CPUs: 2, Memory: "4G"},
		Defaults:  profile.Defaults{CodeMode: "mount"},
	}
	global, err := profile.Parse("global", []byte("[resources]\ncpus = 8\nmemory = \"8G\"\n"), profile.ScopeOwned)
	if err != nil {
		t.Fatal(err)
	}
	project, err := profile.Parse("proj", []byte("[resources]\nmemory = \"16G\"\n"), profile.ScopeProject)
	if err != nil {
		t.Fatal(err)
	}

	got := profile.Apply(base,
		profile.Overlay{Name: "global", Config: global},
		profile.Overlay{Name: "project", Config: project},
	)

	if got.Resources.Memory != "16G" {
		t.Errorf("Memory = %q, want the project's 16G to win", got.Resources.Memory)
	}
	if got.Resources.CPUs != 8 {
		t.Errorf("CPUs = %d, want the global 8 to survive", got.Resources.CPUs)
	}
	// Apply must not mutate its input; callers reuse resolved profiles.
	if base.Resources.Memory != "4G" {
		t.Errorf("Apply mutated base: %+v", base.Resources)
	}
}

func TestBackendEmpty(t *testing.T) {
	tests := []struct {
		name string
		b    profile.Backend
		want bool
	}{
		{"zero", profile.Backend{}, true},
		{"image", profile.Backend{Image: "x"}, false},
		{"orbstack template", profile.Backend{Template: "x"}, false},
		{"lxc", profile.Backend{LXCTemplate: 9000}, false},
		{"vm only", profile.Backend{VMTemplate: 9100}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.Empty(); got != tt.want {
				t.Errorf("Empty() = %v, want %v", got, tt.want)
			}
		})
	}
}

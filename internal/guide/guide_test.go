package guide_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/josegonzalez/ccvm/internal/guide"
	"github.com/josegonzalez/ccvm/internal/profile"
)

func src(files map[string]string) profile.FSSource {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return profile.FSSource{FS: m}
}

func names(layers []guide.Layer) []string {
	out := make([]string, len(layers))
	for i, l := range layers {
		out[i] = l.Name
	}
	return out
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ccvm's own guide is always first: it is how a model that has never seen ccvm
// learns that ccvm-done ends the machine.
func TestPlanAlwaysLeadsWithTheCcvmGuide(t *testing.T) {
	s := src(map[string]string{"base/profile.toml": ""})

	layers, err := guide.Plan(guide.Options{Profile: "base", Source: s})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(layers) == 0 || layers[0].Name != "ccvm" {
		t.Fatalf("layers = %v, want ccvm first", names(layers))
	}
	if !strings.Contains(layers[0].Body, "ccvm-done") {
		t.Error("the ccvm layer does not mention ccvm-done")
	}
}

// Parents first, then the user's file, then the project's, then the one-off.
func TestPlanLayerOrder(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	oneOff := filepath.Join(t.TempDir(), "extra.md")

	write(t, filepath.Join(home, ".config", "ccvm"), "CLAUDE.md", "global text\n")
	write(t, filepath.Join(project, ".ccvm"), "CLAUDE.md", "project text\n")
	if err := os.WriteFile(oneOff, []byte("one-off text\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := src(map[string]string{
		"base/profile.toml":  "",
		"base/CLAUDE.md":     "base text\n",
		"child/profile.toml": "extends = \"base\"\n",
		"child/CLAUDE.md":    "child text\n",
	})

	layers, err := guide.Plan(guide.Options{
		Profile: "child", Source: s, Home: home, Project: project, File: oneOff,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	want := []string{"ccvm", "profile:base", "profile:child", "global", "project", "--claude-md"}
	if got := names(layers); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("layers = %v, want %v", got, want)
	}
}

// Most people have none of these files, so absence is normal rather than an
// error. A blank one is a leftover, not a contribution.
func TestPlanSkipsMissingAndBlankLayers(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".config", "ccvm"), "CLAUDE.md", "   \n\n")

	s := src(map[string]string{"base/profile.toml": ""})

	layers, err := guide.Plan(guide.Options{
		Profile: "base", Source: s, Home: home, Project: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := names(layers); strings.Join(got, ",") != "ccvm" {
		t.Errorf("layers = %v, want only ccvm", got)
	}
}

// The user named this file, so failing to read it is worth stopping for.
// Silently starting a session without the guidance they asked for is worse.
func TestPlanFailsOnAnUnreadableClaudeMD(t *testing.T) {
	s := src(map[string]string{"base/profile.toml": ""})

	_, err := guide.Plan(guide.Options{
		Profile: "base", Source: s,
		File: filepath.Join(t.TempDir(), "does-not-exist.md"),
	})
	if err == nil {
		t.Fatal("expected an error for a --claude-md that cannot be read")
	}
	if !strings.Contains(err.Error(), "claude-md") {
		t.Errorf("err = %v, want it to name the flag", err)
	}
}

// The markers are what make a composed file debuggable from inside a machine.
func TestRenderMarksEachLayer(t *testing.T) {
	got := guide.Render([]guide.Layer{
		{Name: "ccvm", Body: "first"},
		{Name: "project", Body: "second"},
	})

	for _, want := range []string{"<!-- ccvm: ccvm -->", "<!-- ccvm: project -->", "first", "second"} {
		if !strings.Contains(got, want) {
			t.Errorf("render = %q, missing %q", got, want)
		}
	}
	if strings.Index(got, "first") > strings.Index(got, "second") {
		t.Error("layers rendered out of order")
	}
}

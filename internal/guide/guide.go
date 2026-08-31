// Package guide composes the CLAUDE.md a session machine starts with.
//
// The file is assembled on the host and written into the guest at spawn, rather
// than baked into an image, so every backend gets the same content and an
// already-built image does not need rebuilding to gain it.
package guide

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/josegonzalez/ccvm/guest"
	"github.com/josegonzalez/ccvm/internal/profile"
)

// FileName is the per-profile and per-project file this package composes.
const FileName = "CLAUDE.md"

// GuestFile is Claude Code's user-level memory inside the machine. A project's
// own CLAUDE.md is read separately, from the working directory, and needs no
// help from ccvm.
const GuestFile = "/root/.claude/CLAUDE.md"

// Layer is one contributed section, in the order it appears in the file.
type Layer struct {
	// Name identifies where the section came from, and is written into the
	// composed file as a marker.
	Name string
	// Body is the section's text.
	Body string
}

// Options describe what to compose.
type Options struct {
	Profile string
	Source  profile.Source
	Home    string
	Project string
	// File is the --claude-md path, appended last so a one-off wins.
	File string
}

// Plan assembles the layers, in order.
//
// ccvm's own guide is always first and cannot be switched off: it is how a model
// that has never seen ccvm learns that ccvm-done ends the machine. Later layers
// refine it, the same way provisioning layers refine each other.
//
//  1. ccvm's guide
//  2. CLAUDE.md from the profile chain, parents first
//  3. ~/.config/ccvm/CLAUDE.md
//  4. <project>/.ccvm/CLAUDE.md
//  5. --claude-md
//
// A missing file at any layer is not an error, since most people have none. A
// --claude-md that cannot be read is, because the user named it.
func Plan(o Options) ([]Layer, error) {
	layers := []Layer{{Name: "ccvm", Body: guest.Guide}}

	names, err := profile.Names(o.Profile, o.Source)
	if err != nil {
		return nil, err
	}
	if scripts, ok := o.Source.(profile.ScriptSource); ok {
		for _, name := range names {
			data, err := scripts.Script(name, FileName)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("read %s for profile %q: %w", FileName, name, err)
			}
			layers = appendIfContent(layers, "profile:"+name, string(data))
		}
	}

	if o.Home != "" {
		layers = appendFile(layers, "global", filepath.Join(o.Home, ".config", "ccvm", FileName))
	}
	if o.Project != "" {
		layers = appendFile(layers, "project", filepath.Join(o.Project, ".ccvm", FileName))
	}

	if o.File != "" {
		data, err := os.ReadFile(o.File)
		if err != nil {
			// Named explicitly, so dropping it silently would be worse than
			// refusing to start.
			return nil, fmt.Errorf("read --claude-md: %w", err)
		}
		layers = appendIfContent(layers, "--claude-md", string(data))
	}

	return layers, nil
}

// appendFile adds an optional layer. An unreadable file is treated as absent:
// these paths are conventions most people never create.
func appendFile(layers []Layer, name, path string) []Layer {
	data, err := os.ReadFile(path)
	if err != nil {
		return layers
	}
	return appendIfContent(layers, name, string(data))
}

func appendIfContent(layers []Layer, name, body string) []Layer {
	if strings.TrimSpace(body) == "" {
		return layers
	}
	return append(layers, Layer{Name: name, Body: body})
}

// Render joins the layers into the file written to the guest.
//
// Each section is marked with an HTML comment so that, from inside a machine, it
// is possible to tell which layer contributed what. Markdown hides the marker
// when rendered, and it carries no heading level that would disturb the
// document's own structure.
func Render(layers []Layer) string {
	var b strings.Builder
	for i, l := range layers {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "<!-- ccvm: %s -->\n", l.Name)
		b.WriteString(strings.TrimRight(l.Body, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

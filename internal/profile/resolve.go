package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// maxDepth bounds the extends chain. Cycles are detected exactly, so this is a
// backstop against a pathologically deep hierarchy rather than the real guard.
const maxDepth = 32

// Source loads a profile by name.
type Source interface {
	Load(name string) (*Config, error)
}

// ErrNotFound is returned by a Source for an unknown profile name, so Resolve
// can report a missing parent differently from an unreadable one.
var ErrNotFound = errors.New("profile not found")

// FSSource loads <name>/profile.toml from a filesystem, which is how the
// shipped profiles/ directory is read. Taking an fs.FS keeps resolution
// testable without touching disk.
type FSSource struct{ FS fs.FS }

func (s FSSource) Load(name string) (*Config, error) {
	file := path.Join(name, "profile.toml")
	data, err := fs.ReadFile(s.FS, file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return nil, err
	}
	return Parse(file, data, ScopeOwned)
}

// Builtin is layer 1: what a profile gets before any file is read. These are
// the values that make `ccvm up` work in a repo with no configuration at all.
func Builtin() *Config {
	return &Config{
		Defaults: Defaults{
			Backend:  "docker",
			CodeMode: "mount",
			TTL:      "12h",
		},
		Resources: Resources{CPUs: 2, Memory: "4G", Disk: "16G"},
	}
}

// Resolve produces the effective config for a named profile: built-in defaults,
// then the extends chain from the root down, then the profile itself.
//
// Parents are merged before children so a child's values win, and packages
// accumulate in inheritance order.
func Resolve(name string, src Source) (*Config, error) {
	chain, err := chainOf(name, src)
	if err != nil {
		return nil, err
	}

	out := Builtin()
	for _, c := range chain {
		Merge(out, c)
	}
	// The result describes a fully resolved profile, so it no longer extends
	// anything; leaving it set invites double-resolution.
	out.Extends = ""
	return out, nil
}

// chainOf returns the configs for name and its ancestors, root first.
func chainOf(name string, src Source) ([]*Config, error) {
	var (
		chain []*Config
		seen  = map[string]bool{}
		order []string
	)

	for cur := name; cur != ""; {
		if seen[cur] {
			return nil, fmt.Errorf("profile %q: extends cycle: %s",
				name, strings.Join(append(order, cur), " -> "))
		}
		seen[cur] = true
		order = append(order, cur)

		if len(order) > maxDepth {
			return nil, fmt.Errorf("profile %q: extends chain deeper than %d", name, maxDepth)
		}

		c, err := src.Load(cur)
		if err != nil {
			if errors.Is(err, ErrNotFound) && cur != name {
				return nil, fmt.Errorf("profile %q extends %q, which does not exist", order[len(order)-2], cur)
			}
			return nil, err
		}

		// Prepend: we walk child to root, but merge root to child.
		chain = append([]*Config{c}, chain...)
		cur = c.Extends
	}
	return chain, nil
}

// Overlay is a config layer applied after profile resolution.
type Overlay struct {
	// Name is used in error messages, so it should be the file path.
	Name   string
	Config *Config
}

// Apply layers overlays onto a resolved config, in order. This is layers 4 and
// 5 of the precedence chain: the user's global config, then the project's.
// Flags are layer 6 and are applied by the caller, since only it knows which
// were actually set.
func Apply(base *Config, overlays ...Overlay) *Config {
	out := base.Clone()
	for _, o := range overlays {
		Merge(out, o.Config)
	}
	return out
}

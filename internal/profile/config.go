// Package profile resolves ccvm profiles: the TOML files that say which image a
// session uses, how much machine it gets, and what to install.
//
// The file is data, never executed. That is a property of TOML rather than
// something a parser has to enforce, which is why this package replaced an
// earlier shell-sourced format.
package profile

import "maps"

// Config is a single profile layer, exactly as written in one profile.toml.
//
// A zero value means "unset" for every field, which is what lets Merge tell
// "not specified in this layer" from "specified as empty". Nothing in the
// schema has a meaningful zero (no machine gets 0 CPUs), so this stays honest
// without pointer fields everywhere.
type Config struct {
	Description string             `toml:"description"`
	Extends     string             `toml:"extends"`
	Backend     map[string]Backend `toml:"backend"`
	Defaults    Defaults           `toml:"defaults"`
	Resources   Resources          `toml:"resources"`
	Provision   Provision          `toml:"provision"`
	Env         map[string]string  `toml:"env"`
}

// Backend is the per-backend image or template a profile resolves to. Which
// field matters depends on the backend: docker and k8s use Image, orbstack uses
// Template, proxmox uses the two numeric template ids.
type Backend struct {
	Image       string `toml:"image"`
	Template    string `toml:"template"`
	LXCTemplate int    `toml:"lxc_template"`
	VMTemplate  int    `toml:"vm_template"`
}

// Empty reports whether the backend table carries no usable image reference.
// Preflight uses this: a declared but empty [backend.k8s] is as unusable as an
// absent one, and should fail the same way.
func (b Backend) Empty() bool {
	return b.Image == "" && b.Template == "" && b.LXCTemplate == 0 && b.VMTemplate == 0
}

type Defaults struct {
	Backend  string `toml:"backend"`
	CodeMode string `toml:"code_mode"`
	TTL      string `toml:"ttl"`
}

type Resources struct {
	CPUs   int    `toml:"cpus"`
	Memory string `toml:"memory"`
	Disk   string `toml:"disk"`
}

type Provision struct {
	// Pre runs before packages are installed, for anything the install itself
	// depends on: an apt source, a proxy, a mirror.
	Pre []string `toml:"pre"`
	// PreReplace opts a layer out of accumulation, like PackagesReplace.
	PreReplace bool `toml:"pre_replace"`

	// Packages accumulate down the extends chain rather than replacing, since
	// layering packages onto a parent is the main reason to inherit at all.
	Packages []string `toml:"packages"`
	// PackagesReplace opts a layer out of that accumulation, for a profile that
	// genuinely needs to drop what its parent installed.
	PackagesReplace bool `toml:"packages_replace"`

	// Post runs after packages, while the project may still be absent: whether
	// /work is populated before Setup depends on the code mode, so anything
	// touching the project belongs in Setup instead.
	Post []string `toml:"post"`
	// PostReplace opts a layer out of accumulation.
	PostReplace bool `toml:"post_replace"`

	// Setup runs after the code is in place, and is the only phase where the
	// project is present under every code mode.
	Setup []string `toml:"setup"`
	// SetupReplace opts a layer out of accumulation.
	SetupReplace bool `toml:"setup_replace"`
}

// Clone returns a deep copy. Merge mutates its destination, and resolution
// reuses parsed parents across profiles, so sharing maps or slices between them
// would let one resolution corrupt another.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := *c
	if c.Backend != nil {
		out.Backend = make(map[string]Backend, len(c.Backend))
		maps.Copy(out.Backend, c.Backend)
	}
	if c.Env != nil {
		out.Env = make(map[string]string, len(c.Env))
		maps.Copy(out.Env, c.Env)
	}
	// Every slice has to be copied, or one resolution's commands leak into
	// another's the first time append reuses a shared backing array.
	out.Provision.Pre = cloneSlice(c.Provision.Pre)
	out.Provision.Packages = cloneSlice(c.Provision.Packages)
	out.Provision.Post = cloneSlice(c.Provision.Post)
	out.Provision.Setup = cloneSlice(c.Provision.Setup)
	return &out
}

func cloneSlice(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}

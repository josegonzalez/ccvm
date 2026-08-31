// Package provision installs what a session needs, inside the machine, before
// Claude starts.
//
// Every layer runs in the guest, which is the counterpart to the host-side
// restriction in internal/profile: a repository may not choose the image it
// runs on, but inside a machine that is already disposable, arbitrary code is
// the whole point.
package provision

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/profile"
)

// HookName is the per-spawn hook a profile or project supplies.
//
// Distinct from a profile's build.sh, which bakes an image once. Running
// build-time work on every spawn is slow at best, and on a machine that already
// has everything it is pure waste.
const HookName = "provision.sh"

// guestDir is where layers are staged inside the machine. Not /tmp: on some
// backends that is a per-boot tmpfs the file transfer writes into a different
// view of, reporting success and leaving nothing behind.
const guestDir = "/etc/ccvm/provision.d"

// Layer is one step, in the order it runs.
type Layer struct {
	// Name identifies the layer in progress output and errors.
	Name string
	// Script is the shell to run in the guest.
	Script string
	// Command is the single command this layer came from, empty when the layer
	// is a whole provision.sh. A failure quotes it, so the error names what
	// actually failed rather than only the phase it was in.
	Command string
}

// Options describe what to build a plan from.
type Options struct {
	Profile string
	Config  *profile.Config
	Source  profile.Source
	Home    string
	Project string
	// Install is the --install value: packages named on the command line.
	Install string
	// Pre, Post, and Setup are commands named on the command line, each already
	// split into one entry per --pre-install/--post-install/--setup occurrence.
	Pre   []string
	Post  []string
	Setup []string
}

// Plans are the layers split by when they can run. Setup is separate because it
// runs after the code is materialized, which the caller does between the two.
type Plans struct {
	// BeforeCode is everything up to and including the post phase.
	BeforeCode []Layer
	// Setup runs once the project is in place, and is the only phase where that
	// is true regardless of --code mode.
	Setup []Layer
}

// Plan builds the ordered layers, split into what runs before the code is
// materialized and what runs after.
//
// Three command phases sit around two fixed points, package installation and
// the code landing:
//
//	pre       profile chain provision.sh, [provision].pre, --pre-install
//	packages  [provision].packages, --install
//	post      [provision].post, global and project provision.sh, --post-install
//	          -- the caller materializes the code here --
//	setup     [provision].setup, --setup
//
// Phase decides when a command runs relative to those two points; layer decides
// precedence within a phase. So a project's [provision].pre runs before the
// user's global hook, which looks like an inversion and is not: the project
// asked for "before packages", which is the whole point of the key.
//
// Anything touching the project belongs in setup. Whether /work is populated
// any earlier depends on the code mode - a bind mount is there from create
// time, a git clone or rsync is not - so a command that reads it in post works
// under the default and breaks under --code git.
func Plan(o Options) (Plans, error) {
	var p Plans

	names, err := profile.Names(o.Profile, o.Source)
	if err != nil {
		return Plans{}, err
	}
	scripts, _ := o.Source.(profile.ScriptSource)
	for _, name := range names {
		if scripts == nil {
			break
		}
		data, err := scripts.Script(name, HookName)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return Plans{}, fmt.Errorf("read %s for profile %q: %w", HookName, name, err)
		}
		if strings.TrimSpace(string(data)) == "" {
			continue
		}
		p.BeforeCode = append(p.BeforeCode, Layer{Name: "profile:" + name, Script: string(data)})
	}

	cfg := o.Config
	if cfg != nil {
		p.BeforeCode = append(p.BeforeCode, commandLayers("pre", cfg.Provision.Pre)...)
	}
	p.BeforeCode = append(p.BeforeCode, commandLayers("--pre-install", o.Pre)...)

	if cfg != nil && len(cfg.Provision.Packages) > 0 {
		p.BeforeCode = append(p.BeforeCode, Layer{
			Name:   "packages",
			Script: installScript(cfg.Provision.Packages),
		})
	}
	// --install sits with the packages rather than last, so everything after it
	// can rely on those packages being present.
	if pkgs := splitPackages(o.Install); len(pkgs) > 0 {
		p.BeforeCode = append(p.BeforeCode, Layer{Name: "--install", Script: installScript(pkgs)})
	}

	if cfg != nil {
		p.BeforeCode = append(p.BeforeCode, commandLayers("post", cfg.Provision.Post)...)
	}

	if o.Home != "" {
		path := filepath.Join(o.Home, ".config", "ccvm", HookName)
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
			p.BeforeCode = append(p.BeforeCode, Layer{Name: "global", Script: string(data)})
		}
	}

	if o.Project != "" {
		path := filepath.Join(o.Project, ".ccvm", HookName)
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
			p.BeforeCode = append(p.BeforeCode, Layer{Name: "project", Script: string(data)})
		}
	}

	p.BeforeCode = append(p.BeforeCode, commandLayers("--post-install", o.Post)...)

	if cfg != nil {
		p.Setup = append(p.Setup, commandLayers("setup", cfg.Provision.Setup)...)
	}
	p.Setup = append(p.Setup, commandLayers("--setup", o.Setup)...)

	return p, nil
}

// commandLayers gives each command its own layer, so a failure names the
// command rather than the phase it happened to be in.
func commandLayers(phase string, cmds []string) []Layer {
	var out []Layer
	for i, c := range cmds {
		if strings.TrimSpace(c) == "" {
			continue
		}
		out = append(out, Layer{
			Name:    fmt.Sprintf("%s[%d]", phase, i),
			Script:  c,
			Command: c,
		})
	}
	return out
}

// splitPackages accepts commas or spaces, since both are natural to type.
func splitPackages(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// installScript renders a package install that works on the distributions the
// profiles use, and says so rather than failing obscurely on one they do not.
func installScript(pkgs []string) string {
	quoted := make([]string, len(pkgs))
	for i, p := range pkgs {
		quoted[i] = shellQuote(p)
	}
	list := strings.Join(quoted, " ")

	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("if command -v apt-get >/dev/null 2>&1; then\n")
	b.WriteString("  export DEBIAN_FRONTEND=noninteractive\n")
	b.WriteString("  apt-get update -qq\n")
	b.WriteString("  apt-get install -y -qq --no-install-recommends " + list + "\n")
	b.WriteString("elif command -v apk >/dev/null 2>&1; then\n")
	b.WriteString("  apk add --no-cache " + list + "\n")
	b.WriteString("elif command -v dnf >/dev/null 2>&1; then\n")
	b.WriteString("  dnf install -y " + list + "\n")
	b.WriteString("else\n")
	b.WriteString("  echo 'no supported package manager found in this image' >&2\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Runner is what provisioning needs from a backend.
type Runner interface {
	Exec(ctx context.Context, h backend.Handle, argv ...string) ([]byte, error)
	Push(ctx context.Context, h backend.Handle, src, dst string) error
}

// Run executes the layers in order.
//
// A failing layer stops everything. Dropping the user into a half-provisioned
// machine is worse than not starting: the failure is silent from inside, and
// the session goes wrong later for reasons that no longer point at the cause.
func Run(ctx context.Context, r Runner, h backend.Handle, layers []Layer, progress func(string)) error {
	return RunFrom(ctx, r, h, layers, 0, progress)
}

// RunFrom is Run with a starting number for the staged files.
//
// The setup phase runs in a second call, after the code is materialized. Both
// calls stage into the same directory, so without an offset the second would
// number from zero again and overwrite the first's files. Those files are the
// record of what ran, and half a record is worse than none when a spawn fails.
func RunFrom(ctx context.Context, r Runner, h backend.Handle, layers []Layer, first int, progress func(string)) error {
	if len(layers) == 0 {
		return nil
	}
	if _, err := r.Exec(ctx, h, "mkdir", "-p", guestDir); err != nil {
		return fmt.Errorf("create %s: %w", guestDir, err)
	}

	for i, l := range layers {
		if progress != nil {
			progress(l.Name)
		}
		dst := fmt.Sprintf("%s/%02d-%s.sh", guestDir, first+i, sanitize(l.Name))
		if err := push(ctx, r, h, l.Script, dst); err != nil {
			return fmt.Errorf("stage %s: %w", l.Name, err)
		}
		if _, err := r.Exec(ctx, h, "sh", "-e", dst); err != nil {
			if l.Command != "" {
				return fmt.Errorf("%s failed: %s: %w", l.Name, l.Command, err)
			}
			return fmt.Errorf("%s failed: %w", l.Name, err)
		}
	}
	return nil
}

func push(ctx context.Context, r Runner, h backend.Handle, content, dst string) error {
	tmp, err := os.CreateTemp("", "ccvm-provision-*.sh")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	// Closed before the file is read back, so a failure here means the content
	// may be short and has to surface rather than be dropped.
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := r.Push(ctx, h, path, dst); err != nil {
		return err
	}
	// Transfer carries the host's uid, and a script the guest cannot execute
	// fails in a way that looks like the script itself is wrong.
	if _, err := r.Exec(ctx, h, "chown", "root:root", dst); err != nil {
		return err
	}
	_, err = r.Exec(ctx, h, "chmod", "0755", dst)
	return err
}

// sanitize makes a layer name safe as a filename.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

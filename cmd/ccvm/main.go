// Command ccvm runs each Claude Code session in its own disposable machine.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/profile"
	"github.com/josegonzalez/ccvm/internal/run"
	"github.com/josegonzalez/ccvm/internal/sshcfg"
	"github.com/josegonzalez/ccvm/internal/sshkey"
	"github.com/josegonzalez/ccvm/profiles"
)

// version is overridden at build time with -ldflags.
var version = "dev"

type command struct {
	name    string
	summary string
	run     func(*app, []string) error
}

var commands = []command{
	{"up", "start a session in a fresh machine", cmdUp},
	{"ls", "list ccvm machines", cmdLs},
	{"ssh", "open a shell in a machine", cmdSSH},
	{"attach", "reattach to a machine's Claude session", cmdAttach},
	{"keep", "exempt a machine from reaping", cmdKeep},
	{"rm", "destroy machines", cmdRm},
	{"gc", "reap finished machines, or schedule the reaper", cmdGC},
	{"profiles", "list, check, or build profiles", cmdProfiles},
	{"creds", "check or import the Claude credential sessions use", cmdCreds},
	{"doctor", "check whether a machine could be created", cmdDoctor},
	{"version", "print the version", cmdVersion},
}

func main() {
	// A cancelled context has to unwind the rollback stack rather than orphan a
	// half-created machine, so Ctrl-C is handled rather than left to the
	// default SIGINT behaviour.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := realMain(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "ccvm:", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

func realMain(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(stdout)
		if len(args) == 0 {
			return errUsage
		}
		return nil
	}

	name := args[0]
	rest := args[1:]

	// A global --verbose before or after the subcommand means the same thing.
	verbose := false
	filtered := rest[:0]
	for _, a := range rest {
		if a == "-v" || a == "--verbose" {
			verbose = true
			continue
		}
		filtered = append(filtered, a)
	}
	rest = filtered

	for _, c := range commands {
		if c.name != name {
			continue
		}
		a, err := newApp(ctx, verbose, stdout, stderr)
		if err != nil {
			return err
		}
		return c.run(a, rest)
	}

	fmt.Fprintf(stderr, "ccvm: unknown command %q\n\n", name)
	usage(stderr)
	return errUsage
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "ccvm runs each Claude Code session in its own disposable machine.")
	fmt.Fprintln(w, "\nUsage:\n  ccvm <command> [flags]\n\nCommands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w, "\nRun `ccvm <command> -h` for a command's flags.")
	fmt.Fprintln(w, "Global:\n  -v, --verbose   log every backend command verbatim")
}

// app is the shared state a command needs.
type app struct {
	ctx      context.Context
	home     string
	verbose  bool
	out      io.Writer
	err      io.Writer
	runner   run.Execer
	profiles profile.Source
	ssh      sshcfg.File
	sshKey   sshkey.Pair
	backends map[string]backend.Backend
}

func newApp(ctx context.Context, verbose bool, stdout, stderr io.Writer) (*app, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory: %w", err)
	}

	// --verbose routes through the Execer, which is the single place every
	// backend command passes through. That is what keeps the log complete
	// without each backend having to remember to write one.
	var logTo io.Writer
	if verbose {
		logTo = stderr
	}
	runner := run.New(logTo)

	// Every registered backend is built up front so `ccvm ls` and `ccvm doctor`
	// can span all of them. Construction is cheap and does not contact
	// anything; reachability is Preflight's job.
	// ccvm mints its own key rather than reusing one of the developer's:
	// disposable machines should not share a credential with anything real.
	key := sshkey.Default(home)
	created, err := key.Ensure()
	if err != nil {
		return nil, err
	}
	if created {
		fmt.Fprintf(stderr, "ccvm: created %s for reaching session machines\n", key.Private)
	}

	backends := map[string]backend.Backend{}
	cfg := backendConfig()
	for _, name := range backend.Names() {
		b, err := backend.New(name, runner, cfg)
		if err != nil {
			return nil, err
		}
		backends[name] = b
	}

	return &app{
		ctx:      ctx,
		home:     home,
		verbose:  verbose,
		out:      stdout,
		err:      stderr,
		runner:   runner,
		profiles: profile.DefaultSource(home, profiles.FS()),
		ssh:      sshcfg.Default(home),
		sshKey:   key,
		backends: backends,
	}, nil
}

// backendConfig reads the settings backends need beyond the profile. Proxmox
// and kubernetes point at a control plane; the local backends need nothing.
func backendConfig() backend.Config {
	return backend.Config{
		ProxmoxURL:      os.Getenv("CCVM_PROXMOX_URL"),
		ProxmoxNode:     os.Getenv("CCVM_PROXMOX_NODE"),
		ProxmoxTokenID:  os.Getenv("CCVM_PROXMOX_TOKEN_ID"),
		ProxmoxSecret:   os.Getenv("CCVM_PROXMOX_SECRET"),
		ProxmoxStorage:  os.Getenv("CCVM_PROXMOX_STORAGE"),
		ProxmoxInsecure: os.Getenv("CCVM_PROXMOX_INSECURE") != "",
		KubeNamespace:   envOrDefault("CCVM_KUBE_NAMESPACE", "default"),
		KubeContext:     os.Getenv("CCVM_KUBE_CONTEXT"),
	}
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// backendNames returns the configured backends in a stable order.
func (a *app) backendNames() []string {
	names := make([]string, 0, len(a.backends))
	for n := range a.backends {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (a *app) backend(name string) (backend.Backend, error) {
	b, ok := a.backends[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q; available: %s",
			name, strings.Join(a.backendNames(), ", "))
	}
	return b, nil
}

func cmdVersion(a *app, _ []string) error {
	fmt.Fprintln(a.out, version)
	return nil
}

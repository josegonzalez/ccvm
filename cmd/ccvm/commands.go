package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/creds"
	"github.com/josegonzalez/ccvm/internal/profile"
	"github.com/josegonzalez/ccvm/internal/session"
	"golang.org/x/sync/errgroup"
)

func newFlags(name string, a *app) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.err)
	return fs
}

// ---------------------------------------------------------------- ls

func cmdLs(a *app, args []string) error {
	fs := newFlags("ls", a)
	asJSON := fs.Bool("json", false, "emit machine records as JSON")
	all := fs.Bool("all", false, "include machines missing ccvm's ownership marker")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	machines, err := a.listAll(*all)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(a.out)
		enc.SetIndent("", "  ")
		return enc.Encode(machines)
	}

	if len(machines) == 0 {
		fmt.Fprintln(a.out, "no ccvm machines")
		return nil
	}

	now := time.Now()
	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tBACKEND\tSTATE\tAGE\tPROFILE\tTTL\tPROJECT\tSSH")
	for _, m := range machines {
		age := "-"
		if !m.Created.IsZero() {
			age = session.HumanAge(now.Sub(m.Created))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Name, m.Backend, m.State, age,
			dash(m.Profile), dash(m.TTL), shortenHome(m.Project, a.home), dash(m.SSH))
	}
	return tw.Flush()
}

// listAll fans out across backends concurrently. A backend that is simply not
// running must not fail the whole listing: `ccvm ls` is the command you reach
// for when something is wrong.
func (a *app) listAll(includeUnowned bool) ([]backend.Machine, error) {
	var (
		g       errgroup.Group
		results = make([][]backend.Machine, len(a.backendNames()))
		names   = a.backendNames()
	)
	for i, name := range names {
		i, name := i, name
		g.Go(func() error {
			b := a.backends[name]
			ms, err := b.List(a.ctx)
			if err != nil {
				// Say nothing about a backend the user never configured;
				// complain about one they did.
				if !errors.Is(err, backend.ErrNotConfigured) {
					fmt.Fprintf(a.err, "ccvm: %s backend unavailable: %s\n", name, condense(err))
				}
				return nil
			}
			results[i] = ms
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	var out []backend.Machine
	for _, ms := range results {
		out = append(out, ms...)
	}

	// The mutable half of a machine's metadata lives inside it, so fill it in
	// per machine. Failing to read it is not fatal: a machine whose session
	// file is unreadable should still appear in the listing.
	for i := range out {
		if s, err := a.readSession(out[i]); err == nil {
			if s.TTL != "" {
				out[i].TTL = s.TTL
			}
			if s.Profile != "" {
				out[i].Profile = s.Profile
			}
			// orbstack and k8s carry no project field of their own, so the
			// record inside the machine is the only source.
			if out[i].Project == "" {
				out[i].Project = s.Project
			}
			if out[i].Created.IsZero() {
				out[i].Created = s.Created
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Backend != out[j].Backend {
			return out[i].Backend < out[j].Backend
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// readSession pulls the in-machine record. It uses Pull rather than Exec so it
// keeps working on a stopped machine — which is exactly when the reaper needs
// the TTL.
func (a *app) readSession(m backend.Machine) (session.Session, error) {
	b, err := a.backend(m.Backend)
	if err != nil {
		return session.Session{}, err
	}
	tmp, err := os.CreateTemp("", "ccvm-session-*.toml")
	if err != nil {
		return session.Session{}, err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	if err := b.Pull(a.ctx, m.Handle(), backend.SessionFile, path); err != nil {
		return session.Session{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return session.Session{}, err
	}
	return session.Unmarshal(data)
}

// ---------------------------------------------------------------- rm / keep

func cmdRm(a *app, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ccvm rm <name>...")
	}
	machines, err := a.listAll(true)
	if err != nil {
		return err
	}
	byName := map[string]backend.Machine{}
	for _, m := range machines {
		byName[m.Name] = m
	}

	var failed []string
	for _, name := range args {
		m, ok := byName[name]
		if !ok {
			fmt.Fprintf(a.err, "ccvm: no machine named %q\n", name)
			failed = append(failed, name)
			continue
		}
		b, err := a.backend(m.Backend)
		if err != nil {
			return err
		}
		if err := b.Destroy(a.ctx, m.Handle()); err != nil {
			fmt.Fprintf(a.err, "ccvm: destroy %s: %v\n", name, err)
			failed = append(failed, name)
			continue
		}
		// Drop the ssh entry too, or the config accumulates hosts that no
		// longer resolve.
		if err := a.ssh.Remove(name); err != nil {
			fmt.Fprintf(a.err, "ccvm: remove ssh entry for %s: %v\n", name, err)
		}
		fmt.Fprintf(a.out, "destroyed %s\n", name)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not destroy: %s", strings.Join(failed, ", "))
	}
	return nil
}

func cmdKeep(a *app, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ccvm keep <name>")
	}
	name := args[0]

	machines, err := a.listAll(true)
	if err != nil {
		return err
	}
	for _, m := range machines {
		if m.Name != name {
			continue
		}
		s, err := a.readSession(m)
		if err != nil {
			return fmt.Errorf("read session record for %s: %w", name, err)
		}
		s.TTL = session.Keep
		if err := a.writeSession(m, s); err != nil {
			return fmt.Errorf("mark %s as kept: %w", name, err)
		}
		fmt.Fprintf(a.out, "%s is exempt from reaping; destroy it with `ccvm rm %s`\n", name, name)
		a.warnIfEphemeral(m)
		return nil
	}
	return fmt.Errorf("no machine named %q", name)
}

// warnIfEphemeral reports the limit of what `ccvm keep` can promise.
//
// Exempting a machine from the reaper does not exempt it from its own backend:
// a container created without --keep carries docker's AutoRemove, which is
// fixed at creation. Saying so is better than a silent half-guarantee.
func (a *app) warnIfEphemeral(m backend.Machine) {
	b, err := a.backend(m.Backend)
	if err != nil {
		return
	}
	r, ok := b.(backend.EphemeralReporter)
	if !ok {
		return
	}
	auto, err := r.AutoRemoves(a.ctx, m.Handle())
	if err != nil || !auto {
		return
	}
	fmt.Fprintf(a.err,
		"ccvm: warning: %s was created without --keep, so %s will still delete it if it stops.\n"+
			"      The exemption holds only while it keeps running. Start it with `ccvm up --keep` to make it durable.\n",
		m.Name, m.Backend)
}

func (a *app) writeSession(m backend.Machine, s session.Session) error {
	b, err := a.backend(m.Backend)
	if err != nil {
		return err
	}
	data, err := session.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "ccvm-session-*.toml")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	return b.Push(a.ctx, m.Handle(), path, backend.SessionFile)
}

// ---------------------------------------------------------------- gc

func cmdGC(a *app, args []string) error {
	fs := newFlags("gc", a)
	ttl := fs.Duration("ttl", 12*time.Hour, "fallback lifetime for machines with no recorded TTL")
	dryRun := fs.Bool("dry-run", false, "report what would be destroyed without destroying it")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	machines, err := a.listAll(false)
	if err != nil {
		return err
	}

	now := time.Now()
	var reaped int
	for _, m := range machines {
		reason, doomed := a.reapReason(m, now, *ttl)
		if !doomed {
			continue
		}
		if *dryRun {
			fmt.Fprintf(a.out, "would destroy %s (%s)\n", m.Name, reason)
			reaped++
			continue
		}
		b, err := a.backend(m.Backend)
		if err != nil {
			return err
		}
		if err := b.Destroy(a.ctx, m.Handle()); err != nil {
			fmt.Fprintf(a.err, "ccvm: destroy %s: %v\n", m.Name, err)
			continue
		}
		_ = a.ssh.Remove(m.Name)
		fmt.Fprintf(a.out, "destroyed %s (%s)\n", m.Name, reason)
		reaped++
	}
	if reaped == 0 {
		fmt.Fprintln(a.out, "nothing to reap")
	}
	return nil
}

// reapReason decides a machine's fate. Two things get destroyed: anything past
// its TTL that is not kept, and anything carrying the done sentinel regardless
// of TTL.
func (a *app) reapReason(m backend.Machine, now time.Time, fallback time.Duration) (string, bool) {
	if a.hasSentinel(m) {
		return "session ended", true
	}

	s, err := a.readSession(m)
	if err != nil {
		// A machine whose record cannot be read is left alone. Destroying on a
		// failed read would make a transient backend hiccup delete work.
		return "", false
	}
	if s.Kept() {
		return "", false
	}
	if m.Created.IsZero() && s.Created.IsZero() {
		return "", false
	}
	if s.Created.IsZero() {
		s.Created = m.Created
	}
	expired, err := s.Expired(now, fallback)
	if err != nil || !expired {
		return "", false
	}
	return fmt.Sprintf("older than %s", dash(s.TTL)), true
}

func (a *app) hasSentinel(m backend.Machine) bool {
	if m.State != backend.StateRunning {
		return false
	}
	b, err := a.backend(m.Backend)
	if err != nil {
		return false
	}
	_, err = b.Exec(a.ctx, m.Handle(), "test", "-f", backend.DoneSentinel)
	return err == nil
}

// ---------------------------------------------------------------- profiles

func cmdProfiles(a *app, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}

	switch sub {
	case "list":
		return a.profilesList()
	case "lint":
		if len(args) != 1 {
			return fmt.Errorf("usage: ccvm profiles lint <name>")
		}
		return a.profilesLint(args[0])
	case "build":
		// The profile name comes first, so it is pulled off before flag
		// parsing: Go's flag package stops at the first non-flag argument and
		// would otherwise hand the name back as an unparsed positional.
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			// Echo what arrived. An identical message for "no name given" and
			// "flags misparsed" makes a stale binary indistinguishable from a
			// mistake in the command.
			return fmt.Errorf("usage: ccvm profiles build <name> [--backend B]\n"+
				"got: ccvm profiles build %s", strings.Join(args, " "))
		}
		name := args[0]
		fs := newFlags("profiles build", a)
		backendName := fs.String("backend", "", "backend to build for")
		if err := fs.Parse(args[1:]); err != nil {
			return errUsage
		}
		return a.profilesBuild(name, *backendName, fs.Args())
	default:
		return fmt.Errorf("unknown subcommand %q; try list, lint, or build", sub)
	}
}

func (a *app) profilesList() error {
	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tBACKENDS\tDESCRIPTION")
	for _, name := range a.knownProfiles() {
		c, err := profile.Resolve(name, a.profiles)
		if err != nil {
			fmt.Fprintf(tw, "%s\t-\t(broken: %v)\n", name, err)
			continue
		}
		var backends []string
		for b, cfg := range c.Backend {
			if !cfg.Empty() {
				backends = append(backends, b)
			}
		}
		sort.Strings(backends)
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, strings.Join(backends, ","), c.Description)
	}
	return tw.Flush()
}

// knownProfiles enumerates what is resolvable. The built-ins are known
// statically; a user directory contributes whatever it holds.
func (a *app) knownProfiles() []string {
	seen := map[string]bool{"base": true, "go": true, "node": true}
	userDir := filepath.Join(a.home, ".config", "ccvm", "profiles")
	if entries, err := os.ReadDir(userDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				seen[e.Name()] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (a *app) profilesLint(name string) error {
	c, err := profile.Resolve(name, a.profiles)
	if err != nil {
		return err
	}
	var problems []string
	for _, b := range []string{"docker", "orbstack", "proxmox", "k8s"} {
		cfg, ok := c.Backend[b]
		if !ok {
			problems = append(problems, fmt.Sprintf("no [backend.%s] table", b))
			continue
		}
		if cfg.Empty() {
			problems = append(problems, fmt.Sprintf("[backend.%s] names no image or template", b))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("profile %q:\n  - %s", name, strings.Join(problems, "\n  - "))
	}
	fmt.Fprintf(a.out, "profile %q is valid for all backends\n", name)
	return nil
}

// ---------------------------------------------------------------- creds

func cmdCreds(a *app, args []string) error {
	sub := "check"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "check":
	case "import":
		if len(args) != 1 {
			return fmt.Errorf("usage: ccvm creds import <machine>\n" +
				"where <machine> is a broker built with `ccvm up --no-credential` that you ran `/login` in")
		}
		return a.credsImport(args[0])
	default:
		return fmt.Errorf("unknown subcommand %q; try check or import", sub)
	}

	// Report both paths, because which one a session gets depends on a flag
	// and the failure modes are different.
	token, tokenErr := creds.Resolve(a.home, os.Getenv, false)
	login, loginErr := creds.Resolve(a.home, os.Getenv, true)

	if tokenErr != nil {
		fmt.Fprintf(a.out, "token      unavailable: %v\n", firstLine(tokenErr.Error()))
	} else {
		fmt.Fprintf(a.out, "token      ready, from %s\n", token.Origin)
	}

	if loginErr != nil {
		fmt.Fprintf(a.out, "login      unavailable: %v\n", firstLine(loginErr.Error()))
		fmt.Fprintln(a.out, "           (--remote-control needs this; the token path cannot drive sessions from claude.ai)")
	} else {
		expiry, err := creds.ExpiresAt(login.CredentialsFile)
		switch {
		case err != nil:
			fmt.Fprintf(a.out, "login      ready, from %s (expiry unreadable: %v)\n", login.Origin, err)
		case expiry.IsZero():
			fmt.Fprintf(a.out, "login      ready, from %s (no expiry recorded)\n", login.Origin)
		default:
			days := int(time.Until(expiry).Hours() / 24)
			fmt.Fprintf(a.out, "login      ready, from %s (expires in %d days)\n", login.Origin, days)
			// Claude Code warns three days out, and an expired login stalls an
			// unattended session silently.
			if days <= 3 {
				fmt.Fprintf(a.err, "ccvm: warning: the claude.ai login expires in %d days; renew it with `/login` on the broker machine\n", days)
			}
		}
	}

	if tokenErr != nil && loginErr != nil {
		return fmt.Errorf("no usable Claude credential; sessions will not be able to authenticate")
	}
	return nil
}

// condense reduces a tool's error to the line worth reading.
//
// kubectl repeats the same klog line five times before saying anything useful,
// and a listing that dumps all of it on every invocation is unusable. The
// signal is the last line that is not a log record.
func condense(err error) string {
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	best := lines[0]
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || isLogLine(l) {
			continue
		}
		best = l
	}
	const max = 200
	if len(best) > max {
		best = best[:max] + "…"
	}
	return best
}

// isLogLine matches klog's severity-and-timestamp prefix, such as
// "E0830 04:16:25.195915   44618 memcache.go:265]".
func isLogLine(s string) bool {
	if len(s) < 6 {
		return false
	}
	switch s[0] {
	case 'E', 'I', 'W', 'F':
	default:
		return false
	}
	for _, r := range s[1:5] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// credsImport copies a claude.ai login out of a broker machine.
//
// The login cannot be minted on macOS, where the credential lives in the
// Keychain rather than a file, so it has to come out of a Linux guest. Doing
// that by hand means a shell pipeline that silently produces an unusable file
// when the path is wrong, so the tool does it and checks the result.
func (a *app) credsImport(machine string) error {
	machines, err := a.listAll(true)
	if err != nil {
		return err
	}

	var found *backend.Machine
	for i := range machines {
		if machines[i].Name == machine {
			found = &machines[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("no machine named %q; `ccvm ls` shows what is running", machine)
	}

	b, err := a.backend(found.Backend)
	if err != nil {
		return err
	}

	dst := filepath.Join(a.home, ".config", "ccvm", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}

	if err := b.Pull(a.ctx, found.Handle(), creds.GuestCredentialsFile, dst); err != nil {
		return fmt.Errorf("read the login from %s: %w\n"+
			"Run `claude` and `/login` inside it first: ccvm ssh %s", machine, err, machine)
	}
	// Pull carries the guest's mode, and a credential the rest of the machine
	// can read is not one worth having.
	if err := os.Chmod(dst, 0o600); err != nil {
		return err
	}

	// Verify rather than trust: a wrong path yields a file that only fails
	// later, at the point where a session cannot authenticate.
	expiry, err := creds.ExpiresAt(dst)
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("what came out of %s is not a usable login: %w", machine, err)
	}

	fmt.Fprintf(a.out, "imported the claude.ai login from %s to %s\n", machine, dst)
	switch {
	case expiry.IsZero():
		fmt.Fprintln(a.out, "no expiry recorded")
	default:
		fmt.Fprintf(a.out, "expires in %d days\n", int(time.Until(expiry).Hours()/24))
	}
	fmt.Fprintln(a.out, "`ccvm up --remote-control` can now drive sessions from claude.ai and the phone app")
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------- doctor

func cmdDoctor(a *app, args []string) error {
	fs := newFlags("doctor", a)
	only := fs.String("backend", "", "check a single backend")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	names := a.backendNames()
	if *only != "" {
		if _, err := a.backend(*only); err != nil {
			return err
		}
		names = []string{*only}
	}

	spec := backend.Spec{Profile: "base", Name: "ccvm-doctor"}
	failures := 0
	for _, name := range names {
		b := a.backends[name]
		c, err := profile.Resolve("base", a.profiles)
		if err != nil {
			return err
		}
		s := spec
		img, err := imageForBackend(c, name, false)
		if err != nil {
			failures++
			fmt.Fprintf(a.out, "%-9s unusable: %v\n", name, err)
			continue
		}
		s.Image = img

		if err := b.Preflight(a.ctx, s); err != nil {
			failures++
			// The error already says whether it is unconfigured or merely
			// unreachable; prefixing a label here would say it twice.
			if errors.Is(err, backend.ErrNotConfigured) {
				fmt.Fprintf(a.out, "%-9s %v\n", name, err)
			} else {
				fmt.Fprintf(a.out, "%-9s unavailable: %v\n", name, err)
			}
			continue
		}
		fmt.Fprintf(a.out, "%-9s ready\n", name)
	}
	if failures == len(names) {
		return fmt.Errorf("no backend can create a machine right now")
	}
	return nil
}

// ---------------------------------------------------------------- helpers

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// shortenHome keeps the listing readable without hiding which project a
// machine belongs to.
func shortenHome(path, home string) string {
	if home != "" && strings.HasPrefix(path, home) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return dash(path)
}

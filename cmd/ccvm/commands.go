package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/josegonzalez/ccvm/internal/backend"
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
				fmt.Fprintf(a.err, "ccvm: %s backend unavailable: %v\n", name, err)
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
	default:
		return fmt.Errorf("unknown subcommand %q; try list or lint", sub)
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
		s.Image = c.Backend[name].Image

		if err := b.Preflight(a.ctx, s); err != nil {
			failures++
			fmt.Fprintf(a.out, "%-9s unavailable: %v\n", name, err)
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

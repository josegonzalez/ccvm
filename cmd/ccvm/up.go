package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/profile"
	"github.com/josegonzalez/ccvm/internal/session"
	"github.com/josegonzalez/ccvm/internal/sshcfg"
)

// validCodeModes maps each mode to the backends that can serve it. k8s has no
// host to mount from, so asking for one there is a preflight failure rather
// than a surprise later.
var validCodeModes = map[string][]string{
	"mount": {"docker", "orbstack", "proxmox"},
	"rsync": {"docker", "orbstack", "proxmox", "k8s"},
	"git":   {"docker", "orbstack", "proxmox", "k8s"},
	"sshfs": {"proxmox"},
}

func cmdUp(a *app, args []string) error {
	fs := newFlags("up", a)
	var (
		backendName = fs.String("backend", "", "docker, orbstack, proxmox, or k8s")
		profileName = fs.String("profile", "base", "profile to build the machine from")
		codeMode    = fs.String("code", "", "mount, rsync, git, or sshfs")
		keep        = fs.Bool("keep", false, "leave the machine running after the session ends")
		yolo        = fs.Bool("yolo", false, "run Claude with --dangerously-skip-permissions")
		useVM       = fs.Bool("vm", false, "proxmox only: use the VM template instead of LXC")
		install     = fs.String("install", "", "extra packages to install at spawn, comma separated")
		dryRun      = fs.Bool("dry-run", false, "resolve and preflight without creating anything")
	)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	projectDir, err := resolveProject(fs.Arg(0))
	if err != nil {
		return err
	}

	cfg, err := a.resolveConfig(*profileName, projectDir)
	if err != nil {
		return err
	}

	chosen := firstNonEmpty(*backendName, cfg.Defaults.Backend, "docker")
	b, err := a.backend(chosen)
	if err != nil {
		return err
	}

	mode := firstNonEmpty(*codeMode, cfg.Defaults.CodeMode, "mount")
	if err := checkCodeMode(mode, chosen); err != nil {
		return err
	}
	// --vm is proxmox-only. A silently ignored flag is worse than a refusal:
	// the user believes they got a VM and did not.
	if *useVM && chosen != "proxmox" {
		return fmt.Errorf("--vm is proxmox-only; --backend %s does not have LXC and VM variants", chosen)
	}

	image, err := imageForBackend(cfg, chosen, *useVM)
	if err != nil {
		return fmt.Errorf("profile %q: %w", *profileName, err)
	}

	spec := backend.Spec{
		Name:      machineName(projectDir),
		Profile:   *profileName,
		Image:     image,
		Project:   projectDir,
		WorkDir:   "/work",
		CodeMode:  mode,
		CPUs:      cfg.Resources.CPUs,
		Memory:    cfg.Resources.Memory,
		Disk:      cfg.Resources.Disk,
		Env:       cfg.Env,
		Keep:      *keep,
		TTL:       ttlFor(cfg, *keep),
		CreatedAt: time.Now().UTC(),
	}

	if err := b.Preflight(a.ctx, spec); err != nil {
		return &Fault{
			Backend: chosen,
			Step:    "preflight",
			Cause:   err,
			Fix:     fixFor(chosen, err),
		}
	}

	if *dryRun {
		return a.printPlan(spec, chosen, cfg, *install, *yolo)
	}

	if spec.SSHPort == 0 && needsPort(chosen) {
		port, err := sshcfg.FreePort()
		if err != nil {
			return &Fault{Backend: chosen, Step: "allocate an ssh port", Cause: err}
		}
		spec.SSHPort = port
	}

	// Everything past this point can leave state behind, so each step that
	// succeeds registers its undo. A failure unwinds them in reverse rather
	// than leaving a half-created machine and a stale ssh entry.
	var rollback []func()
	unwind := func() {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
	}

	handle, err := b.Create(a.ctx, spec)
	if err != nil {
		return &Fault{
			Backend: chosen,
			Step:    fmt.Sprintf("create machine %s", spec.Name),
			Cause:   err,
			Fix:     fixFor(chosen, err),
		}
	}
	rollback = append(rollback, func() { _ = b.Destroy(a.ctx, handle) })

	if err := b.Start(a.ctx, handle); err != nil {
		unwind()
		return &Fault{
			Backend: chosen, Step: "start", Cause: err,
			Cleanup: "machine destroyed; nothing left running.",
		}
	}

	if err := b.Wait(a.ctx, handle); err != nil {
		unwind()
		return &Fault{
			Backend: chosen,
			Step:    fmt.Sprintf("wait for %s to become ready", spec.Name),
			Cause:   err,
			Cleanup: "machine destroyed; nothing left running.",
		}
	}

	// Only backends reached over a forwarded loopback port need an entry.
	// OrbStack answers to <name>@orb through its own multiplexing ssh server,
	// and proxmox guests have real addresses, so writing one would shadow the
	// working target with a broken one.
	if needsPort(chosen) {
		if _, err := a.ssh.EnsureInclude(); err != nil {
			fmt.Fprintf(a.err, "ccvm: could not update ~/.ssh/config: %v\n", err)
		}
		host := sshcfg.Host{Name: spec.Name, HostName: "127.0.0.1", Port: spec.SSHPort, User: "root"}
		if err := a.ssh.Add(host); err != nil {
			unwind()
			return &Fault{
				Backend: chosen, Step: "write the ssh config entry", Cause: err,
				Cleanup: "machine destroyed; nothing left running.",
			}
		}
		rollback = append(rollback, func() { _ = a.ssh.Remove(spec.Name) })
	}

	if err := a.writeSessionRecord(b, handle, spec); err != nil {
		unwind()
		return &Fault{
			Backend: chosen, Step: "write the session record", Cause: err,
			Cleanup: "machine destroyed; ssh entry removed.",
		}
	}

	fmt.Fprintf(a.out, "%s is up (%s, %s)\n", spec.Name, chosen, mode)
	fmt.Fprintf(a.out, "  ssh %s\n", b.SSHTarget(handle))
	return nil
}

// resolveConfig applies the precedence chain: the profile and its ancestors,
// then the user's global config, then the project's.
func (a *app) resolveConfig(name, projectDir string) (*profile.Config, error) {
	base, err := profile.Resolve(name, a.profiles)
	if err != nil {
		return nil, err
	}

	var overlays []profile.Overlay
	userCfg := filepath.Join(a.home, ".config", "ccvm", "profile.toml")
	if c, err := loadOverlay(userCfg, profile.ScopeOwned); err != nil {
		return nil, err
	} else if c != nil {
		overlays = append(overlays, profile.Overlay{Name: userCfg, Config: c})
	}

	projCfg := filepath.Join(projectDir, ".ccvm", "profile.toml")
	if c, err := loadOverlay(projCfg, profile.ScopeProject); err != nil {
		return nil, err
	} else if c != nil {
		overlays = append(overlays, profile.Overlay{Name: projCfg, Config: c})
	}

	return profile.Apply(base, overlays...), nil
}

func loadOverlay(path string, scope profile.Scope) (*profile.Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return profile.Parse(path, data, scope)
}

func (a *app) writeSessionRecord(b backend.Backend, h backend.Handle, spec backend.Spec) error {
	rec := session.Session{
		Name:     spec.Name,
		Backend:  b.Name(),
		Profile:  spec.Profile,
		Project:  spec.Project,
		WorkDir:  spec.WorkDir,
		CodeMode: spec.CodeMode,
		Created:  spec.CreatedAt,
		TTL:      spec.TTL,
	}
	data, err := session.Marshal(rec)
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

	if _, err := b.Exec(a.ctx, h, "mkdir", "-p", filepath.Dir(backend.SessionFile)); err != nil {
		return err
	}
	return b.Push(a.ctx, h, path, backend.SessionFile)
}

func (a *app) printPlan(spec backend.Spec, chosen string, cfg *profile.Config, install string, yolo bool) error {
	fmt.Fprintf(a.out, "would create %s\n", spec.Name)
	fmt.Fprintf(a.out, "  backend   %s\n", chosen)
	fmt.Fprintf(a.out, "  profile   %s\n", spec.Profile)
	fmt.Fprintf(a.out, "  image     %s\n", dash(spec.Image))
	fmt.Fprintf(a.out, "  code      %s (%s -> %s)\n", spec.CodeMode, spec.Project, spec.WorkDir)
	fmt.Fprintf(a.out, "  resources %d cpu, %s memory, %s disk\n", spec.CPUs, spec.Memory, spec.Disk)
	fmt.Fprintf(a.out, "  ttl       %s\n", dash(spec.TTL))
	if len(cfg.Provision.Packages) > 0 {
		fmt.Fprintf(a.out, "  packages  %s\n", strings.Join(cfg.Provision.Packages, ", "))
	}
	if install != "" {
		fmt.Fprintf(a.out, "  --install %s\n", install)
	}
	if yolo {
		fmt.Fprintln(a.out, "  claude    --dangerously-skip-permissions")
	}
	return nil
}

// imageForBackend resolves what a backend builds a machine from. The word
// means something different in each: a registry reference for docker and k8s, a
// template machine for orbstack, a template vmid for proxmox. Resolving it in
// one place keeps `up` and `doctor` from disagreeing.
func imageForBackend(cfg *profile.Config, backendName string, useVM bool) (string, error) {
	b, ok := cfg.Backend[backendName]
	if !ok || b.Empty() {
		return "", fmt.Errorf("no usable [backend.%s]; run `ccvm profiles build <name> --backend %s`, or pick another backend",
			backendName, backendName)
	}

	switch backendName {
	case "orbstack":
		if b.Template == "" {
			return "", fmt.Errorf("[backend.orbstack] has no template")
		}
		return b.Template, nil

	case "proxmox":
		id := b.LXCTemplate
		kind := "lxc_template"
		if useVM {
			id, kind = b.VMTemplate, "vm_template"
		}
		if id == 0 {
			return "", fmt.Errorf("[backend.proxmox] has no %s", kind)
		}
		return strconv.Itoa(id), nil

	default:
		if b.Image == "" {
			return "", fmt.Errorf("[backend.%s] has no image", backendName)
		}
		return b.Image, nil
	}
}

func checkCodeMode(mode, backendName string) error {
	allowed, ok := validCodeModes[mode]
	if !ok {
		return fmt.Errorf("unknown code mode %q; try mount, rsync, git, or sshfs", mode)
	}
	for _, b := range allowed {
		if b == backendName {
			return nil
		}
	}
	return fmt.Errorf("--code %s is not available on the %s backend; it supports %s",
		mode, backendName, strings.Join(modesFor(backendName), ", "))
}

func modesFor(backendName string) []string {
	var out []string
	for _, mode := range []string{"mount", "rsync", "git", "sshfs"} {
		for _, b := range validCodeModes[mode] {
			if b == backendName {
				out = append(out, mode)
				break
			}
		}
	}
	return out
}

// needsPort reports whether a backend reaches sshd through a loopback port
// rather than its own addressing. orbstack multiplexes ssh itself and proxmox
// gives guests real addresses, so neither needs one.
func needsPort(backendName string) bool {
	switch backendName {
	case "docker", "k8s":
		return true
	default:
		return false
	}
}

func ttlFor(cfg *profile.Config, keep bool) string {
	if keep {
		return session.Keep
	}
	if cfg.Defaults.TTL != "" {
		return cfg.Defaults.TTL
	}
	return "12h"
}

func resolveProject(arg string) (string, error) {
	if arg == "" {
		arg = "."
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("project path %s: %w", abs, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("project path %s is not a directory", abs)
	}
	return abs, nil
}

var unsafeName = regexp.MustCompile(`[^a-z0-9-]+`)

// machineName derives a stable, addressable name from the project directory.
// It has to be usable as a hostname and an ssh alias, so it is lowercased and
// stripped of anything outside that alphabet.
func machineName(projectDir string) string {
	base := strings.ToLower(filepath.Base(projectDir))
	base = unsafeName.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "session"
	}
	const max = 40
	if len(base) > max {
		base = strings.Trim(base[:max], "-")
	}
	return "cc-" + base
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// fixFor turns a backend's own failure into a next action.
func fixFor(backendName string, err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "not reachable"), strings.Contains(msg, "cannot connect"):
		if backendName == "docker" {
			return "start Docker, or use --backend orbstack"
		}
		return fmt.Sprintf("check that the %s backend is running and reachable", backendName)
	case strings.Contains(msg, "not present locally"):
		return "build the profile image with `ccvm profiles build <name>`, or pull it"
	case strings.Contains(msg, "no [backend."):
		return "add the table to the profile, or pick another backend"
	}
	return ""
}

// sshExec runs ssh with the terminal attached, for `ccvm ssh` and `ccvm attach`.
func sshExec(target string, args ...string) error {
	argv := append([]string{"-t", target}, args...)
	cmd := exec.Command("ssh", argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func cmdSSH(a *app, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ccvm ssh <name> [-- command...]")
	}
	name, rest := args[0], args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	return sshExec(name, rest...)
}

// cmdAttach reconnects to the machine's Claude session. tmux new -A attaches if
// the session exists and creates it otherwise, so a dropped connection is
// recoverable rather than fatal.
func cmdAttach(a *app, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ccvm attach <name>")
	}
	name := args[0]
	return sshExec(name, "tmux", "new", "-A", "-s", "cc")
}

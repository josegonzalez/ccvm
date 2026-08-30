package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/josegonzalez/ccvm/internal/attach"
	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/creds"
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
		remoteCtl   = fs.Bool("remote-control", false, "use a claude.ai login so the session can be driven from claude.ai or your phone")
		install     = fs.String("install", "", "extra packages to install at spawn, comma separated")
		dryRun      = fs.Bool("dry-run", false, "resolve and preflight without creating anything")
		detach      = fs.Bool("detach", false, "provision and return instead of entering the session")
		noCred      = fs.Bool("no-credential", false, "provision without installing a Claude credential, for building a broker machine")
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

	name, err := a.uniqueName(projectDir)
	if err != nil {
		return err
	}

	spec := backend.Spec{
		Name:      name,
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

	// A broker machine is where a claude.ai login is minted, so it is the one
	// machine that cannot already have a credential to install. Without this
	// the login path could never be bootstrapped.
	var cred creds.Source
	if *noCred {
		if *remoteCtl {
			return fmt.Errorf("--no-credential and --remote-control are contradictory: " +
				"one provisions a machine with no Claude credential, the other needs one")
		}
		cred = creds.Source{Mode: creds.None}
	} else {
		cred, err = creds.Resolve(a.home, os.Getenv, *remoteCtl)
		if err != nil {
			return err
		}
	}
	if *remoteCtl && *yolo {
		fmt.Fprintln(a.err, "ccvm: warning: --remote-control with --yolo gives a session that bypasses\n"+
			"      permission prompts and can be driven from your phone. The machine is\n"+
			"      disposable, but it holds a live claude.ai login.")
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

	if err := a.installSSHKey(b, handle); err != nil {
		unwind()
		return &Fault{
			Backend: chosen, Step: "install the ssh key", Cause: err,
			Fix:     "check that the guest image runs sshd and allows root login by key",
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
		host := sshcfg.Host{
			Name:         spec.Name,
			HostName:     "127.0.0.1",
			Port:         spec.SSHPort,
			User:         "root",
			IdentityFile: a.sshKey.Private,
		}
		if err := a.ssh.Add(host); err != nil {
			unwind()
			return &Fault{
				Backend: chosen, Step: "write the ssh config entry", Cause: err,
				Cleanup: "machine destroyed; nothing left running.",
			}
		}
		rollback = append(rollback, func() { _ = a.ssh.Remove(spec.Name) })
	}

	if err := a.installCredentials(b, handle, spec, cred); err != nil {
		unwind()
		return &Fault{
			Backend: chosen, Step: "install the Claude credential", Cause: err,
			Cleanup: "machine destroyed; nothing left running.",
		}
	}

	if err := a.writeSessionRecord(b, handle, spec); err != nil {
		unwind()
		return &Fault{
			Backend: chosen, Step: "write the session record", Cause: err,
			Cleanup: "machine destroyed; ssh entry removed.",
		}
	}

	target := b.SSHTarget(handle)
	fmt.Fprintf(a.out, "%s is up (%s, %s, %s)\n", spec.Name, chosen, mode, cred.Describe())

	if *detach {
		fmt.Fprintf(a.out, "  ccvm attach %s\n", spec.Name)
		fmt.Fprintf(a.out, "  ssh %s\n", target)
		return nil
	}

	opts := attach.Options{
		Target:        target,
		WorkDir:       spec.WorkDir,
		SessionName:   spec.Name,
		RemoteControl: cred.SupportsRemoteControl(),
		Yolo:          *yolo,
		IdentityFile:  a.sshKey.Private,
	}
	if err := attach.Run(opts); err != nil {
		return err
	}

	// The session ended. Whether the machine follows is the session record's
	// call, not this flag's: `ccvm keep` and `ccvm-done --keep` both work from
	// inside a running session, after --keep was or was not passed here.
	return a.teardownAfterSession(b, handle, spec)
}

// uniqueName derives a machine name from the project, suffixing when one is
// already taken.
//
// A project can have several concurrent sessions, so the name has to be free
// rather than merely derived. Existing machines are consulted across every
// backend, since a name is also an ssh alias and two would collide there.
func (a *app) uniqueName(projectDir string) (string, error) {
	base := machineName(projectDir)

	existing, err := a.listAll(true)
	if err != nil {
		return "", err
	}
	taken := make(map[string]bool, len(existing))
	for _, m := range existing {
		taken[m.Name] = true
	}

	if !taken[base] {
		return base, nil
	}
	for i := 2; i < 100; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !taken[candidate] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("too many machines already exist for %s; destroy some with `ccvm rm`", projectDir)
}

// teardownAfterSession destroys the machine once the session ends, unless it
// was marked to survive.
//
// The in-machine session record is the authority, not the flag `up` was given:
// `ccvm keep` and `ccvm-done --keep` both mark a machine from inside a session
// that is already running.
func (a *app) teardownAfterSession(b backend.Backend, h backend.Handle, spec backend.Spec) error {
	keep := spec.Keep
	if rec, err := a.readSessionFrom(b, h); err == nil {
		keep = rec.Kept()
	}

	if keep {
		fmt.Fprintf(a.out, "%s is still running; reattach with `ccvm attach %s`\n", spec.Name, spec.Name)
		return nil
	}

	if err := b.Destroy(a.ctx, h); err != nil {
		return fmt.Errorf("destroy %s: %w", spec.Name, err)
	}
	if err := a.ssh.Remove(spec.Name); err != nil {
		fmt.Fprintf(a.err, "ccvm: remove ssh entry for %s: %v\n", spec.Name, err)
	}
	fmt.Fprintf(a.out, "destroyed %s\n", spec.Name)
	return nil
}

// readSessionFrom reads a machine's record given a live handle.
func (a *app) readSessionFrom(b backend.Backend, h backend.Handle) (session.Session, error) {
	tmp, err := os.CreateTemp("", "ccvm-session-*.toml")
	if err != nil {
		return session.Session{}, err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	if err := b.Pull(a.ctx, h, backend.SessionFile, path); err != nil {
		return session.Session{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return session.Session{}, err
	}
	return session.Unmarshal(data)
}

// installSSHKey puts ccvm's public key in the guest's authorized_keys.
//
// It goes through Push and Exec rather than being baked into images, so one
// path serves every backend and rotating the key does not mean rebuilding four
// image types.
func (a *app) installSSHKey(b backend.Backend, h backend.Handle) error {
	line, err := a.sshKey.AuthorizedKey()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "ccvm-authorized-keys-*")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.WriteString(line); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	const dir = "/root/.ssh"
	if _, err := b.Exec(a.ctx, h, "mkdir", "-p", dir); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := b.Push(a.ctx, h, path, dir+"/authorized_keys"); err != nil {
		return fmt.Errorf("write authorized_keys: %w", err)
	}
	// Ownership first: file transfer carries the host's uid, so the key lands
	// owned by whoever ran ccvm rather than by root. sshd refuses an
	// authorized_keys the account does not own, and reports it only in its own
	// log — the client just sees "Permission denied (publickey)".
	if _, err := b.Exec(a.ctx, h, "chown", "-R", "root:root", dir); err != nil {
		return fmt.Errorf("set ownership on %s: %w", dir, err)
	}
	// sshd likewise ignores an authorized_keys that group or others can write.
	if _, err := b.Exec(a.ctx, h, "chmod", "700", dir); err != nil {
		return err
	}
	if _, err := b.Exec(a.ctx, h, "chmod", "600", dir+"/authorized_keys"); err != nil {
		return err
	}
	return nil
}

// installCredentials puts the session's credential and pre-accepted workspace
// trust into the machine.
//
// Secrets go in a file at 0600 rather than on a command line: an ssh argument
// is visible in the process list on both ends and lands in shell history.
func (a *app) installCredentials(b backend.Backend, h backend.Handle, spec backend.Spec, c creds.Source) error {
	if _, err := b.Exec(a.ctx, h, "mkdir", "-p", filepath.Dir(creds.GuestEnvFile)); err != nil {
		return err
	}
	// A broker gets no env file at all. Writing an empty one would shadow the
	// login the user is about to mint inside it, since an environment token
	// outranks a /login credential.
	if c.Mode != creds.None {
		if err := a.pushString(b, h, c.EnvFile(), creds.GuestEnvFile, "600"); err != nil {
			return fmt.Errorf("write %s: %w", creds.GuestEnvFile, err)
		}
	}

	if c.Mode == creds.Login {
		if _, err := b.Exec(a.ctx, h, "mkdir", "-p", filepath.Dir(creds.GuestCredentialsFile)); err != nil {
			return err
		}
		if err := b.Push(a.ctx, h, c.CredentialsFile, creds.GuestCredentialsFile); err != nil {
			return fmt.Errorf("copy the claude.ai login: %w", err)
		}
		if _, err := b.Exec(a.ctx, h, "chown", "-R", "root:root", filepath.Dir(creds.GuestCredentialsFile)); err != nil {
			return err
		}
		if _, err := b.Exec(a.ctx, h, "chmod", "600", creds.GuestCredentialsFile); err != nil {
			return err
		}
	}

	// Trust is best-effort. Claude Code owns this schema and it can change; a
	// trust dialog is an annoyance, whereas refusing to start a session over
	// one would be worse.
	trust, err := creds.TrustFile(spec.WorkDir, spec.Project)
	if err != nil {
		fmt.Fprintf(a.err, "ccvm: could not seed workspace trust: %v\n", err)
		return nil
	}
	if err := a.pushString(b, h, string(trust), creds.GuestConfigFile, "600"); err != nil {
		fmt.Fprintf(a.err, "ccvm: could not seed workspace trust: %v\n", err)
	}
	return nil
}

// pushString writes content into the machine at the given mode.
func (a *app) pushString(b backend.Backend, h backend.Handle, content, dst, mode string) error {
	tmp, err := os.CreateTemp("", "ccvm-push-*")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}

	if err := b.Push(a.ctx, h, path, dst); err != nil {
		return err
	}
	// File transfer carries the host's uid, so ownership has to be set here too.
	if _, err := b.Exec(a.ctx, h, "chown", "root:root", dst); err != nil {
		return err
	}
	_, err = b.Exec(a.ctx, h, "chmod", mode, dst)
	return err
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

// targetFor resolves a machine name to an ssh destination, so `ccvm ssh` works
// on backends whose target is not the machine name.
func (a *app) targetFor(name string) (string, error) {
	machines, err := a.listAll(true)
	if err != nil {
		return "", err
	}
	for _, m := range machines {
		if m.Name != name {
			continue
		}
		if m.SSH != "" {
			return m.SSH, nil
		}
		return m.Name, nil
	}
	return "", fmt.Errorf("no machine named %q", name)
}

func cmdSSH(a *app, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ccvm ssh <name> [-- command...]")
	}
	name, rest := args[0], args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	target, err := a.targetFor(name)
	if err != nil {
		return err
	}
	return attach.Shell(attach.Options{Target: target, IdentityFile: a.sshKey.Private}, rest...)
}

// cmdAttach reconnects to the machine's Claude session. tmux new -A attaches if
// the session exists and creates it otherwise, so a dropped connection detaches
// rather than ending the session.
func cmdAttach(a *app, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ccvm attach <name>")
	}
	name := args[0]
	target, err := a.targetFor(name)
	if err != nil {
		return err
	}
	return attach.Run(attach.Options{
		Target:       target,
		SessionName:  name,
		IdentityFile: a.sshKey.Private,
	})
}

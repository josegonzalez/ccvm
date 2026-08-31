package backend

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/josegonzalez/ccvm/internal/run"
)

// Proxmox runs sessions as LXC containers or VMs on a Proxmox cluster.
//
// It is the only backend whose machines outlive the laptop, and the only one
// driven by an HTTP API rather than a CLI. Guests get deterministic addresses
// derived from their vmid, so reaching one never requires polling for a lease.
type Proxmox struct {
	Runner run.Execer
	API    *pveClient
	Cfg    Config

	// Kind is "lxc" or "qemu", chosen per spec by --vm.
	Kind string

	// Log receives notes about decisions the caller did not ask for, such as
	// falling back to a full clone.
	Log io.Writer

	// WaitTimeout bounds how long Wait polls for a guest to answer over ssh.
	// Zero uses the default.
	WaitTimeout time.Duration

	// configErr defers a missing-configuration failure to first use.
	// Constructing a backend must never fail: `ccvm ls` and `ccvm doctor` span
	// every registered backend, and an unconfigured proxmox should report
	// itself as unavailable rather than break the whole CLI.
	configErr error
}

var (
	_ Backend = (*Proxmox)(nil)
	_ Stopper = (*Proxmox)(nil)
)

const (
	pveDefaultBridge = "vmbr0"
	// pveDefaultSubnet is a /16 prefix, not a /24. A /24 holds 254 hosts, which
	// would cap the vmid range at 254 sessions and silently produce invalid
	// addresses above it — 10.10.0.312 is not an address.
	pveDefaultSubnet   = "10.10"
	pveSubnetMask      = 16
	pveDefaultVMIDBase = 4000
	// pveMinHost skips the network address and the gateway.
	pveMinHost       = 2
	pveMaxHost       = 65534
	pveTaskTimeout   = 10 * time.Minute
	pveCloneAttempts = 3
)

func NewProxmox(e run.Execer, cfg Config) *Proxmox {
	p := &Proxmox{Runner: e, Cfg: cfg, Kind: "lxc"}
	api, err := newPVEClient(cfg)
	if err != nil {
		p.configErr = err
		return p
	}
	p.API = api
	return p
}

// ready reports whether the backend was configured well enough to be used.
func (p *Proxmox) ready() error {
	if p.configErr != nil {
		return p.configErr
	}
	if p.API == nil {
		return fmt.Errorf("proxmox backend is not configured")
	}
	return nil
}

func (p *Proxmox) Name() string { return "proxmox" }

func (p *Proxmox) bridge() string {
	if p.Cfg.ProxmoxBridge != "" {
		return p.Cfg.ProxmoxBridge
	}
	return pveDefaultBridge
}

func (p *Proxmox) subnet() string {
	if p.Cfg.ProxmoxSubnet != "" {
		return p.Cfg.ProxmoxSubnet
	}
	return pveDefaultSubnet
}

func (p *Proxmox) gateway() string {
	if p.Cfg.ProxmoxGateway != "" {
		return p.Cfg.ProxmoxGateway
	}
	return p.subnet() + ".0.1"
}

func (p *Proxmox) vmidBase() int {
	if p.Cfg.ProxmoxVMIDBase != 0 {
		return p.Cfg.ProxmoxVMIDBase
	}
	return pveDefaultVMIDBase
}

// AddressFor derives a guest's address from its vmid.
//
// Deterministic addressing is worth the constraint of a reserved subnet: the
// alternative is DHCP plus polling the guest agent, which adds a wait, a
// failure mode, and a race to every command that touches a guest.
//
// The subnet is a /16 so the whole vmid range maps. A /24 would cap it at 254
// and, worse, produce addresses like 10.10.0.312 for anything above that.
func (p *Proxmox) AddressFor(vmid int) (string, error) {
	host := vmid - p.vmidBase()
	if host < pveMinHost || host > pveMaxHost {
		return "", fmt.Errorf("vmid %d is outside the ccvm range %d-%d, so it has no address",
			vmid, p.vmidBase()+pveMinHost, p.vmidBase()+pveMaxHost)
	}
	return fmt.Sprintf("%s.%d.%d", p.subnet(), host/256, host%256), nil
}

func (p *Proxmox) Preflight(ctx context.Context, s Spec) error {
	if err := p.ready(); err != nil {
		return err
	}
	if _, err := p.API.nodes(ctx); err != nil {
		var pe *pveError
		if asPVEError(err, &pe) {
			switch pe.Status {
			case 401:
				return fmt.Errorf("Proxmox rejected token %q: %w", p.Cfg.ProxmoxTokenID, err)
			case 403:
				return fmt.Errorf("token %q lacks the needed privileges (VM.Clone on the template, VM.Allocate on new guests): %w",
					p.Cfg.ProxmoxTokenID, err)
			}
		}
		return fmt.Errorf("Proxmox API at %s is not reachable: %w", p.Cfg.ProxmoxURL, err)
	}

	if s.Image == "" {
		return fmt.Errorf("profile %q has no [backend.proxmox] template", s.Profile)
	}
	tplID, err := strconv.Atoi(s.Image)
	if err != nil {
		return fmt.Errorf("proxmox template must be a vmid, got %q", s.Image)
	}

	res, err := p.API.resources(ctx)
	if err != nil {
		return err
	}
	want := p.kindFor(s)
	for _, r := range res {
		if r.VMID == tplID {
			if r.Template == 0 {
				return fmt.Errorf("vmid %d exists but is not a template; convert it with `pct template %d`", tplID, tplID)
			}
			// Cloning a qemu template through the lxc endpoints fails deep in
			// the API with a bare 500, so mismatches are named here instead.
			if r.Type != "" && r.Type != want {
				return fmt.Errorf("vmid %d is a %s template but %s was requested; %s",
					tplID, r.Type, want, vmKindHint(want))
			}
			return nil
		}
	}
	return fmt.Errorf("template vmid %d does not exist on this cluster", tplID)
}

// kindFor picks the guest technology for a new machine. --vm sets Spec.Kind;
// p.Kind is the backend-wide override tests use.
func kindOrDefault(kinds ...string) string {
	for _, k := range kinds {
		if k != "" {
			return k
		}
	}
	return "lxc"
}

func (p *Proxmox) kindFor(s Spec) string { return kindOrDefault(s.Kind, p.Kind) }

// kindOf picks it for an existing machine. Start, Stop, and Destroy are handed
// a Handle rather than a Spec, and a qemu guest answers on different endpoints
// than an lxc one, so the handle has to carry which it is.
func (p *Proxmox) kindOf(h Handle) string { return kindOrDefault(h.Kind, p.Kind) }

// Create clones the template.
//
// nextid is advisory rather than a reservation, so a concurrent create can take
// the id between asking and cloning. That specific collision retries with a
// fresh id; auth and permission failures do not.
func (p *Proxmox) Create(ctx context.Context, s Spec) (Handle, error) {
	if err := p.ready(); err != nil {
		return Handle{}, err
	}
	tplID, err := strconv.Atoi(s.Image)
	if err != nil {
		return Handle{}, fmt.Errorf("proxmox template must be a vmid, got %q", s.Image)
	}

	node := p.Cfg.ProxmoxNode
	if node == "" {
		if node, err = p.API.pickNode(ctx); err != nil {
			return Handle{}, err
		}
	}
	kind := p.kindFor(s)

	var lastErr error
	for range pveCloneAttempts {
		vmid, err := p.nextFreeID(ctx)
		if err != nil {
			return Handle{}, err
		}

		form := url.Values{
			"newid":    []string{strconv.Itoa(vmid)},
			"hostname": []string{s.Name},
			// Linked clone: near-instant and near-free, when the storage
			// supports it. ZFS and Ceph do; directory and LVM storage do not.
			"full": []string{"0"},
		}
		if p.Cfg.ProxmoxStorage != "" {
			form.Set("storage", p.Cfg.ProxmoxStorage)
		}

		task, err := p.API.clone(ctx, node, kind, tplID, vmid, form)
		if err == nil {
			err = p.API.waitTask(ctx, node, task, pveTaskTimeout)
		}

		// Storage that cannot do linked clones says so rather than doing a full
		// one, so the retry is ours to make. It is slower and uses real disk,
		// which is worth saying out loud rather than silently absorbing.
		if err != nil && isLinkedCloneUnsupported(err) {
			p.logf("storage does not support linked clones; falling back to a full clone of %d (slower)", tplID)
			form.Set("full", "1")
			task, err = p.API.clone(ctx, node, kind, tplID, vmid, form)
			if err == nil {
				err = p.API.waitTask(ctx, node, task, pveTaskTimeout)
			}
		}

		if err != nil {
			if isVMIDTaken(err) {
				lastErr = err
				continue
			}
			return Handle{}, fmt.Errorf("clone template %d on %s: %w", tplID, node, err)
		}

		h := Handle{Backend: "proxmox", Name: s.Name, ID: strconv.Itoa(vmid), Node: node, Kind: kind}
		if err := p.configure(ctx, h, s, kind, vmid); err != nil {
			// The guest exists but is unusable; leave nothing behind.
			if task, derr := p.API.destroy(ctx, node, kind, vmid); derr == nil {
				_ = p.API.waitTask(ctx, node, task, pveTaskTimeout)
			}
			return Handle{}, err
		}
		return h, nil
	}
	return Handle{}, fmt.Errorf("could not allocate a free vmid after %d attempts: %w", pveCloneAttempts, lastErr)
}

// nextFreeID picks a vmid inside ccvm's reserved range.
//
// /cluster/nextid returns the cluster's next free id, which on any cluster with
// existing guests is nothing to do with ccvm's range — and a guest outside that
// range has no derivable address. So the range is scanned here instead.
//
// Like nextid this is advisory rather than a reservation: a concurrent create
// can take the id before the clone lands, which is why that collision retries.
func (p *Proxmox) nextFreeID(ctx context.Context) (int, error) {
	res, err := p.API.resources(ctx)
	if err != nil {
		return 0, err
	}
	used := make(map[int]bool, len(res))
	for _, r := range res {
		used[r.VMID] = true
	}

	base := p.vmidBase()
	for host := pveMinHost; host <= pveMaxHost; host++ {
		if !used[base+host] {
			return base + host, nil
		}
	}
	return 0, fmt.Errorf("no free vmid in the ccvm range %d-%d; destroy some machines with `ccvm rm`",
		base+pveMinHost, base+pveMaxHost)
}

// configure sets the guest's address and the metadata the listing reads back.
func (p *Proxmox) configure(ctx context.Context, h Handle, s Spec, kind string, vmid int) error {
	addr, err := p.AddressFor(vmid)
	if err != nil {
		return err
	}

	form := url.Values{}
	if kind == "lxc" {
		form.Set("net0", fmt.Sprintf("name=eth0,bridge=%s,ip=%s/%d,gw=%s",
			p.bridge(), addr, pveSubnetMask, p.gateway()))
		form.Set("hostname", s.Name)
		// Docker inside an unprivileged container needs both.
		form.Set("features", "nesting=1,keyctl=1")
	} else {
		form.Set("ipconfig0", fmt.Sprintf("ip=%s/%d,gw=%s", addr, pveSubnetMask, p.gateway()))
	}
	form.Set("tags", LabelOwner)
	form.Set("description", pveDescription(s))

	if err := p.API.setConfig(ctx, h.Node, kind, vmid, form); err != nil {
		return fmt.Errorf("configure guest %d: %w", vmid, err)
	}
	return nil
}

func (p *Proxmox) Start(ctx context.Context, h Handle) error {
	vmid, err := strconv.Atoi(h.ID)
	if err != nil {
		return err
	}
	task, err := p.API.status(ctx, h.Node, p.kindOf(h), vmid, "start")
	if err != nil {
		return err
	}
	return p.API.waitTask(ctx, h.Node, task, pveTaskTimeout)
}

// Wait blocks until the guest answers over ssh. A running guest is not the same
// as a reachable one: the kernel is up long before sshd is.
func (p *Proxmox) Wait(ctx context.Context, h Handle) error {
	timeout := p.WaitTimeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := p.Exec(ctx, h, "true"); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("guest %s did not become reachable over ssh: %w\n"+
		"Unlike the other backends, proxmox reaches a guest only over ssh, so it cannot "+
		"install its own key first. The template must already contain ccvm's public key "+
		"in /root/.ssh/authorized_keys — see `ccvm profiles build --backend proxmox`",
		h.Name, last)
}

// SSHTarget is the guest's derived address. Proxmox guests are real hosts on
// the network, so nothing is forwarded and no port is allocated.
func (p *Proxmox) SSHTarget(h Handle) string {
	vmid, err := strconv.Atoi(h.ID)
	if err != nil {
		return h.Name
	}
	addr, err := p.AddressFor(vmid)
	if err != nil {
		return h.Name
	}
	return "root@" + addr
}

func (p *Proxmox) sshArgs(h Handle) []string {
	args := []string{"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
	}
	if p.Cfg.ProxmoxSSHKey != "" {
		args = append(args, "-i", p.Cfg.ProxmoxSSHKey)
	}
	return append(args, p.SSHTarget(h))
}

func (p *Proxmox) Exec(ctx context.Context, h Handle, argv ...string) ([]byte, error) {
	// ssh joins its remote arguments with spaces and hands the result to the
	// login shell, so the argv has to be quoted into a single command string.
	// Passing the words through unquoted loses the grouping: `sh -c "echo hi"`
	// arrives as `sh -c echo hi`, which runs echo with no arguments and
	// returns nothing rather than failing.
	//
	// The other backends take an argv array directly, so this is proxmox's
	// alone.
	return p.Runner.Run(ctx, append(p.sshArgs(h), run.ShellQuote(argv))...)
}

func (p *Proxmox) Push(ctx context.Context, h Handle, src, dst string) error {
	// The session record is metadata, not a file: it has to be readable when
	// the guest is off, and ssh cannot reach a stopped guest. Proxmox has a
	// mutable field that is readable either way, so the record lives there.
	if dst == SessionFile {
		return p.writeRecord(ctx, h, src)
	}
	args := p.scpArgs()
	args = append(args, src, p.SSHTarget(h)+":"+dst)
	_, err := p.Runner.Run(ctx, args...)
	return err
}

func (p *Proxmox) Pull(ctx context.Context, h Handle, src, dst string) error {
	if src == SessionFile {
		return p.readRecord(ctx, h, dst)
	}
	args := p.scpArgs()
	args = append(args, p.SSHTarget(h)+":"+src, dst)
	_, err := p.Runner.Run(ctx, args...)
	return err
}

func (p *Proxmox) scpArgs() []string {
	args := []string{"scp",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
	if p.Cfg.ProxmoxSSHKey != "" {
		args = append(args, "-i", p.Cfg.ProxmoxSSHKey)
	}
	return args
}

// writeRecord stores the session record in the guest's description field.
func (p *Proxmox) writeRecord(ctx context.Context, h Handle, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	vmid, err := strconv.Atoi(h.ID)
	if err != nil {
		return err
	}
	form := url.Values{"description": []string{string(data)}}
	return p.API.setConfig(ctx, h.Node, p.kindOf(h), vmid, form)
}

func (p *Proxmox) readRecord(ctx context.Context, h Handle, dst string) error {
	vmid, err := strconv.Atoi(h.ID)
	if err != nil {
		return err
	}
	cfg, err := p.API.getConfig(ctx, h.Node, p.kindOf(h), vmid)
	if err != nil {
		return err
	}
	desc, _ := cfg["description"].(string)
	if strings.TrimSpace(desc) == "" {
		return fmt.Errorf("guest %s has no session record", h.Name)
	}
	return os.WriteFile(dst, []byte(desc), 0o644)
}

func (p *Proxmox) List(ctx context.Context) ([]Machine, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	res, err := p.API.resources(ctx)
	if err != nil {
		return nil, err
	}
	var out []Machine
	for _, r := range res {
		if r.Template != 0 || !hasTag(r.Tags, LabelOwner) {
			continue
		}
		m := Machine{
			Name:    r.Name,
			Backend: "proxmox",
			ID:      strconv.Itoa(r.VMID),
			Node:    r.Node,
			State:   normalizePVEState(r.Status),
			// The cluster already knows whether this is a container or a VM,
			// so a machine discovered here can be stopped and destroyed
			// without the caller having to remember how it was made.
			Kind: r.Type,
		}
		// The aggregate lags reality by several seconds, so a machine created
		// moments ago still reads as stopped there. Enumerate from it, then ask
		// each guest for its actual state.
		if live, err := p.API.currentStatus(ctx, r.Node, kindOrDefault(r.Type, p.Kind), r.VMID); err == nil {
			m.State = normalizePVEState(live)
		}
		if addr, err := p.AddressFor(r.VMID); err == nil {
			m.SSH = "root@" + addr
		}
		out = append(out, m)
	}
	return out, nil
}

func normalizePVEState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "running":
		return StateRunning
	case "stopped":
		return StateStopped
	case "paused", "suspended":
		return StatePending
	default:
		return StateUnknown
	}
}

func hasTag(tags, want string) bool {
	for _, t := range strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' }) {
		if strings.TrimSpace(t) == want {
			return true
		}
	}
	return false
}

func (p *Proxmox) Stop(ctx context.Context, h Handle) error {
	vmid, err := strconv.Atoi(h.ID)
	if err != nil {
		return err
	}
	task, err := p.API.status(ctx, h.Node, p.kindOf(h), vmid, "shutdown")
	if err != nil {
		return err
	}
	return p.API.waitTask(ctx, h.Node, task, pveTaskTimeout)
}

func (p *Proxmox) Destroy(ctx context.Context, h Handle) error {
	vmid, err := strconv.Atoi(h.ID)
	if err != nil {
		return err
	}
	// A running guest cannot be destroyed; stopping first is not optional, and
	// a failure here is not worth aborting teardown over.
	if task, err := p.API.status(ctx, h.Node, p.kindOf(h), vmid, "stop"); err == nil {
		_ = p.API.waitTask(ctx, h.Node, task, pveTaskTimeout)
	}
	task, err := p.API.destroy(ctx, h.Node, p.kindOf(h), vmid)
	if err != nil {
		return err
	}
	return p.API.waitTask(ctx, h.Node, task, pveTaskTimeout)
}

func (p *Proxmox) logf(format string, args ...any) {
	if p.Log == nil {
		return
	}
	fmt.Fprintf(p.Log, "ccvm: "+format+"\n", args...)
}

func pveDescription(s Spec) string {
	return fmt.Sprintf("ccvm session %s\nproject: %s\nprofile: %s\n", s.Name, s.Project, s.Profile)
}

// isLinkedCloneUnsupported matches the storage refusing a linked clone, which
// is a property of the storage type rather than a failure worth reporting.
func isLinkedCloneUnsupported(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "linked clone feature") &&
		strings.Contains(msg, "not available")
}

func isVMIDTaken(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "config file already exists")
}

func asPVEError(err error, target **pveError) bool {
	for err != nil {
		if pe, ok := err.(*pveError); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func init() {
	Register("proxmox", func(e run.Execer, cfg Config) (Backend, error) {
		return NewProxmox(e, cfg), nil
	})
}

// NextIDForTest and PickNodeForTest expose two control-plane calls to the
// integration suite. They exist because containerized Proxmox in CI can
// exercise the API but not boot a guest, so these are the only way to assert
// against a real pvedaemon rather than a frozen fixture.
func (p *Proxmox) NextIDForTest(ctx context.Context) (int, error) {
	if err := p.ready(); err != nil {
		return 0, err
	}
	return p.API.nextID(ctx)
}

func (p *Proxmox) PickNodeForTest(ctx context.Context) (string, error) {
	if err := p.ready(); err != nil {
		return "", err
	}
	return p.API.pickNode(ctx)
}

// PVEErrorIsFatalForTest exposes the retryability classification, which is
// otherwise reachable only through a failed request.
func PVEErrorIsFatalForTest(err error) bool {
	var pe *pveError
	if asPVEError(err, &pe) {
		return pe.Fatal()
	}
	return false
}

// vmKindHint says which way the mismatch goes, since the fix differs: one is a
// missing flag, the other is a template id in the wrong profile field.
func vmKindHint(want string) string {
	if want == "qemu" {
		return "point [backend.proxmox].vm_template at a VM template"
	}
	return "pass --vm to use the VM template, or point lxc_template at a container template"
}

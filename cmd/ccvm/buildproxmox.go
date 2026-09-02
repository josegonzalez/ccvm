package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/josegonzalez/ccvm/internal/profile"
	"github.com/josegonzalez/ccvm/internal/run"
)

// buildProxmoxTemplate bakes the template sessions are cloned from.
//
// Everything here runs through `pct` on a cluster node rather than through the
// API, because the API can create and destroy a guest but cannot run a command
// inside one - and the first template has no ccvm key in it, so there is no way
// in over ssh either. That chicken-and-egg is why this was a manual recipe; it
// is automated here rather than removed, since the same node access is already
// assumed by the reaper's cron install.
func (a *app) buildProxmoxTemplate(name string, cfg *profile.Config) error {
	vmid := cfg.Backend["proxmox"].LXCTemplate
	if vmid == 0 {
		return fmt.Errorf("profile %q has no [backend.proxmox].lxc_template to build", name)
	}
	node := a.backendCfg.ProxmoxNodeSSH
	if node == "" {
		return fmt.Errorf("set CCVM_PROXMOX_NODE_SSH to a root ssh target for a cluster node, e.g. root@pve1; " +
			"building a template runs `pct` there, since the API cannot run a command inside a guest")
	}
	osTemplate := a.backendCfg.ProxmoxOSTemplate
	if osTemplate == "" {
		return fmt.Errorf("set CCVM_PROXMOX_OSTEMPLATE to the distro tarball to build from, e.g. " +
			"local:vztmpl/debian-13-standard_13.6-1_amd64.tar.zst; `pveam available` lists them")
	}

	dir, cleanup, err := a.stageProfile(name)
	if err != nil {
		return err
	}
	defer cleanup()

	script := filepath.Join(dir, "build.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("profile %q has no build.sh, so there is nothing to bake into a template", name)
	}
	pub, err := a.sshKey.AuthorizedKey()
	if err != nil {
		return err
	}

	id := strconv.Itoa(vmid)
	// A leftover from a failed build would otherwise be cloned from, or block
	// the create outright.
	if _, err := a.nodeRun(node, "pct", "destroy", id, "--force", "1", "--purge", "1"); err == nil {
		fmt.Fprintf(a.out, "replaced the existing %s template\n", id)
	}

	fmt.Fprintf(a.out, "creating %s from %s\n", id, osTemplate)
	create := []string{
		"pct", "create", id, osTemplate,
		"--hostname", "ccvm-" + name,
		"--cores", strconv.Itoa(maxInt(cfg.Resources.CPUs, 2)),
		"--memory", strconv.Itoa(memoryMB(cfg.Resources.Memory)),
		"--rootfs", storageFor(a.backendCfg.ProxmoxStorage) + ":" + strconv.Itoa(diskGB(cfg.Resources.Disk)),
		"--net0", "name=eth0,bridge=" + bridgeFor(a.backendCfg.ProxmoxBridge) + ",ip=dhcp",
		// nesting and keyctl so a session can run containers and use a keyring,
		// matching what spawned guests get.
		"--features", "nesting=1,keyctl=1",
		"--unprivileged", "1",
	}
	if _, err := a.nodeRun(node, create...); err != nil {
		return fmt.Errorf("create the template container: %w", err)
	}
	if _, err := a.nodeRun(node, "pct", "start", id); err != nil {
		return fmt.Errorf("start the template container: %w", err)
	}

	fmt.Fprintln(a.out, "installing claude, git, tmux, and sshd (a few minutes)")
	if err := a.pctPush(node, id, script, "/root/build.sh", "0755"); err != nil {
		return err
	}
	if _, err := a.nodeRun(node, "pct", "exec", id, "--", "sh", "/root/build.sh"); err != nil {
		return fmt.Errorf("provision the template: %w", err)
	}

	// Without this every guest cloned from the template comes up unreachable:
	// proxmox is the one backend where ccvm cannot install its own key first,
	// because it has no way in until the key is already there.
	if _, err := a.nodeRun(node, "pct", "exec", id, "--", "mkdir", "-p", "/root/.ssh"); err != nil {
		return err
	}
	if err := a.pctPushString(node, id, pub, "/root/.ssh/authorized_keys", "0600"); err != nil {
		return err
	}
	if _, err := a.nodeRun(node, "pct", "exec", id, "--", "chown", "-R", "root:root", "/root/.ssh"); err != nil {
		return err
	}

	// The guest binaries, so ccvm-done works from inside a session on this
	// backend the way it does on the others.
	arch, err := a.nodeArch(node, id)
	if err != nil {
		return err
	}
	for _, bin := range guestBinaries {
		src, err := guestBinaryPath(bin, arch)
		if err != nil {
			fmt.Fprintf(a.err, "ccvm: %v\n", err)
			continue
		}
		if err := a.pctPush(node, id, src, "/usr/local/bin/"+bin, "0755"); err != nil {
			return err
		}
	}

	if _, err := a.nodeRun(node, "pct", "stop", id); err != nil {
		return fmt.Errorf("stop the template container: %w", err)
	}
	if _, err := a.nodeRun(node, "pct", "template", id); err != nil {
		return fmt.Errorf("convert %s into a template: %w", id, err)
	}

	fmt.Fprintf(a.out, "built %s; sessions clone from it\n", id)
	return nil
}

// nodeRun runs a command on the cluster node over ssh.
func (a *app) nodeRun(node string, argv ...string) ([]byte, error) {
	ssh := []string{"ssh", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR", node}
	return a.runner.Run(a.ctx, append(ssh, run.ShellQuote(argv))...)
}

// pctPush copies a host file into the container, by way of the node.
func (a *app) pctPush(node, vmid, src, dst, mode string) error {
	staged := "/tmp/ccvm-" + filepath.Base(dst)
	scp := []string{"scp", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR", src, node + ":" + staged}
	if _, err := a.runner.Run(a.ctx, scp...); err != nil {
		return fmt.Errorf("copy %s to the node: %w", filepath.Base(src), err)
	}
	if _, err := a.nodeRun(node, "pct", "push", vmid, staged, dst, "--perms", mode); err != nil {
		return fmt.Errorf("push %s into the template: %w", filepath.Base(dst), err)
	}
	_, _ = a.nodeRun(node, "rm", "-f", staged)
	return nil
}

// pctPushString writes content into the container without staging a host file.
func (a *app) pctPushString(node, vmid, content, dst, mode string) error {
	staged := "/tmp/ccvm-" + filepath.Base(dst)
	if _, err := a.nodeRun(node, "sh", "-c", "cat > "+staged+" <<'CCVMEOF'\n"+content+"\nCCVMEOF"); err != nil {
		return fmt.Errorf("stage %s on the node: %w", filepath.Base(dst), err)
	}
	if _, err := a.nodeRun(node, "pct", "push", vmid, staged, dst, "--perms", mode); err != nil {
		return fmt.Errorf("push %s into the template: %w", filepath.Base(dst), err)
	}
	_, _ = a.nodeRun(node, "rm", "-f", staged)
	return nil
}

// nodeArch asks the container what it runs, so the guest binaries match it.
func (a *app) nodeArch(node, vmid string) (string, error) {
	out, err := a.nodeRun(node, "pct", "exec", vmid, "--", "uname", "-m")
	if err != nil {
		return "", fmt.Errorf("determine the template's architecture: %w", err)
	}
	switch m := strings.TrimSpace(string(out)); m {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported template architecture %q", m)
	}
}

func storageFor(s string) string {
	if s == "" {
		return "local-lvm"
	}
	return s
}

func bridgeFor(s string) string {
	if s == "" {
		return "vmbr0"
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// memoryMB reads a profile's memory string as megabytes, which is what pct
// wants. An unparseable value falls back rather than failing the build.
func memoryMB(s string) int {
	s = strings.TrimSpace(strings.ToUpper(s))
	switch {
	case strings.HasSuffix(s, "G"):
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "G")); err == nil {
			return n * 1024
		}
	case strings.HasSuffix(s, "M"):
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "M")); err == nil {
			return n
		}
	}
	return 2048
}

// diskGB reads a profile's disk string as gigabytes, which is what pct wants.
func diskGB(s string) int {
	s = strings.TrimSpace(strings.ToUpper(s))
	if before, ok := strings.CutSuffix(s, "G"); ok {
		if n, err := strconv.Atoi(before); err == nil {
			return n
		}
	}
	return 8
}

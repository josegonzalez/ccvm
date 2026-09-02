package code

import (
	"context"
	"fmt"
	"os/user"
	"strconv"
	"strings"
)

// sshfsGuestPort is the port inside the guest that the reverse tunnel binds.
//
// Fixed rather than allocated: each session has its own machine, so two
// sessions cannot collide on it, and a deterministic port keeps the mount
// command reproducible when something goes wrong and you want to run it by
// hand.
const sshfsGuestPort = 2222

// mountSSHFS makes the host's project visible inside the guest, live.
//
// The direction is the opposite of everything else here. `mount` works by
// attaching a directory the host and guest already share, which a remote
// cluster has none of. So the guest mounts the host instead, over a reverse
// tunnel back through the connection ccvm already has - which means the host
// has to be running sshd, and the tunnel has to stay up for as long as the
// mount is used.
func mountSSHFS(ctx context.Context, o Options) error {
	if _, err := o.Backend.Exec(ctx, o.Handle, "sh", "-c", "command -v sshfs >/dev/null"); err != nil {
		return fmt.Errorf("the guest has no sshfs, which --code sshfs needs: "+
			"install it in the image (apt-get install -y sshfs), or use --code rsync (%w)", err)
	}
	if _, err := o.Backend.Exec(ctx, o.Handle, "mkdir", "-p", o.WorkDir); err != nil {
		return err
	}

	who, err := hostUser()
	if err != nil {
		return err
	}

	// allow_other so a process running as another user in the guest can read
	// the tree; reconnect so a dropped tunnel does not wedge the mount.
	opts := strings.Join([]string{
		"StrictHostKeyChecking=no",
		"UserKnownHostsFile=/dev/null",
		"reconnect",
		"allow_other",
		"ServerAliveInterval=15",
	}, ",")

	src := fmt.Sprintf("%s@127.0.0.1:%s", who, strings.TrimSuffix(o.Project, "/"))
	argv := []string{
		"sshfs", "-o", opts,
		"-p", strconv.Itoa(sshfsGuestPort),
		src, o.WorkDir,
	}
	if _, err := o.Backend.Exec(ctx, o.Handle, argv...); err != nil {
		return fmt.Errorf("mount %s in the guest over the reverse tunnel: %w\n"+
			"This needs Remote Login enabled on this machine, since the guest "+
			"connects back to it. Use --code rsync if that is not an option", o.Project, err)
	}
	return nil
}

// SSHFSTunnelArgs renders the reverse tunnel the mount depends on.
//
// Exported so the caller can hold it open: the mount is live only while the
// tunnel is, which is the same shape as the kubernetes port-forward.
func SSHFSTunnelArgs(target, identityFile string) []string {
	argv := []string{
		"ssh", "-N",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		// The guest's port 2222 reaches this machine's sshd.
		"-R", fmt.Sprintf("%d:localhost:22", sshfsGuestPort),
	}
	if identityFile != "" {
		argv = append(argv, "-i", identityFile)
	}
	return append(argv, target)
}

func hostUser() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("determine the user the guest should connect back as: %w", err)
	}
	return u.Username, nil
}

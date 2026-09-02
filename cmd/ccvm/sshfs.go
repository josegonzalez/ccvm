package main

import (
	"fmt"
	"os/exec"
	"time"

	"github.com/josegonzalez/ccvm/internal/code"
	"github.com/josegonzalez/ccvm/internal/run"
)

// sshfsTunnel is the reverse ssh connection a --code sshfs mount rides on.
//
// It is held by this process for the life of the session, the same shape as the
// kubernetes port-forward: the mount is live only while the tunnel is, and
// pretending otherwise would leave a session reading a directory that silently
// stopped updating.
type sshfsTunnel struct {
	cmd *exec.Cmd
}

func startSSHFSTunnel(a *app, target string) (*sshfsTunnel, error) {
	if target == "" {
		return nil, fmt.Errorf("no ssh target for the machine")
	}
	argv := code.SSHFSTunnelArgs(target, a.sshKey.Private)
	if a.verbose {
		fmt.Fprintf(a.err, "+ %s\n", run.ShellQuote(argv))
	}

	cmd := exec.CommandContext(a.ctx, argv[0], argv[1:]...)
	cmd.Stderr = a.err
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("open the reverse tunnel: %w", err)
	}

	// ExitOnForwardFailure makes ssh exit rather than run without the forward,
	// so a short wait distinguishes "bound" from "refused" before anything
	// tries to mount through it.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return nil, fmt.Errorf("the reverse tunnel closed immediately: %w", err)
	case <-time.After(750 * time.Millisecond):
	}

	return &sshfsTunnel{cmd: cmd}, nil
}

func (t *sshfsTunnel) Stop() {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return
	}
	_ = t.cmd.Process.Kill()
}

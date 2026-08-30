#!/bin/sh
# Provisions a ccvm base machine. Runs as root inside the guest.
#
# This is the same script `ccvm profiles build` runs to bake a template and that
# spawn-time provisioning runs on a live machine, so it has to be idempotent.
set -eu

export DEBIAN_FRONTEND=noninteractive

apt-get update -qq
apt-get install -y -qq --no-install-recommends \
    ca-certificates curl git openssh-server tmux ripgrep jq rsync \
    build-essential procps less sudo

# Claude Code. Pinned by the caller when CLAUDE_VERSION is set, so a template
# rebuild does not silently change the agent version under existing sessions.
if ! command -v claude >/dev/null 2>&1; then
    curl -fsSL https://claude.ai/install.sh | bash -s -- "${CLAUDE_VERSION:-latest}"
    ln -sf /root/.local/bin/claude /usr/local/bin/claude
fi
claude --version

# sshd host keys and the directories ccvm writes into at spawn.
ssh-keygen -A
mkdir -p /run/sshd /root/.ssh /etc/ccvm /run/ccvm /work
chmod 700 /root/.ssh

# The only way in is the key ccvm installs per session.
mkdir -p /etc/ssh/sshd_config.d
printf '%s\n' \
    'PermitRootLogin prohibit-password' \
    'PasswordAuthentication no' \
    'UseDNS no' \
    'AcceptEnv LANG LC_*' \
    > /etc/ssh/sshd_config.d/ccvm.conf

systemctl enable ssh 2>/dev/null || true
systemctl restart ssh 2>/dev/null || service ssh restart 2>/dev/null || true

# The ccvm base session image.
#
# Build with `make image`, which stages the guest binaries into the context
# first — they are compiled from this repo rather than fetched.
FROM debian:trixie-slim

ARG TARGETARCH
ARG CLAUDE_VERSION=latest

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git openssh-server tmux ripgrep jq \
      build-essential procps less sudo \
    && rm -rf /var/lib/apt/lists/*

# Claude Code, pinned. An image that silently tracks latest makes sessions
# irreproducible and version-skew bugs impossible to bisect.
RUN curl -fsSL https://claude.ai/install.sh | bash -s -- "${CLAUDE_VERSION}" \
    && ln -sf /root/.local/bin/claude /usr/local/bin/claude \
    && claude --version

# sshd needs its host keys and a place to put its runtime socket. Password auth
# stays off: the only way in is the key ccvm installs.
RUN ssh-keygen -A \
    && mkdir -p /run/sshd /root/.ssh /etc/ccvm /run/ccvm \
    && chmod 700 /root/.ssh \
    && printf '%s\n' \
        'PermitRootLogin prohibit-password' \
        'PasswordAuthentication no' \
        'ChallengeResponseAuthentication no' \
        'UseDNS no' \
        'AcceptEnv LANG LC_*' \
        > /etc/ssh/sshd_config.d/ccvm.conf

# The guest binaries, cross-compiled from this repo by `make image`.
COPY dist/ccvm-done-linux-${TARGETARCH} /usr/local/bin/ccvm-done
COPY dist/ccvm-init-linux-${TARGETARCH} /usr/local/bin/ccvm-init
RUN chmod 0755 /usr/local/bin/ccvm-done /usr/local/bin/ccvm-init

# Seeded so Claude discovers ccvm-done without being told the command name. A
# command the model does not know about is not reachable from inside a session.
COPY guest/CLAUDE.md /root/.claude/CLAUDE.md

WORKDIR /work

# PID 1 waits on the done sentinel, never on the session — see cmd/ccvm-init.
ENTRYPOINT ["/usr/local/bin/ccvm-init"]

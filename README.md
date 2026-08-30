# ccvm

Run every Claude Code session in its own disposable machine.

A session gets a fresh container or VM, its own copy of the code, and is thrown
away when you are done. That makes `--dangerously-skip-permissions` a reasonable
thing to hand it, because the blast radius is a machine you were about to
delete.

## Backends

| | Spawn | Isolation | Survives Mac sleep | Reached by |
| --- | --- | --- | --- | --- |
| `docker` | ~1s | weakest: shared kernel, same host | no | forwarded loopback port |
| `orbstack` | seconds, copy-on-write clone | a real Linux machine, `--isolated` | no | `<name>@orb`, built in |
| `proxmox` | 1-3s LXC, longer for a VM | separate kernel or namespace | yes | a real address on your network |
| `k8s` | seconds | pod, plus whatever the cluster enforces | yes | `kubectl port-forward` |

All four are implemented. Proxmox needs `CCVM_PROXMOX_URL`, `CCVM_PROXMOX_TOKEN_ID`,
and `CCVM_PROXMOX_SECRET`; k8s uses your current kubectl context. `ccvm doctor`
reports which are ready.

Two limitations worth knowing. Reaching a k8s session over ssh needs a
`kubectl port-forward` you run yourself — ccvm does not yet supervise one, and
it is a foreground process that dies on network blips. And proxmox guest boot
is only exercised by hand; see Testing.

## Install

```
make build          # binaries into dist/
make image          # the base session image
```

## Use

```
ccvm up                      # a machine for the current directory
ccvm up ~/src/foo --keep     # one that survives the session
ccvm ls                      # what is running
ccvm ssh cc-foo              # a shell in it
ccvm attach cc-foo           # back to its Claude session
ccvm rm cc-foo               # destroy it
ccvm gc --dry-run            # what the reaper would collect
ccvm doctor                  # can a machine be created right now?
```

From inside a session, `ccvm-done` ends it and destroys the machine.
`ccvm-done --keep` just detaches.

`--verbose` prints every backend command verbatim, shell-quoted so you can
re-run it.

## Profiles

A profile is a directory holding a `profile.toml` and, optionally, the files
needed to build its image for each backend.

```toml
description = "Go toolchain, delve, golangci-lint"
extends     = "base"

[backend.docker]
image = "ccvm/go:latest"

[resources]
cpus = 4; memory = "8G"

[provision]
packages = ["delve", "golangci-lint"]
```

Configuration is layered, later winning: built-in defaults, the `extends` chain,
the profile, `~/.config/ccvm/profile.toml`, the project's `.ccvm/profile.toml`,
then flags. Scalars override, `[env]` and `[backend.*]` merge per key, and
`[provision].packages` accumulates down the chain unless a layer sets
`packages_replace`.

A project's `.ccvm/profile.toml` may size a machine but may not set
`[backend.*]` or `[env]`. Parsing TOML already stops a repository executing
code; the restriction stops one choosing the image your session runs.

## Two things worth knowing

**The guest holds no infrastructure credentials.** `ccvm-done` touches a
sentinel file and something already holding credentials acts on it. A machine
that could destroy machines could destroy ones that are not its own.

**A claude.ai login cannot be shared, so `--remote-control` is one session at a
time.** This is measured, not cautious: refreshing the token rotates it, and
every other copy stops working. In testing, one session refreshing invalidated
two sibling sessions, the broker machine, and the host's own stored copy —
all with `OAuth session expired and could not be refreshed`, hours after the
sessions started.

ccvm therefore treats the login as movable rather than copyable. Starting a
second concurrent `--remote-control` session is refused, and ending one carries
the possibly-rotated credential back to the host so the next session can use it.
If a machine holding the login is lost before that happens, recover with
`ccvm creds import <machine>`. The default token path has no such constraint —
run as many concurrent sessions as you like.

**`ccvm keep` exempts a machine from the reaper, not from its backend.** Docker
fixes auto-remove when a container is created, so a machine started without
`--keep` will still be deleted if it stops. `ccvm keep` says so rather than
implying a durability it cannot deliver.

## Testing

```
make test           # unit, with -race
make cover          # coverage of shipped code
make itest          # integration; see CCVM_ITEST_BACKENDS
```

Integration skips are opt-in: name the backends in `CCVM_ITEST_BACKENDS` and the
suite fails, rather than skips, if one is unavailable. A suite that silently
skips reports green while testing nothing.

The suite asserts the Backend contract rather than any one implementation, so a
backend inherits all of it by registering. Where a backend genuinely cannot meet
a claim, it is skipped with the reason: k8s reports pull failures from the pod's
events rather than from preflight, and a stopped k8s session has no filesystem
to read a TTL back from.

CI covers docker and k8s, the latter on `kind`. Proxmox gets its control plane
exercised against a containerized PVE pinned by digest, which catches upstream
API drift that frozen fixtures cannot — they keep passing against a release that
changed a response shape. That job is non-gating: it depends on someone else's
repackaging of a distro, and a flaky required check is one you learn to ignore.

Hosted runners have no nested virtualization, so **proxmox guest boot is never
tested in CI**, and OrbStack has no automated coverage at all — it is macOS-only
and GUI-installed. A green badge does not cover either. `make itest-local` is
the only check, and it is a process dependency rather than a guarantee.

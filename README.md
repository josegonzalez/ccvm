# ccvm

Run every Claude Code session in its own disposable machine.

A session gets a fresh container or VM, its own copy of the code, and is thrown
away when you are done. That makes `--dangerously-skip-permissions` a reasonable
thing to hand it, because the blast radius is a machine you were about to
delete.

## Status

The docker backend works end to end. orbstack, proxmox, and k8s are designed
and their profiles ship, but their backends are not implemented yet.

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

CI covers docker and k8s. Proxmox gets its control plane exercised against a
containerized PVE, which catches upstream API drift that frozen fixtures cannot.
OrbStack has no automated coverage at all — it is macOS-only and GUI-installed —
so a green badge does not cover it, or proxmox guest boot.

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

One limitation worth knowing: proxmox guest boot is only exercised by hand.
See Testing.

Reaching a kubernetes session goes through a supervised `kubectl port-forward`,
which ccvm establishes for the duration of any command that needs ssh and
restarts if it dies. A forward only exists while something holds it, so a
detached k8s session is unreachable between commands — that is the shape of
`kubectl port-forward`, not a choice. Claude runs under tmux, so a dropped
forward detaches the session rather than ending it, and `ccvm attach` returns to
it.

## Install

```
make build          # binaries into dist/
make image          # the base session image
```

## Use

```
ccvm up                      # a machine for the current directory
ccvm up ~/src/foo --keep     # one that survives the session
ccvm up --detach             # provision and return, instead of entering
ccvm up --code rsync         # copy the tree in, and back out on teardown
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

## Provisioning

A machine can be prepared beyond its image, in the guest, before Claude starts.
Work runs in phases, arranged around the two fixed points of installing packages
and putting the code in place:

| Phase | What runs, in order | Is `/work` there? |
| --- | --- | --- |
| **pre** | `provision.sh` from the profile chain, parents first; `[provision].pre`; `--pre-install 'cmd'` | not guaranteed |
| **packages** | `[provision].packages`; `--install pkg,pkg` | not guaranteed |
| **post** | `[provision].post`; `~/.config/ccvm/provision.sh`; `<project>/.ccvm/provision.sh`; `--post-install 'cmd'` | not guaranteed |
| **setup** | `[provision].setup`; `--setup 'cmd'` | **yes** |

**Anything that touches your code belongs in `setup`.** Whether `/work` is
populated earlier depends on `--code`: a `mount` is attached when the machine is
created, so it is there from the start, while `git` and `rsync` do not land
until after the `post` phase. A command reading `/work` any earlier therefore
works on the default and breaks the moment someone passes `--code git`. `setup`
runs after the code is in place under every mode.

The phase decides when something runs; the order within a phase is the usual
one, profile then yours then the project's. So a project's `[provision].pre`
runs before your own `provision.sh` — not an inversion, but the project asking
for "before packages", which is what the key is for.

`--pre-install`, `--post-install`, and `--setup` are repeatable rather than
comma-separated, because a shell command may contain a comma.

A failing command aborts and destroys the machine, naming the command that
failed. A half-provisioned session fails later for reasons that no longer point
at the cause.

`--install` installs alongside `[provision].packages` rather than last, so
everything after it can rely on those packages being present.

A profile's `build.sh` is different: it bakes an image once, via
`ccvm profiles build`. Build-time work belongs there, since running a full
package install on every spawn is slow at best. Prototype in a `provision.sh`,
promote to `build.sh` once it stabilizes.

The project hook runs arbitrary code in the guest — that is the point, in a
machine that is already disposable. It is the counterpart to a repository not
being allowed to choose the image it runs on, which is enforced on the host.

## Guiding Claude

Every session starts with a `CLAUDE.md` at `~/.claude/CLAUDE.md` in the machine,
composed on the host and written in at spawn. Layers, in order:

1. ccvm's own guide, which is how Claude learns that `ccvm-done` ends the session
2. `CLAUDE.md` from the profile chain, parents first
3. `~/.config/ccvm/CLAUDE.md`, your own guidance on every machine
4. `<project>/.ccvm/CLAUDE.md`, committed so anyone working on that repo gets it
5. `--claude-md path.md`, a one-off that goes last

**Your project's own `CLAUDE.md` needs no setup.** It arrives with your code and
Claude Code reads it from the working directory, the same as on your laptop.
The layers above are for guidance that has nowhere else to live: personal
preferences, something a profile implies, or a note for one session.

ccvm's layer is always first and cannot be turned off. A machine whose Claude
cannot discover `ccvm-done` is one you have to clean up by hand.

Missing layers are skipped, so most people have none of these files. A
`--claude-md` that cannot be read is an error rather than a warning, since you
named it. Each section is marked with an HTML comment, so from inside a machine
you can see which layer contributed what.

To reuse your laptop's own memory verbatim, symlink it:

```sh
ln -s ~/.claude/CLAUDE.md ~/.config/ccvm/CLAUDE.md
```

It is not copied automatically, because that file is usually full of host paths
and personal notes that mean nothing inside a disposable Debian guest.

## Getting the code in

`--code` picks how a project reaches the machine. The default is the cheapest
mode a backend can serve while still carrying uncommitted work: `mount` on
docker and orbstack, `rsync` on proxmox, `git` on kubernetes.

| Mode | What it does | Backends |
| --- | --- | --- |
| `mount` | The host directory is attached live. Nothing is copied. | docker, orbstack |
| `rsync` | Copied in at spawn and back out when the session ends. | all |
| `git` | Cloned from origin at the branch you have checked out. Uncommitted work stays behind. | all |
| `sshfs` | The guest mounts this machine over a reverse tunnel, so edits are live in both directions. | docker, orbstack, proxmox |

`mount` needs a filesystem the host and the guest share, which is why it exists
on the two local backends and nowhere else. kubernetes has no host filesystem to
reach at all; proxmox runs the guest on a remote cluster while your project sits
on this machine. Both refuse `mount` rather than accepting it and leaving `/work`
silently empty.

`sshfs` is how a remote guest gets a live view instead: rather than attaching a
shared filesystem, the guest mounts *this* machine over a reverse tunnel. That
needs Remote Login enabled here, `sshfs` present in the guest, and ccvm running
for as long as the mount is used - the tunnel is held by the process, the same
way a kubernetes port-forward is, so `--code sshfs` is refused with `--detach`
rather than leaving a session reading a directory that stopped updating. It is
not the default anywhere for those reasons; proxmox defaults to `rsync`.

Under `rsync` the machine holds the only copy of anything edited inside it, so
`ccvm rm` returns changes before destroying and refuses if it cannot.
`ccvm rm --force` discards them deliberately. Heavy directories — `node_modules`,
`.venv`, build output — are never copied, on top of the project's own
`.gitignore`.

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
pre      = ["update-ca-certificates"]
packages = ["delve", "golangci-lint"]
post     = ["git config --global init.defaultBranch main"]
setup    = ["go mod download"]
```

`[env]` reaches the guest through the session's env file, so it applies on every
backend rather than only the two that can carry variables natively, and is
visible to provisioning commands as well as to Claude.

Configuration is layered, later winning: built-in defaults, the `extends` chain,
the profile, `~/.config/ccvm/profile.toml`, the project's `.ccvm/profile.toml`,
then flags. Scalars override, `[env]` and `[backend.*]` merge per key, and
`[provision].packages` accumulates down the chain unless a layer sets
`packages_replace`.

`pre`, `post`, and `setup` accumulate the same way, with `pre_replace`,
`post_replace`, and `setup_replace` to opt out. Unlike packages they are not
deduplicated: a package list is a set, while a command list is a sequence, and
two layers asking for the same command both meant to run it.

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

A login lasts about a month. `ccvm creds renew` builds or starts a broker
machine, opens Claude in it for you to run `/login`, and imports the result when
you exit. The broker is deliberately not kept in sync with the live credential:
a second copy is the thing that breaks, so it holds one only in the moment
between signing in and importing.

**`ccvm keep` exempts a machine from the reaper, not from its backend.** Docker
fixes auto-remove when a container is created, so a machine started without
`--keep` will still be deleted if it stops. `ccvm keep` says so rather than
implying a durability it cannot deliver.

## Proxmox setup

Proxmox differs from the other backends in one structural way: ccvm reaches a
guest only over ssh, so it cannot install its own key first the way it does on
docker, orbstack, and kubernetes. **The template must already contain ccvm's
public key** in `/root/.ssh/authorized_keys`, or every guest comes up
unreachable.

Building one, from a node:

```
pveam update && pveam download local debian-13-standard_13.6-1_amd64.tar.zst
pct create 9000 local:vztmpl/debian-13-standard_13.6-1_amd64.tar.zst \
    --hostname ccvm-base --cores 2 --memory 2048 --rootfs local:8 \
    --net0 name=eth0,bridge=vmbr0,ip=dhcp \
    --features nesting=1,keyctl=1 --unprivileged 1
pct start 9000
pct push 9000 profiles/base/build.sh /root/build.sh --perms 755
pct exec 9000 -- sh /root/build.sh
pct exec 9000 -- mkdir -p /root/.ssh
pct push 9000 ~/.ssh/ccvm_ed25519.pub /root/.ssh/authorized_keys --perms 600
pct exec 9000 -- chown -R root:root /root/.ssh
pct stop 9000 && pct template 9000
```

Then point a profile's `[backend.proxmox].lxc_template` at 9000.

Settings come from the environment, and the network ones are not optional if
your cluster differs from the defaults:

```
CCVM_PROXMOX_URL, CCVM_PROXMOX_TOKEN_ID, CCVM_PROXMOX_SECRET
CCVM_PROXMOX_NODE       pin a node, or the least loaded one is chosen
CCVM_PROXMOX_BRIDGE     default vmbr0
CCVM_PROXMOX_SUBNET     a /16 prefix, default 10.10
CCVM_PROXMOX_GATEWAY    default <subnet>.0.1
CCVM_PROXMOX_SSH_KEY    default ~/.ssh/ccvm_ed25519
CCVM_PROXMOX_INSECURE   set for a private CA
```

Guests get deterministic addresses derived from their vmid, and ccvm allocates
vmids from its own reserved range rather than the cluster's next free id — a
guest outside that range has no derivable address. Linked clones are used where
the storage supports them, with a full clone as the fallback; directory and LVM
storage refuse linked clones, ZFS and Ceph allow them.

## The reaper

`ccvm gc` collects machines past their TTL and machines whose session ended, but
it only helps if something runs it. `ccvm gc install` schedules it every two
minutes via launchd, covering docker and orbstack.

Two minutes rather than hourly because the reaper is not only cleanup: on
backends without a sentinel-aware PID 1 it is what acts on `ccvm-done`, so a
slow schedule makes ending a session from inside look broken.

The cluster backends need their own schedules, since a sleeping Mac reaps
nothing and cannot reach a guest to check it:

```
make reaper-image                    # ccvm/reaper:latest, push it where the cluster can pull it
kubectl apply -f k8s/reaper.yaml
scp k8s/proxmox-reaper.cron root@<node>:/etc/cron.d/ccvm-reaper
```

The reaper runs from its own image rather than the session one. A session image
carries `ccvm-done` and `ccvm-init` for the guest, not `ccvm` and not a
Kubernetes client, and there is no reason to put a cluster credential inside the
machine Claude is running in.

Both are verified. The kubernetes manifest was applied to a real cluster, with
RBAC that grants deleting jobs and exec and nothing else — not deleting pods,
creating jobs, or reading secrets. The proxmox cron file was exercised in a
Debian container running real cron against a containerized Proxmox: cron fired
it on schedule and the reaper authenticated and queried the cluster. What is
still untested there is guest boot, which needs a hypervisor.

One trap worth repeating from that file: **cron silently ignores anything in
/etc/cron.d not owned by root** — no error, no log line, the job simply never
runs. Any copy that does not preserve root ownership leaves a reaper that looks
installed and does nothing.

`ccvm gc status` reports whether the agent is loaded and when its log was last
written. A reaper that has silently stopped looks exactly like one with nothing
to do, so the log is the evidence.

## Testing

```
make test           # unit and golden, with -race
make cover          # coverage of shipped code
make itest          # integration; see CCVM_ITEST_BACKENDS
```

The golden tests under `cmd/ccvm/testdata/script` run the real binary and
compare what a user actually sees: argument handling, contradictory flags, a
missing credential, and the order those checks happen in. Reaching a failed
`Create` needs a backend that fails on demand, which the golden runner cannot
inject, so that case is asserted against the fake backend instead.

**What CI does not cover.** Booting a guest on proxmox or orbstack is verified
by hand, not by CI. Hosted runners have no nested virtualization, and there is
no OrbStack on them at all, so the proxmox job exercises the control plane
against a containerized PVE and stops short of a running guest. Run
`make itest-local` before tagging a release; it is the only thing that proves a
guest actually boots and is reachable, which is a bug class fixtures cannot
catch.

Integration skips are opt-in: name the backends in `CCVM_ITEST_BACKENDS` and the
suite fails, rather than skips, if one is unavailable. A suite that silently
skips reports green while testing nothing.

The suite asserts the Backend contract rather than any one implementation, so a
backend inherits all of it by registering. Where a backend genuinely cannot meet
a claim, it is skipped with the reason: k8s reports pull failures from the pod's
events rather than from preflight.

CI covers docker and k8s, the latter on `kind`. Proxmox gets its control plane
exercised against a containerized PVE pinned by digest, which catches upstream
API drift that frozen fixtures cannot — they keep passing against a release that
changed a response shape. That job is non-gating: it depends on someone else's
repackaging of a distro, and a flaky required check is one you learn to ignore.

Proxmox guest boot is exercised against a containerized Proxmox: LXC needs
namespaces rather than KVM, so guests really do boot there, and the full suite
passes including ssh into a guest and reading a record back from a stopped one.
What that cannot cover is a VM rather than a container, which needs KVM, and
anything specific to real cluster storage or networking.

OrbStack has no automated coverage at all — it is macOS-only and
GUI-installed — so a green badge does not cover it. `make itest-local` is the
only check there, and it is a process dependency rather than a guarantee.

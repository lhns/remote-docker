# 0019. A dockerd per account, behind one SSH port

- Status: Accepted
- Date: 2026-08-08
- Supersedes part of [ADR 0012](0012-shared-dockerd-across-users.md)
- Promotes a fallback measured in [ADR 0016](0016-replaying-change-events-as-real-syscalls.md)
  to load-bearing

## Context

ADR 0012 recorded one dockerd shared by every enrolled account, and named its
own revisit trigger: "when the user set stops being small and mutually
trusted". It also stated the cost plainly — every account can list, inspect,
exec into, stop and remove every other account's containers, read their images
and volumes, and see their environment variables.

That cost is larger in practice than the sentence suggests. It is not that a
hostile user *could* interfere; it is that an ordinary one *does*. `docker ps`
shows somebody else's work. `docker system prune` removes it. A name collision
on `rd-cwd` hands one account another's volume, complete with the first
account's NFS port.

That collision was later found one level down, between one account's own
machines, and it is fixed rather than separated: a volume carries the CLIENT
that created it (ADR 0029), so `rd-cwd` no longer exists as a name two parties
can both derive.

The requirement that shaped the answer: **one SSH port.** One published port,
one Swarm service, one address to hand out.

## Say this out loud, because it will otherwise be believed

**This is separation, not isolation.**

Each per-account daemon runs `--privileged`, and privileged is root on whatever
hosts it, so a determined account A can still break out and reach account B.
What changes is that A no longer *casually* sees B's work — which is the
failure that actually happens.

**ADR 0012's revisit trigger is therefore NOT satisfied by this record.** A
workspace is still a shared machine and must still be treated as one. Genuine
isolation is still one workspace container per account, and 0012's description
of that remains correct and remains the answer for a mutually untrusting user
set.

## Decision

Each enrolled account gets its own dind, started lazily and adopted after a
restart. The agent routes each account's session to its own daemon.

### The dinds are nested, not siblings on the host

The brief for this work said the daemons should be siblings on the host's
daemon. Two findings make that the wrong call, and neither has a cheap
workaround.

**`/proc/<pid>` visibility.** Everything below depends on the agent reaching a
daemon through `/proc/<pid>/ns/net` and `/proc/<pid>/root`. A sibling on the
host has a *host* pid, which does not exist in the workspace container's pid
namespace. Making it exist needs `pid: host` — every enrolled account's shell
seeing every process on the node. Worse than the problem being solved.

**The host socket.** `elevate/plan.go`'s `childMounts` exists solely to keep
the host's Docker socket *out* of the privileged container, and says why: "a
privileged container holding the host socket gives every enrolled workspace
user root on the node." The sibling design puts it back, in the one container
where accounts have shells.

Nested, the pid a daemon reports in `.State.Pid` is one the agent can open
under its own `/proc` — which `test/integration.sh` already relies on. Storage
works out too: each account's `/var/lib/docker` is a named volume on the
*workspace's* daemon, so it lands on a real filesystem and overlay2-on-overlay2
never arises.

### The NFS export is bound inside the account's netns

The reverse tunnel used to live in the agent's network namespace, which was
also dockerd's — the only reason `addr=127.0.0.1,port=<uid port>` in
`workspace.NFSVolumeOptions` resolved at all. A namespace per account breaks
that, and the two obvious repairs are both wrong:

- `--network container:<agent>` — two dockerds fighting over `docker0`,
  published ports colliding across accounts, and every account's ports landing
  in the namespace where every account's shell lives.
- Giving the agent an address on a per-account bridge — requires relaxing
  `isLoopback` in `forward.go`, the single rule standing between an
  unauthenticated NFS export and everyone. Docker's isolation blocks
  bridge-to-bridge traffic, not container-to-host, so another account's
  container could route to it.

Instead the agent creates the listener **inside** the account's daemon's
netns. Only that daemon can reach it. `ForwardPolicy`, `isLoopback`,
`NFSVolumeOptions` and `workspace.Info` are all unchanged.

`gliderlabs/ssh` hardcodes `net.Listen` and `net.Dial`, so both forwarding
handlers are near-copies of theirs differing in that one call.

**One listener, not two.** The plan called for binding in the agent's namespace
as well, to keep the agent's own `~/workspace` mount working. [ADR
0018](0018-one-way-to-do-each-thing.md) deleted that mount, so the second
listener now has no user and a real cost: an unauthenticated NFS export bound
in the namespace where every account's shell runs.

### Routing, and what had to move with it

- `serveDockerSocket` resolves the account to its own socket. Its comment used
  to say there was no per-account restriction "because there is none to make";
  that lookup is now the only thing between one account's session and another's
  containers, and getting it wrong does not fail — it succeeds, against the
  wrong daemon.
- `serveExec` exports `DOCKER_HOST`. Without it, `docker ps` in an SSH session
  finds `/var/run/docker.sock` — the *parent* daemon, which holds every
  account's dind — and the separation ends at the first shell prompt.
- The shared `docker` group goes, including for accounts that already exist.
  Provisioning returns early for an existing account, so changing which groups
  new accounts join would have fixed nothing on any workspace that had users.
- `notify.DockerVolumes` gains `Host` and `Root`. `notify.go` is untouched: its
  interface promises "the directory in the agent's filesystem holding this
  export", and that stays true — the directory just moves.

### Lifecycle

- **Created** lazily by `Ensure`, single-flighted per account, warmed at
  authentication so a cold boot hides behind the round trips that follow.
- **Destroyed** never, automatically. A `docker run -d` must survive its
  author's disconnect. `--rm` on a per-account daemon would delete that
  account's containers, images and volumes the moment it stopped, and
  `elevate`'s child takes `--rm` only because it is a singleton whose state is
  worthless.
- **Adopted** at startup by label, keyed on a workspace id persisted in the
  state directory — *not* the container id, which changes on every redeploy and
  would orphan every account's daemon on the first `compose up -d`. `Ensure` on
  a stopped container runs `docker start`, not `docker run`.

## Consequences

*(2026-08-11: the sentence below is harder on a VM. In a container, an account
breaking out of its dind reaches the workspace container -- disposable, and
holding nothing but other accounts' work. On a machine (ADR 0025) it reaches
the machine: its files, its other services, its network position. Nothing about
the code differs; the blast radius does.)*

- **Layer cache duplication.** ADR 0012 named the shared cache as a benefit
  rather than a saving, and it was right. Five accounts on `node:22` and
  `golang:1.25` is five copies. A registry mirror recovers bandwidth but **not**
  disk: Docker has no shared read-only image store. There is no mitigation to
  imply.
- **Disk becomes a shared failure mode with a confusing presentation.** N
  unbounded graph directories on one volume, so one account's runaway build can
  now take down every *other* account's daemon.
- **Memory.** Roughly 100–150MB per idle dockerd plus containerd. Ten idle
  accounts is ~1.5GB before anybody runs anything.
- **Startup latency**, 3–10s for a cold account, worse on `fuse-overlayfs`.
  Warming at authentication hides most of it.
- **The graph driver is inherited from the workspace's own dockerd**, and this
  record originally said only that it was *not* — which documented a trap
  instead of removing one. A deployment on Ceph- or NFS-backed storage sets
  `--storage-driver=fuse-overlayfs` because overlay2 refuses that filesystem,
  and a per-account daemon's storage is a volume on that same filesystem, so it
  needs the same answer. Left to itself dockerd falls back to **vfs**, which
  has no copy-on-write and copies the entire image on every container create:
  `docker ps` stays instant while `docker create debian` takes 90 to 113
  seconds, nothing fails, and nothing says why. That happened on a real
  workspace within a day of this becoming the default.
  `WORKSPACE_DIND_STORAGE_DRIVER` still overrides; vfs is now reported loudly
  when it happens anyway.
- **`/run/rd` is 0755 and the per-account directory inside it is 0750.** The
  parent has to be traversable or an account cannot reach its own socket --
  `DOCKER_HOST` correct, and `docker ps` answering "permission denied while
  trying to connect to the Docker daemon socket". Traversing the parent reveals
  only the names of directories nobody else may enter; the 0750 below it is
  what separates the accounts.
- **A per-account daemon is untrusted input.** It reports its own volume
  mountpoints, and the account is root inside it. `relocate` therefore checks
  that a relocated path stays under the daemon's root rather than trusting
  `path.Join` — which CLEANS, so joining `/proc/42/root` to `/../../etc/shadow`
  yields `/proc/etc/shadow`: outside, silently, looking correct. The
  `O_NOFOLLOW` and `AT_SYMLINK_NOFOLLOW` in `SyscallPoker` were incidental
  tidiness before and are now the guard against a symlink planted in a
  filesystem the agent does not control.
- **A netns helper must never return a thread whose namespace it could not
  restore.** `socket(2)` uses the calling thread's namespace, so the switch and
  the socket call happen on one `LockOSThread`ed thread; if the restoring
  `Setns` fails, that thread is parked forever rather than unlocked. An
  unlocked thread goes back to the runtime's pool still in someone else's
  namespace, and the next goroutine scheduled onto it opens sockets there.
  Leaking one thread is the correct answer to a problem with no safe recovery.
- **Upgrading is breaking, and the release notes have to say so.** Images and
  volumes an account built under the shared daemon are invisible from its own,
  and there is no cheap migration. `WORKSPACE_PER_USER_DIND=false` keeps the old
  behaviour, and the old data is still in the shared `/var/lib/docker` for
  anyone who changes their mind.
- **Persistence moves one level deeper, and nowhere else.** Each account's
  `/var/lib/docker` is a named volume `rd-dind-<account>-lib` on the workspace's
  own daemon, so it lands inside the same `/var/lib/docker` the shared mode
  used. A deployment that persisted that already persists this. Named rather
  than anonymous, and labelled, for two reasons: the daemon container in front
  of it is disposable — removed and recreated by upgrades and by adoption —
  and an operator pruning the workspace's daemon needs to be able to *see*
  which volumes are somebody's entire account. `docker system prune -a
  --volumes` there removes stopped containers and then unused volumes, which is
  an idle account's daemon followed by its storage.
- **`workspace-id` becomes as load-bearing as the uid map.** Both live in
  `/etc/workspace` and both survive a redeploy; losing the id orphans every
  running daemon exactly as losing the uid map would renumber every account.
- **A daemon is created once and started thereafter, so its settings are fixed
  at creation.** Each one carries a digest of the spec it was built from, and a
  daemon whose digest no longer matches is recreated -- the container is
  disposable, the graph volume beside it is the data. It waits until that
  account has nothing running, because recreating a daemon stops its
  containers. The storage driver is the exception, since a graph written by one
  driver cannot be read by another; `remote-dockerd daemons reset` is where
  that decision is made by a person.
- **The image is the workspace's own**, passed in by `elevate`, which has
  already inspected the container it is relaunching. Stock `docker:dind` does
  not carry fuse-overlayfs, so a per-account daemon on it dies at startup on
  exactly the workspaces where the driver matters. The entrypoint is dind's own
  script, not `dockerd`: the script removes a stale /var/run/docker.pid, and
  without it the first start works and every restart fails.
- **Readiness is a round trip, not a socket file.** dockerd binds its socket
  before initialising storage, so a daemon that dies during startup leaves one
  behind that looks healthy.
- **Amended by ADR 0020.** This arrangement arrived as a nullable manager, so
  every use of a daemon grew an `if Daemons != nil`. There were nine, guarding
  an invariant that fails by succeeding, and none of them could be unit tested.
  The mode is now chosen once and resolved through one interface; the shared
  daemon of ADR 0012 is an implementation of it rather than the nil case.
- Two things become correct for free: `rd-cwd` stops colliding across accounts
  (every account produced that same name, and the second `EnsureVolume`
  silently returned the first account's volume with the *first* account's NFS
  port), and "loopback" in a local forward now means the account's own dind's
  loopback rather than the agent's, where the SSH port lives.

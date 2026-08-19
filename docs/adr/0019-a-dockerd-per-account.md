# 0019 — A dockerd per account, behind one SSH port

- Status: Accepted; supersedes part of
  [ADR 0012](0012-shared-dockerd-across-users.md). Consolidates ADR 0020 (one
  daemon target) and ADR 0019's supervision rules, formerly ADR 0036: both
  existed only to make this work.
- Date: 2026-08-08, extended 2026-08-10 and 2026-08-15, consolidated 2026-08-19
- Promotes a fallback measured in
  [ADR 0016](0016-replaying-change-events-as-real-syscalls.md) to load-bearing
- Current answer: **one dind per enrolled account**, started by the agent when
  that account connects and by nothing else, reached through one resolver. ADR
  0012's shared daemon is an implementation of that resolver, not a mode branch.

## Context

ADR 0012 shared one dockerd and named its own revisit trigger: "when the user set
stops being small and mutually trusted". The cost is not that a hostile user
*could* interfere, it is that an ordinary one *does*: `docker ps` shows somebody
else's work, `docker system prune` removes it, and a name collision on `rd-cwd`
hands one account another's volume complete with the first account's NFS port.
(That last one was later found one level down, between one account's own
machines, and fixed rather than separated: a volume carries the CLIENT that
created it, ADR 0029.)

Constraint that shaped the answer: **one SSH port.** One published port, one
Swarm service, one address to hand out.

## Say this out loud, because it will otherwise be believed

**This is separation, not isolation.** Every per-account daemon runs
`--privileged`, which is root on whatever hosts it, so a determined account A can
still break out and reach B. What changes is that A no longer *casually* sees B's
work — the failure that actually happens.

**ADR 0012's revisit trigger is NOT satisfied by this record.** Genuine isolation
is still one workspace container per account, and remains the answer for a
mutually untrusting user set.

## The decisions

**Nested dinds, not siblings on the host.** The brief said siblings; two findings
make that wrong, neither with a cheap workaround:

- **`/proc/<pid>` visibility.** The agent reaches a daemon through
  `/proc/<pid>/ns/net` and `/proc/<pid>/root`. A sibling has a *host* pid, absent
  from the workspace's pid namespace; making it present needs `pid: host` — every
  account's shell seeing every process on the node.
- **The host socket.** `elevate/plan.go`'s `childMounts` exists to keep it *out*
  of the privileged container: "a privileged container holding the host socket
  gives every enrolled workspace user root on the node." Siblings put it back, in
  the one container where accounts have shells.

Nested, `.State.Pid` is openable under the agent's own `/proc`, and each account's
`/var/lib/docker` is a named volume on the *workspace's* daemon, so overlay2 on
overlay2 never arises.

**The NFS export binds inside the account's netns.** The tunnel used to live in
the agent's namespace, which was also dockerd's. Both obvious repairs are wrong:

- `--network container:<agent>` — two dockerds fighting over `docker0`, published
  ports colliding across accounts, every account's ports in the namespace where
  every account's shell lives;
- an agent address on a per-account bridge — requires relaxing `isLoopback` in
  `forward.go`, the single rule between an unauthenticated NFS export and
  everyone. Docker blocks bridge-to-bridge, not container-to-host, so another
  account's container could route to it.

The agent creates the listener **inside** the account's daemon's netns; only that
daemon can reach it. `ForwardPolicy`, `isLoopback`, `NFSVolumeOptions` and
`workspace.Info` are unchanged. `gliderlabs/ssh` hardcodes `net.Listen`/`net.Dial`,
so both forwarding handlers are near-copies differing in that one call.

**One listener, not two.** Binding in the agent's namespace as well was planned,
to keep the agent's own `~/workspace` mount working.
[ADR 0018](0018-one-way-to-do-each-thing.md) deleted that mount, so the second
listener has no user and a real cost: an unauthenticated NFS export in the
namespace where every account's shell runs.

**One resolver, asked everywhere, chosen once.** This arrived as a nullable field
(`Daemons *daemons.Manager`, nil meaning shared), so every use grew a branch.
There were **nine** across `sshd`: Docker socket, notify's volume host and
filesystem root, a shell's `DOCKER_HOST`, the version and storage-driver queries,
both forward paths, the warm-up at authentication, the mode string. Two reasons
that is worse than ordinary duplication — **the failure mode is success** (a
missed branch runs somebody else's containers, or binds an unauthenticated export
into another account's namespace), and **it was untestable**, since the call sites
took a concrete manager that needs a real daemon.

```go
type Target struct {
    Socket    string // splice a dial-stdio session here
    Host      string // DOCKER_HOST; "" means the default socket
    NetNSPath string // "" means this process's namespace
    Root      string // the daemon's filesystem as we see it; "/" when it is ours
}

type Targets interface {
    Ensure(ctx, account) (Target, error) // waits, starting it if needed
    Lookup(ctx, account) (Target, bool)  // never waits
    Warm(account)
    Mode() string
}
```

- `*daemons.Manager` implements it here; `daemons.Shared(socket)` implements ADR
  0012 — **an implementation, not a nil check**.
- The mode is read from the environment in `cmd/remote-dockerd/serve.go` and
  nowhere else in the agent branches on it.
- **The empty string carries "no redirection needed"**: `netns.Do("")` stays in
  this namespace, an empty `Host` leaves `DOCKER_HOST` unset. That is what lets
  both modes be one code path.

**Routing, and what moved with it:**

- `serveDockerSocket` resolves the account to its own socket. That lookup is the
  only thing between one account's session and another's containers, and getting
  it wrong does not fail — it succeeds, against the wrong daemon.
- `serveExec` exports `DOCKER_HOST`; without it a shell's `docker ps` finds the
  *parent* daemon and the separation ends at the first prompt.
- The shared `docker` group goes, including for accounts that already exist —
  provisioning returns early for those, so changing what new accounts join would
  have fixed nothing on a workspace that had users.
- `notify.DockerVolumes` gains `Host` and `Root`; `notify.go` is untouched, since
  its promise ("the directory in the agent's filesystem holding this export")
  stays true.

**Lifecycle, and who supervises it:**

- **Created** lazily by `Ensure`, single-flighted per account, warmed at
  authentication so a cold boot hides behind the round trips that follow.
- **Destroyed** never, automatically. `--rm` would delete an account's
  containers, images and volumes the moment it stopped; `elevate`'s child takes
  `--rm` only because it is a singleton whose state is worthless.
- **Adopted** at startup by label, keyed on a workspace id persisted in the state
  directory — *not* the container id, which changes on every redeploy and would
  orphan every account's daemon on the first `compose up -d`. `Ensure` on a
  stopped container runs `docker start`.
- **No restart policy.** It carried `--restart unless-stopped`, making the parent
  dockerd a second supervisor with no backoff and nothing in our log. Symptom: a
  workspace refusing every session for minutes with `tcpip-forward request denied
  by peer`, because the account's daemon was crash-looping (`exitCode=1`, four
  restarts in eighty seconds) and the reverse forward binds inside that daemon's
  namespace. `Ensure` already starts a stopped daemon, so the policy was a
  duplicate rather than a safety net.
- **A daemon that is not running is not busy.** The same incident stayed broken
  because `reconcile` will not rebuild until nothing runs inside, and it asked the
  daemon — which a crash-looping container never answers, so "cannot tell" counted
  as busy and logged *"has containers running"* about a container restarting every
  nineteen seconds. `idle` now asks the parent for container state first: exited,
  restarting, created or dead cannot be running anything. The old rule survives
  for a daemon that IS running and cannot be asked, where being wrong costs
  somebody's containers.
- **Shared mode stops what it does not serve.** Leftover per-account daemons
  answer nobody. Stopped, never removed — the volume behind each holds somebody's
  images and containers. Filtered by this workspace's id, and refused outright
  when there is no id to filter by.

## Consequences

*(2026-08-11: the first point is harder on a VM. In a container, breaking out of
a dind reaches the workspace container — disposable, holding only other accounts'
work. On a machine (ADR 0025) it reaches the machine: files, services, network
position. The code does not differ; the blast radius does.)*

- **Layer cache duplication.** Five accounts on `node:22` and `golang:1.25` is
  five copies. A registry mirror recovers bandwidth but **not** disk: Docker has
  no shared read-only image store. No mitigation to imply.
- **Disk is a shared failure mode with a confusing presentation.** N unbounded
  graph directories on one volume: one account's runaway build takes down every
  *other* account's daemon.
- **Memory** ~100–150MB per idle dockerd plus containerd; ten idle accounts is
  ~1.5GB before anybody runs anything.
- **Startup latency** 3–10s cold, worse on `fuse-overlayfs`. Warming at
  authentication hides most of it.
- **Detached containers do not survive a workspace restart on their own.** They
  come back when that account next connects. Deliberately traded for the
  supervision above, and smaller than it sounds: anything with a bind mount cannot
  run without its client session anyway.
- **The first thing an account does after a restart pays the boot**, bounded by
  `DefaultReadyTimeout`. Found by CI: `per-user-dind.sh` probed a shell for an
  account that had not reconnected and timed out waiting for a prompt.
- **The graph driver is inherited from the workspace's own dockerd.** This record
  originally said it was *not*, which documented a trap instead of removing one.
  Ceph- or NFS-backed storage needs `--storage-driver=fuse-overlayfs`, and a
  per-account daemon's storage is a volume on that same filesystem. Left alone,
  dockerd falls back to **vfs**: no copy-on-write, the whole image copied per
  create, `docker ps` instant while `docker create debian` takes 90–113 seconds,
  nothing failing and nothing saying why. Happened on a real workspace within a
  day of this becoming the default. `WORKSPACE_DIND_STORAGE_DRIVER` overrides; vfs
  is now reported loudly.
- **`/run/rd` is 0755, the per-account directory inside it 0750.** The parent must
  be traversable or an account cannot reach its own socket (`DOCKER_HOST` correct,
  `docker ps` answering "permission denied"). Traversal reveals only names of
  directories nobody else may enter; the 0750 separates the accounts.
- **A per-account daemon is untrusted input.** It reports its own volume
  mountpoints and the account is root inside it, so `relocate` checks the result
  stays under the daemon's root: `path.Join` CLEANS, so `/proc/42/root` joined to
  `/../../etc/shadow` is `/proc/etc/shadow` — outside, silently, looking correct.
  `O_NOFOLLOW` and `AT_SYMLINK_NOFOLLOW` in `SyscallPoker` stopped being tidiness
  the moment those paths left the agent's filesystem.
- **A netns helper must never return a thread whose namespace it could not
  restore.** `socket(2)` uses the calling thread's namespace, so the switch and the
  socket call are pinned to one `LockOSThread`ed thread; a failed restoring `Setns`
  parks that thread forever rather than unlocking it, because an unlocked thread
  rejoins the pool still in someone else's namespace.
- **Upgrading is breaking.** Images and volumes built under the shared daemon are
  invisible from an account's own, with no cheap migration.
  `WORKSPACE_PER_USER_DIND=false` keeps the old behaviour and the old data is
  still there.
- **Persistence moves one level deeper and nowhere else.**
  `rd-dind-<account>-lib` is a named volume on the workspace's own daemon, so a
  deployment that persisted `/var/lib/docker` already persists this. Named and
  labelled because the container in front is disposable and because an operator
  pruning the parent must be able to *see* which volumes are somebody's entire
  account — `docker system prune -a --volumes` there removes an idle account's
  daemon and then its storage.
- **`workspace-id` is as load-bearing as the uid map.** Losing it orphans every
  running daemon exactly as losing the uid map would renumber every account.
- **Settings are fixed at creation.** Each daemon carries a digest of its spec and
  is recreated when the digest no longer matches, waiting until that account has
  nothing running. The storage driver is the exception, since a graph written by
  one driver cannot be read by another: `remote-dockerd daemons reset` is where a
  person decides that.
- **The image is the workspace's own**, passed in by `elevate`. Stock
  `docker:dind` lacks fuse-overlayfs, so a per-account daemon on it dies at
  startup exactly where the driver matters. The entrypoint is dind's own script,
  not `dockerd`: it clears a stale `/var/run/docker.pid`, without which the first
  start works and every restart fails.
- **Readiness is a round trip, not a socket file.** dockerd binds its socket
  before initialising storage, so a daemon dying during startup leaves one behind
  that looks healthy.
- **Routing is unit tested** on a machine with no Docker.
  `agent/internal/sshd/routing_test.go` asserts each session resolves its own
  account, two accounts never share a target, the shared daemon answers
  identically for everybody, and the info queries use `Lookup` and start nothing;
  verified by breaking the resolver on purpose.
  `agent/internal/daemons/runner_test.go` covers the supervision rules, including
  the one that was wrong.
- **`Lookup` versus `Ensure` is a contract, not a habit.** `workspace-info` is the
  client's first round trip and must not wait for a cold dind.
  `notify.DockerVolumes.Host` is lazy for the same reason.
- **A zero `sshd.Config` no longer means "shared".** A `Server` built by hand with
  no resolver panics on its first session rather than quietly serving the wrong
  daemon.
- **Nothing here explains why a daemon exits 1.** It makes the failure visible,
  attributable and repairable; the cause of the incident behind the supervision
  rules was never established, and `lastWords` carrying the daemon's log tail into
  the error is what would have.
- Two things became correct for free: `rd-cwd` stopped colliding across accounts,
  and "loopback" in a local forward means the account's own dind's loopback rather
  than the agent's, where the SSH port lives.

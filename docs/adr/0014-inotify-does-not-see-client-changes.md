# 0014. inotify does not see client-side changes

- Status: **Open — no accepted solution**
- Date: 2026-08-07

This record exists to stop the problem being rediscovered, and to say plainly
what is not solved. It is not a decision. It should stay open until one of the
candidates below is either accepted or ruled out.

## The measurement

A container watching a directory on the share, two ways at once, while a file
is created on the client (`test/watchprobe`, run by `test/integration.sh`):

```
RESULT inotify_events=0 poll_entries=1 inotify=[] poll=[created-after-watch.txt]
```

**Polling sees the change. inotify sees nothing at all.** Not a delayed event,
not a partial one — zero.

The polling result is the control, and it matters: the file is present and
readable, so this is purely a failure of *notification*, not of the mount.

## Why it happens

NFS carries no change-notification protocol. The Linux NFS client raises
inotify events only for operations performed through that mount — so a write
made on the client, which the server applies to its own local filesystem, is
invisible to a watcher in the container. The container's kernel was never told.

This is inherent to the protocol, not a configuration mistake, and not
something a different NFS server would fix.

## Why it matters more than it looks

The project's central claim is a *real filesystem, not a sync*. That claim is
only worth something if changes are **noticed**. Every hot-reload workflow —
vite, webpack, nodemon, air, watchexec, `dotnet watch` — depends on inotify and
will silently do nothing while appearing to work. Silently is the problem: the
file is there, the tool is running, and nothing happens.

A survey of the alternatives (see the README's prior-art section) found that
Mutagen, Okteto, Blimp, DDEV and Docker's own Synchronized File Shares all
answer this problem with **file sync into a volume** rather than a network
filesystem. It is easy to read that as inertia. This measurement suggests it is
not: sync makes changes land as ordinary local writes, so inotify fires
normally. That may be the whole reason the industry converged there.

Until this is solved, the honest description of what remote-docker is good at
is: **builds, tests, one-shot tooling, and anything that reads files when it
starts** — not the edit-reload loop.

## Candidates, none accepted

**1. Polling watchers.** Works today. Most tools support it —
`CHOKIDAR_USEPOLLING=1`, `WATCHPACK_POLLING=true`, `--poll` — at the cost of CPU
proportional to the tree size, which over a network filesystem is worse than
locally.

Not injected automatically, deliberately: silently changing the behaviour of a
user's build tool is a worse failure than the one it papers over. Documented
instead.

**2. Replay the event through the mount.** The most promising, and unverified.

The client already watches its own filesystem — it must, to know what changed.
It could tell the agent, which touches the changed path so that a watcher
notices.

The catch, and the reason this is a candidate rather than a plan: inotify
watches are per-mount in the kernel's view. A touch performed through the
agent's own mount will not necessarily notify a watcher inside a container
holding its *own* mount of the same export. Doing it inside the target
container's mount namespace might work — the agent runs privileged and can
enter one — but this has not been tested.

**Whoever picks this up should start here.** It is cheap to test, and it is the
only candidate that would preserve the "real filesystem, not a sync" claim
rather than abandoning it.

**3. FUSE on the container side.** A FUSE filesystem can generate events for
operations it performs. But the operations still have to originate locally,
which means shipping the changes there first — sync again, under another name,
with a FUSE dependency added.

**4. Accept and document.** The status quo. The README says so at the top, and
this record explains why.

## Consequences

- The README leads with this limitation rather than burying it. A user who
  discovers it themselves, after an hour of a watcher not firing, is a user
  who will not trust the rest of the documentation.
- `test/integration.sh` records the behaviour rather than asserting a
  particular answer, so if a change ever makes inotify fire, the suite reports
  it instead of failing — and this record gets revisited.
- This does not affect builds, tests, `docker run`, `docker compose up`, or any
  tool that reads its inputs once. Those are unaffected and are what the
  integration suite covers.

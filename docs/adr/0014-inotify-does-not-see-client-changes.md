# 0014. inotify does not see client-side changes

- Status: **Open** for a mount — `consistent` and `cached`, narrowed to
  deletions and renames. **CLOSED for `delegated`** (ADR 0044), where there is a
  local filesystem in the path and the event is real rather than approximated.
- Date: 2026-08-07
- Updated: 2026-08-07 once candidate 2 was measured; 2026-09-01 when the union
  closed it for one mode
- Current answer: a share with `write != through` (spelled `delegated` below;
  [ADR 0042](0042-mount-consistency-modes.md) renamed the modes to two axes)
  gets the real event through its union, deletions included. A plain mount gets
  writes and creations replayed as syscalls
  ([ADR 0016](0016-replaying-change-events-as-real-syscalls.md)) and no faithful
  deletion, which is what stays open.

This record exists to stop the problem being rediscovered, and to say plainly
what is not solved. It is not a decision. It should stay open until one of the
candidates below is either accepted or ruled out.

**Most of it is now solved.** Candidate 2 was tested and works: writes and
creates are replayed into the container as genuine inotify events, and
[ADR 0016](0016-replaying-change-events-as-real-syscalls.md) records the
mechanism. What follows describes the original problem, unchanged, because it
is still exactly right about deletions — and about why the industry chose sync.
See "What is left", at the end.

## The measurement

A container watching a directory on the share, two ways at once, while a file
is created on the client (`test/probes/watchprobe`, run by `test/integration.sh`):

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

**2. Replay the event through the mount.** ✅ **Measured, and adopted for
writes and creates.** See [ADR 0016](0016-replaying-change-events-as-real-syscalls.md).

The concern below turned out not to apply. inotify marks live on the *inode*,
not the mount, and dockerd bind-mounts each volume from one NFS mount it makes
itself — so a poke at the volume mountpoint and a watcher inside the container
are measured to share `dev` and `ino`, and no namespace entering is needed at
all. The original wording is kept below because the reasoning was sound and the
conclusion was wrong, which is worth being able to see.

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

*It was cheap to test, and it did preserve the claim — for writes and creates.*

**3. FUSE on the container side.** A FUSE filesystem can generate events for
operations it performs. But the operations still have to originate locally,
which means shipping the changes there first — sync again, under another name,
with a FUSE dependency added.

**4. Accept and document.** The status quo. The README says so at the top, and
this record explains why.

## What is left

Deletions, and the source half of a rename.

`unlink()` of a name the client has already removed fails with `ENOENT` before
the kernel generates anything, so there is no operation to perform: the thing
that would produce `IN_DELETE` requires the file still to be there. The same
applies to `IN_MOVED_FROM`.

Two things could still be tried, neither attempted:

1. **Lie in our own NFS server.** We control it. A `REMOVE` for a file that is
   already gone could return success, which is precisely the trick Docker
   Desktop's FUSE client plays on its replay thread. The measured failure was
   `ENOENT` raised locally in the container's kernel, which suggests the VFS
   short-circuits on a negative dentry before any RPC is sent — in which case
   the server never gets the chance and this cannot work. Worth confirming
   rather than assuming, because it is the only route that would close the gap
   properly.
2. **Accept the coarse approximation.** `REMOTE_DOCKER_WATCH=coarse` pokes the
   parent directory, which produces `IN_MODIFY|IN_ISDIR`. A watcher that
   rescans on any directory event will notice the deletion; one that trusts the
   event kind will not. This ships, as an explicit setting rather than a
   heuristic, because misrepresenting an event kind is the user's trade to make.

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
- **A `delegated` share CLOSES this** (ADR 0044), and it is the only thing in
  this project that does. The workspace mounts a union whose upper layer is a
  cache, and every change from this machine is written THROUGH that union -- so
  the container's kernel performs a real filesystem operation and emits the
  event itself, rather than being poked into approximating one. Measured through
  the union in `test/union-probe.sh`: `IN_MODIFY`, `IN_CLOSE_WRITE`, and
  `IN_DELETE`, which nothing here had managed before.

  It does not close for `consistent` or `cached`, which are NFS mounts and are
  what everything above still describes. The difference is not the mode's
  ambition but its mechanism: there is a local filesystem in the path.

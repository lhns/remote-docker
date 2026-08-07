# 0016. Replaying change events as real syscalls

- Status: Accepted
- Date: 2026-08-07

## Context

ADR 0014 measured that a container watching a directory on the share sees
**zero** inotify events when the user edits a file on their own machine. NFS
carries no change notification, so the container's kernel is never told. Every
hot-reload workflow silently does nothing while appearing to work, and that
narrowed the honest description of this project to "builds, tests and one-shot
tooling, not the edit-reload loop".

The obvious shape of a fix is: the client watches its own filesystem, where the
changes actually happen and where notification works natively, and tells the
workspace what changed. The question was what the workspace can then *do* with
that information.

## What is not possible

**Linux offers no way to inject a synthetic inotify or fanotify event.** There
is no syscall, no ioctl, and no rejected proposal to point at. `fanotify(7)`
states it outright: fanotify "reports only events that a user-space program
triggers through the filesystem API. As a result, it does not catch remote
events that occur on network filesystems." The only in-kernel entry points are
the `fsnotify_*()` inlines in `include/linux/fsnotify.h`, callable only from
VFS code.

Two apparent escape hatches were checked and are not real:

- **SMB2 `CHANGE_NOTIFY`** is implemented by cifs.ko, but delivered to
  userspace through a private ioctl (`CIFS_IOC_NOTIFY`). It is not wired into
  fsnotify at all, so an inotify watch on a CIFS mount still sees nothing.
- **NFSv4.1 `CB_NOTIFY`** exists in the protocol and is **not implemented** in
  the Linux client. Server-side directory-delegation patches were still RFC as
  of June 2025, and even those route nothing into inotify. Waiting for the
  protocol to solve this is not a plan.

So a real VFS operation, performed inside the workspace, is the only mechanism
available — to us or to anyone. Docker Desktop reached the same conclusion and
ships it as "Event Injection": the host watches, forwards over gRPC, and a
thread inside the VM replays the operation so the kernel emits the event.

## Decision

The client streams change events to the agent over a long-lived SSH channel
(`workspace-notify`), and the agent replays each one as a **minimal,
non-destructive syscall** on the file as the workspace sees it.

### Where the agent pokes

dockerd's `local` driver mounts each `rd-<id>` NFS volume once at
`/var/lib/docker/volumes/rd-<id>/_data` and bind-mounts it into every container
using it. A bind mount shares the superblock, and inotify marks live on the
**inode**, not the path or the mount — so a poke at the volume mountpoint
reaches the same mark a watcher inside the container set.

Measured, rather than assumed:

```
workspace: POKE stat /var/lib/docker/volumes/rd-1ff11cc0dcd1bd6a/_data/poke-openclose.txt ok dev=54 ino=12898025653570747775
container: POKE stat /data/poke-openclose.txt                                            ok dev=54 ino=12898025653570747775
```

Same device, same inode. So there is no container enumeration, no PID lookup,
no `setns` and no `nsenter` — which is just as well, since `util-linux` is not
in the image. `/proc/<pid>/root/...` was tested as a fallback and also works,
but is not needed.

### Which syscalls, and why those

The matrix, measured in CI against a real dind daemon and a real kernel NFS
mount (`test/integration.sh` section 11d, `test/pokeprobe`):

| poke | events the container's watcher saw |
|---|---|
| `open(O_WRONLY)` + `close()` | `IN_OPEN`, `IN_CLOSE_WRITE` |
| `utimensat(atime=UTIME_OMIT, mtime=current)` | `IN_MODIFY` |
| `utimensat(both times)` — the naive "touch" | `IN_ATTRIB` **only** |
| `open(O_WRONLY\|O_CREAT)` on a file the client just made | `IN_CREATE`, `IN_OPEN`, `IN_CLOSE_WRITE` |
| `unlink()` of a name the client just deleted | **nothing** |
| `utimensat` on the parent directory | `IN_MODIFY\|IN_ISDIR` |

Three of these decide the design.

**`UTIME_OMIT` is the whole trick.** From `fsnotify_change`:

```c
/* both times implies a utime(s) call */
if ((ia_valid & (ATTR_ATIME | ATTR_MTIME)) == (ATTR_ATIME | ATTR_MTIME))
        mask |= FS_ATTRIB;
else if (ia_valid & ATTR_ATIME)   mask |= FS_ACCESS;
else if (ia_valid & ATTR_MTIME)   mask |= FS_MODIFY;
```

Omitting atime drops `ATTR_ATIME` from `ia_valid`, falls to the third branch,
and produces a real `IN_MODIFY`. Setting *both* times — what `touch` does, and
what every touch-based workaround in the wild does — takes the first branch and
produces `IN_ATTRIB`, which most watchers ignore. The third row above is in the
suite as a control precisely so that asymmetry stays measured rather than
remembered. mtime is written back as its own current value, so nothing
observable changes and no build system sees a newer file.

**`open(O_WRONLY)` + `close()` is free.** The close event's mask comes from
`f_mode`, not from whether anything was written, so `IN_CLOSE_WRITE` costs no
bytes. `O_TRUNC` would also produce it and would destroy the file; it is never
correct here.

**Creates work, which was not expected.** `open(O_CREAT)` on a file the client
had just created still fired `IN_CREATE` — the container's dcache holds a
negative dentry for a path it has looked up and not found, so the create goes
to the wire and the VFS treats it as a creation.

**Deletes do not work, and this is the honest gap.** `unlink()` of a name the
client already removed fails with `ENOENT` before any event is generated. ADR
0014 therefore stays **open**, narrowed to exactly this.

### Three states, defaulting to off

`REMOTE_DOCKER_WATCH` is `off` (default), `partial` or `coarse`.

`partial` replays only what can be synthesised faithfully — writes and creates
— and never fires an event that did not happen. `coarse` adds a
directory-level poke for deletes and renames, which produces
`IN_MODIFY|IN_ISDIR` on the parent: enough for a watcher that rescans, and a
lie about the event *kind* for one that does not. That is the user's trade to
make, which is why it is a setting and not a heuristic.

**Default off, deliberately, not provisionally.** inotify is not recursive:
`inotify_add_watch` covers one directory and reports only its direct entries,
so a tree costs one watch per directory. macOS is worse -- fsnotify's kqueue
backend opens a file descriptor per *file*, not per directory, which is why the
budget there is 512 against Linux's 4096. Build outputs are deliberately not
excluded, so a Rust or Java tree exhausts the budget inside `target/`, and the
first thing such a user would meet is a warning naming that directory.

That is an acceptable cost for someone who wants hot reload and a poor
introduction for everyone else, so it is opt-in. Turning it on is one
environment variable.

Worth recording because it was got wrong once: fsnotify *does* implement
recursive watching on Windows, where `ReadDirectoryChangesW` supports it
natively -- `backend_windows.go` passes `watch.recurse` straight through. It is
simply not reachable: "Recursive watching is not currently enabled through
fsnotify's public API; the recursive code path is gated and only exercised by
fsnotify's own tests." So on Windows the per-directory cost is a library
limitation rather than an OS one, and a future fsnotify release could remove it
without any change here.

### Closing the echo loop

The container writes → our NFS server applies it to the client's disk → the
client's watcher reports it → we ship it back → the agent pokes → the
container's watcher fires for its own write. If the poke itself travels back
over NFS, it loops forever.

Two properties close it, and both fall out of owning the NFS server:

- `open(O_WRONLY)` + `close()` produces **no NFS traffic at all**. NFSv3 is
  stateless and has no OPEN operation, so this primitive cannot echo by
  construction.
- the `utimensat` poke writes mtime back as its existing value, so the
  resulting `SETATTR` is an identity the server declines to apply. Not a lie —
  there is genuinely nothing to change.

Docker Desktop solves the same problem by having its FUSE client lie to a
well-known PID. Owning both ends of the protocol is a better position than
owning one end and a marker.

## Consequences

- **The edit-reload loop works for writes and creates**, which is most of it:
  editors save, and save-as-create is the atomic-save idiom. This is the first
  time the "real filesystem, not a sync" claim buys anything for hot reload.
- **Deletes remain unrepresentable** in `partial`, and are a directory-level
  approximation in `coarse`. ADR 0014 stays open on exactly that and nothing
  else.
- **No file contents ever cross this channel.** The bytes are already in the
  container through the NFS mount; only the notification was missing. If that
  ever changes, this stops being a filesystem and becomes a sync, and the
  project's central claim changes with it.
- The agent gains a second thing it does with paths from the client, so
  `FSEvent.Validate` is checked on **both** sides — this stream tells a root
  process which path to touch, and neither end may assume the other checked.
- `test/watchprobe` reads raw inotify rather than using fsnotify, permanently.
  fsnotify's inotify mask omits `IN_OPEN` and `IN_CLOSE_WRITE`, so the library
  cannot observe the primitive this record is chiefly about. A probe that
  cannot see the thing under test reports "nothing happened" and is believed.
- **Nothing is delivered until the session connects**, which is on the first
  Docker request (ADR 0015), not when `up` starts. This is a consequence of
  lazy connections rather than a decision here, and it is benign for the
  reason ADR 0015 already gives: `hasLiveDependents` holds the connection open
  while any owned container runs, and a hot-reload workflow has a running
  container by definition. Edits made before the first connection are counted
  and announced as a `disconnected` notice rather than dropped in silence.
  It did cost a CI round trip, because the first version of the integration
  test started a second client purely to watch -- which never issued a Docker
  request, never connected, and would have collided on the account's single
  NFS port if it had.
- The matrix stays in the integration suite rather than being deleted as
  scaffolding. It is cheap, and it is the only thing that would notice a kernel
  or dockerd change quietly taking one of these primitives away.

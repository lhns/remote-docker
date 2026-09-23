# 0016. Replaying change events as real syscalls

- Status: Accepted
- Date: 2026-08-07

## Context

A container watching the share sees **zero** inotify events for an edit made
on the user's machine ([ADR 0014](0014-inotify-does-not-see-client-changes.md)),
so every hot-reload workflow silently does nothing. The client can watch its
own filesystem, where notification works, and tell the workspace; the question
is what the workspace can then *do*.

## What is not possible

**Linux offers no way to inject a synthetic inotify or fanotify event**: no
syscall, no ioctl, no rejected proposal. `fanotify(7)`: fanotify "reports only
events that a user-space program triggers through the filesystem API. As a
result, it does not catch remote events that occur on network filesystems."
The only entry points are the `fsnotify_*()` inlines in
`include/linux/fsnotify.h`, callable only from VFS code.

Two apparent escape hatches are not real:

- **SMB2 `CHANGE_NOTIFY`** is implemented by cifs.ko, but delivered to
  userspace through a private ioctl (`CIFS_IOC_NOTIFY`). It is not wired into
  fsnotify at all, so an inotify watch on a CIFS mount still sees nothing.
- **NFSv4.1 `CB_NOTIFY`** exists in the protocol and is **not implemented** in
  the Linux client. Server-side directory-delegation patches were still RFC as
  of June 2025, and even those route nothing into inotify. Waiting for the
  protocol to solve this is not a plan.

So a real VFS operation inside the workspace is the only mechanism, for anyone.
Docker Desktop ships the same as "Event Injection": the host watches, forwards
over gRPC, and a thread in the VM replays the operation.

## Decision

The client streams change events to the agent over a long-lived SSH channel
(`workspace-notify`), and the agent replays each one as a **minimal,
non-destructive syscall** on the file as the workspace sees it.

### Where the agent pokes

dockerd's `local` driver mounts each `rd-<id>` NFS volume once at
`/var/lib/docker/volumes/rd-<id>/_data` and bind-mounts it into every container
using it. A bind mount shares the superblock, and inotify marks live on the
**inode**, so a poke at the volume mountpoint reaches the mark a watcher in the
container set. Measured:

```
workspace: POKE stat /var/lib/docker/volumes/rd-1ff11cc0dcd1bd6a/_data/poke-openclose.txt ok dev=54 ino=12898025653570747775
container: POKE stat /data/poke-openclose.txt                                            ok dev=54 ino=12898025653570747775
```

So no container enumeration, no PID lookup, no `setns`, no `nsenter` (and
`util-linux` is not in the image).

> **Amended by [ADR 0019](0019-a-dockerd-per-account.md).** With a dockerd per
> account the mountpoint is reported by *that* daemon, in its own filesystem,
> so `/proc/<pid>/root/...` becomes the route. The account is root inside that
> daemon, so the path is attacker-controlled input to a root process:
> `replay.Relocate` (`core-agent/replay/relocate.go`) checks the result stays
> under the daemon's root rather than trusting `path.Join`, and `O_NOFOLLOW` /
> `AT_SYMLINK_NOFOLLOW` stop a planted symlink redirecting a poke.

### Which syscalls, and why those

Measured in CI against a real dind daemon and a real kernel NFS mount
(`test/integration.sh` section 11d, `test/probes/pokeprobe`):

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
and produces a real `IN_MODIFY`. Setting *both* times, which is what `touch`
and every touch-based workaround does, produces `IN_ATTRIB`, which most
watchers ignore. That row stays in the suite as a control so the asymmetry is
measured rather than remembered. mtime is written back as its current value,
so no build system sees a newer file.

**`open(O_WRONLY)` + `close()` is free.** The close mask comes from `f_mode`,
not from whether anything was written. `O_TRUNC` would also produce it and
destroy the file; it is never correct here.

**`O_CREAT` would give a real `IN_CREATE`, and is not used.** The container's
dcache holds a negative dentry for the new path, so the create goes to the
wire and the VFS treats it as a creation. But the file may have been deleted
again since the client saw it, and `O_CREAT` would then create it in the
user's project. So a create is replayed as a poke of the file plus its parent
directory (`core-agent/replay/replay.go`): a watcher keyed on the file sees a
write, one keyed on the directory rescans.

**Deletes do not work.** `unlink()` of a name the client already removed
fails with `ENOENT` before any event is generated. ADR 0014 stays **open**,
narrowed to exactly this.

### Three states, defaulting to off

`REMOTE_DOCKER_WATCH` is `off` (default), `partial` or `coarse`.

- `partial` replays only what can be synthesised faithfully (writes, creates)
  and never fires an event that did not happen.
- `coarse` adds a directory poke for deletes and renames: `IN_MODIFY|IN_ISDIR`
  on the parent, enough for a watcher that rescans and a lie about the event
  *kind* for one that does not. The user's trade, so a setting, not a
  heuristic.

**Default off, deliberately, not provisionally.** inotify is not recursive, so
a tree costs one watch per directory; fsnotify's kqueue backend on macOS opens
one descriptor per *file*, hence a budget of 512 there against 4096 on Linux
(`core-client/fswatch/backend.go`). Build outputs are not excluded, so a Rust
or Java tree exhausts the budget inside `target/` and the user's first sight
of the feature is a warning. Fine for someone who wants hot reload, a poor
introduction for everyone else.

On Windows the per-directory cost is fsnotify's, not the OS's:
`backend_windows.go` implements recursion over `ReadDirectoryChangesW`, but
"Recursive watching is not currently enabled through fsnotify's public API", so
a future release could remove the cost with no change here. *(Checked
2026-09-23 against fsnotify v1.10.1: `grep -n 'Recursive watching'
fsnotify.go` in the module cache.)*

### Closing the echo loop

A container write reaches the client's disk through our NFS server, the
client's watcher reports it, the agent pokes, and the container's watcher fires
for its own write. If the poke travelled back over NFS it would loop forever.
Owning the NFS server closes it:

- `open(O_WRONLY)` + `close()` produces **no NFS traffic**: NFSv3 has no OPEN.
- The `utimensat` poke writes mtime back as its existing value, so the
  `SETATTR` is an identity the server declines to apply.

Docker Desktop instead has its FUSE client lie to a well-known PID; owning both
ends is a better position than one end and a marker.

## Consequences

- **The edit-reload loop works for writes and creates**, which is most of it:
  editors save, and save-as-create is the atomic-save idiom.
- **Deletes remain unrepresentable** in `partial` and approximated in `coarse`;
  ADR 0014 stays open on exactly that.
- **No file contents ever cross this channel.** The bytes are already there
  through NFS. If that changes, this is a sync, and the project's central claim
  changes with it.
- `notify.Event.Validate` is checked on **both** sides: this stream tells a
  root process which path to touch, and neither end may assume the other
  checked.
- `test/probes/watchprobe` reads raw inotify, permanently. fsnotify's mask
  omits `IN_OPEN` and `IN_CLOSE_WRITE`, so a probe built on it cannot see the
  primitive under test and reports "nothing happened", and is believed.
- **Nothing is delivered until the session connects**, on the first Docker
  request (ADR 0015). Benign: `hasLiveDependents` holds the connection while
  any owned container runs, and hot reload has one by definition. Edits before
  the first connection are counted and announced as a `disconnected` notice. A
  client started only to watch never connects, since it issues no Docker
  request.
- The matrix stays in the suite: it is the only thing that would notice a
  kernel or dockerd change taking one of these primitives away.

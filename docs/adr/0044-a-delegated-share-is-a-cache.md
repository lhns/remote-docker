# 0044 — A delegated share is a cache, not a snapshot

- Status: Accepted
- Date: 2026-09-01
- Replaces the implementation in [ADR 0043](0043-delegated-is-a-copy.md), whose
  decision — that `delegated` is a copy — stands only in the sense that a cache
  contains one
- Closes [ADR 0014](0014-inotify-does-not-see-client-changes.md) **for delegated
  shares**, and for the first time as the event rather than an approximation of
  one

## What forced it

ADR 0042's measurement left one number unexplained. `cached` removes every
attribute revalidation and still takes 98s to read 300 files at 160ms RTT,
because 300 READs and 422 ACCESSes remain: the file's own bytes and the
permission check, which no attribute cache can avoid. Removing them means not
mounting — and ADR 0043's snapshot did exactly that, reaching 0.06s by giving up
everything else:

| | `cached` | `delegated` as a snapshot |
|---|---|---|
| read 300 files at 160ms | 98.20s | 0.06s |
| a file created here afterwards | visible | **invisible** |
| an edit here | visible | **never** |
| a container's write | reaches this machine | **never** |

A mode that is 1,600× faster and wrong about three things is not a trade
anybody should have to make, and the three are all the same mistake: a copy has
no way to answer for what it does not hold.

## The decision

**A union, not a copy.** Per share the workspace mounts the live NFS export as
the lower layer and a local cache as the upper, and the container binds the
merged view. A read the cache holds costs the workspace's own disk; a read it
does not falls through and is **correct**.

That single property is what the whole design rests on, and it turns every hard
question into a cheap one. A cache still filling, a budget that ran out, a file
skipped for being excluded, a scan that has not reached a directory yet — all of
them are the same state as a file that simply is not cached, and all of them are
right. **Nothing here can make a share wrong; it can only make one slower.**

### The union is fuse-overlayfs, and that was measured

The kernel's own overlay cannot be used. An overlay whose lower is NFS is
readable **only from the mount namespace that created it**: a container gets
EOPNOTSUPP on every lower-backed file while upper-backed files work, and so does
the host in a plain `unshare --mount` with no container involved. Binding the
lower in beside it does not help, so it is namespace identity rather than
visibility, and a volume of `type=overlay` fails the same way whoever mounts it.
docker's own overlay2 escapes this because its lower is ext4.

fuse-overlayfs has none of that, because its lower reads happen in its own
daemon's namespace. It costs 0.01s for 200 cached reads against 0.00s for the
kernel union — nothing beside one 160ms round trip — and the workspace image
already ships it for the Ceph storage driver. All of this is
`test/union-probe.sh`, which runs on every pull request.

### The agent is the only writer, and always through the union

overlayfs leaves the result **undefined** when a layer changes underneath a
mounted union, and it is not theoretical: a file written straight into the cache
layer stays invisible to a container that had already looked for it and missed.
The obvious implementation — fill the cache volume from a second container — is
therefore a silent bug, and the probe asserts both halves so it cannot come
back.

Everything the agent does goes through the merged mount instead, which has the
consequence the whole feature turns on: **the write is a real filesystem
operation in the container's own view, so its inotify fires natively.** Measured
through the union: `IN_MODIFY`, `IN_CLOSE_WRITE`, and — for the first time in
this project — `IN_DELETE`.

Two kernel facts force the mounting into a child process: `setns(CLONE_NEWNS)`
refuses a caller that shares filesystem state, which every Go thread does,
because entering a mount namespace also replaces the caller's root; and entering
the mount namespace alone leaves the process holding a pid from the agent's
namespace while reading the daemon's `/proc`, so `/proc/self` resolves to
nothing and libfuse fails with an ENOENT it reports as a missing upper
directory. The child enters the **pid** namespace as well and runs
fuse-overlayfs as its own child, since `setns(CLONE_NEWPID)` decides where
children are born rather than moving the caller.

### Filling it: cheapest first, without waiting for the scan

The win is a round trip saved per file, so a thousand small files are worth far
more than one large one that costs the same bandwidth and saves a single round
trip. But sorting the whole tree before sending anything makes the first byte
wait for a stat of every file in the project, which on a large one takes longer
than the upload it was meant to speed up.

So the scan feeds a buffer of candidates bounded by the share's own budget,
which **evicts its largest** to make room for smaller ones as more of the tree
is seen, and the upload takes the smallest out of it. After the first hundred
files the two run together. A file sent early may be larger than one found
later; that is the price of not waiting, and it is small, because the buffer
holds everything that fits in the budget and eviction only ever discards files
that were never going to be sent.

**The budget bounds what is copied, never whether the mode runs.** "The budget
ran out" is the same state as "the fill has not reached it yet", so there is no
project size at which `delegated` stops working — it is cached in part, which is
what it is for the whole of its fill anyway.

### The cache is a subset of what is watched

A cached copy of a file that changed here is the one way this mode can be
**wrong** rather than merely slow, and it is worse than a stale attribute:
`cached` goes stale for at most `actimeo`, an uninvalidated cache entry until
something removes it. So the fill honours the watcher's exclude list, and a path
the watcher cannot cover is served live — slower and right, rather than fast and
wrong.

Invalidation rides the watcher, which hands the cache every change **before**
the mode strips anything. That distinction is the point: a deletion cannot be
replayed faithfully over NFS, which is what `partial` is about, but it can be
applied to a cache exactly — and it is the one event the cache must not miss.
The Docker API cannot help here at all: it can write into a volume and never
remove from one, which is the whole reason the agent needs a channel.

### Write-back: baselines first, clocks last

The upper layer **is** the record of what the container changed, so no heuristic
is needed. The manifest — what the fill sent, with each file's size and time as
it was **here** — makes both sides answerable separately:

| your file vs manifest | cached file vs manifest | outcome |
|---|---|---|
| unchanged | changed | the container wrote it → write it back |
| changed | unchanged | you wrote it → nothing comes back |
| unchanged | whiteout | the container deleted it → delete it here |
| changed | whiteout | conflict; your file is kept |
| changed | changed | conflict; last writer wins |
| not in the manifest | anything | left alone |

Only the last-writer case needs a clock, and the offset between the two machines
is measured through `workspace-info` rather than assumed away. Every conflict is
reported by path whichever way it resolves.

**Nothing is written back while the cache is incomplete.** A file the fill never
sent looks exactly like one the container created, and the cost of that
confusion is content appearing in somebody's source tree that they never wrote.

## Consequences

- **`delegated` requires the watcher**, exactly as `cached` does, and for a
  stronger reason: without it the cache goes stale rather than merely lagging.
- **It requires `fuse-overlayfs` where the account's daemon runs.** In
  per-account mode that is the dind's image, which is why the workspace's own
  image is the right one to run there — already this project's recommendation
  for the storage driver (`daemons/plan.go:38`). The workspace reports whether
  it can serve a union, and the client refuses the mode by name, before creating
  anything, naming the remedy.
- **A container's write reaches this machine after a delay**, not immediately.
  That is the one guarantee a plain mount has and this does not, and it belongs
  in the README rather than a footnote.
- **A FUSE daemon per share is a process to supervise.** It fails loudly —
  ENOTCONN — and "up" means the MOUNT answers, never that the process is
  running: after an agent restart the server is an orphan whose mount still
  serves, and a killed server leaves a mount that answers nothing.
- **A mount that has gone wrong stays wrong until the last container lets go of
  it**, which CLAUDE.md already says of every mount here. Remounting at the same
  path does not repair a container already bound to the dead one, so a live
  union is adopted rather than replaced.
- **Disk**: one cache per share per client, growing with what the container
  writes as well as with the tree.

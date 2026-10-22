# 0044 — A share with `write != through` is a union, not a snapshot

- Status: Accepted
- Date: 2026-09-01, last amended 2026-10-22
- Supersedes the retired 0043 (`delegated` as a copy), which stands only in
  the sense that a cache contains one
- Closes [ADR 0014](0014-inotify-does-not-see-client-changes.md) **for a
  union**, with the real event rather than an approximation
- Current answer: a share with `write != through` ([ADR 0042](0042-mount-consistency-modes.md))
  is a union whose lower is the live export, mounted with the share's read
  mode, and whose upper is a local layer the agent alone writes; the page
  cache is the cache and the union is where a batch lands ([ADR 0045](0045-prefetch-follows-the-reads.md)).
  A landed file is overwritten in place, never emptied first and never renamed
  in; write-back on this machine renames a complete temporary over the file.
  Write-back never stops for good: it progresses per file with a deadline per
  call, tells a failure that mends (a full disk, the network) from one that
  never will (a name this machine cannot hold), backs off from 5s to 1 min and
  says so in `remote status`. The cache channel is reopened when it dies, a
  failed fill batch is retried, and the agent drains a payload it did not read.
  An overlay's own marks are read and never written into the user's tree; a
  replaced directory and a link the container made come back (`Change.Opaque`,
  `Change.Link`, negotiated by `Hello.Carry`). Nothing is deleted or
  overwritten here without checking the ground truth as it is done: a deletion
  a directory implies only if the consumer's merged view confirms it
  (`OpPresent`), and every path only if this machine still has what decide saw.
  A union is released only once no container is bound to it AND the machine's
  last cache session has ended, and never one a `Prepare` answered with while
  `Release` was asking the daemon.
  A share the workspace calls unknown while its filesystem still has the union
  mounted (an agent restarted under a running container) is prepared again,
  which adopts it, before it is given up.

## What forced it

`cached` (ADR 0042) removes every attribute revalidation and still takes 98.1s
to read 300 files at 160ms RTT: 300 READs and 422 ACCESSes remain, which no
attribute cache can avoid. Removing them means not mounting, and a snapshot of
the tree did that, reaching 0.06s by giving up everything else:

| | `cached` | `delegated` as a snapshot |
|---|---|---|
| read 300 files at 160ms | 98.1s | 0.06s |
| a file created here afterwards | visible | **invisible** |
| an edit here | visible | **never** |
| a container's write | reaches this machine | **never** |

A copy has no way to answer for what it does not hold.

## The decision

**A union, not a copy.** Per share the workspace mounts the live NFS export as
the lower layer and a local cache as the upper, and the container binds the
merged view. A read the cache holds costs the workspace's own disk; a read it
does not falls through and is **correct**. So these are one state, and all
correct: the fill is still running, the budget stopped it short, the path is
excluded, the scan has not reached that directory.

### Where the policy lives

`dircache`, a module with **no requires at all** (ADR 0021): what to copy and
in what order, what a local change means for a cache, what a cached change
means for somebody's source tree, naming no transport and no storage.

| | where | knows |
|---|---|---|
| policy | `dircache` | nothing of SSH, Docker, tar, zstd, overlayfs |
| the wire | `core/cache`, `client/internal/session/cache.go`, `agent/internal/sshd/cache.go` | the frame, the codec, the tar |
| the mount | `core-agent/union`, `agent/internal/unions` | fuse-overlayfs, the namespaces, the volume |

`dircache.Store` is the seam: apply a batch, drop paths, ask what changed,
fetch files. It hands FILES rather than an archive both ways, so the channel
builds and unpacks the tar and the policy never sees one. What the split buys:
the engine can be taken without `core-client`'s websocket, fsnotify, go-nfs,
go-billy, gliderlabs/ssh and x/crypto, which a package inside that module could
not offer.

### The union is fuse-overlayfs, and that was measured

The kernel's overlay cannot be used. An overlay whose lower is NFS is readable
**only from the mount namespace that created it**: a container gets EOPNOTSUPP
on every lower-backed file (upper-backed files work), and so does the host
under a plain `unshare --mount`. Binding the lower in beside it does not help,
so it is namespace identity rather than visibility, and a `type=overlay` volume
fails the same way whoever mounts it. docker's overlay2 escapes this because
its lower is ext4.

fuse-overlayfs does its lower reads in its own daemon's namespace. It costs
0.01s for 200 cached reads against 0.00s for the kernel union, and the
workspace image already ships it for the Ceph storage driver. Both are
`test/union-probe.sh` sections 6 to 6d, on every pull request; the kernel
union's refusal is recorded there, not asserted.

### The agent is the only writer, and always through the union

overlayfs leaves the result **undefined** when a layer changes under a mounted
union, and in practice a file written straight into the cache layer stays
invisible to a container that already missed on it. So filling the cache volume
from a second container is a silent bug. `test/union-probe.sh` section 4
asserts that a write THROUGH the merged mount is seen after a miss, and records
(does not assert) the write into the layer.

Everything the agent does goes through the merged mount, so **the write is a
real filesystem operation in the container's own view and its inotify fires
natively**: `IN_MODIFY`, `IN_CLOSE_WRITE` and, for the first time here,
`IN_DELETE` (sections 5 and 6d).

### A file lands without being emptied first (amended 2026-10-15)

- `writeEntry` used to open the target with `O_TRUNC`, so a file a container
  had open or mapped was emptied and rewritten under it: a reader saw it
  short, and a mapping (git maps its index, packs and the files it hashes) died
  with SIGBUS. It now overwrites from the start and calls `ftruncate` only when
  the new content is SHORTER. A refill of unchanged content, which is what most
  invalidations and prefetch batches are, never changes the size a reader
  sees. Still open: a changed file is torn for a reader mid-write, and a
  mapped file that shrinks faults on the part past the new end.
- **Writing a temporary and renaming it over the target was tried and
  rejected.** It would have been atomic, but fuse-overlayfs does not whiteout
  on a later unlink of a renamed-in file: after `rm`, the lower's file shows
  through (`test/union-probe.sh` section 6d, run 2026-10-15: GONE after `rm` of
  a file landed in place; VISIBLE after `rm` of one landed by rename, whether
  or not the target was copied up, read or looked up first; GONE only after a
  second `rm`). A container deleting a file the cache had refilled would see it
  come back, and the end-to-end case `read=cached,write=ephemeral` failed on
  it. Re-check with the probe if fuse-overlayfs changes.
- Write-back on this machine does rename, because there is no overlay in the
  way: `dircache/writeback.go` writes `.rd-writeback-<random>` under the
  share's `os.Root` (`O_EXCL`) and renames it over the user's file. It used to
  truncate the user's file and then copy the body from the network, so a
  stream that failed half way left a source file cut short. A body that fails
  leaves the file as it was. A rename Windows refuses (another program holds
  the file without share-delete) fails the round before the manifest moves,
  so the change is still pending and the next round, five seconds later, tries
  again; it never falls back to truncating. The fill's walk and the watcher
  (`core-client/fswatch`) ignore that prefix. The temporary is visible to
  `ls` and to a git status for the length of one write; accepted.

### The child enters three namespaces

Mounting is done by a child that enters the daemon's **pid**, **network** and
**mount** namespaces, in that order:

- `setns(CLONE_NEWNS)` refuses a caller that shares filesystem state, which
  every Go thread does, hence a child.
- The mount namespace alone leaves an agent-namespace pid reading the daemon's
  `/proc`, so `/proc/self` resolves to nothing and libfuse fails with an ENOENT
  reported as a missing upper directory. So the child enters **pid** too and
  runs fuse-overlayfs as its own child (`setns(CLONE_NEWPID)` decides where
  children are born).
- **network**: with a daemon per account the reverse forward carrying the
  export is bound inside that daemon's netns (ADR 0019), so a lower mounted
  from the agent's namespace has no server.

### The union is a landing zone, not a cache (amended 2026-09-04)

- Coherent caching of what HAS been read is `cached`'s: page cache for the
  bytes, `actimeo=60` for the attributes, the watcher's replayed SETATTR for
  the one inode that changed. A union adds nothing to a re-read.
- It adds a place a file can be put BEFORE it is read that merges with the
  live view: the first read of a small file is two serial round trips, a
  batch over the cache channel is one.
- The fill is opt-in (`prefetch: eager|tree`, off by default); policy, rules
  and walk are [ADR 0045](0045-prefetch-follows-the-reads.md). The
  smallest-first walk this record used to describe is `eager`.
- **The lower carries the share's read mode.** Until 2026-09-04 it was
  mounted `consistent`, `actimeo=1`, so every file the fill had not reached
  revalidated every second. `Spec.Read` names it, the child receives it as
  `RD_UNION_READ`, and `union_test.go` pins the option string.

**The budget bounds what is copied, never whether the mode runs.** "The budget
ran out" is the same state as "the fill has not reached it yet".

### Compression is a negotiation, not a format

- A codec wraps the payload byte stream with no protocol change; the frame's
  codec field has been there since version 1.
- The agent announces what it reads in its greeting and the client picks from
  THAT list: a workspace older than compression names none, and a client
  choosing for itself would send what it refuses.
- **zstd, which cost the agent a dependency**: 24 `go.sum` lines to 28 (ADR
  0021 carries the count). Paid deliberately, since the fill is the one bulk
  transfer and zstd compresses a source tree harder and faster than gzip.
- Client-to-agent only. Write-back carries what one container wrote since the
  last round, so it stays a plain tar rather than paying a compressor per poll.

### The cache is a subset of what is watched

A cached copy of a file that changed here is the one way this mode is
**wrong** rather than slow: `cached` is stale for at most `actimeo`, an
uninvalidated cache entry until something removes it. So the fill honours the
watcher's exclude list, and a path the watcher cannot cover is served live.

Invalidation rides the watcher, which hands the cache every change **before**
the mode strips anything: a deletion cannot be replayed over NFS (the
`partial` gap), but it can be applied to a cache exactly. The Docker API can
write into a volume and never remove from one, which is why the agent needs a
channel.

### The lower's options are not a mount(2) argument

`workspace.NFSVolumeOptions` is the list docker's local driver takes, and that
driver splits kernel FLAGS out before calling mount(2). Passed whole,
`noatime` (`MS_NOATIME`, not an NFS option) makes the NFS parser refuse the
entire list with EINVAL, printed as `invalid argument`, about a list whose
every word is valid. `Spec.LowerMount` returns the two halves and the error
prints both. This kept the union from ever mounting until 2026-09-04.

### "Up" means MOUNTED, not that the path exists

A union's directories exist before the mount and outlive it, so a stat says
yes for a share that never mounted or whose server died. Everything reaches a
share by path, so the mode keeps "working" against the bare directory; only
the lower is missing, so a fallthrough read returns nothing and the container's
writes land where nothing looks. The suites assert that a container's share
reports fuse-overlayfs, the one thing a bare directory cannot fake.

Releasing never asks (2026-10-06). The lower is a soft NFS mount whose server
is usually what has just gone, and a stat of it waits out `timeo=600`
retries, minutes, while `umount2(MNT_DETACH)` does not revalidate the root it
detaches. So `Release` detaches the merged path and the lower unconditionally
and reads EINVAL or ENOENT as not mounted. The stat held an ephemeral run's
cleanup, and its cache volume, for over two minutes (`test/ephemeral.sh` 6b).

### A deletion nobody observed

A cache volume outlives its session, and a fill only overwrites and adds. A
file deleted here while nothing ran stays in the cache and in every container,
with no event to explain it.

- The client records what every batch sent (`client/internal/session/cached.go`),
  per workspace, bound to machine and account. The next fill stats those paths
  and drops the ones gone here. Only paths a batch put there are considered: a
  path no batch sent is a container's own file and is never removed.
- Every batch is recorded whatever sent it (prefetch, invalidation), BEFORE it
  is applied, since an interrupted batch may have landed in part. A batch that
  landed unrecorded was a deletion nothing could reconcile (2026-09-24).
- A path leaves the record when it leaves the cache, so a container that later
  creates it is not mistaken for the fill.
- Write-back refuses a path in the record that this run did not send and this
  machine no longer has: it is a cached copy of a file deleted here. A path in
  no record is the container's new file and still comes back.
- A watcher overflow is the same problem inside a session, so
  `fswatch.Observer.Lost` answers it with the same reconcile.
- Not covered, deliberately: a container already running when the client
  restarts keeps what its cache holds until that share is filled again.

### One union per share per machine

- An account's machines share a daemon (ADR 0029), and two of them naming one
  share id is ordinary: one path on two machines is one id. Keyed by account
  and export alone, the second machine's Prepare found the first one's union
  alive and was handed its merged path, so its container read the other
  machine's files.
- So the manager keys a union by account, **client** and export, the
  mountpoints are `/run/rd-union/<client>/<id>/{lower,merged}`, and a cache
  session ending releases only its own machine's unions.
- A union an older agent mounted at `/run/rd-union/<id>` is still reported to
  the collector and waited out before this share mounts again, so an upgrade on
  a VM (ADR 0025) fails a Prepare loudly until the old union goes rather than
  stacking a second one on its upper.

### The collector cannot see a cache volume in use

A union is bound by path, so the daemon reports the volume holding its layer
unused for as long as it exists. Collecting it empties the directory under a
live mount: the container keeps running and the files it wrote are gone.

- `rewrite.Guard` covers the shares this session prepared, not a container
  left running across a client restart. `OpMounted` covers that; cannot ask
  means keep.
- The agent answers from the filesystem, not its own record: after an agent
  restart the unions are still serving and the manager knows nothing, so a
  truthful "none mounted" would delete the cache under a running container.
  The ids come from the mounts under `/run/rd-union/<client>` and the client
  digest from the authenticated key, so a machine hears only of its own caches.
- The agent's own cleanup of an expired ephemeral run (ADR 0050) asks
  `MountedCaches` the same way, and keeps a cache volume it names.

### A union outlives the channel that asked for it

The cache channel rides the SSH connection, which is released when the session
goes idle (ADR 0015). Unmounting with it frees nothing and leaves a running
container a mount that can never be repaired (docker's local driver
refcounts mounts; `test/nfs-resilience.sh` section 6b).
So a share is released only when no container is bound to it. Only the daemon
can say, since a union is bound by path rather than as a volume; on any doubt
the mount is KEPT.

Amended 2026-10-22: and only when the machine's LAST cache session ends.
"No container is bound" was necessary and not sufficient. A union a session has
just prepared is held by nothing until its container is created, and a client
replaces a channel it stopped trusting while the agent is still ending the old
one; that end released what the new channel had prepared, and the container
bound an empty directory. So:

- `Manager.Open` counts a machine's cache sessions, and `Release` does nothing
  while one is open.
- A union a `Prepare` answered with after `Release` asked the daemon is kept:
  that answer cannot include its container. Each `Prepare` stamps the union
  (`live.prepared`), so nothing grows per machine.
- An expired ephemeral run's cleanup (ADR 0050) calls `Release` with no session
  open, unchanged.

### An agent restart loses the record, not the union (amended 2026-10-22)

Where dockerd outlives the agent (ADR 0025) a restarted agent has no record of a
union a running container still binds. Every request about it answered
`no union for this share`, which the client read as released, so write-back
stopped for that share until a new container prepared it.

- Before it takes `Reply.Unknown` as gone, the client asks `OpMounted`. A share
  whose cache volume the workspace's filesystem still has mounted is prepared
  again exactly as its mount prepared it (export, cache volume, port, read
  mode, owner), and the request is asked once more. `Prepare` ADOPTS a serving
  union, so this mounts nothing.
- Not mounted, or the prepare refused, or still unknown: gone, as before.
  Preparing a released union would mount one for nobody, released and prepared
  again on every connection. A channel that broke while asking is an error the
  caller retries, not a reason to forget.
- One reprepare per share at a time per channel; every share request takes
  this path, so a fill or an invalidation meeting the restart recovers too.
- Rejected: the agent adopting lazily from the filesystem on the first unknown
  request. A `live` needs the whole `union.Spec` to remount a union that dies,
  and the port, read mode and owner are known only to the client; `Prepare`
  already adopts, chowns and single-flights.

### Write-back: baselines first, clocks last

The upper holds the container's writes **and the fill's own copies**, since
the fill goes through the union too. The manifest separates them: what the fill
sent, with each file's size and time as it was **here**. An entry matching its
baseline is the fill's copy; anything else is the container's.

| your file vs manifest | cached file vs manifest | outcome |
|---|---|---|
| unchanged | changed | the container wrote it → write it back |
| changed | unchanged | you wrote it → nothing comes back |
| unchanged | whiteout | the container deleted it → delete it here |
| changed | whiteout | conflict; your file is kept |
| changed | changed | conflict; last writer wins |
| not in the manifest | anything | left alone |
| unchanged | identical to the manifest | the fill wrote it; nothing happened |

- Only last-writer-wins needs a clock; the offset between the machines is
  measured through `workspace-info`. Every conflict is reported by path.
- The fill's copies are filtered on **both** ends. The client's check is the
  rule; wrong there, a file is written back with the bytes it already has and
  settles next round. The agent's record of what it applied keeps the reply
  proportional; without it a fully cached tree is listed every five seconds.
- **Nothing is written back while the cache is incomplete.** A file the fill
  never sent looks exactly like one the container created, and the cost is
  content appearing in somebody's source tree that they never wrote.

### Write-back never stops for good (amended 2026-10-21)

Write-back never stops for good. Each failure is classified (below) and either
backed off or set aside and named, and a share that is stalled says why in
`remote status`. The rule behind every answer: **nothing here is deleted or
overwritten on bookkeeping or on the absence of a report; each is checked
against the ground truth at the moment it is acted on.**

**The channel and the agent**

| cause | what it did | answer |
|---|---|---|
| a failed apply left the rest of its tar unread | the next request was read out of the middle of it: every request on the channel, for every share, failed with `invalid character ... looking for beginning of value` | the agent drains the payload whatever the extraction did |
| the channel was opened once per connection | after any timeout (which closes the stream) nothing could be asked until the connection was replaced, which a running container can prevent for ever | `cacheChannel` marks itself dead on a closed stream or a failed read or write, and `ensureCacheChan` reopens it, one open at a time, closing the old one. A refusal in words (`Reply.Err`) does not retire it |
| fuse-overlayfs writes a replaced directory as `trusted.overlay.opaque=y` AND a character device `.wh..opq` AND a regular file `.wh..wh..opq`; a deleted file is a char device 0:0 (`test/union-probe.sh` section 13, fuse-overlayfs 1.13-dev) | the regular marker was reported as the container's file, written into the user's tree, then sent back by the fill or an invalidation, which fuse-overlayfs refuses with EINVAL | the agent reads `.wh.<name>` as a deletion and `.wh..wh..opq`, `.wh..opq` and the xattr as one `Opaque` directory; the client refuses any `.wh.` component both ways and says the file is a leftover safe to delete |

**Progress**

| cause | what it did | answer |
|---|---|---|
| one round was all or nothing, under one 2 minute deadline | a burst bigger than the budget moved nothing, and one file this machine refuses stopped every file behind it | a deadline per call, 200 files per pull, at most 2,000 landed per pass; the manifest and the record move per file |
| one failed pull chunk ended the pass, and the agent failed the chunk for one file | an unreadable file starved every file behind it | a failed chunk ends only itself (three in a row end the pass); under `Carry` the agent names an unreadable file in `Reply.Skipped`, set aside until its change differs |
| files that keep failing sorted among the rest | 2,000 files that never land starved the others | paths that keep failing rank behind the rest (`wbRanked`); a pass that landed anything backs off only 5s |
| a failed fill batch ended the fill | the share was never complete, so write-back, which waits for a complete cache, never ran | a refused batch is taken back out of the tree (`Tree.Unstore`) and retried, 1s doubling to 5 min, said once; status shows the error and the next try |
| a landed file keeps its time to the second (a tar does), and the consumer's own reading never equals that | the same version read as a new change every round and was written back again, a loop at 2,000 files a pass | `Baseline.Consumer` remembers the consumer's time for a version carried back, and persists |
| a conflict was logged every round | "your file is kept" every 5s | said once per path, again only after the path lands or stops being listed |

**Restarts**

| cause | what it did | answer |
|---|---|---|
| restoring waited for a MOUNT or root handle | a container that only writes into its union presents neither, so a restarted client carried back nothing | the session restores the recorded unions the workspace reports mounted as it connects (`Session.reattachUnions`, ADR 0027) |
| the record held paths and not baselines | a file the container modified after a restart read as one this machine might have made and was never carried back | a restored share is attached with its recorded words and the fill record persists each file's size and time beside its path |
| a share is recorded before its volume exists (a union is prepared first, which takes seconds), and the prune after connect forgot records with no volume | a client restarted under that container had no record to restore | a prune keeps the record of every share this session has registered |
| a resumed fill sent the whole tree again | it overwrote what a running container had written over those files | the fill skips what the record says went and has not changed here (`alreadyHeld`, `Tree.MarkSent`) |
| a resumed fill re-sent a file changed here while nothing ran | when the container had changed it too, its version was overwritten in the cache and the baseline reset, so the both-changed rule never ran | the fill leaves out a path it sent before that the consumer lists as changed (and not as the fill's own copy, which a restarted agent also lists), says so once, and write-back decides it with the persisted baseline. Only the fill: an invalidation is the user writing now, and leaving it out would keep the container on its version for good once a conflict kept the user's |

**Never taking the user's work**

| cause | what it did | answer |
|---|---|---|
| the agent lists a replaced directory on every Changes while its marker exists, and hides what the client wrote into it | a file this machine created or edited there read as deleted by the container, and was deleted here and then from the container | a deletion a directory implies is acted on only for a path the consumer's MERGED view confirms absent: `OpPresent`, lstat through the merged mount, never following a link. Present, an error or an agent without `Hello.Carry` all mean keep, and with such an agent no deletion is implied at all |
| the same silence about a file this write-back had just landed in such a directory | the watcher sent it to the cache again, the agent stopped listing it, and write-back read that as the consumer deleting it: 5,000 files made in the container were gone within seconds | a file carried back from the consumer (`Consumer != 0`, kept by `noteSent` while unchanged) is never deleted on a directory's say-so |
| the candidates were computed after skipped changes were set aside | a link this account could not make at `D/F` read next round as the deletion of the user's `D/F` | the candidates come from every change reported |
| decide looked at this machine once, and a pass lands up to 2,000 files over minutes | a file edited here meanwhile was overwritten; one created here meanwhile was replaced | each action carries what decide saw (exists, size, time, link target) and it is compared with a fresh stat just before the remove, the link or the rename (`stillAsSeen`, and the guard `writeUnder` takes); a path that changed is left, said once, and decided afresh |
| the agent removed a file after a failed write if it had not existed a moment before | a file the container created in that moment was removed | the file is opened `O_EXCL` when absent, so only a file the entry created is removed; a temporary renamed in would do the same and is what a landed file must not be (above) |

**What a failed write means.** `classifyWrite` sorts it, by platform:

| class | errors | what happens |
|---|---|---|
| space | ENOSPC, EDQUOT; Windows 112, 39 | the pass stops, the share backs off 5s doubling to 1 min and resets on success, and the file and the changes behind it stay pending |
| name | EINVAL, ENAMETOOLONG, EILSEQ; Windows 123, 206, 1314; a path leaving the share; a case collision | that path is given up on and named once, in the log and in `remote status`, and tried again only when its change differs; the others carry on |
| other | anything else, such as a file another program holds | left for the next round with the same backoff; the others carry on |
| unreadable | the agent could not read it (`Reply.Skipped` on a pull) | given up on like a name, until its change differs |

A pass that landed anything waits only the 5s floor, whatever else failed, so
one file that never lands does not slow every other to once a minute. One Warn
says a share stalled (why, the first path, how many are pending, when it tries
next) and one Info says it moved again.

**An overlay marker never reaches the user's tree.** A name starting `.wh.` is
the union's own vocabulary and no file can be named so inside one. The agent
reports `.wh.<name>` as `<name>` deleted; the client drops any change with such
a component, an older agent's included, and the fill and an invalidation leave
such a name out and warn once.

**A removed or replaced directory is expanded on the client, and confirmed.**
The layer says a deletion once, at the level it happened: a whiteout for a
directory, or an `Opaque` directory. The client takes every path the FILL SENT
under it as a candidate (`replacedDeletions`), asks the consumer which of them
its merged view still has (`OpPresent`), and deletes only what is confirmed
gone; each deletion then goes through `decide` like any other, so a file
changed here since is kept and reported. The listing cannot answer this alone:
the agent hides what the client wrote through the union, so a file the user
created or edited there is silent in it exactly like one the container deleted.
A file the fill never sent is deliberately not touched: under `prefetch=tree`
most of a large tree was never sent, and a directory the container replaced
leaves what was only ever on this machine, where it was.

**Links are carried back by the rule the live share already follows**
(README, "What differs from a bind mount"; `core-client/nfsserve/symlink.go`):
make the link if this machine allows it, otherwise say so once and let nothing
stand in for it. The agent reports a link in the layer with its target, verbatim
and never followed; the client makes it under the share's `os.Root` through a
temporary and a rename. An absolute target, or one leading out of the share, is
still made: a link is a name and making one writes nothing through it, and
`os.Root` keeps every later write from following it out. Without Developer
Mode or elevation Windows refuses (1314): the link is a permanent per-path skip,
counted and named in status, and never a copy of its target or a junction. A
link here that differs from the container's is kept and reported. The fill does
not send links: the live export serves them on a read that falls through.

**Negotiated, and nothing breaks across versions.** `cache.Hello.Carry` is the
agent announcing it; `Request.Carry` is the client asking, and only if
announced. An agent without it ignores the field: it reports no link or
replaced directory (status says the workspace is too old, with the fix), fails
a batch on an unwritable entry as it always did, and the client never asks it
`OpPresent` and implies no deletion. With `Carry` the agent leaves an
unwritable entry out and names it in `Reply.Skipped`; the client takes it out of
the record and the manifest, so the file is read live and a copy the consumer
makes later is the consumer's. A client without it never sees a field it would
misread: a link reported to it would read as an empty file.

**Deliberately not carried back:** a name this machine refuses (named once), a
case collision on a filesystem that treats the two names as one (named once;
landing it would replace the other file), a link this account may not make, and
under `prefetch=tree` or `off` the files a replaced directory hid that the fill
never sent.

**A link write-back made is then invalidated like any change.** The watcher sees
it, `invalidate` stats it (following the link) and sends the target's bytes under
the link's name, and the agent writes them through the union. That is wasteful
and not lossy: the content is the target's own. Skipping links there would
leave a stale regular copy shadowing a file later replaced by a link, and a
drop would be a whiteout over the lower's link, so it is left as it was.

**Not decided here.** Whether a connection released while a container still
writes can lose its tail is open; it is not shown by `integration.sh` 16b (18
files, no client command, three minutes).

## What it measured

- **Write-back reproductions, 2026-10-21 (PR 342).** Before the fixes: a
  directory the container replaced left `tools/old` here and wrote
  `tools/.wh..wh..opq` into the tree; a restarted client carried back neither a
  modified file nor a new one; every request after a failed apply answered
  `invalid character`; and a Windows client never received `good` created beside
  `bad?name`. After: a full disk (a 2 MiB tmpfs for a 4 MiB file) recovered by
  itself, 5,000 files in one burst all arrived, and a container writing a file
  every 10s for three minutes with no client command lost none.

`test/bench.sh` on a GitHub runner, 2026-09-01, 300 files, ALL ROWS FROM ONE
RUN. Seconds; `nfs_ops` is what the mount was asked for during the read.
Re-check with the `bench` label on a pull request. ADR 0042's table is a
different run: compare within a table, never across.

| RTT | mode | start | walk | read 300 | write | nfs_ops during the read |
|---|---|---|---|---|---|---|
| 0.1ms | `read=direct,write=through` | 0.14 | 0.09 | 0.41 | 0.75 | READ=300 ACCESS=535 GETATTR=437 |
| 0.1ms | `read=cached,write=through` | 0.14 | 0.09 | 0.30 | 0.19 | READ=300 ACCESS=422 |
| 0.1ms | `read=cached,write=back` | 0.28 | 0.44 | 0.33 | 0.14 | READ=349 ACCESS=300 |
| 40ms | `read=direct,write=through` | 0.38 | 18.91 | 32.49 | 12.68 | GETATTR=3552 |
| 40ms | `read=cached,write=through` | 0.38 | 2.99 | 24.46 | 12.28 | READ=300 ACCESS=422 |
| 40ms | `read=cached,write=back` | 0.14 | **0.06** | **0.09** | **0.08** | **none** |
| 160ms | `read=direct,write=through` | 1.10 | 96.98 | 164.47 | 74.00 | GETATTR=4220 |
| 160ms | `read=cached,write=through` | 1.10 | 11.64 | 98.12 | 49.43 | READ=300 ACCESS=422 |
| 160ms | `read=cached,write=back` | **0.15** | **0.06** | **0.08** | **0.08** | **none** |
| 10mbit | `read=cached,write=back` | 0.14 | 0.06 | 0.08 | 0.08 | none |

The union rows are AFTER the fill has landed. Once the upper holds the tree
the wall clock stops tracking latency: 0.08s at 160ms against 98.12s and
164.47s for the mounted modes, and `nfs_ops` shows nothing asked of the mount
where `read=cached` still pays 300 READs and 422 ACCESSes. Start stays 0.15s
against 1.10s because a container never waits for the fill. A COLD union, and
how long the fill takes to land, is ADR 0045's table (2026-09-04, which failed
its criteria). The old `cold`/`settle`/`warm` table is retired: `settle` means
nothing for a fill driven by reads.

## Consequences

- **A union turns the watcher on for its share**, as `read=cached` does, and
  more so: without it the cache goes stale rather than lagging. A mount naming
  `watch=off` on a union is refused (ADR 0042, amended 2026-10-14). Its merged
  root is owned by the share's `owner=` too: the child chowns the upper, and a
  workspace that cannot is refused for a non-root owner (ADR 0046).
- **It requires `fuse-overlayfs` where the account's daemon runs.** Per
  account that is the dind's image, so it must be the workspace's own
  (`elevate.ImageEnv`, `WORKSPACE_IMAGE`); `daemons.DefaultImage`
  (`docker:28-dind`) is the last resort and lacks it. The workspace reports
  whether it can serve a union (`workspace.Info.Union`), and the client refuses
  the mode by name, before creating anything, naming the remedy. It also needs
  FUSE in the workspace kernel and `/dev/fuse` in that container; no kernel
  older than the CI runners' 6.x has served one (README, "Running a workspace").
- **A union that cannot start is reported on its second exit (amended
  2026-10-14).** `Prepare` waits for the mount, not the process. The first exit
  of the child is left to the supervisor's two-second restart, since a lower
  mounted a moment before the reverse forward is up fails once and then works;
  a second exit before the mount answered ends the wait with the child's last
  error and discards the union, so one transient failure is absorbed and a
  permanent one is not retried by a loop nobody waits on. A chown that fails
  discards the union rather than leaving one serving the wrong owner. A
  prepare naming another owner than a live union has is refused (ADR 0046);
  since the owner joined the share id (2026-10-15, ADR 0042) a different owner
  is a different union, so that refusal is only a guard.
- **A container's write reaches this machine after a delay.** The one
  guarantee a plain mount has and this does not; the README says so.
- **A FUSE daemon per share is a process to supervise.** It fails loudly
  (ENOTCONN), and "up" means the MOUNT answers, never that the process runs:
  after an agent restart the server is an orphan whose mount still serves.
- **A mount gone wrong stays wrong until the last container lets go of it**,
  so remounting at the same path repairs nothing. A live union is ADOPTED: the
  supervisor waits for a serving mount to go before making another. That is
  safe only because "alive" means MOUNTED; against a stat it would wait forever
  on the empty directory a dead union leaves. st_dev against the parent answers
  from outside the namespace too (2026-09-01, `test/union-probe.sh` section 12:
  unmounted dev 59 against parent 59, mounted 63 against 59). Reachable only
  where dockerd outlives the agent (the VM deployment, ADR 0025; in a container
  the agent is pid 1 and takes every dind with it), so `test/vm.sh` section 5b
  asserts it by counting union servers across an agent restart, and by a write
  the running container makes after the restart reaching the runner before any
  new container prepares the share.
- **Disk**: one cache per share per client, growing with what the container
  writes as well as with the tree.

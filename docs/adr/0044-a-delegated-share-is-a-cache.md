# 0044 — A share with `write != through` is a union, not a snapshot

- Status: Accepted
- Date: 2026-09-01, last amended 2026-09-23
- Supersedes the retired 0043 (`delegated` as a copy), which stands only in
  the sense that a cache contains one
- Closes [ADR 0014](0014-inotify-does-not-see-client-changes.md) **for a
  union**, with the real event rather than an approximation
- Current answer: a share with `write != through` ([ADR 0042](0042-mount-consistency-modes.md))
  is a union whose lower is the live export, mounted with the share's read
  mode, and whose upper is a local layer the agent alone writes; the page
  cache is the cache and the union is where a batch lands ([ADR 0045](0045-prefetch-follows-the-reads.md)).

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

### A union outlives the channel that asked for it

The cache channel rides the SSH connection, which is released when the session
goes idle (ADR 0015). Unmounting with it frees nothing and leaves a running
container a mount that can never be repaired (docker's local driver
refcounts mounts; `test/nfs-resilience.sh` section 6b).
So a share is released only when no container is bound to it. Only the daemon
can say, since a union is bound by path rather than as a volume; on any doubt
the mount is KEPT.

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

## What it measured

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

- **A union requires the watcher**, as `read=cached` does, and more so:
  without it the cache goes stale rather than lagging.
- **It requires `fuse-overlayfs` where the account's daemon runs.** Per
  account that is the dind's image, so it must be the workspace's own
  (`elevate.ImageEnv`, `WORKSPACE_IMAGE`); `daemons.DefaultImage`
  (`docker:28-dind`) is the last resort and lacks it. The workspace reports
  whether it can serve a union (`workspace.Info.Union`), and the client refuses
  the mode by name, before creating anything, naming the remedy.
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
  asserts it by counting union servers across an agent restart.
- **Disk**: one cache per share per client, growing with what the container
  writes as well as with the tree.

# 0044 — A delegated share is a cache, not a snapshot

- Status: Accepted
- Date: 2026-09-01, last amended 2026-09-02
- Supersedes the retired 0043, whose answer — that `delegated` is a copy —
  stands only in the sense that a cache contains one
- Closes [ADR 0014](0014-inotify-does-not-see-client-changes.md) **for delegated
  shares**, and for the first time as the event rather than an approximation of
  one

## What forced it

ADR 0042's measurement left one number unexplained. `cached` removes every
attribute revalidation and still takes 98.1s to read 300 files at 160ms RTT,
because 300 READs and 422 ACCESSes remain: the file's own bytes and the
permission check, which no attribute cache can avoid. Removing them means not
mounting — and a snapshot of the tree did exactly that, reaching 0.06s by
giving up everything else:

| | `cached` | `delegated` as a snapshot |
|---|---|---|
| read 300 files at 160ms | 98.1s | 0.06s |
| a file created here afterwards | visible | **invisible** |
| an edit here | visible | **never** |
| a container's write | reaches this machine | **never** |

All three failures are the same mistake: a copy has no way to answer for what
it does not hold.

## The decision

**A union, not a copy.** Per share the workspace mounts the live NFS export as
the lower layer and a local cache as the upper, and the container binds the
merged view. A read the cache holds costs the workspace's own disk; a read it
does not falls through and is **correct**.

That single property is what the whole design rests on. These are all the same
state, and all correct:

- the fill is still running
- the budget stopped it short
- the path is excluded, so it is never cached
- the scan has not reached that directory

**Nothing here can make a share wrong; it can only make one slower.**

### Where the policy lives

The policy is `dircache`, a module of its own with **no third-party requires at
all** (ADR 0021). What to copy and in what order, what a local change means for
a cache, what a cached change means for somebody's source tree: none of it names
a transport or a storage.

| | where | knows |
|---|---|---|
| policy | `dircache` | nothing of SSH, Docker, tar, zstd, overlayfs |
| the wire | `core/workspace`, `client/internal/session/cache.go` | the frame, the codec, the tar |
| the mount | `core-agent/union`, `agent/internal/unions` | fuse-overlayfs, the namespaces, the volume |

`dircache.Store` is the seam, and it is four operations: apply a batch, drop
paths, ask what changed, fetch files. It hands FILES rather than an archive in
both directions, which is what keeps the encoding on the transport's side of the
line: the channel builds the tar going out and unpacks the one coming back, and
the policy has never seen one.

The consequence worth stating, because it is what the split was for: the engine
can be taken without `core-client`'s websocket, fsnotify, go-nfs, go-billy,
gliderlabs/ssh and x/crypto. A package inside that module could not offer this.

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

- The win is a round trip saved per file, so a thousand small files are worth
  far more than one large one that costs the same bandwidth.
- Sorting the whole tree first makes the first byte wait for a stat of every
  file, which on a large project takes longer than the upload it was to speed up.
- So: a candidate buffer bounded by the share's own budget, which **evicts its
  largest** as more of the tree is seen; the upload takes the smallest out of
  it, and after a hundred files the scan and the upload run together.
- A file sent early may be larger than one found later. That is the price of not
  waiting, and eviction only ever discards files that were never going to be
  sent. The mechanism is `dircache/fill.go`.

**The budget bounds what is copied, never whether the mode runs.** "The budget
ran out" is the same state as "the fill has not reached it yet", so there is no
project size at which `delegated` stops working.

### Compression is a negotiation, not a format

The payload is a byte stream, so a codec wraps it with no protocol change —
which is exactly what the frame's codec field has been there for since version
1. The agent announces what it can read in its greeting and the client picks
from THAT list, never from what it can produce: a workspace older than
compression names no codecs, and a client that chose for itself would send one
it would refuse.

**zstd, and it costs the agent a dependency.** ADR 0021 keeps that side's graph
small and states the number, and this takes it from 24 `go.sum` lines to 28. Paid
deliberately: the fill is the one bulk transfer this protocol makes, and zstd
compresses a source tree harder and faster than the standard library's gzip. The
point of stating the count was never that it must not grow, but that growing it
is a decision somebody made rather than something that happened.

It applies to the client's direction only, which is where the bulk is: the fill
sends the whole tree, and invalidation sends whatever an editor or a checkout
touched. Write-back carries what one container wrote since the last round, which
is small by nature, so it stays a plain tar rather than paying a compressor per
poll.

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

### The child enters three namespaces

**pid**, **network** and **mount**, in that order. The network one is the
lower's: with a daemon per account the reverse forward carrying the NFS export
is bound inside that daemon's netns and reaches nowhere else (ADR 0019), so a
mount attempted from the agent's namespace has no server to talk to.

### The lower's options are not a mount(2) argument

`workspace.NFSVolumeOptions` builds the list docker's local volume driver takes,
and that driver splits kernel FLAGS out of it before calling mount(2). Passed
whole as filesystem data, `noatime` — which is `MS_NOATIME` and not something
the NFS client parses — makes the parser refuse the entire list. It reports
EINVAL, printed as `invalid argument`, about a list whose every word is valid on
its own. `Spec.LowerMount` returns the two halves, and the error prints both.

This is what kept the union from ever mounting, for the whole life of the mode.

### "Up" means MOUNTED, not that the path exists

A union's directories are made before it is mounted and outlive it, so a stat
says yes for a share that never mounted and for one whose server has died. Both
then read as serving — and because everything here reaches a share through a
path, the whole mode keeps working against the bare directory: the agent writes
the cache into it, the container reads it, an edit here is written into it, a
deletion removes from it. Only the lower is missing, so a read that should fall
through returns nothing and the container's writes land where nothing looks.

The suites assert that a container's share reports fuse-overlayfs rather than
the daemon's own disk, which is the one thing a bare directory cannot fake.

### A deletion nobody observed

A cache volume outlives the session that filled it, and a fill only ever
overwrites and adds — it has no way to notice what is GONE. So a file deleted
here while nothing was running is still in the cache, and still in every
container, with no event anywhere to explain it.

The client keeps a record of what each fill sent, per workspace, bound to the
machine and account that wrote it. At the next fill it stats those paths and
drops the ones this machine no longer has. Only paths a fill put there are ever
considered, and that is what makes it safe: a path in the cache that no fill
sent is a container's own file, and this must never remove one.

A watcher overflow is the same problem inside a session — the events it dropped
may have been deletions — so `fswatch.Observer` is told, and answers with the
same reconcile rather than a log line.

What it does NOT cover: a container already running when the client restarts
keeps what its cache holds until that share is filled again. Narrower, and
deliberate.

### The collector cannot see a cache volume in use

A union is bound into a container by path, so nothing ever references the volume
holding its layer and the daemon reports it unused for as long as it exists.
Collecting it does not fail and does not unmount anything: it empties the
directory under a live mount, so the container keeps running and the files it
wrote are gone.

`rewrite.Guard` answers this for the shares a session prepared. It cannot answer
for one prepared by an earlier session, which is precisely the container left
running across a client restart — and that is what `OpMounted` is for. Cannot
ask means keep: an uncollected cache costs disk, a collected one in use costs
somebody's work.

The agent answers that from the filesystem rather than from its own record, and
the filesystem is the half that matters. A union outlives the agent that started
it, so after an agent restart the mounts are serving and the manager knows
nothing about them; "none mounted" would be truthful and would delete the cache
under a running container. The ids come from the mounts under `/run/rd-union`
and the client digest from the key that authenticated, so a machine is told
about its own caches and never another's.

### A union outlives the channel that asked for it

The cache channel rides the SSH connection, and that connection is released
whenever the session goes idle and reopened on the next request (ADR 0015). The
union must not follow it: a container binds the merged PATH, so unmounting under
a running container does not free anything and does not stop it either — it
leaves that container holding a mount that can never be repaired, which is the
same refcount rule that makes `compose down` cure a broken mount where
restarting the session does not.

So a share is released only when no container is bound to it. The daemon is the
only thing that can say, because a union is bound by path rather than as a
volume and nothing else in the workspace relates the two. On any doubt the mount
is KEPT: one nobody needs costs a process, and one taken while in use costs
somebody's container for as long as it runs.

### Write-back: baselines first, clocks last

The upper layer is where everything written through the union lands — which is
the container's writes **and the fill's own copies**, because the fill goes
through the union too. So the layer alone does not say who wrote what, and the
manifest is what does: what the fill sent, with each file's size and time as it
was **here**. An entry matching its baseline exactly is the copy the fill put
there; anything else is the container's. That makes both sides answerable
separately:

| your file vs manifest | cached file vs manifest | outcome |
|---|---|---|
| unchanged | changed | the container wrote it → write it back |
| changed | unchanged | you wrote it → nothing comes back |
| unchanged | whiteout | the container deleted it → delete it here |
| changed | whiteout | conflict; your file is kept |
| changed | changed | conflict; last writer wins |
| not in the manifest | anything | left alone |
| unchanged | identical to the manifest | the fill wrote it; nothing happened |

Only the last-writer case needs a clock, and the offset between the two machines
is measured through `workspace-info` rather than assumed away. Every conflict is
reported by path whichever way it resolves.

The fill's own copies are filtered on **both** ends, and the two failures are
different sizes. The client's check is the rule, and being wrong there costs a
file written back with the bytes it already has, which settles on the next round.
The agent keeps its own record of what it applied so the reply stays proportional
to what changed; without it a fully cached tree is listed in one reply every five
seconds for as long as the session runs.

**Nothing is written back while the cache is incomplete.** A file the fill never
sent looks exactly like one the container created, and the cost of that
confusion is content appearing in somebody's source tree that they never wrote.

## What it measured

`test/bench.sh` on a GitHub runner, 2026-09-01, 300 files, one shaped link per
row, ALL ROWS FROM ONE RUN so the modes are comparable with each other. Seconds,
and `nfs_ops` is what the mount was asked for during the read. Re-check with the
`bench` label on a pull request.

ADR 0042's table is a different run and its absolute numbers differ; compare
within a table, never across the two.

| RTT | mode | start | walk | read 300 | write | nfs_ops during the read |
|---|---|---|---|---|---|---|
| 0.1ms | `consistent` | 0.14 | 0.09 | 0.41 | 0.75 | READ=300 ACCESS=535 GETATTR=437 |
| 0.1ms | `cached` | 0.14 | 0.09 | 0.30 | 0.19 | READ=300 ACCESS=422 |
| 0.1ms | `delegated` | 0.28 | 0.44 | 0.33 | 0.14 | READ=349 ACCESS=300 |
| 40ms | `consistent` | 0.38 | 18.91 | 32.49 | 12.68 | GETATTR=3552 |
| 40ms | `cached` | 0.38 | 2.99 | 24.46 | 12.28 | READ=300 ACCESS=422 |
| 40ms | `delegated` | 0.14 | **0.06** | **0.09** | **0.08** | **none** |
| 160ms | `consistent` | 1.10 | 96.98 | 164.47 | 74.00 | GETATTR=4220 |
| 160ms | `cached` | 1.10 | 11.64 | 98.12 | 49.43 | READ=300 ACCESS=422 |
| 160ms | `delegated` | **0.15** | **0.06** | **0.08** | **0.08** | **none** |
| 10mbit | `delegated` | 0.14 | 0.06 | 0.08 | 0.08 | none |

The claim was that the wall clock stops tracking the latency knob, and it does:
`delegated` reads in 0.08s at 160ms RTT where `cached` takes 98.12s and
`consistent` 164.47s, and it does so while remaining a live mount. The
`nfs_ops` column is the reason and the proof: nothing is asked of the mount at
all, where `cached` still pays 300 READs and 422 ACCESSes it cannot avoid.

Start does not grow, which is the other half of the claim: 0.15s at 160ms
against 1.10s for both mounted modes, because a container never waits for the
fill.

The cache's own table, same run. `cold` is a read straight after the container
starts, `warm` the same read once the fill has settled. This run used a
3000-file tree per shape; it is 1000 now, for the reason below:

| RTT | cold | settle | warm | invalidate | write-back |
|---|---|---|---|---|---|
| 0.1ms | 3.61 | 7.91 | **0.09** | 1.13 | 2.01 |
| 40ms | 377.18 | did not settle in 300s | 7.17 | 1.25 | did not arrive in 60s |

Cold to warm at no latency is 3.61s to 0.09s, which is the feature stated as a
number: the same read, before and after the cache holds the tree.

**The 40ms row is a measurement of the bench, not of the mode.** The cold read
races the fill for the link, so it reports the two competing rather than what a
miss costs; and the tree did not finish settling inside the 300s the script
waits, which is why write-back had nothing to report either. The tree was cut
from 3000 to 1000 files and the job's timeout raised from 60 to 120 minutes
afterwards. Both remaining shapes, 80ms
and 10mbit, ran out of job time before starting. Do not quote this table for
anything but the 0.1ms row until it has run whole.

## Consequences

- **`delegated` requires the watcher**, exactly as `cached` does, and for a
  stronger reason: without it the cache goes stale rather than merely lagging.
- **It requires `fuse-overlayfs` where the account's daemon runs.** In
  per-account mode that is the dind's image, which is why the workspace's own
  image is the right one to run there — already this project's recommendation
  for the storage driver (`daemons.DefaultImage`). The workspace reports whether
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
  path does not repair a container already bound to the dead one.

  So a live union is ADOPTED rather than replaced: the supervisor waits for a
  serving mount to go before it makes another. That rests on "alive" meaning
  MOUNTED rather than "the path is there" — against a stat it would wait forever
  on the empty directory a dead union leaves behind. st_dev against the parent
  answers that from outside the namespace as well as inside (measured
  2026-09-01, `test/union-probe.sh` section 12: an unmounted directory reads dev
  59 against parent 59, a mounted one 63 against 59).

  Reachable only where dockerd outlives the agent, which is the VM deployment
  (ADR 0025). With the agent in a container it is pid 1, so restarting it takes
  its dockerd and every dind with it and there is nothing left to adopt — which
  is why `test/vm.sh` is where this is asserted, by counting union servers for a
  share across an agent restart.
- **Disk**: one cache per share per client, growing with what the container
  writes as well as with the tree.

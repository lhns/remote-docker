# 0042 — Docker's mount consistency, applied to the NFS mount

- Status: Accepted
- Date: 2026-09-01
- Current answer: a share's mount is `consistent` unless something asks for
  `cached`, which raises the attribute cache from 1s to 60s and relies on the
  watcher for coherence, or for `delegated`, which is a cache over that mount
  ([ADR 0044](0044-a-delegated-share-is-a-cache.md)).

## What forced it

A shared directory is slow enough that people tar their project into the
workspace by hand instead. Measured by `test/bench.sh` on a GitHub runner, 300
files in 20 directories, netem on the workspace's loopback (2026-09-01):

| shape | RTT | walk | read | write |
|---|---|---|---|---|
| unshaped | 0.1ms | 0.09s | 0.41s | 0.79s |
| 20ms | 40ms | 4.86s | 58.76s | 17.23s |
| 80ms | 160ms | 42.70s | 291.99s | 74.12s |
| 10mbit | 0.3ms | 0.19s | 0.84s | 0.41s |

Two things follow, and both decide the shape of the answer:

- **Cost tracks LATENCY, not bandwidth.** A 10mbit link costs almost nothing;
  160ms RTT costs 400x. So the thing to remove is round trips.
- **`actimeo=1` is where they come from.** Every attribute older than a second
  is revalidated, and a source tree is nothing but attributes. The same run
  counted 1,888 GETATTRs against 300 READs at 160ms.

All the numbers in this record are from one run, so the rows can be compared:
`bench` on pull request 98, 2026-09-01.

## The decision

**Use Docker's own vocabulary, and give it a meaning here.**
`api/types/mount` has defined the axis for years and every client already parses
it:

```bash
docker run -v ./project:/app:ro,cached
docker run --mount type=bind,source=./project,target=/app,consistency=cached
#  compose:  volumes: [{type: bind, source: ./project, target: /app, consistency: cached}]
```

| Docker's word | Docker's meaning | here |
|---|---|---|
| `consistent`, `default`, unset | behaves as a bind mount | `actimeo=1` — what this project always did |
| `cached` | the container may cache read data and FS structure; the HOST is authoritative | `actimeo=60,nocto`, invalidated by the watcher |
| `delegated` | the container may cache reads **and writes**; the CONTAINER is authoritative | a union over the mount; [ADR 0044](0044-a-delegated-share-is-a-cache.md) |

**Inventing an option was not open to us.** The CLI and the daemon reject mount
options they do not know, so `volume-opt=cache=…` on a bind, or a suffix of our
own, fails before the rewriter sees it. `consistency` is the one field that
already exists, is already parsed, and is inert everywhere else. A flag of our
own would also sit in docker's namespace (ADR 0024) and no compose file could
produce it.

**It combines with `ro` as a comma.** A `-v` has at most three colon-separated
fields and the third is an option LIST, so `-v /a:/b:ro,cached`, never
`/a:/b:cached:ro`. The consistency is taken OUT of that list before the bind is
forwarded: once it is a volume the word describes nothing the daemon can act on.

**Precedence, one rule:** the mount outranks a per-path rule, which outranks the
workspace default.

```json
{"consistency": "cached", "consistencyPaths": {"/home/me/live": "consistent"}}
```

`REMOTE_DOCKER_CONSISTENCY` sets the default for a CI run. There is no variable
for the rules: a map has no spelling in an environment variable that is not a
small language of its own.

**Coherence comes from the watcher, and `cached` therefore requires it.** A
change here is replayed into the workspace as a real syscall through the export
(ADR 0016), and that SETATTR refreshes the kernel's cached attributes for the
inode: long cache for what has not changed, immediate refresh for what has.
Selecting `cached` with watching off is refused naming the setting — a long
attribute cache with nothing to invalidate it is a stale mount, and a stale
mount fails by succeeding.

**One directory is one share, one volume and one consistency.** Two mounts of
the same source asking for different things are refused. The alternative is
silent: the second `EnsureVolume` recreates the volume the first just made, and
both containers run under whichever was written last.

## What it bought

Same runner, same tree, same run: the mode is written on the mount and nothing
else differs.

| RTT | mode | walk | read | write | GETATTR | LOOKUP |
|---|---|---|---|---|---|---|
| 0.1ms | consistent | 0.09s | 0.41s | 0.79s | 435 | 110 |
| | **cached** | 0.09s | 0.30s | 0.19s | **0** | **0** |
| 40ms | consistent | 4.86s | 58.76s | 17.23s | 1233 | 3 |
| | **cached** | **3.00s** | **24.50s** | 12.30s | **0** | **0** |
| 160ms | consistent | 42.70s | 291.99s | 74.12s | 1888 | 3 |
| | **cached** | **11.65s** | **98.20s** | 49.18s | **13** | **0** |
| 0.3ms, 10mbit | consistent | 0.19s | 0.84s | 0.41s | 546 | 3 |
| | **cached** | 0.17s | 0.62s | 0.32s | **0** | **0** |

- **The revalidation is gone**, which is what the mode is: GETATTR goes to zero
  or near it at every shape.
- **Worth ~3x at 160ms** on a walk and a read, and less on a write, which is
  bounded by something else.
- **What remains is READ and ACCESS**, 300 and 422 in every row above. Those are
  the file's own bytes and the permission check, and no attribute cache can
  remove them: a live mount has to fetch what it is asked for. Removing THOSE
  means not mounting, which is where [ADR 0044](0044-a-delegated-share-is-a-cache.md)
  starts.

## Consequences

- **Switching costs a volume recreation and nothing else.** `replaceIfStale`
  already rebuilds a managed volume whose driver options changed, so a mode
  change needs no migration. A volume a container still holds is refused with
  the remedy, which is that container.
- **What the poke refreshes is the inode it names, and nothing else.** An edit
  to an existing file is the case this covers exactly: the SETATTR reply carries
  the file's real attributes, the workspace's NFS client sees the new mtime and
  size, and it drops the pages it had. Two other cases are bounded by `actimeo`
  instead, and are 60s rather than 1s now:
  - a DELETED file can still appear present, because the parent's cached entry
    is what says it exists;
  - a NEW file can be missing from a listing, for the same reason.

  `coarse` watching pokes the directory as well and closes both; `partial` does
  not. This is the cost of the mode, and it is why `consistent` remains the
  default.
- **`nocto` drops close-to-open consistency.** A file being written on the
  workspace and read here mid-write is now less immediately coherent. That
  direction is the one the watcher does not cover; it is also not the direction
  anything in this project's motivating workload writes in.
- **The benchmark is the gate.** `test/bench.sh` is run from a pull request
  label or `workflow_dispatch`, and a mode with no row does not ship.

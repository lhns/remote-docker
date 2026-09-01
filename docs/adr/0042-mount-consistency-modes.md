# 0042 — Docker's mount consistency, applied to the NFS mount

- Status: Accepted
- Date: 2026-09-01
- Current answer: a share's mount is `consistent` unless something asks for
  `cached`, which raises the attribute cache from 1s to 60s and relies on the
  watcher for coherence.

## What forced it

A shared directory is slow enough that people tar their project into the
workspace by hand instead. Measured by `test/bench.sh` on a GitHub runner, 300
files in 20 directories, netem on the workspace's loopback:

| shape | RTT | walk | read | write |
|---|---|---|---|---|
| unshaped | 0.1ms | 0.25s | 0.59s | 1.00s |
| 20ms | 40ms | 5.35s | 45.12s | 10.29s |
| 80ms | 160ms | 44.12s | 238.84s | 40.18s |
| 10mbit | 0.3ms | 0.34s | 1.11s | 1.06s |

Two things follow, and both decide the shape of the answer:

- **Cost tracks LATENCY, not bandwidth.** A 10mbit link costs almost nothing;
  160ms RTT costs 400×. So the thing to remove is round trips.
- **`actimeo=1` is where they come from.** Every attribute older than a second
  is revalidated, and a source tree is nothing but attributes.

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
| `delegated` | the container may cache reads **and writes**; the CONTAINER is authoritative | refused; see ADR 0043 |

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

## Consequences

- **Switching costs a volume recreation and nothing else.** `replaceIfStale`
  already rebuilds a managed volume whose driver options changed, so a mode
  change needs no migration. A volume a container still holds is refused with
  the remedy, which is that container.
- **Deletions remain ADR 0014's gap**, and `cached` does not widen it: the
  watcher observes a removal here and pokes, and what a container's own watcher
  sees over NFS is unchanged.
- **`nocto` drops close-to-open consistency.** A file being written on the
  workspace and read here mid-write is now less immediately coherent. That
  direction is the one the watcher does not cover; it is also not the direction
  anything in this project's motivating workload writes in.
- **The benchmark is the gate.** `test/bench.sh` is run from a pull request
  label or `workflow_dispatch`, and a mode with no row does not ship.

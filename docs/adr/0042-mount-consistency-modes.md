# 0042 — Docker's mount consistency, applied to the NFS mount

- Status: Accepted
- Date: 2026-09-01, last amended 2026-09-04
- Current answer: a mount has two settings, `read=direct|cached` and
  `write=through|back|ephemeral`, and those two words are the only spelling:
  `-v /a:/b:ro,read=cached`, `--mount '...,"consistency=read=cached,write=back"'`,
  `{"consistency": "read=cached"}`. An axis not named comes from the rule,
  then the workspace, then the default `read=direct,write=through`. `direct`
  revalidates every second; `cached` trusts attributes for a minute and needs
  the watcher. `through` writes here as it happens; `back` and `ephemeral`
  write into a union ([ADR 0044](0044-a-delegated-share-is-a-cache.md)), and
  only `back` carries it home.
- Docker's own values for the field (`api/types/mount.Consistency`) are
  aliases, and each keeps Docker's meaning:
  `default` and `consistent` (behaves as a bind mount) are `read=direct,write=through`;
  `cached` (the container may cache reads; the host is authoritative) is `read=cached,write=through`;
  `delegated` (the container may cache reads and writes; the container is authoritative) is `read=cached,write=back`.

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

**Two axes, amended 2026-09-04.** Docker's three words name three corners of
a grid with six, and the missing corners are the ones a build directory wants:

| read | | write | | union |
|---|---|---|---|---|
| `direct` | revalidated every second, `actimeo=1` | `through` | on this machine as it happens | no |
| `cached` | trusted for a minute, `actimeo=60,nocto` | `back` | in the workspace, carried back | yes |
| | | `ephemeral` | in the workspace, never carried back | yes |

- Two axes and not three, because a copy exists exactly when there is a union
  and a union exists exactly when writes are not synchronous: overlayfs writes
  to the upper by construction, so `write=through` means no union.
- The names are the cache literature's where it has them (write-through,
  write-back), `direct` for what the mount does, and `ephemeral` because no
  cache policy discards a dirty line. Rejected: `live` (esoteric for a
  default), `uncached` (names the default by what it lacks), `fresh`, `sync`,
  `async`, `discard` (a second vocabulary beside one that exists).
- `write=ephemeral` is `node_modules` and `target/`: capture the container's
  writes and never ship them here. `read=direct,write=back` is a union for
  the writes alone. What prefetch means per corner is
  [ADR 0045](0045-prefetch-follows-the-reads.md).
- A word is consumed by the rewriter whether or not the bind is rewritten:
  the daemon's `linuxValidMountMode` rejects anything it does not know, and a
  bind not rewritten would carry ours to it.

**One spelling of ours, namespaced, plus Docker's four.** Every word of ours
is `read=X` or `write=Y`, and Docker's `default`, `consistent`, `cached` and
`delegated` are accepted as the aliases above. Any other bare word, joined
with `+` or not, is refused: a word that is neither namespaced nor Docker's
cannot be told from an option that is not ours. In a `-v` option list the
commas are ours. `--mount` is itself a comma-separated list the CLI splits
before any value is seen, so both axes go in one csv-quoted field,
`"consistency=read=cached,write=back"`, the convention Docker documents for
`volume-opt`. Repeating an axis is refused; naming one leaves the other to
the rule and the workspace.

**The CLI passes our words through; the daemon is what rejects them.** At
`docker/cli@v29.7.2`, the version `client/go.mod` requires,
`internal/volumespec/volumespec.go` ignores unknown `-v` options and
`opts/mount.go` takes any string for `consistency=`, so both reach the proxy
intact; only a NEW `--mount` field fails at the CLI. Re-check:
`client/internal/rewrite/mode_test.go` pins the words arriving.

**It combines with `ro` as a comma.** A `-v` has at most three colon-separated
fields and the third is an option LIST, so `-v /a:/b:ro,cached`, never
`/a:/b:read=cached:ro`. The mode is taken OUT of that list before the bind is
forwarded: once it is a volume the words describe nothing the daemon can act on.

**Precedence, one rule:** the mount outranks a per-path rule, which outranks the
workspace default.

```json
{"consistency": "read=cached", "consistencyPaths": {"/home/me/live": "read=direct"}}
```

`REMOTE_DOCKER_CONSISTENCY` sets the default for a CI run. There is no variable
for the rules: a map has no spelling in an environment variable that is not a
small language of its own.

**Coherence comes from the watcher, and `read=cached` therefore requires it.** A
change here is replayed into the workspace as a real syscall through the export
(ADR 0016), and that SETATTR refreshes the kernel's cached attributes for the
inode: long cache for what has not changed, immediate refresh for what has.
Selecting a cached read with watching off is refused naming the setting — a long
attribute cache with nothing to invalidate it is a stale mount, and a stale
mount fails by succeeding.

**One directory is one share, one volume and one consistency.** Two mounts of
the same source asking for different things are refused. The alternative is
silent: the second `EnsureVolume` recreates the volume the first just made, and
both containers run under whichever was written last.

## What it bought

Same runner, same tree, same run: the mode is written on the mount and nothing
else differs. ADR 0044's table is a different run; compare within a table,
never across the two.

| RTT | mode | walk | read | write | GETATTR | LOOKUP |
|---|---|---|---|---|---|---|
| 0.1ms | `read=direct,write=through` | 0.09s | 0.41s | 0.79s | 435 | 110 |
| | **`read=cached,write=through`** | 0.09s | 0.30s | 0.19s | **0** | **0** |
| 40ms | `read=direct,write=through` | 4.86s | 58.76s | 17.23s | 1233 | 3 |
| | **`read=cached,write=through`** | **3.00s** | **24.50s** | 12.30s | **0** | **0** |
| 160ms | `read=direct,write=through` | 42.70s | 291.99s | 74.12s | 1888 | 3 |
| | **`read=cached,write=through`** | **11.65s** | **98.20s** | 49.18s | **13** | **0** |
| 0.3ms, 10mbit | `read=direct,write=through` | 0.19s | 0.84s | 0.41s | 546 | 3 |
| | **`read=cached,write=through`** | 0.17s | 0.62s | 0.32s | **0** | **0** |

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
  not. This is the cost of the mode, and it is why `read=direct` remains the
  default.
- **`nocto` drops close-to-open consistency.** A file being written on the
  workspace and read here mid-write is now less immediately coherent. That
  direction is the one the watcher does not cover; it is also not the direction
  anything in this project's motivating workload writes in.
- **The benchmark is the gate.** `test/bench.sh` is run from a pull request
  label or `workflow_dispatch`, and a mode with no row does not ship.

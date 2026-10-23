# 0042 — Docker's mount consistency, applied to the NFS mount

- Status: Accepted
- Date: 2026-09-01, last amended 2026-10-23
- Current answer: a mount has two settings, `read=direct|cached` and
  `write=through|back|ephemeral`, and those two words are the only spelling:
  `-v /a:/b:ro,read=cached`, `--mount '...,"consistency=read=cached,write=back"'`,
  `{"consistency": "read=cached"}`. An axis not named comes from the rule,
  then the workspace, then the default `read=direct,write=through`. `direct`
  revalidates every second; `cached` trusts attributes for a minute and needs
  the watcher. `through` writes here as it happens; `back` and `ephemeral`
  write into a union ([ADR 0044](0044-a-delegated-share-is-a-cache.md)), and
  only `back` carries it home.
- A mount may also carry words of its SHARE, beside the two axes: `owner=`,
  `fmask=`, `dmask=`, `watch=off|partial|coarse` and `prefetch=off|eager|tree`
  (amendments 2026-10-14, 2026-10-23). The session settings do not accept them;
  each has a default of its own (`REMOTE_DOCKER_SHARE_OWNER`,
  `REMOTE_DOCKER_SHARE_FMASK`, `REMOTE_DOCKER_SHARE_DMASK`,
  `REMOTE_DOCKER_WATCH`, `REMOTE_DOCKER_PREFETCH`).
- A share is identified by (directory, read, write, owner, fmask, dmask)
  (amendments 2026-10-15, 2026-10-23); a mask is in the id only when it is not
  0, so no earlier id moved. A mount asking for a new combination gets its own
  share, export, volume and union beside the others, and nothing is refused or
  replaced.
  `watch=` and `prefetch=` are effort spent on one share: it runs at the
  highest level any mount of it asked for (`off < partial < coarse`,
  `off < tree < eager`), raised silently and never lowered while the session
  runs, and events are not filtered per container.
- `read=cached` and every union need the watcher, so a mount that resolves to
  either gets at least `watch=partial` whatever the session says, and asking for
  `watch=off` on one is refused. A session with watching off starts the watcher
  on the first such mount.
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
`docker/cli@v29.8.2`, the version `client/go.mod` requires,
`internal/volumespec/volumespec.go` ignores unknown `-v` options and
`opts/mount.go` takes any string for `consistency=`, so both reach the proxy
intact; only a NEW `--mount` field fails at the CLI. Re-check:
`client/internal/rewrite/mode_test.go` pins the words arriving.

**Words of the share, amended 2026-10-14.** `owner=` (ADR 0046) and `watch=`
(ADR 0016) and `prefetch=` (ADR 0045) are read from the same two places as the axes and consumed the same
way. It describes the
share and not one mount of it, so it is not part of the mode: `ParseMode`, the
session-level parser behind `consistency`, `consistencyPaths` and
`REMOTE_DOCKER_CONSISTENCY`, refuses it and names the setting that supplies its
default, and `consistencyPaths` cannot carry it. `workspace.ParseMount` reads a
mount's words, and `IsModeWord` claims the key by name so a misspelt value is
refused here rather than forwarded.
- `watch=` takes `off`, `partial` or `coarse` only; the session setting's
  aliases (`on`, `true`, `1`) stay with the environment variable. Unnamed, it is
  the session's `watch`, raised to `partial` when the mount needs the watcher.
  An explicit `off` there is refused (`fix: drop watch=off, or use read=direct`,
  or `use write=through` for a union).
- `prefetch=` takes `off`, `eager` or `tree`. Unnamed, it is the session's
  `prefetch`, and off wherever the mode does not prefetch (anything but
  `read=cached` with a union). Naming `eager` or `tree` there is refused
  (`fix: drop prefetch=, or use read=cached with write=back or write=ephemeral`);
  `prefetch=off` is always accepted.
- `owner=uid` or `owner=uid<sep>gid`, with ANY one non-digit other than `=`
  and `,` as the separator. `-v` cannot carry `1000:2000` in a list: docker/cli
  splits the volume spec on colons and refuses a fourth ("too many colons")
  before this program sees it. So write `owner=1000.2000` there; `:` works in
  `--mount` and in compose's `consistency:`. One number is both ids. Empty
  sides, values over 32 bits and anything else are refused naming the forms.
- `fmask=` and `dmask=` (amended 2026-10-23, ADR 0046) take octal, 1 to 4
  digits, at most `0777`. Anything else is refused naming the word, and so is a
  `dmask` clearing all three execute bits.
- Checked 2026-10-14 at docker/cli v29.8.2. Re-check: `grep -rn 'too many colons' internal/volumespec` in the module `client/go.mod` requires.

**It combines with `ro` as a comma.** A `-v` has at most three colon-separated
fields and the third is an option LIST, so `-v /a:/b:ro,cached`, never
`/a:/b:read=cached:ro`. The mode is taken OUT of that list before the bind is
forwarded: once it is a volume the words describe nothing the daemon can act on.

**Compose's short syntax drops the words, amended 2026-10-14.** compose-go's
short volume parser knows `ro`, `rw`, `nocopy`, `propagation` and `z`/`Z` and
ignores any other option (`format/volume.go`, v2.15.0), so
`./src:/app:read=cached` mounts with the default mode and says nothing. The
long syntax's `consistency:` is a free string that compose passes to
`mount.Consistency`, which the rewriter already reads, but only for a bind
sent through the Mount API: a bind with no advanced option goes as a `Binds`
string and loses the field too (compose v5.5.1 `bindRequiresMountAPI`). Adding
`bind: {create_host_path: false}` selects the Mount API. The embedded compose
therefore warns about a short-syntax volume carrying one of our words, and
about a long-syntax bind carrying `consistency:` without
`create_host_path: false`, naming the fix (`composewarn.go`); it does not fail,
since the mount still works.

**Precedence, one rule:** the mount outranks a per-path rule, which outranks the
workspace default.

```json
{"consistency": "read=cached", "consistencyPaths": {"/home/me/live": "read=direct"}}
```

`REMOTE_DOCKER_CONSISTENCY` sets the default for a CI run. There is no variable
for the rules: a map has no spelling in an environment variable that is not a
small language of its own.

**Coherence comes from the watcher, and `read=cached` therefore requires it.**
(Amended 2026-10-14: requires it for ITS share. A session with watching off no
longer refuses the mount; the mount turns the watcher on, at `partial`.) A
change here is replayed into the workspace as a real syscall through the export
(ADR 0016), and that SETATTR refreshes the kernel's cached attributes for the
inode: long cache for what has not changed, immediate refresh for what has.
Selecting a cached read with `watch=off` is refused — a long attribute cache
with nothing to invalidate it is a stale mount, and a stale mount fails by
succeeding.

**One directory is one share per read, write and owner** (amended 2026-10-15).

- **Replaced:** one share per directory, with a second mount asking for other
  words refused, since a share's filesystem is built with its words and a
  second `EnsureVolume` would recreate the first's volume. The registry never
  shrinks, so a compose project brought `down` with `prefetch=eager` and `up`
  without it was refused until `remote restart`. The refusal was also uneven:
  a different `read=` alone passed and reused the volume name with other
  options.
- **Identity.** `read`, `write` and `owner` apply per container, so they are in
  the share id: `ShareIDFor` hashes `canonical path, read, write, uid:gid`, still
  16 hex characters, so `/m/<id>`, the volume name, the cache and notify
  protocols and the whole agent are unchanged. The agent keys on
  `(account, client, export)`, so unions, layers, cache volumes, pokes and
  collection separate per id by themselves. Every variant gets a new id,
  the default one included; the old path-only formula survives as
  `LegacyShareID`, for restoring records ADR 0027 describes.
- **Effort.** `watch=` and `prefetch=` rise: a mount of an existing variant
  raises the share's words to the stronger of the two (`Watch.Max`,
  `Prefetch.Max`) and never lowers them. The registry raises them under a
  lock and the share is read through an accessor (`Share.Words`), because the
  watcher and the cache hold the pointer. Prefetch's order is a choice made
  here: `eager` sends everything up to the budget, `tree` what reads point at.
- **Still refused.** `watch=off` on a mount that needs the watcher (`read=cached`
  or a union) contradicts itself within one mount, and says so. Nothing
  disagrees between mounts any more.
- **Cost.** Each variant is a volume and, for a union, a cache volume and a
  fuse-overlayfs process. A container sees what is written through another
  share only after that share's writes reach this machine (a union's write-back
  delay) or its cache is invalidated by the watcher.

**The masks are identity too** (amended 2026-10-23).

- `fmask=` and `dmask=` (ADR 0046) are fixed per share for the reason the owner
  is: the filesystem is built with them, and a union's cache already holds
  files copied up with the old modes.
- `ShareIDFor` appends `\x00f<mask>` and `\x00d<mask>` only when the mask is
  not 0. Every share made before the masks keeps its id, volume and union;
  `export_test.go` pins two ids computed by the code before the change.
- The share record gains `fmask` and `dmask`, written only when not 0, with no
  version bump. A record without them restores with both at 0, as its id was
  derived, never with the session's default.

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

- **Switching mode is a new volume, not a recreation** (amended 2026-10-15). The
  mode is in the share id, so a container asking for another one mounts another
  volume beside the first; `replaceIfStale` no longer sees a mode change. The
  old volume is collected once no container names it.
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

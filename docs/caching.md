# The cache

What makes a shared directory fast, and what is known about it. A shared
directory is an NFS mount over an SSH tunnel, and NFS is a round-trip
protocol, so a project of thousands of small files costs one file's round
trips times the count; latency and not bandwidth decides it. The decisions
are in [ADR 0042](adr/0042-mount-consistency-modes.md) (the two axes),
[ADR 0044](adr/0044-a-delegated-share-is-a-cache.md) (the union) and
[ADR 0045](adr/0045-prefetch-follows-the-reads.md) (the prefetch policy and
every table); the user-facing table and examples are in the README.

## The two settings

| read | what the mount does | write | where a write goes | union |
|---|---|---|---|---|
| `direct` | revalidates every second | `through` | this machine, as it happens | no |
| `cached` | trusts attributes for a minute | `back` | the union, carried back within seconds | yes |
| | | `ephemeral` | the union, and never carried back | yes |

Spelled `read=cached,write=back` in a `-v` list, or as one csv-quoted field
in `--mount`: `'type=bind,...,"consistency=read=cached,write=back"'`. Docker's
own `default`, `consistent`, `cached` and `delegated` are aliases; ADR 0042
has the mapping.

## The union

A share with `write != through` is a union on the workspace: the live export
is the lower layer, a local cache is the upper, and the container binds the
merged view. A read the upper holds is local disk; one it does not falls
through and is correct, so an incomplete upper is never wrong, only slower.
The lower carries the share's read mode, the agent is the union's only writer
and writes through the merged mount, and `ephemeral` is never asked for its
changes. ADR 0044 is where each of those is reasoned about and measured.

## Prefetch

`prefetch: off|eager|tree` (`REMOTE_DOCKER_PREFETCH`), off by default. `tree`
fills a `read=cached` union from the NFS misses, the neighbourhood of what was
read first; `eager` is the same sender with the quiet rule off, the whole
tree smallest first. Files over 1 MiB are never prefetched. The rules, the
walk, the simulated table, the bench of 2026-09-04 and why it is off are
ADR 0045.

## What is measured and what is assumed

| claim | status |
|---|---|
| a union falls through for what it does not hold | measured, `test/union-probe.sh` |
| the lower is served through fuse-overlayfs, not a directory that resembles one | measured, `integration.sh` 15c |
| an edit here reaches a running container under `actimeo=60` | measured, `integration.sh` 15c: 2s (2026-09-04) |
| the tree's amplification bound, cap, shape independence | unit, `dircache/sim_test.go` |
| the walk yields to reads | unit, `dircache/prefetch_test.go` |
| `ephemeral` is never asked for changes | unit, `dircache/prefetch_test.go` |
| the six corners parse and map | unit, `core/workspace`, `client/internal/rewrite` |
| every union corner end to end | measured, `integration.sh` 15e, `per-user-dind.sh` 7c, `two-clients.sh` 9 (2026-09-04) |
| landing a batch with no network in it | measured, `test/union-probe.sh`: 3,000 files through the union in 1.07s, 2,800 files/s (2026-09-04) |
| the bench table | measured, 2026-09-04, and it failed its criteria: the apply is the bottleneck (ADR 0045) |
| the apply pipelined | not built |
| page-cache warming as an alternative landing zone | not run; ADR 0045 step 0.5 |
| a container's mount sharing a superblock with the agent's | assumed |

## Prior art

Read for this design, 2026-09-04. Lazy loading never reaches parity with an
eager copy (Slacker, FAST'16, run phase 17% slower; FlacIO, FAST'25, existing
lazy loaders 4.6x off) and everyone ships it anyway. Nobody ships pure lazy:
Coda's `spy`, eStargz `optimize`, DADI and Nydus all record an access order
from a real run, coalesce it into few large transfers, and background-fill
the rest. Online access predictors (Kroeger and Long 1996/2001; Griffioen and
Appleton 1994; Vitter and Krishnan, JACM 1996) exist and were never adopted by
a shipping filesystem. The escalating tree appears unpublished. Its ancestors
are Linux readahead (Wu, Xi, Li, OLS 2007: 4x under `max/16`, 2x up to
`max/2`, then clamp), buddy allocation (Knowlton 1965) and competitive
prefetching (Li, Shen, Papathanasiou, EuroSys 2007: factor 2 for depth).
JuiceFS's prefetch is the depth-one cautionary case; patent art at depth one
is US 6,529,998 and US 11,474,948.

## Where the code lives

| what | where |
|---|---|
| the tree, the two rules, the cap | `dircache/tree.go` |
| the per-share sender, the walk that yields, the link estimate | `dircache/prefetch.go` |
| the walk over a share, and its per-share state | `dircache/walk.go` |
| attach, budget, batches, bytes sent | `dircache/cache.go` |
| invalidation and write-back, and the ephemeral skip | `dircache/invalidate.go`, `dircache/writeback.go` |
| the simulator and its cost model | `dircache/sim_test.go` |
| the miss observer | `core-client/nfsserve/observe.go` |
| the axes and their spellings | `core/workspace/mode.go` |
| consuming the words, choosing the union | `client/internal/rewrite/mode.go` |
| the lower's read mode | `core-agent/union/union.go` |
| wiring, RTT, the policy switch | `client/internal/session` |

`dircache` depends on nothing, in-repo or out, which is the membership test
for what belongs in it: fill a local copy of a tree in a bounded order,
invalidate what changes here, carry the consumer's writes back, naming no
transport and no storage.

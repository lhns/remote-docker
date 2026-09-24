# 0045 — Prefetch follows the reads: an escalating tree over small files

- Status: Accepted; off by default since 2026-09-04
- Date: 2026-09-04
- Current answer: `prefetch: off|eager|tree` (`REMOTE_DOCKER_PREFETCH`);
  `tree` follows the NFS misses, `eager` sends the whole tree smallest first.
  Default off, pending the apply; see "What it measured".
- Extends [ADR 0044](0044-a-delegated-share-is-a-cache.md), which decides
  that the share is a union; this record decides what goes into it and when.
  The read and write axes it hangs off are
  [ADR 0042](0042-mount-consistency-modes.md).

## What forced it

ADR 0044's fill ignored the reads: smallest first over the whole tree, at full
speed, up to a budget.

- **A subtree read paid for the tree.** A build touching two of twenty
  directories waited behind the other eighteen.
- **The fill competed with the reads.** Batches went back to back on the SSH
  connection the live NFS traffic shares, so a miss queued behind a 16 MiB
  batch: about 13s on a 10 Mbit link.
- **The budget was the only bound**, 2 GiB by default.
- **The lower was mounted `consistent`, `actimeo=1`**, so every file the fill
  had not reached lost the attribute cache `cached` had bought. Fixed here: the
  lower carries the share's read mode (ADR 0044).

Re-reads are already `cached`'s, free and correct. What remains is the FIRST
read of each small file, two serial round trips no server can merge: 300 files
at 160ms is 98s after `cached` has done everything it can. That is a batching
problem, so the union is **the landing zone for a batch**, not a cache (ADR
0044, "The union is a landing zone").

## The decision

### What is in the tree

| | rule | why |
|---|---|---|
| files | at or below `LargeFile`, 1 MiB | the overlay holds whole files; a 100-byte read must never pull 2 GB |
| order | the walk's, so siblings adjacent and cousins near | locality is the walk order and needs no distance metric |
| windows | 32 leaves of walk order, sorted by size inside | locality against size is two-dimensional; this is its axis-aligned cut |
| leaves | 256 KiB, packed with files that fit | fixed, so the structure never depends on the link |
| larger files | a childless node at level `ceil(log2(size / LeafBytes))` | buddy placement: every node at a level is within 2x of every other |
| branching | binary; below 2 refused by name (`ErrBranching`) | k only decides how fine the sizes on offer are, and 2 is finest |
| counters | `bytes`, `fetched`, `read` per node | named for what they are; nothing else is tracked |

Above `LargeFile` a file is NFS's: block-granular, kernel readahead, page
cache for re-reads. It costs what it costs today and is never sent twice.

### The two rules

- **Leaf rule.** Promote when `read >= f x unfetched`,
  `f = clamp(mean_unfetched / BDP, 0.05, 1)`, and only while the leaf still
  holds a file not yet read whole. Small files are read whole, so `read` is
  exact with no coverage tracking; a polled file can promote its own leaf once
  and nothing more. A single-file node never promotes on its own read: that
  would send a file the consumer just finished, for nobody.
- **Node rule.** Promote when `fetched >= 0.6 x bytes`. Fetched bytes are the
  only evidence that can exist above a leaf, since a hit never leaves the
  workspace. 0.5 cascaded in a binary tree, because one filled child IS half;
  0.6 needs evidence from both sides.
- **The cap.** One `Read` sends at most `4 x BDP` cumulatively up the climb,
  and the climb evaluates EVERY ancestor rather than stopping at the first
  that declines, so evidence lower down is not lost to a large parent.
- **Double-send is accepted**, at most `f` of each leaf: the files read before
  it promoted. Removing it needs page-cache state the server cannot see, and
  an upper that is complete is worth 5%.

### The walk

Over small files only, smallest first, bounded by the budget, and it
**yields**: nothing is walked within 2s of the last miss (`quietFor`), walk
batches are 1 MiB (`walkBatch`), and demand batches go first.

### Where the numbers come from

| number | source |
|---|---|
| RTT | measured around `readInfo` on every connection |
| bandwidth | EWMA over the cache channel's own Apply batches of 256 KiB and up |
| BDP | the product, per promotion; `Tree.SetLink` per batch, no rebuild |
| misses | `nfsserve`'s read observer, one callback per READ with share and path |

### The six corners, and where prefetch runs

| read | write | union | prefetch | Docker's alias |
|---|---|---|---|---|
| `direct` | `through` | no | no | `consistent` |
| `cached` | `through` | no | no | `cached` |
| `direct` | `back` | yes | no | |
| `cached` | `back` | yes | if `prefetch` is on | `delegated` |
| `direct` | `ephemeral` | yes | no | |
| `cached` | `ephemeral` | yes | if `prefetch` is on | |

Prefetch runs exactly when `read=cached` AND a union exists. In a `direct`
corner the upper holds only what the container wrote: a file in
the upper is served with no revalidation, so prefetching would hand somebody
who asked for live reads a cache. `read=cached,write=through` has no upper to
land in; step 0.5 below could change that.

### The switch

- `prefetch` (`REMOTE_DOCKER_PREFETCH`): `off` (default), `eager`, `tree`.
  One sender in `dircache/prefetch.go`; `eager` differs from `tree` in three
  ways: `Touch` ignored, no quiet wait, 16 MiB batches (`batchBytes`). Kept
  because a dense workload is its case once the apply is fixed.
- `remote status` reports bytes sent per share.

## What it measured

Simulated, `dircache/sim_test.go`, 2026-09-04. The model charges two round
trips per file for a miss and makes a batch available after `RTT + transfer`.
300 files in 20 directories, 160ms RTT, seconds. Re-check with
`cd dircache && go test -run TestPolicyComparison -v .`

| workload | tree | eager | none |
|---|---|---|---|
| dense, every file once | 65.6 | **39.4** | 98 |
| subtree, 10% of directories | **12.1** | 39.4 | 9.8 |
| sparse, 2% of files | 8.8 | 39.4 | **8.1** |
| 10 Mbit, any | fetches nothing | | |

- Dense once is the one row an eager fill wins, and it is within 2x.
- Subtree is the point: the tree ships what the build touches and its
  neighbours, and stops.
- Sparse costs 9% over no cache at all, which is the bound holding.

Four findings from the simulator that changed the design:

| tried | measured | became |
|---|---|---|
| node rule at 0.5, binary | one filled child promoted every parent to the root | 0.6 |
| 64 KiB leaves | 12.5 MB of 18.9 MB fetched was singleton dead weight | 256 KiB |
| climb stops at the first declining ancestor | a large parent hid a ready grandparent | `continue` |
| no cap | fetched bytes varied 60x across branching factors | `4 x BDP`, spread 2x |

**Measured**, `test/bench.sh` on GitHub runners, 2026-09-04, run 33868598926
on PR 110: one job per shape, every row a fresh container over a fresh
300-file tree, the workload started the moment the container was up. Seconds;
`union` is `read=cached,write=back`. The run predates the `prefetch` switch;
its `blind` is today's `eager`. Re-check with the `bench` label on a pull
request.

| RTT | workload | `read=direct` | `read=cached` | union, tree | union, eager |
|---|---|---|---|---|---|
| 0.1ms | dense | 0.41 | 0.38 | 0.46 | 0.29 |
| | subtree | 0.10 | 0.10 | 0.12 | 0.15 |
| | sparse | 0.07 | 0.08 | 0.15 | 0.12 |
| | dense x3 | 0.72 | 0.43 | 0.66 | 0.48 |
| 20ms | dense | 21.4 | 13.8 | 20.2 | 19.7 |
| | subtree | 2.1 | 1.5 | 7.6 | 2.6 |
| | sparse | 0.7 | 0.7 | 14.5 | 24.2 |
| | dense x3 | 49.2 | 13.9 | 20.5 | 19.9 |
| 40ms | dense | 44.1 | 27.2 | 39.2 | 39.0 |
| | subtree | 4.3 | 2.9 | 14.9 | 6.7 |
| | sparse | 1.3 | 1.3 | 29.8 | 11.1 |
| | dense x3 | 100.3 | 27.2 | 39.5 | 39.4 |
| 160ms | dense | 213.9 | 108.8 | 207.5 | 209.5 |
| | subtree | 20.8 | 11.2 | 48.5 | 27.8 |
| | sparse | 5.0 | 5.1 | 44.0 | 47.6 |
| | dense x3 | 498.1 | 152.4 | 229.4 | 218.6 |
| 10 Mbit | dense | 0.87 | 0.74 | 0.86 | 0.70 |
| | sparse | 0.09 | 0.09 | 0.28 | 0.21 |

**Every pass criterion failed at latency, and the reason is not the tree.**

- A cold union reads a tree in the time `read=direct` takes, and six sparse
  files in ten to forty times a plain mount. `read=cached` beats every union
  row at every latency on every workload but dense x3, where the union's
  second and third passes are local.
- The cause is where a batch LANDS. The agent applies the tar through the
  merged mount one file at a time, with `O_TRUNC` on a name the lower has, and
  fuse-overlayfs answers with a copy-up: lookup, open and read of the lower
  over NFS before the write reaches the upper. Landing a file is several
  serial round trips on the link and the one-request-at-a-time server the
  container reads through, so the fill cannot get ahead of the reader and
  slows it; a sparse read of six files queues behind three hundred copy-ups.
- `tree` against `eager`: the same, or worse on subtree and sparse, because
  the first demand batch at a 256 KiB leaf is the whole 125 KB tree, applied
  at that cost. The tree's choices cannot be read from this table until
  landing a batch is cheaper than reading a file.
- At 160ms `fetched` reads 0 for rows whose apply had not returned when the
  workload ended, and after seventeen rows no further container started in
  that job. The agent's log was not in the output, so the cause is
  unrecorded, and nothing dumps it yet (checked 2026-09-23: `grep -n logs
  test/bench.sh .github/workflows/bench.yml` prints nothing).
- Unshaped and on 10 Mbit every mode is within a few hundred milliseconds of
  every other.

What survives is ADR 0044's claim: once the upper HOLDS the tree, a read of it
is local (dense x3 at 160ms: 229s for three passes against 499s for
`read=direct`, so passes two and three cost about 20s together). What does
not is that this mechanism fills the upper faster than the container reads,
over a link with latency.

**What follows, decided by this table and not yet built:**

- The apply has to pipeline: a pool of writers puts many files in flight, and
  a serial server answers them in turn with the latency paid once per batch.
  Writing under a temporary name and renaming over the lower's also avoids
  the copy-up's data read. Both belong in `agent/internal/unions/write.go`,
  measured by `union-probe.sh` over a shaped NFS lower before the bench runs
  again.
- Prefetch is off by default until then; `eager` stays selectable so the next
  table compares both in one job.
- The union's write path has the same shape: a create looks the new name up
  in the lower, so 100 new small files cost 13s at 40ms under every mode.
  Namespace operations reach the lower; only data stays local.

**Not yet measured, and do not quote otherwise:**

- **Step 0.5: where a prefetched file should land.** The alternative to tar
  into the upper is the agent warming the page cache by reading the paths
  through the NFS mount in parallel: nothing written, pipelined for free, works
  with every write mode, so prefetch would run in all six corners. It rests on
  the container's mount sharing a superblock with the agent's (`sharecache`,
  the kernel default for one export and one option set), an assumption until
  a container's READ count reads zero after a warm. If warming is at least as
  fast, the union becomes write capture only and the applied manifest,
  `OpDrop` and the fill's Apply path go.

## Rejected, with the reason each was retired

| design | why it was retired |
|---|---|
| large files in the tree, as leaves promoted on re-read | a cumulative counter made a 1-byte poll eventually pull the file; caching one repays only on the SECOND future read |
| coverage bitsets, one bit per 64 KiB block | correct and cheap; with large files out of the tree there is no re-read rule to need them. Return here if partial caching of large files ever comes back |
| copy-up as promotion (`chmod` through merged) | coverage is not residency, and for small files it is one open+read chain per file where tar is one request; residency is visible with `mincore(2)` if it returns |
| a range-granular store: our own, fscache, or the page cache | the page cache plus `cached`'s invalidation already IS the partial store for everything but batching first reads |
| a FUSE of our own replacing fuse-overlayfs | sees hits as well as misses; retired as too hard, the write path is where the cases live (rename over open files, hard links, O_APPEND, mmap, whiteouts) |
| a FUSE of our own UNDER fuse-overlayfs, read-only | hundreds of lines and overlayfs treats a changing lower as undefined (ADR 0044 measured that once); too much for what it buys once the page cache is the cache |
| directory-batched prefill, a miss in D fetches D | a 5,000-file directory gets an arbitrary slice, content-addressed trees get no batching, fifty directories are fifty batches |
| cross-session recorded access order (Coda, eStargz, Nydus) | many containers per share, no trace to bind to |
| k-ary with k >= 3 as the anti-cascade measure | at `f_min = 0.05` one filled child of four is 25% and cascades too; the cap is what stops a runaway |
| FS-Cache on the workspace | content only, no bulk fill, and a corruption history around NFSv3 on 5.17 |
| `--mount consistency=direct,through`, then a `+`-joined pair | `--mount` is split on commas by the CLI, and a bare joined word is not namespaced, so it cannot be told from an option that is not ours. Both axes go in one csv-quoted `--mount` field (ADR 0042) |

## Prior art, checked 2026-09-04

Nobody reaches parity with an eager copy (Slacker, FAST'16: run phase 17%
slower; FlacIO, FAST'25: existing lazy loaders 4.6x off), and everyone ships
lazy anyway. Nobody ships PURE lazy: Coda's `spy`, eStargz `optimize`, DADI
and Nydus all record an access order from a real run, coalesce it into few
large transfers, and background-fill the rest. Online predictors (Kroeger and
Long 1996/2001; Griffioen and Appleton 1994; Vitter and Krishnan, JACM 1996)
are real and were never adopted by a shipping filesystem. The escalating tree
appears unpublished; its ancestors are Linux readahead (Wu, Xi, Li, OLS 2007:
4x under `max/16`, 2x to `max/2`, then clamp), buddy allocation (Knowlton,
1965) and competitive prefetching (Li, Shen, Papathanasiou, EuroSys 2007,
factor 2 for depth). JuiceFS is the depth-one cautionary case; patent art at
depth one is US 6,529,998 and US 11,474,948.

## Consequences

- **A `read=cached` union revalidates every 60s, not every second.**
- **Prefetch is bounded by the link, not by a config number.** The budget
  still caps the walk; it no longer decides how much a read pulls.
- **Landing a batch costs 0.36ms a file with no network in it**: 3,000 files
  through the union in 1.07s (`test/union-probe.sh`, 2026-09-04). The 2.6ms a
  file the old settle column suggested was the link, not the union.
- **A fresh share fills at once.** Nothing has been read, so the walk has
  nothing to yield to; the first miss stops it for 2s.
- **The bench contradicted the simulator.** The model charges a batch
  `RTT + transfer`; the real apply charges several round trips per file. The
  model is corrected when the apply is, so the next table can be read against
  it.

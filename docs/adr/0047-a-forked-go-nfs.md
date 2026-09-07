# 0047 — A forked go-nfs, consumed through a `replace`

- Status: Accepted
- Date: 2026-09-06
- Current answer: `github.com/willscott/go-nfs` is replaced by
  `github.com/lhns/go-nfs`, branch `all-fixes`, forked from upstream `v0.0.4`,
  in both `core-client/go.mod` and `client/go.mod`. It carries the fixes below
  and nothing else, and is dropped when upstream merges them.

## What forced it

`test/probes/fsprobe` runs the same filesystem operations against a share and
against a native bind mount and compares the answers. Five differences were the
server library's, not this project's: nothing in `core-client/nfsserve` or in
the export namespace could produce them. Two more are costs rather than
answers, so no conformance probe can see either.

### The conformance fixes

| fix, in the fork | what a user sees without it |
|---|---|
| `nfs_onremove.go` (`dirNotEmpty`) | `rmdir` and `rename` over a non-empty directory answer EIO; a script testing for ENOTEMPTY takes the wrong branch |
| `nfs_onrename.go` | `rename` onto an existing empty directory is refused, where rename(2) replaces it atomically |
| `helpers/cachinghandler.go` (`Rename`) | the cached handle is invalidated rather than moved, so a file renamed while open goes `Stale file handle` — including the silly-rename an unlinked open file gets |
| `nfs_onlink.go` | LINK is parsed in SYMLINK's layout, so a hard link dies in the XDR parser with EINVAL before any filesystem call |
| `nfs_onremove.go` (`isInvalid`), used by create, mkdir, symlink and rename | a filesystem EINVAL (a name the host cannot spell) is reported as EACCES or EIO, sending the user after a permission or disk fault that is not there |

- All five are wire-level or handler-level: they cannot be worked around from
  the `billy.Filesystem` this project supplies.
- `rmdir` is `onRemove` in go-nfs (`nfs_onrmdir.go` delegates), so one mapping
  covers both.

### Resolving a handle cost O(cache), and the cache is a million entries

`CachingHandler.FromHandle` refreshes the LRU recency of a handle's ancestors,
so a parent is not evicted while a live child still needs it. It reached them
by walking the WHOLE cache: `activeHandles.Keys()` allocating a slice of every
key, a `Peek` per key, and a `Get` under the exclusive LRU lock per prefix
match. It runs on EVERY request, the cache here is sized at a million
(`handleCacheSize` in `core-client/nfsserve/server.go`), and a handle is minted
per path the workspace touches, so the cost of resolving one handle grew with
how many files the session had ever seen. The ancestors are already indexed by
path in `reverseHandles`, so the fix reaches them directly: O(depth) rather
than O(cache).

Every answer is identical, which is why fsprobe cannot see it. Measured by
`core-client/nfsserve/handlecost_test.go`:

| handles cached | per resolution, before | after |
|---|---|---|
| 100 | 12.1us | 4.4us |
| 1,000 | 94us | 3.5us |
| 10,000 | 1.18ms | ~0 |
| 50,000 | 10.3ms | 3.1us |

- Linear before, flat after. 16 bytes of garbage per cached handle per request
  became 88 bytes flat.
- At 50,000 handles that is 1.9 seconds of pure cache walking for one 256MB
  write, on top of the write itself. It is enough to push the RPCs queued
  behind it past a soft mount's timeout, which is EIO on a write that worked
  earlier in the same session.
- It logs nothing and degrades for as long as a session runs, so it presents as
  a share that gets slower the longer it is used.

### A connection was serial

`conn.serve` handled one request, wrote its reply, and only then read the next.
A client multiplexes every outstanding RPC for a mount onto one connection, so
the slowest operation was the latency of everything queued behind it — which is
the other half of the timeout this project hit at `timeo=30`
(`core/workspace/export.go`). The fork dispatches up to
`DefaultMaxConcurrentRequests` (8) at a time per connection and lets replies
complete out of order, which the protocol allows: a reply is matched to its
call by XID.

Two things it required, and both are the fix rather than tidiness:

- **The request body is read into memory before it is dispatched.** Handlers
  parse their arguments lazily out of `req.Body`, so a body left on the
  connection can only be handled while nothing else reads that connection.
  Bounded at 2 MiB, because the length is the client's own number and FSINFO
  advertises a 1 GiB wtmax; over the cap the body stays a reader over the
  connection and is handled inline. Linux negotiates a wsize of at most 1 MiB,
  so that is the exotic case.
- **`ToHandle` mints under the reverse index's lock.** Two concurrent LOOKUPs
  of one path that both missed would otherwise mint a handle each and hand the
  client two handles for one file, which a client is entitled to treat as two
  objects.

## The decision

| | |
|---|---|
| fork | `github.com/lhns/go-nfs`, branch `all-fixes` |
| base | upstream `v0.0.4` |
| consumed as | `replace github.com/willscott/go-nfs => github.com/lhns/go-nfs@<pseudo-version>` |
| in | `core-client/go.mod` (direct) and `client/go.mod` (indirect, through core-client) |
| tests | each conformance fix carries one in the fork (`nfs_fixes_test.go`, `nfs_einval_test.go`, `helpers/cachinghandler_rename_test.go`), and concurrency carries `nfs_concurrent_test.go` with the fork's CI running every package under `-race`. `refreshAncestors` is gated HERE instead, in `core-client/nfsserve/handlecost_test.go`, because what it fixes is a cost rather than an answer and only this repository's cache size shows it |
| upstream | a pull request per fix, to follow, so the fork can be dropped rather than maintained |
| licence | Apache-2.0, unchanged from upstream |

Not forked: `github.com/willscott/go-nfs-client`, which supplies the XDR codec
and is upstream and untouched in both modules.

## What it costs

- **The pseudo-version is refreshed by hand when the branch moves.** In each of
  `core-client/` and `client/`:

  ```
  go mod edit -replace github.com/willscott/go-nfs=github.com/lhns/go-nfs@<sha>
  go mod tidy
  ```

- **Two modules to keep in step.** `client` reaches go-nfs only through
  `core-client`, and Go does not inherit a `replace` from a required module, so
  a replace in one module and not the other builds two different servers with
  nothing failing.
- **The notices file names the fork.** `THIRD-PARTY-NOTICES.md` is generated
  from what is linked (`scripts/third-party-notices.sh`), and a `replace`
  changes that, so the entry is the fork and its licence rather than upstream
  `v0.0.4`.
- **The fork must be re-based on any upstream release before it can be
  dropped**, and a rebase is where a fix silently stops applying. The tests in
  the fork are what catch that.
- **Exit condition:** upstream merges these fixes and cuts a release. Then both
  `replace` lines go, `go.mod` requires that version, and this record is
  deleted.

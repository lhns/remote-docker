# 0033 — The root handle is derived, the rest are not

- Status: Accepted
- Date: 2026-08-13

> A share's ROOT handle is a function of its export path. Everything below it
> stays a cache, because the kernel can ask again for those and cannot ask again
> for the root.

## What forced it

Restart the client while containers are running and every one of them reads
`Stale file handle` against a mount that still looks fine. Measured in
`test/nfs-resilience.sh` section 6 (E3).

Handles come from `helpers.NewCachingHandler`, which mints `uuid.New()` per path
into an in-memory LRU (`helpers/cachinghandler.go:63`). They mean nothing to a
process that did not issue them, and NFSv3 expects the opposite: a handle stays
valid for the life of the file, across server restarts. That expectation is what
makes the protocol stateless, and it is the one we break.

The part that decides the shape of the fix is where the FIRST handle comes from:

```go
rootHndl := userHandle.ToHandle(handle, []string{})   // go-nfs mount.go:42
```

The MOUNT reply carries a handle for the share root, and the kernel keeps it for
the life of the mount. **It never mounts again.** So when our root handle stops
resolving, every lookup begins at something dead, and no amount of retrying can
recover — which is why a plain `cat` of a file, a fresh path lookup with no
cached file handle involved, still failed every second for the rest of the
container's life.

Beneath the root it is a different story. Linux retries a path-based syscall
once on `ESTALE` with `LOOKUP_REVAL`, re-resolving each component from the mount
root. Given a root that answers, the kernel can obtain fresh handles for
everything under it by asking again.

## The decision

**Derive the root handle from the export path; leave the rest to the cache.**

```
a share root:  [ sha256(export path)[:8] ][ the caching handler's uuid ]   24 bytes
anything else: [ the caching handler's uuid ]                             16 bytes
```

Two things about that shape were learned the hard way, and both are the reason
it is written down rather than tidied:

- **The cache answers first.** A root handle carries the cache's own handle as
  well as the derived key, and resolution asks the cache before the key. So a
  running client resolves a root exactly as it did before any of this existed,
  and the key is consulted only when the cache cannot answer -- a client that
  has restarted, which is the entire point of the feature.
- **Ordinary handles keep the bytes and the length they had.** The first attempt
  put a one-byte tag in front of every handle, which is the tidy way to tell two
  formats apart. Every suite then failed identically: every mount succeeded and
  every read returned "permission denied", with the daemon unable to open the
  volume's own directory. Nothing in go-nfs or in this package explains that
  yet. What is established is only the measurement, so a share root is
  recognised by LENGTH and everything else is passed through untouched.

Recognising by length means depending on go-nfs's handles being a fixed 16
bytes, which a unit test pins rather than assumes.

A share root therefore resolves in a process that never issued it, and the
export it names is checked against what is registered NOW, which is ADR 0027's
rule applied to handles: the workspace may name a capability, never supply a
path.

Nothing else changes. There is no table of file ids, nothing new on disk, and no
walk.

## What was rejected, with the measurement that rejected it

The first draft of this record proposed deriving EVERY handle from its path:
`sha256(relative path)`, resolved on a miss by walking the share and hashing
what was found, since a one-way hash can be inverted by enumerating its domain.

Measured on 2026-08-13, on the development machine (Windows 11), walking a real
tree of 460,893 files and 84,028 directories:

| | |
|---|---|
| `filepath.WalkDir`, cold | 3m15s |
| walk + hash + store, warm | 1m10s |

The same walk on Linux, over 200,000 files in a container on a workspace, took
**111ms**. Windows is roughly a thousand times slower per file, and Windows is
where this client runs. Re-check with any equivalent recursive walk; the
throwaway used here counted files and hashed each path.

That is fatal, and not by a small margin. The volume mounts are
`soft,timeo=30,retrans=2`, so an RPC gives up after about sixty seconds: a
rebuild triggered by a container's read would still be walking when the kernel
abandoned the read that started it. The lazy repair fails precisely in the case
it exists for.

It is also the wrong shape. A walk costs what the tree CONTAINS; the problem is
proportional to what a container has TOUCHED, and for a `node_modules` those
differ by orders of magnitude. Persisting the map instead trades the walk for a
file that grows, needs pruning that cannot know which handles are live, and must
be invalidated when a share changes — all to make durable something the kernel
is willing to ask for again.

## Consequences

- **The change is a few lines**, in `core-client/nfsserve`, with no new state
  anywhere.
- **It rested on the kernel re-looking-up after `ESTALE`**, which was the one
  thing this record assumed rather than proved. E3 now proves it: with a root
  handle that resolves, a container that was running before a client restart
  goes on reading through the mount it already had. Per-file stability, and the
  persisted map behind it, are not needed.
- **An open file descriptor still breaks.** A container holding a file open
  across a client restart gets `ESTALE` on that fd, because there is no path
  lookup left to retry. Correct and unavoidable; the container gets a clear
  error rather than silence.
- **The export key must be checked against the live registry**, or a handle
  becomes a way to name a share that is no longer exported.
- **`SetAttrs` rebuilds `share.fs` on every connect** (`registry.go:215`), so
  nothing may key on the filesystem pointer. The export path is the identity.

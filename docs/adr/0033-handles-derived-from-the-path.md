# 0033 — A file handle is derived from the path, not remembered

- Status: Proposed
- Date: 2026-08-13

> A file handle must resolve in a process that never issued it.

## What forced it

Restart the client while containers are running and every one of them reads
`Stale file handle` against a mount that still looks fine: the port answers, the
volume is there, the directory is right where it was. Measured in
`test/nfs-resilience.sh` section 6 (E3).

The cause is `helpers.NewCachingHandler`, which mints a handle per path:

```go
id := uuid.New()        // go-nfs@v0.0.4 helpers/cachinghandler.go:63
```

A random 16 bytes, with an in-memory LRU mapping it back to a path. The handle
carries nothing about the file, so it is meaningless to any process but the one
that issued it.

Nothing on the workspace side is at fault, and there is nothing to fix there.
The kernel holds the handles it was given and keeps presenting them, which is
exactly what NFSv3 requires of it: **a handle is expected to stay valid for the
life of the file, across server restarts**. That expectation is what makes the
protocol stateless, and it is the one we have been breaking.

A conventional NFS server never stores a map. Its handles are built from the
inode number and a generation count, which the filesystem already keeps durably,
so a reboot changes nothing. We cannot copy that: going from an inode back to a
path needs `open_by_handle_at`, which requires `CAP_DAC_READ_SEARCH`, and this
client deliberately runs as an ordinary user on somebody's laptop with no setup.
*(Checked 2026-08-13 against `man 2 open_by_handle_at`.)*

The path is then the only durable name of a file we can both compute and act on
without privilege.

## The decision

**The handle is a function of the path, so any process can resolve it:**

```
handle = exportID (8 bytes, as today) || sha256(relative path)[:16]
```

Half of this already exists. Share ids are derived from the path so that a
reconnect reuses the same exports (ADR 0027); only the file half was random.
This applies the rule the code already follows to the other half of the
identity.

A hash is one-way, which sounds disqualifying and is not. Resolution does not
need arithmetic reversal, it needs the preimage, and the preimage is a file on
this machine's own disk. So an unresolvable handle is answered by **walking the
export and hashing what is found**, which is a local `readdir` and needs nothing
from the workspace. What the hash buys is not reversibility but
**reproducibility**: a UUID can never be rebuilt by walking, because it was never
a function of anything.

Three rules make that safe:

- **One walk per export per process, not per handle.** A deleted file's handle
  is legitimately unresolvable, so a miss that walks every time would walk
  forever. Once an export has been rebuilt, later misses are `ESTALE` at once,
  which is the correct answer for a file that is gone.
- **A collision serves the wrong file**, which makes it a correctness fault
  rather than a slow path. 16 bytes is chosen for that, not for tidiness, and
  the rebuild refuses a colliding pair rather than picking one.
- **A handle resolves only inside a currently registered export.** The same rule
  ADR 0027 states for the share record: the workspace may name a capability, it
  may never supply a path.

Rejected, and why:

- **Persisting the map.** It works, and it is a file that grows, needs pruning
  that must not strand a live handle, and must be invalidated when a share
  changes. The walk gets the same result with nothing on disk.
- **Putting the path in the handle.** No storage and no walk, but NFSv3 caps a
  handle at 64 bytes and a `node_modules` path spends that on its own.

## Consequences

- **A client restart stops breaking running containers**, which is the point.
  E3's assertion flips from documenting the failure to asserting the fix.
- **The walk is the cost.** With watching on the client already walks the shared
  trees to place watches, with a budget and an exclude list, so the map can be
  built by a walk that happens anyway. With `--watch off` it is paid on the
  first miss after a restart. A hostile tree is the case that decides whether
  this is free or merely cheap, and it is measured before this is accepted, not
  after.
- **A tree past the budget degrades to today's behaviour**, not worse than it:
  entries beyond the cap are absent and their handles read `ESTALE`, which is
  what all of them do now.
- **`ToHandle` and `FromHandle` become ours.** They currently panic and delegate
  to the caching handler, which also supplies the directory verifiers
  (`VerifierFor`, `DataForVerifier`). Those stay its business; only the two
  handle methods move.
- **The hash is over the path as the registry spells it.** Two spellings of one
  file on a case-insensitive filesystem hash differently and get different
  handles, which is correct but worth knowing before somebody normalises it.

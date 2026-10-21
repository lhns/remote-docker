# 0027 — Restoring an export the workspace remembers

- Status: Accepted; amends [ADR 0007](0007-virtual-nfs-export-namespace.md)
- Date: 2026-08-11; amended 2026-09-25, 2026-10-14, 2026-10-15, 2026-10-21
- Current answer: restore lazily, from a MOUNT or a share ROOT HANDLE that
  misses, through the checks below. Never eagerly, never from a lookup. A
  restored union share is attached to the cache with its recorded words, and
  the fill record carries the baselines write-back compares against
  (amended 2026-10-21).

## Context

`docker compose up -d` failed against containers that already existed:

```
error while mounting volume '/var/lib/docker/volumes/rd-6dbfcaadebf870b5/_data':
mount :/m/6dbfcaadebf870b5 ... no such file or directory
```

The directory was right there. `docker compose down && docker compose up` fixed
it, which is a fix that tells you nothing about the cause.

The cause is a lifetime mismatch. The export registry (ADR 0007) is in memory
and populated lazily: a directory is registered the first time a bind mount
names it, which happens on `POST /containers/create` and nowhere else. A
**volume** created from that share outlives the process. So starting a container
that already exists sends no create request, registers nothing, and dockerd
nevertheless mounts the volume it made last time. The MOUNT names `/m/<id>`, the
registry has never heard of it, and the server answers MNT3ERR_NOENT. `down`
worked because deleting the containers forces `up` to create them again, which
runs the rewrite, which registers the share.

Nothing could recover it unaided: a share id is a truncated digest and cannot be
inverted, so given only an export path there is no way back to a directory.

See also [ADR 0032](0032-the-workspace-is-the-record.md), which is the same
problem -- state outliving a session -- resolved the other way round. What
outlives a session here is a capability the workspace may NAME, so the client
remembers it; what outlives one there is an address the workspace BINDS, so the
workspace does.

## Decision

Record `export path -> local directory` per workspace, and restore a share
**lazily, on a MOUNT or a root handle that misses**, never eagerly at session
start.

- A root handle carries a digest of its export (ADR 0033), matched against the
  recorded exports. A kernel keeps the handle MOUNT gave it and never mounts
  again, so after a client restart a running container presents the handle and
  nothing else: without this it gets `Stale file handle`.
- Both are asked only by a kernel that has mounted that export, so neither
  restores anything a container is not using.

Eager restore was rejected on one ground. ADR 0007's guarantee is that the
workspace's view of this machine is exactly the set of paths the user asked for.
Re-exporting everything a workspace has ever mounted, because a session started,
turns that into every path ever asked for, including in a session that has
nothing to do with them.

**The file is a capability list, not a lookup table.** The workspace names an id
and this machine chooses among entries it wrote down itself; the far side never
supplies a path. Every entry is checked again before it is believed, because a
file on disk is not evidence:

- the id is **recomputed** from the path, so a corrupted or hand-edited record
  cannot make `/m/<id>` resolve elsewhere: the entry would have to carry the
  digest of that elsewhere, and only this machine can produce one;
- the file is bound to this hostname and local account, refused **wholesale**
  rather than entry by entry, because a configuration directory is a thing
  people sync and a partial match is the case most likely to be a different
  directory with the same spelling;
- the path must still be a directory;
- an entry nothing has wanted for thirty days is dropped, as is one whose volume
  the workspace no longer has.

Only a session that serves gets the record. A query session exports nothing, and
giving it one would let asking a question re-export a directory.

## Consequences

A container created in one session and started in another works, which is the
ordinary `compose` cycle and was the whole complaint.

The set of exports is no longer only what this run was asked for. It is that,
plus directories this machine previously offered to this workspace and which
that workspace still holds a volume for. ADR 0007 is amended to say so.

**The record must not feed `rewrite.Guard`.** The obvious next change is to make
a recorded share count as "in use" so the collector spares its volume. It must
not: `VolumesInUse` lists containers with `all=true`, so a stopped container
already pins its volume, and the collector was never the hazard here. Wiring the
record in would keep every recorded volume alive until the record expired, which
is a real cost for a case that is already covered.

Two things are deliberately not checked, because neither can be portably. A
directory deleted and recreated as something else at the same path is
indistinguishable without recording an inode. And a 64-bit id collision would
resolve to the wrong directory, which ADR 0007 already answers by saying the
digest is an identity rather than a security boundary.

## The record carries the share's words and variant (amended 2026-10-14, 2026-10-15)

- **Words** (`owner=`, `watch=`, `prefetch=`, ADR 0042). A restore registers
  the share with the words it had, or a container started later would see the
  session's default owner on a volume created for another, or an unwatched share
  where a watched one was. The record carries `owner` (resolved, `uid:gid`),
  `watch` and `prefetch`, omitted when unset; a record without one takes the
  session's default. `remember` writes the share as it stands, with watch and
  prefetch raised to what its mounts asked for.
- **Variant** (`read`, `write`, and the owner above). The share id is derived
  from the path AND the variant (ADR 0007, ADR 0042), and a record is checked by
  what it carries: one with a variant must have
  `export == ShareIDFor(path, variant)`, and one without, written by 0.11 or
  earlier, must have `export == LegacyShareID(path)`.
- **A legacy record restores by export only.** Its mode is unknown, so the
  share keeps the legacy export, which no mount derives (the registry is keyed
  by export alone): it keeps serving the containers that mount it, and a new mount of the directory
  gets a share, export and volume of its own. This is what makes upgrading safe.
- **A restored share that watches starts the watcher** and is handed to it at
  once (`Registry.Restored`), because the mount that brought it back registered
  nothing and so told nothing. **It is attached to the cache** (amended
  2026-10-21), as a mount would, with the words it was recorded with: its
  prefetch resumes (ADR 0045) and, above all, its write-back runs again.
  Before, a restart left a restored share unattached, and write-back, which
  polls only attached shares, never carried a container's writes back from it.
- **The fill record holds baselines** (amended 2026-10-21). The record (`caches/<workspace>.json`)
  keeps, beside each share's list of paths, the size and modification time every
  file had when it was sent. After a restart the paths alone read every file as
  one the fill never sent, so a file the container modified was mistaken for one
  this machine made and was never written back, only new files were. The paths
  keep their field, so a client before this reads the file as it did, and the
  file version stays 1.
- **The file version stays 1** through both: `readBound` drops a record whose
  version differs, so a bump would make every restore fail after an upgrade.

## Where

`client/internal/session/shares.go` is the record; `Registry.Restore` in
`core-client/nfsserve/registry.go` is the hook, consulted only from
`LookupOrRestore`, which only `mountHandler.Mount` calls. `Lookup` and `Shares`
never resurrect anything, or "in use" would depend on who asked.

`TestServeRestoresAnUnregisteredExport` mounts an export nothing registered over
a real NFSv3 conversation and reads the file back.

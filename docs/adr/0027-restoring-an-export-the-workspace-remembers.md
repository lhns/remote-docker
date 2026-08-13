# 0027: Restoring an export the workspace remembers

Accepted, 2026-08-11.

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

Nothing could recover it unaided: `ShareID` is `sha256(path)[:16]` and cannot be
inverted, so given only an export path there is no way back to a directory.

See also [ADR 0032](0032-the-workspace-is-the-record.md), which is the same
problem -- state outliving a session -- resolved the other way round. What
outlives a session here is a capability the workspace may NAME, so the client
remembers it; what outlives one there is an address the workspace BINDS, so the
workspace does.

## Decision

Record `export path -> local directory` per workspace, and restore a share
**lazily, on a MOUNT that misses**, never eagerly at session start.

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
- `/cwd` is never restored, since the session registers it from the directory
  the command actually ran in;
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

## Where

`client/internal/session/shares.go` is the record; `Registry.Restore` in
`core-client/nfsserve/registry.go` is the hook, consulted only from
`LookupOrRestore`, which only `mountHandler.Mount` calls. `Lookup` and `Shares`
never resurrect anything, or "in use" would depend on who asked.

`TestServeRestoresAnUnregisteredExport` mounts an export nothing registered over
a real NFSv3 conversation and reads the file back.

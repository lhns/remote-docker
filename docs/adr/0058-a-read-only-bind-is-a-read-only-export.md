# 0058 — A read-only bind is a read-only export

- Status: Accepted
- Date: 2026-11-12
- Current answer: an opt-in per-workspace setting, `read-only-exports`
  (`readOnlyExports`, `REMOTE_DOCKER_READONLY_EXPORTS`, `remote set --read-only-exports
  on|off`), off by default. Off, nothing changes. On, `ro` on a bind (a `-v`
  option, or `ReadOnly` on a `--mount` entry) still stays on the container's
  mount and also makes the share read-only: it is a variant of its own, its NFS
  filesystem on this machine refuses every mutation, and `write=back` becomes
  `write=ephemeral` for it. Only the agent's no-op time-set still succeeds.

## What forced it

- A bind becomes a volume over an export that was read-write whatever the
  container asked for (ADR 0006, ADR 0046). `ro` on the container's mount
  protects only that container: the daemon enforces it there.
- Anything else on the workspace that reaches the export wrote to the user's
  directory: its operator or root, a shell on a shared daemon (ADR 0012), a
  host-networked container, or another container mounting the `rd-` volume by
  name without `ro`. A named volume is never rewritten, so that last one is
  ordinary Docker.
- `write=back` is a second path in: the workspace can write the union's upper
  layer and the client carries it home (ADR 0044).

## The decision

| Piece | Answer |
|---|---|
| Switch | `read-only-exports`, per workspace: JSON `readOnlyExports`, `REMOTE_DOCKER_READONLY_EXPORTS` (on, off, true, false, 1, 0, yes, no), `remote set <name> --read-only-exports on\|off`, with the precedence of the neighbouring settings (flag, environment, file, default off). A session setting, so `remote status` lists it when on and a running session with another value is reported like any other. Read at session start; it applies to mounts made afterwards |
| Off | Byte for byte as before: `ro` is not part of the variant, no id changes, no split between ro and rw mounts, `write=back` stays `write=back`, no wrapper. Tests pin the id and the rewrite result |
| Variant | `ShareVariant.ReadOnly`. `ShareIDFor` appends `\x00ro` only when set, so no existing id moves. A read-only and a read-write mount of one directory are two shares: two exports, two volumes, two unions |
| Source | Docker's `ro`, never consumed. `rewriteBinds` reads it from the option list (`ro` among the comma-separated options, `ro,z` included), `rewriteMounts` from `ReadOnly: true`. A single file (ADR 0039) goes through the same walk |
| Write mode | `write=back` resolves to `write=ephemeral` in `resolveMount`; `through` and `ephemeral` stay, the read mode is unchanged. The share keeps its cache and prefetch and carries nothing back. `attachShare` also treats a read-only share as ephemeral, so a record cannot turn write-back on |
| Enforcement | `nfsserve.readOnlyFS`, a wrapper directly under the attribute layer of the session's existing stack (so `defaultAttrs`, including `AlwaysExecutable`, is untouched), and `readOnlyChange` in place of `attrChange` |
| Volume options | Unchanged. `ro` is not added to `NFSVolumeOptions` |
| Record | `ro` in the share record, absent meaning false, no version bump. The export is recomputed from the variant before a record is believed, so editing the flag drops the record |

### Why a wrapper and not a capability flag

go-nfs (the fork, ADR 0047) checks `billy.WriteCapability` in WRITE, CREATE,
MKDIR, SYMLINK, MKNOD, REMOVE, RENAME, LINK and SETATTR and answers
NFS3ERR_ROFS, strips write from ACCESS, and answers COMMIT with a server fault.
The agent's poker (ADR 0016) sets a file's mtime to the value it already has
over NFS and opens it for writing; the kernel raises its inotify event only if
that SETATTR succeeds and ACCESS allowed the open. Without the capability both
fail, and `ro` combined with `watch=` or `read=cached` would stop working
silently. So the capability stays and the refusal is one layer down.

### What is refused

- **Filesystem layer** (`readOnlyFS`): `OpenFile` with any of `O_WRONLY`,
  `O_RDWR`, `O_APPEND`, `O_CREATE`, `O_TRUNC` (WRITE, and SETATTR with a size),
  `Create`, `Rename` (both directories), `Remove` (REMOVE, RMDIR, and the
  target clearing a RENAME does), `MkdirAll`, `Symlink`, `TempFile`.
  `Chroot` re-wraps, so a mount of `/m/<id>/sub` is as read-only as the root.
- **Change layer** (`readOnlyChange`): `Chmod`, `Chown`, `Lchown`, `Link`,
  `Mknod`, `Mkfifo`, `Socket`, and `Chtimes` unless it is an identity.
- **Status**: NFS3ERR_ACCES. go-nfs answers NFS3ERR_ROFS only from the
  capability check this design avoids. A container sees `Permission denied`,
  not `Read-only file system`.
- **COMMIT** reaches no filesystem call and cannot be refused here; with no
  WRITE able to succeed it has nothing to flush. It is pinned as changing
  nothing.

### The identity time-set

go-nfs calls `Chtimes` for every SETATTR that carries a time (it compares two
pointers, so the "unchanged" test never holds). `readOnlyChange.Chtimes`
therefore decides itself: it accepts the call, without applying it, exactly when
both times equal the file's current modification time to the nanosecond, and
refuses it otherwise.

- go-nfs substitutes the file's current value for a time the client left out,
  and the share reports atime, mtime and ctime as one value, so the poker's
  mtime-only set arrives as an identity.
- Any other time, including `touch` and a time set to the server's, is refused.
- Size, mode and owner are refused whatever they say, including a value equal
  to the current one.
- Like the read-write share's `Chtimes`, an accepted set is not applied.

### Containment

- No path reaches a share's filesystem except through its own mount: `/`, `/m`
  and a `..` that leaves a share name nothing, and LOOKUP of `..` from a share
  root does not reach its siblings. Tests pin it over the wire.
- RENAME and LINK between two shares are refused by go-nfs (its two
  filesystems differ), in both directions, so nothing moves into or out of a
  read-only share through another.

## Why it is off by default

- The benefit is narrow. It protects a directory against the workspace side
  only, and only one that is mounted read-only and never read-write, since a
  read-write export reaches the same files. The threat model accepts the
  nearby cases (the workspace's operator, a shared daemon's shells).
- It changes what a user sees: a read-only and a read-write mount of one
  directory stop sharing a union (below), and the refusal is `EACCES`.
- Containers already running keep their volumes, so turning it on protects
  nothing until they are recreated.

## What it costs

- **A read-only and a read-write mount of one directory are no longer one
  share.** They used to share one export and, under `write=back`, one union, so
  a reader saw a writer's changes at once. Now they are two. Under
  `write=back` the read-only container sees the other container's writes only
  after write-back lands them on this machine and its own share is refreshed
  (seconds), and the directory is cached twice. Under `write=through` nothing
  changes in what is visible.
- **While any read-write mount of a directory exists, its read-write export
  reaches the same files**, so the read-only export protects a directory only
  when nothing mounts it read-write. The same holds for a read-write share of a
  parent directory. This is expected and is not code's to prevent: the user
  asked for both.
- **Existing containers keep their read-write volume until they are recreated**
  (`compose down && up`), since a volume's driver options are immutable and the
  volume names the old export. Turning the setting off again does the same in
  the other direction.
- **A read-only `write=back` mount no longer carries anything home**, which is
  what read-only means; it is `ephemeral`, so a container's own writes cannot
  exist there anyway.
- **The refusal is `EACCES`**, not `EROFS`.
- **Nothing on the agent changes**, so an old workspace works with a new
  client.
- The integration suite does not plant a file in the union's cache layer from
  the workspace side to show it never arrives: it asserts the share is a union
  with no write-back row in `remote status`. The layer's path belongs to the
  agent.

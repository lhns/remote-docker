# 0039 — A single file is a one-file export

- Status: Accepted; extends [ADR 0006](0006-per-bind-nfs-volumes.md) and
  [ADR 0007](0007-virtual-nfs-export-namespace.md)
- Date: 2026-08-19
- Current answer: a file bind is exported as a **synthesised directory holding
  only that file**, and mounted with `VolumeOptions.Subpath` so the container
  sees a file.

## What forced it

`-v ./nginx.conf:/etc/nginx/nginx.conf` was refused:

```
nfsserve: /home/you/nginx.conf is not a directory; only directories can be exported
```

- Single-file binds are ordinary in compose: `./nginx.conf`, `./my.cnf`,
  `/etc/localtime`. Anyone bringing an existing stack met this as a 500 from a
  program they believe is Docker.
- The refusal was deliberate — exporting the containing directory would hand the
  workspace every file beside the one asked for — and it was documented nowhere
  outside that one comment.

## The decision

| step | what happens |
|---|---|
| export | a synthesised directory whose only entry is that file |
| volume | as for any bind: `rd-<client>-<id>`, NFS options, managed labels |
| mount | `Type: volume`, plus `VolumeOptions.Subpath: <base name>` |

**No host-side trickery.** No symlink farm, no temp directory, no tmpfs, no
chroot. The export namespace is ours to compose (ADR 0007): each share already
carries its own `billy.Filesystem`, so `singleFileFS` presents one entry and
answers `ErrNotExist` for every other name in the real directory. Nothing is
created on the user's machine, and it is the same code on Windows, macOS and
Linux.

**One export per file, not one per directory.** Exporting the parent once and
using two subpaths would be fewer volumes and would share the whole directory,
which is the property the original refusal protected.

**The subpath is what makes it a file.** Read out of the daemon rather than
assumed — `internal/safepath/join_linux.go` stats the resolved path and creates a
temp FILE to bind-mount onto when it is not a directory:

```go
isDir := (stat.Mode & unix.S_IFMT) == unix.S_IFDIR
if isDir { return os.MkdirTemp("", "safe-mount") }
f, err := os.CreateTemp("", "safe-mount")
```

*(Read 2026-08-19 from `docker/docker@v28.5.2`. Re-check with
`grep -rn "func tempMountPoint" -A 25` in that module.)*

**A `-v` of a file leaves `Binds`.** A bind string has no field for a subpath,
so the entry is removed from `Binds` and appended to `Mounts` in the same walk —
the daemon rejects one target named in both lists. `ro` becomes `ReadOnly`; any
other bind option is refused **by name**, because a rewritten mount keeps every
option it arrived with and `ro` is the one standing between a container and the
user's files.

**Sockets, devices and FIFOs stay refused, with the real reason.** A socket is a
kernel object reached through a path, so what crosses NFS is the name and
nothing behind it: `connect()` needs the object in the local kernel. That is
equally true of a socket inside a directory this client already exports, which
is why the message says so rather than saying "not a directory" — the old
wording sent two people looking for a single-file limitation that was not the
cause.

**Refused early on a workspace that cannot do it.** `VolumeOptions.Subpath` is
API v1.45, so Docker 26. The client already knows the workspace's version from
`workspace.Info`, and a file mount against an older daemon is refused before a
share or a volume is created. An unreadable version string counts as capable:
refusing a working setup because a string was an odd shape is worse than letting
the daemon answer.

## Consequences

- **One volume per exported file.** A stack binding five config files gets five
  volumes. They carry the managed labels, so the collector handles them like any
  other, and `rewrite.Guard` already spans registering the share and creating the
  volume.
- **The watch moves to the containing directory.** Events for a file arrive
  there — an editor writing through a temporary file replaces the inode, and a
  watch on the old one sees nothing. Everything but the exported name is dropped
  in `rootFor`, which is the one funnel every event passes, and the walk stops at
  that single directory so a file share cannot spend the watch budget on
  siblings' subtrees.
- **`docker inspect` shows a volume with a subpath**, not a bind. Same trade as
  every other rewrite (ADR 0006), one level finer.
- **A file replaced by a directory, or removed, is not re-derived.** The export
  is registered from what the path was when the container was created. Recreating
  the container re-registers it.
- **Untested: a symlinked file source.** `safeOpenFd` resolves symlinks and may
  refuse one. Nothing here depends on it and nothing asserts it.

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

**No host-side trickery**: no symlink farm, temp directory, tmpfs or chroot.
The export namespace is ours to compose (ADR 0007), so `singleFileFS` presents
one entry and answers `ErrNotExist` for every other name in the real directory.
Nothing is created on the user's machine, and it is one code path on all three
platforms.

**One export per file, not one per directory.** Exporting the parent once with
two subpaths would be fewer volumes and would share the whole directory, which
is the property the original refusal protected.

**The subpath is what makes it a file**, read out of the daemon rather than
assumed — `internal/safepath/join_linux.go` creates a temp FILE to bind-mount
onto when the resolved path is not a directory:

```go
isDir := (stat.Mode & unix.S_IFMT) == unix.S_IFDIR
if isDir { return os.MkdirTemp("", "safe-mount") }
f, err := os.CreateTemp("", "safe-mount")
```

*(Read 2026-08-19 from `docker/docker@v28.5.2`. Re-check with
`grep -rn "func tempMountPoint" -A 25` in that module.)*

**A `-v` of a file leaves `Binds`** for `Mounts`, in the same walk: a bind
string has no subpath field, and the daemon rejects one target named in both
lists. `ro` becomes `ReadOnly`; any other bind option is refused **by name**,
since a rewritten mount keeps what it arrived with and `ro` is what stands
between a container and the user's files.

**Sockets, devices and FIFOs stay refused, with the real reason**: `connect()`
needs the kernel object, and a file share carries the name and nothing behind
it. Equally true of a socket inside an exported directory, which is why the
message says so — "not a directory" pointed at single files and hid the
cause.

**Refused early on a workspace that cannot do it.** `Subpath` is API v1.45, so
Docker 26; the version is already in `workspace.Info`, and an older daemon is
refused before a share or volume exists. An unreadable version counts as
capable — refusing a working setup over an odd string is worse than letting the
daemon answer.

## Consequences

- **One volume per exported file.** Five config files, five volumes. They carry
  the managed labels, so the collector treats them like any other.
- **The watch moves to the containing directory**, because that is where a
  file's events arrive: an editor writing through a temporary file replaces the
  inode, and a watch on the old one sees nothing. `rootFor` drops every other
  name, and the walk stops at that one directory so a file share cannot spend the
  budget on siblings' subtrees.
- **`docker inspect` shows a volume with a subpath**, not a bind. The same trade
  as every rewrite (ADR 0006), one level finer.
- **A file replaced by a directory is not re-derived.** The export is what the
  path was when the container was created; recreating it re-registers.
- **Untested: a symlinked file source.** `safeOpenFd` resolves symlinks and may
  refuse one. Nothing here depends on it.

# 0046 — A share reports wide mode bits and an owner that is root unless named

- Status: Accepted
- Date: 2026-09-04, restructured 2026-10-14, amended 2026-10-15 (a share per
  owner) and 2026-10-23 (`fmask=` and `dmask=`)
- Current answer: every file in a share reports mode 0666 and every directory
  0777 (plus the execute bits a file really has), owned by the `owner=` word of
  its mount, else `REMOTE_DOCKER_SHARE_OWNER`, else root (`0:0`). `fmask=` and
  `dmask=` (else `REMOTE_DOCKER_SHARE_FMASK` and `_DMASK`, else 0) then clear
  bits from those modes, the file's after its execute bit. A union's upper is
  created 0777 and chowned to the same owner, and no mask reaches it. Chown
  stays a no-op.

## What forced it

- A container running as a uid of the image's choosing failed `mkdir` inside a
  `delegated` share with EACCES (2026-09-04). A plain mount never showed it: the
  kernel asks the server (ACCESS) and go-nfs grants every request (upstream
  `v0.0.4` and the fork of [ADR 0047](0047-a-forked-go-nfs.md) alike). A union
  checks the mode overlayfs copied from the lower, locally, and refuses.
- git refuses a worktree whose top directory is owned by another uid ("detected
  dubious ownership"), and root is not exempt. With the account's uid (10000) as
  owner, git failed in every container, so wide bits alone were not enough.

## The decision

| what | reports | why |
|---|---|---|
| owner | the mount's `owner=`, else `REMOTE_DOCKER_SHARE_OWNER` (config key `shareOwner`), else `0:0` | git compares it with the uid it runs as; root is the uid most containers run as |
| file mode | 0666, plus 0111 where the file is executable (always on Windows) | usable by whatever uid runs |
| directory mode | 0777 | the same |
| masks | file mode `&^ fmask`, directory mode `&^ dmask` (amended 2026-10-23) | what FAT, ntfs-3g and WSL's drvfs offer, chosen per mount rather than guessed from content |
| union upper | 0777, chowned to the owner | the merged root takes the upper's mode and owner |
| chown | accepted, dropped | ownership is synthesised; no chmod/chown pair hands a file to another uid |

- **Spelling.** `uid` or `uid:gid`, the gid defaulting to the uid. In a mount
  word the separator is any one character that is not a digit, `=` or `,`,
  because `-v` cannot carry a fourth colon (ADR 0042). An invalid value is an
  error naming its source, because a wrong owner is the silent failure this
  decision exists to remove.
- **Fixed per share, and a share is per owner** (amended 2026-10-15). The
  share's attributes are the session's with only the uid and gid replaced (the
  session's carry `AlwaysExecutable`, which Windows cannot do without), set when
  the share is registered. The owner is part of the share id (ADR 0042), so a
  second mount of the directory naming another owner is a share of its own with
  its own export and volume, and nothing is changed under the first. The
  setting is read once per session, so `Registry.SetAttrs` and the rebuild on
  connect no longer exist.
- **The union root.** The merged root is the upper the agent creates, so git as
  another uid refused a `write=back` share even with the owner set. The client
  sends the owner with `OpPrepare` (`cache.Request.Owner`, `uid:gid`, omitted for
  root); the child chowns the upper after its chmod, and the agent chowns it
  again once the union answers, which covers a union adopted after an agent
  restart. The greeting announces the capability (`cache.Hello.Owner`): a
  workspace without it ignores the field, so a non-root owner on a union is
  refused by name before anything is created (`fix: update the workspace, or
  use write=through`), and root keeps working. A live union's owner cannot
  change: a prepare naming another is refused, since the supervisor restarts the
  child from its spec and would revert it. With the owner in the share id that
  refusal is unreachable and stays as a guard (ADR 0044).
- **Why not the caller's uid.** Per request is not possible: the kernel caches
  attributes per inode, so an entrypoint that drops from root to a user would
  see whichever uid stat'd first, and go-nfs gives the filesystem no
  credentials. Files copied up keep the reported owner; files a container
  creates carry its real uid.

## Amendment 2026-10-23: `fmask=` and `dmask=`

- **Why.** From Windows every file reports 0777 (`AlwaysExecutable`). A repository
  `git init` made in a container has `core.filemode=true` (the container's own
  files in a union's upper are real 0644 files), so git records every Windows
  file as 100755 and Git for Windows then reports them all modified.
- **The word.** `fmask=` and `dmask=`, octal, 1 to 4 digits, at most 0777; a
  `dmask` clearing all three execute bits is refused. Session defaults
  `REMOTE_DOCKER_SHARE_FMASK` and `_DMASK` (config `shareFmask`, `shareDmask`),
  refused naming their source as the owner is. Part of the share id (ADR 0042).
- **Order.** `perm = (FileMode | exec) &^ fmask`, `perm = DirMode &^ dmask`. The
  mask applies after the execute bit is decided, so `fmask=0111` clears a real
  one as well as an invented one, and hides a later `chmod +x` on a Linux host.
- **Recommend `fmask=0111`, never a mask with write bits.** It gives 0666, which
  git records as 100644 (it reads only S_IXUSR). A union checks modes locally,
  so `0133` or `dmask=022` makes the files or directories read-only for every
  uid but the owner; a plain mount still grants every access through ACCESS.
- **Not masked.** The union's root stays 0777: only the agent sets it, and
  changing that needs a protocol field. Files a container creates in a union
  keep their real modes.
- **Where a filled file's mode comes from** (read 2026-10-23). The fill's tar
  carries the host's real mode (`core/cache/tarstream.go`), but the agent's
  `O_EXCL` create through the merged mount fails where the lower already shows
  the file (`agent/internal/unions/write.go`), so it opens that file and the
  overlay copies it up with the lower's mode: the masked one `nfsserve`
  reports. The tar's mode applies only to a file the lower does not show yet,
  which from Windows is 0666 and already matches `fmask=0111`. Accepted.
- **Nothing on the workspace changes.** Modes are synthesised by this client's
  NFS server, so it works against any workspace version.

Covered by `integration.sh` 14b: an executable, a plain file and a directory
under no mask, `fmask=0111` (on `-v` and `--mount`) and `dmask=022`, and git as
uid 1000 on a `write=back,fmask=0111` union recording 100644 and still writing;
`machine.yml`: a file from a Windows runner reading 0777, then 0666 under
`fmask=0111`.

## What it costs

- `ls` in a container shows every file writable by everyone. The boundary is the
  tunnel and the account (ADR 0003, ADR 0029), not the mode bits; a read-only
  bind (`ro`) is still enforced by the mount.
- A file this machine cannot write is reported writable; the write fails on the
  host and go-nfs reports it as EACCES.
- One more setting to document, for images that do not run as root.

Covered by `integration.sh` 15f: the reported triple, a uid-1000 mkdir on a
plain mount and at the top and inside a union, and `git status` as root.

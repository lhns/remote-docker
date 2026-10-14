# 0046 — A share reports wide mode bits, owned by root unless configured

- Status: Accepted
- Date: 2026-09-04
- Current answer: every file in a share is reported as uid and gid 0:0, or the
  owner named by `REMOTE_DOCKER_SHARE_OWNER`, with mode 0666, every directory
  0777 (plus the execute bits a file really has); a union's upper is created
  0777. Chown stays a no-op.

## What forced it

- A container running as a uid of the image's choosing (`user: opencode`)
  failed `mkdir` inside a `delegated` share with EACCES (2026-09-04).
- The export reported every directory as `<account>:<account> 0755`
  (`client/internal/session/session.go` `defaultAttrs`), and the union's
  root is the upper the agent creates as root, `0:0 0755`.
- A plain mount never showed it: the kernel asks the server (ACCESS) and
  go-nfs grants every request (upstream `v0.0.4` and the fork of
  [ADR 0047](0047-a-forked-go-nfs.md) alike). A union checks the mode
  overlayfs copied from the lower, locally, and refuses.

## The decision

| what | reports | why |
|---|---|---|
| owner | 0:0, or `REMOTE_DOCKER_SHARE_OWNER` (`uid[:gid]`) | one number for every file; git compares it with the uid it runs as (amendment below) |
| file mode | 0666, plus 0111 where the file is executable (always on Windows) | usable by whatever uid runs |
| directory mode | 0777 | the same |
| union upper | 0777 | the merged root takes the upper's mode |
| chown | accepted, dropped | ownership is synthesised; no chmod/chown pair hands a file to another uid |

- `nfsserve.DefaultAttrs` already said this and every unit test ran under it;
  the session's defaults were the tighter pair.
- The owner setting this section first declined was added on 2026-10-14
  (amendment below).

## What it costs

- `ls` in a container shows every file writable by everyone. The boundary
  is the tunnel and the account (ADR 0003, ADR 0029), not the mode bits;
  a read-only bind (`ro`) is still enforced by the mount.
- A file this machine cannot write is reported writable; the write fails on
  the host and go-nfs reports it as EACCES.

Covered by `integration.sh` 15f: the reported triple, a uid-1000 mkdir on a
plain mount and at the top and inside a union, and `git status` as root.

## Amendment 2026-10-14: root is the default owner, and the owner is a setting

- The first decision assumed that with wide bits nothing needs the numbers to
  match. git does: it refuses a worktree whose top directory is owned by
  another uid ("detected dubious ownership"), and root is not exempt. With the
  account's uid (10000) as owner, git failed for every container.
- Default is now 0:0, the uid most containers run as. An image that runs as
  another user sets `REMOTE_DOCKER_SHARE_OWNER=uid[:gid]` (config key
  `shareOwner`); the gid defaults to the uid. An invalid value is an error
  naming its source, because a wrong owner is the silent failure this fixes.
- Fixed for the life of the client process. The workspace no longer supplies
  the owner, so `Registry.SetAttrs` and the rebuild on connect are gone.
- Reporting the caller's uid per request is not possible: the kernel caches
  attributes per inode, so an entrypoint that drops from root to a user would
  see whichever uid stat'd first, and go-nfs gives the filesystem no
  credentials.
- Unions need nothing on the client. Lower files keep the reported owner
  through copy-up, files a container creates carry its real uid, and git
  checks the share root.
- The dropped bullet's cost is real: one more setting to document, for images
  that do not run as root.

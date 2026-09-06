# 0046 — A share reports wide mode bits, owned by the account

- Status: Accepted
- Date: 2026-09-04
- Current answer: every file in a share is reported as the workspace account's
  uid and gid with mode 0666, every directory 0777 (plus the execute bits a
  file really has); a union's upper is created 0777. Chown stays a no-op.

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
| owner | the account's uid and gid | one number for every file; a uid the container chose cannot be known here |
| file mode | 0666, plus 0111 where the file is executable (always on Windows) | usable by whatever uid runs |
| directory mode | 0777 | the same |
| union upper | 0777 | the merged root takes the upper's mode |
| chown | accepted, dropped | ownership is synthesised; no chmod/chown pair hands a file to another uid |

- `nfsserve.DefaultAttrs` already said this and every unit test ran under it;
  the session's defaults were the tighter pair.
- A per-workspace uid/gid setting was not added: with wide bits nothing
  needs the numbers to match, and a setting nobody needs is a setting to
  document.

## What it costs

- `ls` in a container shows every file writable by everyone. The boundary
  is the tunnel and the account (ADR 0003, ADR 0029), not the mode bits;
  a read-only bind (`ro`) is still enforced by the mount.
- A file this machine cannot write is reported writable; the write fails on
  the host and go-nfs reports it as EACCES.

Covered by `integration.sh` 15f: the reported triple, a uid-1000 mkdir on a
plain mount and at the top and inside a union.

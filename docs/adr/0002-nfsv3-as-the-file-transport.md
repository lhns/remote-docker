# 0002. NFSv3 as the file transport

- Status: Accepted
- Date: 2026-08-07 (retrospective; decided during the original build)

## Context

ADR 0001 requires a real remote filesystem mounted where the remote daemon can
see it. The workload is a source tree, so the bottleneck is round-trips ×
metadata operations, not throughput. A `git status` over the share is the
number that matters; `dd` is not.

| option | verdict |
|---|---|
| **NFSv3** | the Linux client is `nfs.ko`: page cache, readahead, dentry and attribute caching, no userspace round-trip per `stat()` |
| SFTP | chatty but pipelined; means SSH inside SSH, so double encryption |
| WebDAV | worst — one HTTP request per operation, `PROPFIND` per listing, and davfs2 is painful with many small files |
| 9p | no maintained Windows server; only genuinely good as virtio-9p inside a hypervisor |
| native SMB3 | would likely beat all of these — kernel implementations at both ends, SMB3 compounding — but requires Windows File and Printer Sharing, which policy may block |

## Decision

Serve the client's files over **NFSv3** and mount them with the kernel NFS
client inside the workspace container.

The deciding factor is not the protocol, which is unremarkable. It is the
*client*: NFS is the only option here whose Linux client is in the kernel.
Every other protocol available to us forces FUSE on the Linux side, and a FUSE
round-trip per `stat()` is exactly the cost a source tree cannot absorb.

## Consequences

- Everything runs inside the SSH tunnel (ADR 0003), so the NFS transport is
  plaintext and there is no double encryption. `aes128-gcm@openssh.com` is
  chosen for the tunnel because AES-NI makes it several GB/s where ChaCha20 is
  markedly slower.
- Mount options are load-bearing and are documented where they are set:
  `soft` with a short `timeo` so a dead tunnel fails I/O with `EIO` instead of
  parking container processes in uninterruptible sleep; `nolock` because the
  server implements no NLM; `port` equal to `mountport` to skip rpcbind.
- **No `inotify` over NFS.** Hot-reloaders need polling. This is inherent to
  the protocol, not a configuration mistake.
- **No databases on the share.** `nolock` plus `fcntl` locking is a corruption
  risk.
- Build artifacts — `node_modules`, `.git`, `target/`, coursier and ivy caches
  — must be kept off the share. This is worth roughly 20×, where protocol
  tuning is worth roughly 2×.
- SMB3 remains the option to revisit if the policy constraint ever lifts.

# Design brief

Where this came from, what was considered, and why the result looks like it
does. The README covers *how to use it*; this covers *why it is shaped this
way*, so the next person (or the next session) does not re-litigate settled
questions.

## Problem

Docker is needed on a Windows machine where Docker cannot be installed. A
remote Docker host solves the daemon problem but breaks volume mounts: a
`DOCKER_HOST=ssh://` client resolves bind-mount paths **client-side**, so
`./src:/app` becomes `C:\projects\myapp\src` and gets shipped to a daemon
where that path does not exist. (This is docker/compose#8484.)

Requirement, stated explicitly: a *real remote filesystem* — WebDAV, NFS,
CIFS, or 9p — not a copy or a sync.

## Options considered

### Copy into a volume

`copy-to-volume.sh` — create a named volume, `docker cp` into it through a
throwaway `FROM scratch` container. Zero dependencies, works anywhere, and is
the right tool for shipping build inputs. Useless for live editing. Rejected
as the primary mechanism, still worth keeping around.

### Volume plugins

Docker's built-in `local` driver wraps `mount(8)` and needs no plugin for
CIFS or NFS (`--opt type=cifs|nfs`). The rclone volume plugin covers
everything else. Both were rejected once it became clear the mount could
simply live on the host filesystem: a plain bind mount needs no driver at
all, and every tool that shells out to Docker — Compose, Testcontainers,
act/Gitea runners, sbt plugins — then works unmodified.

Worth recording: there is no "rclone protocol". The rclone plugin *is* rclone
running on the Docker host, speaking whatever standard backend is configured.
Its manifest declares `"network": {"type": "host"}`, so it *can* reach a
loopback-forwarded port — that path would have worked, it is just more moving
parts than a bind mount.

### Which direction the filesystem is served

Two topologies:

- **Windows serves, Linux mounts.** Chosen. Needs nothing installed on
  Windows beyond a portable binary.
- **Linux serves, Windows mounts** (Samba container, mapped drive letter).
  Genuinely good — no inbound connections to the Windows box, containers get
  local-disk performance — but it inverts where the files live, which was not
  what was wanted here.

### Protocol

Ranked for a source tree, where the bottleneck is round-trips × metadata
operations, not throughput:

| option | verdict |
|---|---|
| NFSv3 (`rclone serve nfs`) | **chosen** — the client is `nfs.ko`: page cache, readahead, dentry/attribute caching, no FUSE round-trip per `stat()` |
| SFTP | chatty but pipelined; means SSH-inside-SSH, so double encryption |
| WebDAV | worst — one HTTP request per op, `PROPFIND` per listing, davfs2 is painful with many small files |
| 9p | no maintained Windows server; only genuinely good as virtio-9p inside a hypervisor |
| native SMB3 | would likely beat all of the above (kernel implementations both ends, SMB3 compounding) but needs Windows File and Printer Sharing enabled, which policy may block. rclone cannot serve SMB. |

Everything runs inside the SSH tunnel, so the transport is plaintext — no
double encryption. `-c aes128-gcm@openssh.com` because AES-NI makes it several
GB/s where ChaCha20 is markedly slower.

## Architecture

```
Windows                                   workspace container (privileged)
rclone.exe serve nfs .  --(ssh -R)-->     mount -t nfs 127.0.0.1:<port>
                                              -> ~/workspace
                                          dockerd (dind)
```

Three decisions carry the whole thing:

1. **Docker-in-Docker, not a proxied host socket.** A proxied socket resolves
   bind mounts on the host, where the files are not. Its own dockerd shares a
   mount namespace with the NFS mount, so paths resolve identically.

2. **Reverse tunnel, not inbound.** The workspace never connects to the
   Windows machine. No firewall rule, no inbound exposure; rclone binds
   loopback only.

3. **Port derived from uid** (`30000 + uid - 10000`). No coordination between
   users, no collisions, and — the part that turned out to matter — the port
   is *stable*, so a dropped tunnel does not require a remount.

## Requirements as stated, and how each was met

| asked for | done |
|---|---|
| image with ssh, rclone, dind | `image/Dockerfile` |
| read pubkeys from a directory | `image/bin/key-watcher`, inotify + 60s poll |
| a user per pubkey | filename becomes the account; uid persisted in `uidmap` |
| PowerShell + shell client | `client/dockerbox.ps1`, `client/dockerbox.sh` |
| generate a keypair per connection | `dockerbox enroll` |
| establish the tunnel | `-R`, port learned from `workspace-info` |
| mount the current local dir | `rclone serve nfs $PWD` + `workspace-mount` |
| proxy docker commands | `dockerbox docker ...` (and `shell` for the real thing) |
| Swarm deployment | `deploy/swarm-launcher.yml`, per the supplied reference |

## Decisions made without being asked

- **Shared dockerd across users.** All enrolled users see each other's
  containers. Isolation means one workspace container per user, one Swarm
  service each. Fine for a small trusted group; revisit otherwise.
- **Revoke, don't delete.** Removing a `.pub` empties `authorized_keys` but
  keeps the account and home directory. Auto-deleting users is a silent way
  to lose data.
- **Enrollment is out-of-band.** Someone with access drops the `.pub` into
  the directory. A self-service enrollment endpoint was considered and
  rejected as scope.
- **`fuse-overlayfs` baked into the image**, driven by
  `WORKSPACE_DOCKERD_ARGS`, rather than the entrypoint-injection hack the
  reference Swarm config needed.

## Corrected mid-build

The initial design claimed `bind-propagation=rslave` would let running
containers survive a reconnect. Testing showed it does not: propagation
carries mounts made *inside* the workspace, but a new mount placed *at* the
workspace belongs to the parent's peer group, which the container never had.
The container silently keeps the old, empty mount.

The fix was to stop needing it — `workspace-mount` is idempotent, and the
stable uid-derived port means a tunnel blip reconnects rather than remounts.
Both behaviours are now pinned by `test/propagation.sh`.

## Known limitations

- No `inotify` over NFS; hot-reloaders need polling.
- Ownership is fixed at uid 1000 — rclone's `--uid`/`--gid`/`--umask` are
  unsupported on Windows. Worked around with `--file-perms 0666
  --dir-perms 0777`; `chown` in a container will still fail. `bindfs` is the
  escape hatch if real ownership is ever needed.
- Build artifacts (`node_modules`, `.git`, `target/`, coursier/ivy caches)
  must stay off the share. Worth ~20×; protocol tuning is worth ~2×.
- No databases on the share — `nolock` plus `fcntl` locking is a corruption
  risk.

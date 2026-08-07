# docker-ssh-workspace

Use Docker from a Windows machine that cannot have Docker installed, with your
local working directory really mounted — not copied, not synced.

```
  Windows (nothing installed)                Workspace container (privileged)
  ---------------------------                --------------------------------
  rclone.exe serve nfs .                     sshd :2222
        |                                          |
        | 127.0.0.1:54321                          | mount -t nfs 127.0.0.1:30000
        |                                          |    -> ~/workspace
        +---- ssh -R 30000:127.0.0.1:54321 ------> |
                                                   | dockerd (dind)
                                                   |    bind mounts of
                                                   |    ~/workspace/... just work
```

The reason this works with no path translation and no volume plugin: the NFS
mount and `dockerd` live in the **same mount namespace**. When a container asks
for `-v $PWD/src:/app`, dockerd resolves it against the same `/home/you/workspace`
the mount is at. That is the whole trick, and it is why the workspace runs
Docker-in-Docker rather than proxying the host's socket — a proxied socket would
resolve bind mounts on the *host*, where your files are not.

## Why NFS and not WebDAV/SFTP

For a source tree the bottleneck is round-trips × metadata operations, not MB/s.
NFSv3 wins here not because the protocol is good but because the *client* is
`nfs.ko`: page cache, readahead, dentry and attribute caching, no userspace
round-trip per `stat()`. Every other protocol rclone can serve forces you into
FUSE on the Linux side. Both ends already sit inside an SSH tunnel, so the
transport is plaintext NFS — no double encryption.

## Server side

### Plain Docker

```bash
cd deploy
mkdir -p authorized_keys.d state
cp /path/to/alice.pub authorized_keys.d/alice.pub    # filename == unix account
docker compose up -d --build
```

### Docker Swarm

Swarm cannot run privileged tasks, so `deploy/swarm-launcher.yml` uses
`swarm-launcher` to start the container through the node's `docker.sock`, with
`LAUNCH_NETWORK_MODE: container:{{.Task.Name}}` so the Swarm service publishes
2222 on its behalf.

### How accounts work

`key-watcher` provisions one unix account per `*.pub` file in
`/etc/workspace/authorized_keys.d`:

| file        | account | uid   | reverse-tunnel port |
|-------------|---------|-------|---------------------|
| `alice.pub` | `alice` | 10000 | 30000               |
| `bob.pub`   | `bob`   | 10001 | 30001               |

The uid is allocated once and persisted in `state/uidmap`, so it survives
container recreation — and with it the port assignment and file ownership.
Deriving the port from the uid means no coordination and no port collisions
between concurrent users.

Removing a `.pub` file revokes access but **keeps the account and its home
directory**; deleting users automatically would be a silent way to lose data.

The watcher uses inotify *and* polls every 60s, because inotify never fires for
changes made on another host when the keys directory is on CephFS/NFS/SMB.

Keys are installed with `restrict,pty,port-forwarding` — everything denied
except a pty and forwarding, which is all the client needs.

## Client side

Nothing to install. `ssh.exe` and `ssh-keygen.exe` ship with Windows 10 1809+;
`dockerbox.ps1` downloads portable `rclone.exe` into `%LOCALAPPDATA%\dockerbox`
on first use.

```powershell
$env:DOCKERBOX_HOST = 'dockerbox.lan'
.\dockerbox.ps1 enroll          # prints your public key -> send it to the admin

cd C:\projects\myapp
.\dockerbox.ps1 shell           # mounts $PWD, drops you in a shell there
.\dockerbox.ps1 docker compose up -d
.\dockerbox.ps1 shell -Forward 8080:127.0.0.1:8080   # reach the app from Windows
```

`dockerbox.sh` is the identical POSIX client for Linux/macOS/WSL.

Config resolution: parameters → `$DOCKERBOX_HOST` / `$DOCKERBOX_SSH_PORT` /
`$DOCKERBOX_USER` → `~/.dockerbox.json`.

| command  | what it does |
|----------|--------------|
| `shell`  | mount `$PWD`, open a shell in it, unmount on exit |
| `docker` | run one docker command in the workspace |
| `run`    | run any command in the workspace |
| `mount`  | start a background session and leave it up |
| `umount` | tear the background session down |
| `status` | remote + local session state |
| `enroll` | print the public key to be enrolled |

`mount` once and subsequent `docker`/`run` calls reuse the live tunnel, which
matters because **Win32-OpenSSH does not support `ControlMaster`** — every
invocation on Windows is a fresh SSH handshake. The POSIX client enables
connection multiplexing where the platform supports it.

## Things that will bite you

**No inotify.** File watching does not work over NFS. Every hot-reloader needs
polling: `CHOKIDAR_USEPOLLING=1`, `--poll`, `spring.devtools` polling, and so on.

**Ownership is fixed at uid 1000.** rclone's `--uid`/`--gid`/`--umask` are
explicitly unsupported on Windows, so files always arrive as uid 1000 whatever
your container account is. The client compensates with `--file-perms 0666
--dir-perms 0777`, which makes them usable by any uid, but `chown` inside a
container will fail. If you need real ownership, layer `bindfs` over the mount.

**Keep build artifacts off the share.** `node_modules`, `.git`, `target/`, the
coursier/ivy caches — put those in a plain named volume on the workspace. This
is worth roughly 20×; protocol tuning is worth about 2×.

**No databases on the share.** SQLite and anything using `fcntl` locking over
NFS with `nolock` ranges from slow to corrupting.

**Remounting orphans running containers.** This one is counter-intuitive and I
tested it rather than assuming (`test/propagation.sh`): marking the workspace
`rshared` and binding it with `bind-propagation=rslave` does **not** make a
running container follow a *replacement* of the workspace mount. Propagation
carries mounts made *inside* the workspace; a new mount placed *at* the
workspace belongs to the parent's peer group, which the container never had.
The container silently keeps the old, now-empty mount.

Measured, with a tmpfs standing in for the NFS mount:

```
  rslave container, before remount   tick=2 generation-1
  rslave container, after remount    tick=7 generation-1   <-- did NOT follow
  rprivate container, after remount  tick=7 generation-1
```

The design works around this rather than fighting it: because the reverse-tunnel
port is derived from the uid and therefore stable, a dropped tunnel does **not**
need a remount — NFSv3/TCP reconnects to the same endpoint and the mount keeps
working. `workspace-mount` is idempotent for exactly this reason, and warns
when you force a real remount:

```
sudo workspace-mount           # no-op if already mounted
sudo workspace-mount --force   # replaces it; restart your containers
```

Starting a *new* client session does force a remount, because a fresh rclone
process generates fresh NFS file handles and the old ones are stale. So:
tunnel blip is free, `dockerbox umount` + `dockerbox shell` means restart your
containers.

The `rshared` marking is still done, and is still worth using, for mounts made
inside the workspace:

```yaml
    volumes:
      - type: bind
        source: /home/alice/workspace/src
        target: /app
        bind:
          propagation: rslave
```

**The graph driver.** `/var/lib/docker` on Ceph/NFS-backed storage needs
`WORKSPACE_DOCKERD_ARGS=--storage-driver=fuse-overlayfs`; overlay2 refuses and
vfs copies every layer. Local disk is much better if you have it.

**Shared daemon.** All enrolled users share one dockerd, so they can see each
other's containers. For real isolation, run one workspace container per user
(one Swarm service each, different published ports).

## Verification status

Run as root on a machine with a working Docker daemon:

```bash
bash test/key-watcher.sh     # 35 assertions
bash test/propagation.sh     # 10 assertions (needs the test/ image built first)
```

Verified: account provisioning, uid allocation and stability across
recreation, port derivation, key rotation and revocation, rejection of
malformed key files and hostile filenames, sudo helper argument validation,
and both mount-propagation behaviours above.

**Not** verified end-to-end, because the sandbox this was built in had no
container registry access, no `nfsd`, and no `nfs` client module: the actual
image build, `rclone serve nfs` on Windows, and the NFS mount itself. The
mount option string passed `mount.nfs`'s userspace validation but could not
be exercised against a live server. Treat the first real deployment as a
smoke test, in this order: image builds → sshd accepts your key →
`workspace-info` returns a port → tunnel establishes → mount succeeds.

## Layout

```
image/          Dockerfile, sshd config, entrypoint, key-watcher, mount helpers
deploy/         docker-compose.yml, swarm-launcher.yml
client/         dockerbox.ps1, dockerbox.sh
test/           key-watcher.sh, propagation.sh (run as root on a docker host)
```

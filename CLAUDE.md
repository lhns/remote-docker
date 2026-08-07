# CLAUDE.md

Project context for Claude Code. Read `DESIGN.md` for why the architecture is
what it is; read `README.md` for usage.

## What this is

A privileged Docker-in-Docker container you SSH into, which mounts the
client's current directory over a reverse SSH tunnel (`rclone serve nfs` on
the client, kernel NFS mount in the container). Because dockerd and the mount
share a namespace, bind mounts in compose files resolve with no translation.

## Layout

```
image/      Dockerfile, sshd_config, sudoers, entrypoint, key-watcher, mount helpers
deploy/     docker-compose.yml (plain), swarm-launcher.yml (Swarm, privileged via launcher)
client/     dockerbox.ps1 (Windows), dockerbox.sh (POSIX)
test/       key-watcher.sh, propagation.sh, watchdir.go + Dockerfile for the test image
```

## Build and test

```bash
# image
docker build -t docker-ssh-workspace:latest image/

# test image used by propagation.sh (stdlib-only Go, FROM scratch)
cd test && CGO_ENABLED=0 go build -ldflags="-s -w" -o watchdir ./watchdir.go \
    && docker build -t proptest:latest .

# suites -- must run as root on a host with a working docker daemon
bash test/key-watcher.sh     # 35 assertions, creates and removes real unix users
bash test/propagation.sh     # 10 assertions, creates real mounts

# lint
shellcheck -S warning -s sh image/bin/*
shellcheck -S warning -s bash client/dockerbox.sh test/*.sh
visudo -cf image/sudoers.workspace
```

Both suites are currently green. `shellcheck -S warning` is clean; keep it
that way.

## Invariants — break these and things fail quietly

- **The uid→port formula appears in two places**: `workspace-info` and
  `workspace-mount`. Both source `/etc/workspace/config`, written once by
  `entrypoint.sh`. If they ever disagree, the client tunnels to one port and
  the mount reads another, and the failure looks like a network problem.
- **`sudoers.workspace` pins argument forms** (`workspace-mount ""` and
  `workspace-mount --force`). A bare command entry in sudoers permits *any*
  arguments — that is the usual way these rules go wrong. Adding a new flag
  means adding a new sudoers line.
- **Mount helpers derive everything from `$SUDO_USER`, never from
  arguments.** This is what stops one user mounting into another's home or
  taking another's port. Do not add a path or user argument.
- **`workspace-mount` is idempotent on purpose.** See DESIGN.md — a remount
  is invisible to running containers, so the design avoids remounting rather
  than trying to propagate it. `--force` must keep its warning.
- **`key-watcher` polls as well as using inotify.** The keys directory is
  expected to be on CephFS/NFS, where inotify never fires for changes made on
  another host.
- **Accounts use `usermod -p '*'`, not a locked (`!`) password.** Some sshd
  builds refuse public-key auth for locked accounts.
- **The POSIX client may use `ControlMaster`; the PowerShell client may
  not.** Win32-OpenSSH does not implement it. Do not "simplify" the Windows
  client by adding multiplexing.

## Not yet verified

Built in a sandbox with no container registry, no `nfsd`, and no `nfs` client
module, so these are untested end-to-end:

- the image actually building (`FROM docker:28-dind` could not be pulled)
- `rclone serve nfs` on Windows
- the NFS mount itself — the option string passed `mount.nfs` userspace
  validation but was never exercised against a live server
- `dockerbox.ps1` has never been executed; no PowerShell available in the
  sandbox

First-deployment smoke order: image builds → sshd accepts a key →
`workspace-info` returns a port → tunnel establishes → mount succeeds →
`docker run -v ~/workspace/x:/x` sees the file.

## Likely next work

- Smoke-test on the real Swarm, fix whatever the first run turns up
- Decide on shared vs per-user dockerd (currently shared; users can see each
  other's containers)
- Consider a Traefik TCP entrypoint in front of 2222 rather than a published
  host port
- Benchmark: `time git status` and `time find` over the mount are the numbers
  that matter, not `dd`

## Conventions

- POSIX `sh` for anything in `image/bin/` (the image is Alpine); bash is fine
  in `client/` and `test/`.
- Comments explain *why*, not *what* — several of them encode findings that
  cost real debugging (propagation, locked accounts, ControlMaster, rclone's
  Windows uid limitation). Do not strip them.

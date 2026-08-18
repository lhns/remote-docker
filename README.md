# remote-docker

[![build](https://github.com/lhns/remote-docker/actions/workflows/release.yml/badge.svg)](https://github.com/lhns/remote-docker/actions/workflows/release.yml)
[![integration](https://github.com/lhns/remote-docker/actions/workflows/integration.yml/badge.svg)](https://github.com/lhns/remote-docker/actions/workflows/integration.yml)

Use Docker from a machine that cannot have Docker installed, with your own
directories **really mounted** into the containers, not copied or synced, and
published ports reachable locally.

One binary. Nothing else to install: the SSH client, the NFS server and the
Docker CLI are all inside it.

```
docker run --rm -v D:\data:/data alpine ls /data     # a directory on YOUR machine
docker compose up -d                                  # ports land on YOUR localhost
```

## Quick start

Take the archive for your platform from
[the latest release](https://github.com/lhns/remote-docker/releases/latest) and
unpack the single binary out of it. There is nothing else in it.

You also need a workspace to connect to. If nobody has set one up yet, see
[Running a workspace](#running-a-workspace).

```bash
# 1. say where the workspace is
export REMOTE_DOCKER_HOST=workspace.example

# 2. print your public key and hand it to whoever runs the workspace
remote-docker remote enroll

# 3. once your key is enrolled, start a session
remote-docker remote start

# 4. and then it is just docker
remote-docker run --rm -v .:/w alpine ls /w
```

`start` prints the endpoint and returns. No terminal has to stay open.

**On Android, in Termux**, take the `android_arm64` archive, or `android_amd64`
on an emulator or a Chromebook. It is a normal Termux program from there: run it
from a directory you can execute, and everything above works the same. Do not
use the `linux_arm64` archive, which will not load on a phone at all
([ADR 0023](docs/adr/0023-running-where-the-loader-is-not-us.md)).

**If you already have a docker CLI**, point it at the workspace:

```bash
docker --context <workspace> ps          # the context is created with the workspace
```

**If you do not**, this binary is one. Rename it:

```bash
mv remote-docker docker                  # docker.exe on Windows
docker run --rm -v .:/w alpine ls /w
```

That is the whole installation. The Docker CLI is this program's root command,
so the file's name is the only thing that decides how you spell it, and there
is nothing to put on PATH, keep in step, or uninstall. A standalone docker CLI
does exist if you want one (`winget install Docker.DockerCLI` on Windows, or
the static zip from download.docker.com), but it is a second thing to install
and update.

`docker compose` and `docker build` (BuildKit, through buildx) are included, so
the whole toolchain is one file.

## A workspace on this machine (Windows)

When there is no Linux host to point at, this builds one locally and registers
it as an ordinary workspace. It needs WSL, which Windows installs itself
(`wsl --install`, then reboot).

```powershell
remote-docker remote machine create dev
remote-docker run --rm -v .:/w alpine ls /w
```

`create` pulls the workspace image — the same one the container deployment runs,
built and tested on every push — and flattens it into the machine's filesystem.
It is kept, by digest, so a second machine or a `rebuild` costs nothing.
`--rootfs <file>` builds from a file you supply instead, for an air-gapped
machine or an image of your own.

What comes out is a workspace like any other: `remote ls` lists it, the docker
context is created for it, bind mounts and published ports work exactly as they
do against a host in another country. `remote machine start` and `stop` are its
lifecycle, and `remote rm dev` removes the workspace **and** the machine.

Nothing is installed into it and no package manager ever runs. Changing
versions replaces the filesystem rather than upgrading it, which is why
`remote machine rebuild` is the repair path: it is this same command run again.
It discards the images and containers inside the machine, never your files —
those live here and are served to it.

A Hyper-V backend exists (`--backend hyperv`, from a Flatcar disk image) and
**has never been run by anybody**; see `docs/testing-machines.md` if you have
Hyper-V and are willing to be the first.

## What works

- **Bind mounts from anywhere on your machine.** Another drive, above the
  working directory, unrelated to it. Not only a synced project folder.
- **Published ports reach your localhost.** `-p 8080:80` means
  `localhost:8080` here, opened automatically as containers start. The number
  is yours alone: the workspace publishes on a port of its own choosing, so two
  people sharing a workspace can both ask for 8080. On the workspace itself,
  `docker ps` shows the port it picked rather than the one you typed
  ([ADR 0037](docs/adr/0037-the-published-port-belongs-to-the-client.md)).
- **The real tooling, unmodified.** `docker`, `docker compose`,
  Testcontainers, IDE plugins, anything that speaks the Docker API. The
  translation happens at the API, not in a command wrapper. The Docker CLI,
  buildx and Compose are all inside this binary, so a machine with nothing
  installed still gets `docker compose up`.
- **Named volumes stay named volumes.** Only host paths are rewritten.
- **File watchers can see your edits**, once `REMOTE_DOCKER_WATCH` is on. See
  [File watching](#file-watching); it is off by default and worth
  understanding before you rely on it.

Every one of those is asserted end to end on each push, against a real
Docker-in-Docker daemon and a real kernel NFS mount. See
[`test/integration.sh`](test/integration.sh).

## How it works

```
YOUR MACHINE                                 WORKSPACE (privileged dind)
─────────────────────────────                ───────────────────────────
docker / compose / IDE
        │ DOCKER_HOST
        ▼
┌──────────────────────┐
│  background session  │═════ ONE SSH CONNECTION ═════▶ sshd
│                      │
│  • API proxy         │──── dial-stdio ─────────────▶ dockerd
│  • NFS server        │◀─── reverse forward ─────────  mounts NFS from
│  • port forwards     │◀─── local forwards ──────────  127.0.0.1:<your port>
└──────────────────────┘
        ▲
   your actual files
```

Your machine is the **file server**; the workspace is the client. The proxy
rewrites each bind mount into an NFS-backed Docker volume, which the remote
daemon mounts for itself when the container starts. Nothing has to propagate
into a running container, and a bind source anywhere on your disk works.

The endpoint is a **named pipe** on Windows (`\\.\pipe\docker_remote`) and a
unix socket elsewhere, owner-only in both cases. Never a TCP port: anything
that can reach it can start containers that read and write your filesystem.
Nothing here needs administrator rights.

The reasoning behind each decision is in [`docs/adr/`](docs/adr/), and what
this trusts, what it does not, and where the checks are is in
[`docs/threat-model.md`](docs/threat-model.md).

## One account from two machines

An account is the identity and a machine is a client, so a laptop and a desktop
enrolled with a key each share a workspace: the same daemon, the same images,
the same containers. Files are not shared, because they are on one machine or
the other, and neither are published ports: each machine opens the number its
own containers asked for, and sees the other machine's containers at whatever
the workspace published them on.

**One thing does collide, and it is worth knowing before it bites: compose
projects.** Compose names a project after the directory it runs in, so the same
compose file on both machines is one project on the daemon they share, with one
set of container names and one network. What happens next depends on where the
project lives:

- if the paths differ, each machine sees the other's containers as out of date
  and recreates them, so an `up` on one stops the service the other is running;
- if the paths match, the second machine reports everything up to date and
  leaves the first machine's containers running, **serving the first machine's
  files**.

Give each machine its own project name and neither happens:

```bash
export COMPOSE_PROJECT_NAME=demo-laptop     # demo-desktop on the other
docker compose up -d
```

This is a limitation rather than a design: the requirement is recorded in
[ADR 0029](docs/adr/0029-one-account-many-machines.md) and nothing enforces it
yet.

## Commands

**This binary is the Docker CLI.** `remote-docker run`, `remote-docker ps`,
`remote-docker compose up` are the real commands with their real flags, talking
to the workspace. Rename the file to `docker` and they are spelled the way they
are everywhere else, with no install step and nothing on PATH to manage.

Everything that is ours lives under `remote`:

| | |
|---|---|
| `remote-docker remote enroll` | print the public key to hand over for enrolment |
| `remote-docker remote start` | start a background session and return |
| `remote-docker remote start --foreground` | run it in this terminal instead |
| `remote-docker remote stop` | stop it |
| `remote-docker remote restart` | stop and start, refusing while something depends on it |
| `remote-docker remote status` | is it working, and what is it talking to |
| `remote-docker remote gc` | remove share volumes nothing is using |
| `remote-docker remote version` | |
| `remote-docker remote create <name> --host …` | add a workspace and its docker context |
| `remote-docker remote rm <name>` | remove both again |
| `remote-docker remote ls` | list them |
| `remote-docker remote use <name>` | make it the default here, and docker's current context |
| `remote-docker remote inspect [name]` | settings, endpoint, context, whether a session is up |

Any command that needs a session starts one, including the embedded CLI. For a
shell on the workspace, use `ssh`; the agent serves one to any enrolled key.

`workspace use` sets two things: the default in `~/.remote-docker.json`, which
only this binary reads, and `currentContext` in `~/.docker/config.json`, which
is what compose, buildx, Testcontainers and IDE plugins resolve. The second is
machine-wide, so it redirects those tools too. `--no-context` sets only ours,
matching `create --no-context`, and an exported `DOCKER_HOST` overrides both.

The context is created if it is missing, so this works on a machine that has
never had a docker CLI: the binary is one, and writes the context itself.

There is no `context` command. A docker context is written when a workspace is
created and removed when it is
([ADR 0018](docs/adr/0018-one-way-to-do-each-thing.md)). Re-run `workspace
create` to rewrite one that has drifted.

## Settings

Every setting can be an environment variable, a key in `~/.remote-docker.json`,
and for some a flag. Precedence, highest first: **flag, environment, file,
default.**

| environment | file key | flag | default |
|---|---|---|---|
| `REMOTE_DOCKER_HOST` | `host` | `--host` | none; required. A host, or `ssh://`, `ws://`, `wss://` with one |
| `REMOTE_DOCKER_PORT` | `port` | `--port` | `2222`, or the scheme's (443 for `wss`, 80 for `ws`). Optional |
| `REMOTE_DOCKER_CA_FILE` | `caFile` | `remote create --ca-file` | system roots |
| `REMOTE_DOCKER_INSECURE` | `insecure` | `remote create --insecure` | off |
| `REMOTE_DOCKER_USER` | `user` | `--user` | your local username |
| `REMOTE_DOCKER_ENDPOINT` | `endpoint` | `--endpoint` | `\\.\pipe\docker_remote`, or a socket in the state directory |
| `REMOTE_DOCKER_WORKSPACE` | (`default`) | `--workspace` | the file's default |
| `REMOTE_DOCKER_WATCH` | `watch` | `workspace create --watch` | `off` |
| `REMOTE_DOCKER_WATCH_BUDGET` | `watchBudget` | | 4096 Linux, 1024 Windows, 512 macOS |
| `REMOTE_DOCKER_WATCH_EXCLUDE` | `watchExclude` | | `.git`, `node_modules`, `.venv`, `__pycache__`, `.gradle`, `.terraform` |
| `REMOTE_DOCKER_IDLE_TIMEOUT` | `idleTimeout` | | `1m` before an unused connection is dropped |
| `REMOTE_DOCKER_DAEMON_IDLE` | `daemonIdle` | | `30m` before an unused session exits; negative never |
| `REMOTE_DOCKER_TRACE` | | | off; `1` logs one line per API request |
| `REMOTE_DOCKER_STATE_DIR` | | | keys, known_hosts, logs. `%APPDATA%\remote-docker`, `~/.config/remote-docker` |
| `REMOTE_DOCKER_SHIM_DIR` | | | `%LOCALAPPDATA%\remote-docker\bin`, `~/.local/bin` |

Durations are written the way you say them: `90s`, `45m`, `-1s` for never.

`REMOTE_DOCKER_TRACE` belongs to the **session**, which is the process that
forwards the requests, so set it there:
`REMOTE_DOCKER_TRACE=1 remote-docker remote start`. On a docker command it does
nothing, and says so.

### Several workspaces

```bash
remote-docker remote create dev --host dev.example --user alice --watch partial
remote-docker remote create ci  --host ci.example  --user alice
```

which writes `~/.remote-docker.json` and creates a docker context for each:

```json
{
  "workspaces": {
    "dev": {"host": "dev.example", "user": "alice", "watch": "partial"},
    "ci":  {"host": "ci.example",  "user": "alice"}
  },
  "default": "dev"
}
```

Any setting from the table can sit at the top level, where it applies to all
of them, or inside one workspace, where it applies to that one. Each workspace
gets its own endpoint, so sessions run side by side:

```bash
remote-docker remote start --workspace dev
docker --context dev ps
```

## File watching

A container watching a directory on the share receives **no inotify events**
when you change a file, because NFS carries no change-notification protocol.
Hot reload silently does nothing: vite, webpack, nodemon, `air` and
`dotnet watch` sit there while the file is plainly present.

Turning watching on makes remote-docker watch this machine and replay each
change inside the workspace as a real syscall, so the kernel there emits a
genuine inotify event:

```bash
export REMOTE_DOCKER_WATCH=partial    # writes and creations
export REMOTE_DOCKER_WATCH=coarse     # also deletions, approximately
```

| mode | what happens |
|---|---|
| `off` *(default)* | nothing is watched; hot reload does not work |
| `partial` | writes and creations fire real events. Deletions are not reported at all |
| `coarse` | as `partial`, plus a directory-level event for deletions and renames |

It is off by default because watching costs one inotify watch per directory,
and on macOS one file descriptor per file. Only the directories you share are
watched, never your whole disk, and there is a budget. What that costs, why
deletions are the honest gap, and what to do when the budget runs out are in
[Caveats](#file-watching-in-detail).

What is in this release, and what is still unproven:
[`CHANGELOG.md`](CHANGELOG.md).

## Running a workspace

The workspace runs one binary, `remote-dockerd`. It supervises dockerd,
provisions an account per enrolled key, and serves SSH itself. There is no
sshd, no sudo and no shell scripts in the image.

### Behind a reverse proxy

An open SSH port is often the thing that makes a workspace hard to reach. The
agent also serves SSH over a WebSocket, so any HTTP reverse proxy can front it:

```
--ws-addr :2280      the WebSocket listener; empty disables it
```

Both listeners run by default. Point the proxy at `:2280`, make sure it passes
WebSocket upgrades, and give the client the URL:

```
remote-docker remote create dev --host wss://dev.example.com --user alice
```

The tunnel is on the root, and the agent accepts an upgrade on **any path**, so
it does not matter whether the proxy strips its prefix. Put it under a path if
the proxy routes on one:

```
remote-docker remote create dev --host wss://example.com/rd --user alice
```

Opening the endpoint in a browser gets a short reply rather than a hang.

**The agent never terminates TLS.** It has no certificate options at all, so
there is nothing to renew and nothing to expire; the proxy does that. Serving
plaintext between the proxy and the agent is not the weakness it looks like,
because the same SSH handshake runs inside it: the host key still proves this is
the workspace and your key still proves which machine is calling.

For a proxy with a self-signed certificate, either point the client at your CA
with `--ca-file`, or use `--insecure` for that workspace. `--insecure` gives up
knowing which front door answered and nothing more — SSH inside still
authenticates both ends, which is why the flag exists here at all.

### On Kubernetes

```bash
helm install ws oci://ghcr.io/lhns/charts/remote-docker-workspace   --namespace remote-docker --create-namespace   --set ingress.host=ws.example.com   --set-file authorizedKeys.alice=$HOME/.ssh/id_ed25519.pub

kubectl label namespace remote-docker pod-security.kubernetes.io/enforce=privileged
```

One privileged pod with its image store and its host keys on volumes, reached
through an ordinary Ingress — no load balancer and no node port, because the
tunnel is an HTTP upgrade. Then `remote create dev --host wss://ws.example.com`
and a laptop with no Docker installed is working against it.

`charts/remote-docker-workspace/README.md` has the values and the two things
worth knowing first: which storage driver your volumes need, and why both
volumes are ReadWriteOnce. The chart is installed on a real cluster by CI on
every change (ADR 0035).

### The image

Published to GHCR for `linux/amd64` and `linux/arm64`:

```
ghcr.io/lhns/remote-docker-workspace:<version>   # on a v<version> tag
ghcr.io/lhns/remote-docker-workspace:latest      # on a v<version> tag
ghcr.io/lhns/remote-docker-workspace:sha-<short> # every commit to main
```

`latest` follows the most recent `v*` tag and exists from `v0.1.0` onwards.
*(Checked 2026-08-12 with `docker manifest inspect
ghcr.io/lhns/remote-docker-workspace:latest`.)* Pin `sha-<short>` to track main
between releases, or build it yourself, with the repository root as the context:

```bash
docker build -f image/Dockerfile -t remote-docker-workspace:latest .
```

### Plain Docker

```bash
cd deploy
mkdir -p authorized_keys.d state
cp /path/to/alice.pub authorized_keys.d/alice.pub   # filename == unix account
docker compose up -d --build
```

`state/` holds the host keys and the uid map and **must persist**. Losing it
gives every client a changed-host-key warning and reassigns every account's
uid, which changes its tunnel port and orphans the ownership of everything it
has written.

The container is privileged, because dind runs its own daemon, sets up its own
bridge and iptables rules, and mounts NFS in its own namespace.

### On a VM, with no container

The same binary, as a systemd service, when the workspace is a machine rather
than an image ([ADR 0025](docs/adr/0025-the-agent-as-a-guest.md)). Nothing
about the agent changes: it obeys the same switches, and the one that differs
is `WORKSPACE_ENABLE_DIND=false`, because the machine already has a dockerd and
a second would fight it for the socket.

```bash
tar xf remote-dockerd_<version>_linux_amd64.tar.gz
install -m 0755 remote-dockerd /usr/local/bin/
install -d -m 0700 /etc/workspace/authorized_keys.d /etc/workspace/host_keys
install -D -m 0600 remote-dockerd.env.example /etc/remote-docker/env
install -m 0644 remote-dockerd.service /etc/systemd/system/
systemctl enable --now remote-dockerd

cp /path/to/alice.pub /etc/workspace/authorized_keys.d/alice.pub
```

What the machine has to provide, which depends on the daemon mode:

| | a daemon per account (default) | one shared daemon |
|---|---|---|
| docker engine, CLI on `PATH` | yes | yes |
| `useradd` / `usermod` (shadow) | yes | yes |
| NFS client (`nfs-common`) | no | **yes** |

The last row is the one that catches people. With a daemon per account the NFS
mount happens inside `docker:dind`, which ships an NFS client; with one shared
daemon this machine mounts, and a missing client shows up as a container that
will not start, naming the volume rather than the package.

`/etc/workspace` must persist for the same reason `state/` does above: it holds
the host keys and the uid map.

Two things are worth knowing before running this on a machine that does other
work. Enrolled keys become **real users on that machine**, not disposable
container ones. And a per-account daemon is separation, not isolation
([ADR 0019](docs/adr/0019-a-dockerd-per-account.md)) — each runs privileged, so
an account that breaks out of one reaches the VM itself rather than a workspace
container somebody can recreate.

### Docker Swarm

Swarm cannot run privileged tasks, so the service starts **unprivileged** and
relaunches itself through the node's Docker socket
([ADR 0013](docs/adr/0013-self-elevation-instead-of-a-launcher.md)). No
launcher image is involved.

Two things must be true first, and neither fails in an obvious way:

```bash
# 1. Label the node. Without this the service is accepted and never schedules.
docker node update --label-add workspace=true <node>

# 2. Create the state directories ON THAT NODE. They are bind mounts, so a
#    missing path becomes an empty root-owned directory instead of an error.
export WORKSPACE_DATA=/var/lib/remote-docker
ssh <node> "mkdir -p $WORKSPACE_DATA/{state,authorized_keys.d}"

docker stack deploy -c deploy/swarm.yml workspace
```

Port 2222 is published with `mode: host`, so it lands on the node actually
running the task. That node's 2222 is what clients connect to.

The host Docker socket mount in `swarm.yml` is **the whole trust boundary**.
Whoever can deploy this stack can already start privileged containers on the
node. The socket is deliberately not passed to the privileged child.

### Workspace settings

| variable | default | |
|---|---|---|
| `WORKSPACE_STATE_DIR` | `/etc/workspace` | host keys, uid map, workspace id |
| `WORKSPACE_KEYS_DIR` | `<state>/authorized_keys.d` | one `<account>.pub` per user |
| `WORKSPACE_HOSTKEY_DIR` | `<state>/host_keys` | |
| `WORKSPACE_KEY_POLL_INTERVAL` | `60` | seconds; the keys directory is polled as well as watched |
| `WORKSPACE_DOCKERD_ARGS` | empty | passed to the workspace's own dockerd |
| `WORKSPACE_ENABLE_DIND` | `true` | |
| `WORKSPACE_PER_USER_DIND` | `true` | a daemon per account; `false` shares one |
| `WORKSPACE_DIND_IMAGE` | the workspace's own image | image a per-account daemon runs |
| `WORKSPACE_DIND_STORAGE_DRIVER` | inherited from `WORKSPACE_DOCKERD_ARGS` | |
| `WORKSPACE_DIND_MOUNTS` | empty | extra bind mounts for every per-account daemon; see below |
| `WORKSPACE_SHELL` | `/bin/bash` | shell an SSH session lands in |
| `WORKSPACE_UID_BASE` | `10000` | first uid handed to an account |
| `WORKSPACE_PORT_BASE` | `30000` | first reverse-tunnel port; uid decides the rest |
| `WORKSPACE_IMAGE` | | the service's own image, for Swarm elevation |
| `WORKSPACE_SELF` | | this task's name, set by `deploy/swarm.yml` |
| `WORKSPACE_DATA` | `/var/lib/remote-docker` | read by `deploy/swarm.yml`, not by the agent |

### A private or insecure registry

A workspace pulls images with its own daemon, and with a daemon per account
(the default) each account's daemon does its own pulling. Configuration you
give the workspace's daemon does not reach them, so a registry that works on
the workspace fails inside every account with `http: server gave HTTP response
to HTTPS client` or an unknown certificate authority.

Give them the same files:

```yaml
services:
  workspace:
    volumes:
      - /etc/docker/daemon.json:/etc/docker/daemon.json:ro
      - /etc/docker/certs.d:/etc/docker/certs.d:ro
    environment:
      WORKSPACE_DIND_MOUNTS: >-
        /etc/docker/daemon.json:/etc/docker/daemon.json:ro,
        /etc/docker/certs.d:/etc/docker/certs.d:ro
```

The first two lines are the workspace's own daemon, which you already needed.
`WORKSPACE_DIND_MOUNTS` passes the same paths on to each account's daemon, as
`source:destination` or `source:destination:ro`, comma-separated. Both paths
must be absolute: docker reads a relative source as a volume NAME, so it would
quietly mount an empty volume and the daemon would read no configuration at all.

Two things to know before you use it. A `daemon.json` that sets `storage-driver`
or `hosts` collides with the flags the agent passes, and dockerd refuses to
start saying so; keep those out of the file and use
`WORKSPACE_DIND_STORAGE_DRIVER` instead. And changing this setting applies to a
daemon that already exists only when that account has nothing running, because
applying it means recreating the container (its images and containers are on a
volume and are kept).

Operator commands, on the workspace:

| | |
|---|---|
| `remote-dockerd serve` | the agent; the image's default |
| `remote-dockerd elevate` | the Swarm entry point |
| `remote-dockerd healthcheck` | is this workspace serving? Both deployments use it |
| `remote-dockerd daemons ls` | which accounts have a daemon |
| `remote-dockerd daemons reset <account> [--purge]` | rebuild one; `--purge` discards its images |

### The storage driver, worth getting right once

If `WORKSPACE_DATA` is on Ceph- or NFS-backed storage, worth doing if the
workspace should survive moving nodes, you must set
`WORKSPACE_DOCKERD_ARGS=--storage-driver=fuse-overlayfs`. overlay2 refuses
such a filesystem outright, and vfs, the only other fallback, copies every
layer.

Per-account daemons inherit that setting, so this is the one place to set it.

**If it is set and anything it needs is missing, dockerd falls back to vfs
rather than failing.** vfs has no copy-on-write, so it copies the whole image
on every `docker create`. Nothing errors, `docker ps` stays instant, and
`docker run` takes minutes. The agent logs it and `remote-docker remote status` shows
it, because the cost of this one is entirely in how quiet it is.

fuse-overlayfs needs a **4.18 kernel or newer** with `CONFIG_FUSE_FS`
(`modprobe fuse` is enough), `/dev/fuse` in the container, and the
`fuse-overlayfs` binary **in the image the daemon runs**. Stock `docker:dind`
does not ship it; this workspace image does, which is why per-account daemons
default to the workspace's own image.

It is not the default because where overlay2 works it is the kernel doing the
work and is markedly faster. fuse-overlayfs is a userspace filesystem, so
every layer read crosses into a userspace process.

### Enrolment

Out of band: someone with access drops a `<name>.pub` into the keys directory,
and the filename becomes that user's unix account. Removing a key revokes
access but keeps the account and its home directory, because a key file is
removed far more often than a person leaves for good.

The keys directory is re-read on change and polled every 60 seconds, because
inotify never fires for a change made on another host when that directory is
on shared storage.

### A daemon per account

Each enrolled account gets its own Docker daemon behind the same single SSH
port ([ADR 0019](docs/adr/0019-a-dockerd-per-account.md)), which is the
default. Accounts stop seeing each other's containers, images and volumes, two
accounts can publish the same port at once, and a shell lands on its own
daemon.

**It is separation, not isolation.** Each per-account daemon runs privileged,
which is root on whatever hosts it, so a determined account can still break
out and reach another's. What this buys is that nobody sees anyone else's work
*by accident*, which is the failure that actually happens. A workspace is
still a shared machine. Genuine isolation is one workspace container per
account.

It costs real resources:

- **the layer cache is duplicated.** Five accounts on `node:22` is five
  copies. A registry mirror recovers bandwidth but not disk.
- **memory**, roughly 100-150MB per idle daemon plus containerd.
- **disk becomes a shared failure mode.** One account's runaway build can fill
  the volume and take down every other account's daemon.
- **3-10s** for an account's first connection after its daemon has stopped.

#### Changing settings later

A per-account daemon is created once and started thereafter, which is what
keeps an account's containers and images across a redeploy, so its image,
flags and mounts are fixed at creation.

The agent applies a changed configuration by itself: each daemon is stamped
with a digest of what it was built from, and one that no longer matches is
recreated. The container is disposable and the graph volume beside it is kept.
It waits until that account has nothing running, because recreating a daemon
stops its containers.

**The storage driver is the exception**, because a graph written by one driver
cannot be read by another. There is no recreation that keeps the data, so the
agent says so and leaves it. Deciding is a command:

```bash
docker exec <workspace> remote-dockerd daemons ls
docker exec <workspace> remote-dockerd daemons reset alice           # rebuild it
docker exec <workspace> remote-dockerd daemons reset --all --purge   # and discard images
```

`--purge` is the account's entire Docker state, and it is needed for exactly
that one case.

#### What persists

| | shared daemon | a daemon per account |
|---|---|---|
| host keys, uid map, workspace id | `/etc/workspace` | `/etc/workspace` |
| images, containers, named volumes | the workspace's `/var/lib/docker` | a named volume per account, `rd-dind-<account>-lib`, **inside** the workspace's `/var/lib/docker` |
| `rd-*` share volumes | the workspace's daemon | that account's daemon |
| the account's docker socket | n/a | `/run/rd/<account>/`, recreated on every start |

Both deployments in `deploy/` already persist `/var/lib/docker` and
`/etc/workspace`. Neither mode persists anything the other does not; a daemon
per account nests the same data one level deeper.

Three consequences of that nesting:

- **`rd-dind-<account>-lib` is the most valuable object in the deployment.**
  It is everything that account has. It is a *named* volume on purpose: the
  daemon container in front of it can be removed and recreated, and the
  account's images and containers come back with it.
- **`docker system prune -a --volumes` on the workspace's own daemon is
  destructive.** It removes stopped containers first and then unused volumes,
  so an idle account's daemon and then its storage go together.
  `docker volume ls --filter label=remote-docker.daemon` lists what must not
  be pruned.
- **`/etc/workspace/workspace-id` matters more than it looks.** It is how the
  agent recognises its own daemons after a redeploy. Lose it and the running
  daemons are orphaned: still running, still holding their users' work, no
  longer adopted.

`rd-*` share volumes are the exception and can be destroyed freely. They hold
no data, only a pointer to a directory on a client's machine, and
`remote-docker remote gc` removes the unused ones as a matter of routine.

#### Upgrading an existing workspace is a breaking change

Images and volumes an account built under the shared daemon are invisible from
its own, and there is no cheap migration. **Set
`WORKSPACE_PER_USER_DIND=false` before upgrading** if that matters. The old
data is still in the shared `/var/lib/docker` either way, so the decision is
reversible.

With it off, all enrolled users share one daemon and can see each other's
containers ([ADR 0012](docs/adr/0012-shared-dockerd-across-users.md)). That
stays supported rather than deprecated: a single-account workspace has nothing
to separate.

## Caveats

### What not to put on the share

- **Build artifacts.** `node_modules`, `.git`, `target/`, package caches.
  Keeping them off the share is worth roughly 20×; protocol tuning is worth
  about 2×.
- **Databases.** `nolock` plus `fcntl` locking is a corruption risk.
- **Very large trees over a WAN.** NFSv3 is synchronous per operation, so
  latency multiplies. Over a LAN it is fine.

### A session must be running

Not in a terminal you are watching, but running: it is the endpoint and the
file server, so stopping it takes running containers' mounts with it. Any
command that needs one starts it. A background session reclaims itself after
30 minutes with nothing to do, and never while a container of yours is running
or a stream is open.

### File watching in detail

**Deletions are the honest gap.** `unlink` of a name that is already gone
fails before the kernel generates anything, so a deletion cannot be replayed
faithfully. `coarse` approximates it with a directory-level event; `partial`
says nothing rather than something wrong. A watcher that rescans notices; one
that trusts the event *kind* is told something untrue.
[ADR 0014](docs/adr/0014-inotify-does-not-see-client-changes.md) stays open on
exactly this, and
[ADR 0016](docs/adr/0016-replaying-change-events-as-real-syscalls.md) records
how the rest works.

**Watching is not free.** inotify is not recursive: `inotify_add_watch` covers
one directory and reports only its direct entries, so a tree costs one watch
per directory. macOS is worse, because kqueue needs an open file descriptor
per *file*.

| platform | budget | what binds |
|---|---|---|
| Linux | 4096 directories | `fs.inotify.max_user_watches` (8192 by kernel default; many distros raise it) |
| Windows | 1024 directories | one `ReadDirectoryChangesW` buffer per watch |
| macOS | 512 directories | `RLIMIT_NOFILE`, because kqueue costs an fd per file |

Build outputs like `dist/` and `target/` are **not** excluded by default,
because serving `dist/` and reloading when it changes is exactly the workflow
this is for. So a Rust or Java tree will spend its budget inside `target/` and
say so. Tune with `REMOTE_DOCKER_WATCH_BUDGET` and
`REMOTE_DOCKER_WATCH_EXCLUDE` (comma- or `PATH`-separated). When the budget
runs out, the directory it stopped at is named rather than silently dropped.

Watching starts delivering once the session has connected, which happens on
the first Docker command. Edits made before that are counted and reported, not
silently lost.

The per-tool polling flags (`CHOKIDAR_USEPOLLING=1`, `WATCHPACK_POLLING=true`,
`--poll`) still work and are never set for you.

### Why a container takes a few seconds to start

Measured against a workspace whose `/var/lib/docker` is on CephFS, which is
the case this is worth knowing for.

| | |
|---|---|
| the tunnel | **~10ms per request.** An SSH channel opens in 2-3ms and a `/_ping` round trip is 8ms. Not where the time goes. |
| this binary starting | **~400ms per command.** ~210ms to load a 45MB binary and build the command tree, about as much again for the embedded Docker CLI. Once per command, not per request. |
| `docker create` | **~250ms**, the same measured on the workspace itself. Container creation is many small synchronous metadata writes. |
| `docker start` | **~800ms**, likewise the same locally. |

So a `docker run` is roughly 400ms of us, a second of the daemon, then the
container's own runtime. **The remote part is not the expensive part**; the
storage is. The same daemon on local disk does create and start in tens of
milliseconds. If that matters more than surviving a node move, put
`/var/lib/docker` on local disk.

### What is not tested

The integration suites run the Linux client against a real workspace on every
push. **macOS has never been executed at all**, in CI or anywhere else.
**Windows is unit tested**, including the named-pipe endpoint, but no Windows
machine has taken a session end to end, because the suite needs a Linux
kernel's NFS client. Swarm itself needs a real cluster and CI cannot cover it.
**Android is built and inspected, and CI runs nothing on it**: it checks that
the binary is loadable on a phone and links the system libc, which is what makes
DNS work there. A session and a container were confirmed by hand from Termux on
2026-08-14, on one arm64 device. `android_amd64` has never been executed.

## Prior art

Nothing else appears to do this. The pieces exist separately:
[docker-injector](https://github.com/rse/docker-injector) mutates
container-create requests, [sshocker](https://github.com/lima-vm/sshocker)
serves files from the client to a VM, but not the combination.

Everyone else answers this problem with **file sync**: Mutagen, Okteto,
Docker's own Synchronized File Shares. Testcontainers Cloud proxies a local
socket to a remote runtime and states that mounting local files "is not
implemented". VS Code and Codespaces document it as unsupported.
[docker/compose#8484](https://github.com/docker/compose/issues/8484) was
closed by a stale bot;
[podman#13358](https://github.com/containers/podman/issues/13358) was closed
"won't fix, far from trivial".

Sync is not obviously the wrong answer. It makes changes land as ordinary
local writes, so file watchers work, which is very likely the whole reason it
won.

remote-docker answers it differently, and not originally: Docker Desktop ships
the same idea as "Event Injection", forwarding host events into its VM so a
replay thread reproduces them. Linux offers no way to inject a synthetic
inotify event, `fanotify(7)` says so outright, so performing a real operation
is the only mechanism available to anyone. The difference here is that we own
the NFS server as well as the agent, which is what keeps the replay from
echoing back as a change of its own
([ADR 0016](docs/adr/0016-replaying-change-events-as-real-syscalls.md)).

## Project layout

Five Go modules in one repository ([ADR 0021](docs/adr/0021-three-modules.md),
[ADR 0031](docs/adr/0031-if-it-knows-about-docker-it-is-glue.md)). Three of them
are the core and know nothing about Docker; the two binaries are the glue that
does.

```
core/                  what both ends must agree on
  workspace/           the contract: paths, uid→port, volume names
  tunnel/              one bidirectional copy, one answer to half-closing
  logx/                one log handler, so both look the same
  probes/              helpers the integration suites run in containers

core-client/           this machine, minus Docker
  tunnelclient/        dialling the tunnel
  nfsserve/            the in-process NFSv3 server
  fswatch/             watching directories, on three platforms
  keys/                this machine's identity to a workspace

core-agent/            the workspace, minus Docker
  tunnelserver/        answering the tunnel
  accounts/            one unix account per enrolled key
  notify/              replaying changes as real syscalls
  netns/               running inside another process's netns

client/                the client binary (docker/cli, buildx)
  cmd/remote-docker/   also answers to `docker`
  internal/            api proxy, bind rewriting, ports, machines, session

agent/                 the agent binary (four direct dependencies)
  cmd/remote-dockerd/  the workspace binary
  internal/            per-account daemons, dockerd supervision, elevate

image/  deploy/        the workspace container and its deployments
test/                  the integration suites
docs/adr/              why everything is the way it is
```

## Development

The repository root is not a module, and `./...` stops at a module boundary, so
every command loops over the five.

```bash
for m in ./core ./core-client ./core-agent ./agent ./client; do (cd $m && go build ./... && go test ./...); done
for m in ./core ./core-client ./core-agent ./agent ./client; do (cd $m && golangci-lint run ./...); done
bash test/integration.sh      # needs docker and NFS client support
```

## License

Licensed under Apache 2.0.

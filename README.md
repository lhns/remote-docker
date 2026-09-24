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

Unpack the single binary from your platform's archive in
[the latest release](https://github.com/lhns/remote-docker/releases/latest).
You also need a workspace; if nobody has set one up, see
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

**On Android, in Termux**, take the `android_arm64` archive (`android_amd64` on
an emulator or a Chromebook) and run it from a directory you can execute. The
`linux_arm64` archive will not load on a phone
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

That is the whole installation: the Docker CLI is this program's root command
([ADR 0024](docs/adr/0024-the-docker-cli-is-the-root.md)), so the file's name
decides how you spell it. `docker compose` and `docker build` (BuildKit,
through buildx) are included. A standalone docker CLI also exists
(`winget install Docker.DockerCLI`, or the static zip from download.docker.com)
if you prefer one. *(Checked 2026-09-23 with
`winget show --id Docker.DockerCLI --exact`.)*

## Windows: an installer

Every release also carries an MSI per architecture, beside the zip:
`remote-docker_<version>_windows_amd64.msi` and `..._arm64.msi`. It installs
`remote-docker.exe` into `C:\Program Files\remote-docker` and **appends** that
directory to the system PATH. Per machine, so it needs administrator rights.
About 19 MB on amd64 and 18 on arm64, the same binary the zip holds.

```powershell
# with a UI, and a feature tree
msiexec /i remote-docker_0.6.0_windows_amd64.msi

# silently, remote-docker.exe only
msiexec /i remote-docker_0.6.0_windows_amd64.msi /qn

# silently, and also as docker.exe
msiexec /i remote-docker_0.6.0_windows_amd64.msi /qn ADDLOCAL=Main,DockerName

# uninstall
msiexec /x remote-docker_0.6.0_windows_amd64.msi /qn
```

**The `docker.exe` option is off by default.** Selected, it installs a second
copy of the same binary as `docker.exe`, so `docker run ...` on this machine is
this program. The copy is made at install and removed at uninstall, so it costs
nothing to download.

It does **not** take the name from anybody else. If a `docker.exe` is already
installed, the install stops and says where:

```
A docker.exe is already installed at C:\Program Files\Docker\docker.exe, and
the "docker" name would shadow it.
  fix: leave that feature unselected, or pass ALLOWDOCKERSHADOW=1 to install it anyway.
```

```powershell
msiexec /i remote-docker_0.6.0_windows_amd64.msi /qn ADDLOCAL=Main,DockerName ALLOWDOCKERSHADOW=1
```

The check reads four directories (the two system directories and Docker
Desktop's two), not PATH, which Windows Installer cannot enumerate
([ADR 0048](docs/adr/0048-a-windows-installer.md)). A `docker.exe` elsewhere on
PATH is not noticed, and it keeps winning, because the install directory is
appended.

**The MSI is unsigned** (the project has no code-signing certificate), so
SmartScreen warns about an unrecognised publisher. It is also **not** in the
release's `checksums.txt`, which is written before the installers are built. If
either matters, take the zip: the same binary, and in `checksums.txt`.

## A workspace on this machine (Windows)

With no Linux host to point at, this builds one locally and registers it as an
ordinary workspace ([ADR 0026](docs/adr/0026-a-machine-is-a-workspace-we-provision.md)).
It needs WSL (`wsl --install`, then reboot).

```powershell
remote-docker remote machine create dev
remote-docker run --rm -v .:/w alpine ls /w
```

`create` pulls the workspace image, the same one the container deployment runs,
and flattens it into the machine's filesystem. The image is kept by digest, so
a second machine or a `rebuild` downloads nothing. `--rootfs <file>` builds
from a file you supply instead, for an air-gapped machine or an image of your
own.

The result is a workspace like any other: `remote ls` lists it, it gets a docker
context, bind mounts and published ports work. `remote machine start`, `stop`
and `status` are its lifecycle, and `remote rm dev` removes the workspace
**and** the machine.

Changing versions replaces the filesystem rather than upgrading it, so
`remote machine rebuild` is the repair path. It discards the images and
containers inside the machine, never your files, which live here.

A Hyper-V backend exists (`--backend hyperv`, from a Flatcar disk image) and
**has never been run by anybody**; see `docs/testing-machines.md` if you have
Hyper-V and are willing to be the first.

## What works

- **Bind mounts from anywhere on your machine**: another drive, above the
  working directory, unrelated to it. **Single files work too**
  (`-v ./nginx.conf:/etc/nginx/nginx.conf`), sharing only that file
  ([ADR 0039](docs/adr/0039-a-single-file-is-a-one-file-export.md)).
- **Published ports reach your localhost.** `-p 8080:80` means
  `localhost:8080` here, opened as containers start. The workspace publishes
  on a port of its own choosing, so two people sharing a workspace can both
  ask for 8080, and `-p 8080:80 -p 9090:80` gives you both; `docker ps` on the
  workspace itself shows the port it picked
  ([ADR 0008](docs/adr/0008-published-ports-reach-the-client.md)).
  **UDP works too** ([ADR 0038](docs/adr/0038-udp-crosses-the-tunnel.md)), but
  datagrams travel inside the SSH stream, so a delayed one delays those behind
  it. Fine for DNS, syslog and metrics; not for measuring latency.
- **The real tooling, unmodified.** `docker`, `docker compose`,
  Testcontainers, IDE plugins, anything that speaks the Docker API. The
  translation happens at the API, not in a command wrapper.
- **Named volumes stay named volumes.** Only host paths are rewritten.
- **File watchers can see your edits**, once `REMOTE_DOCKER_WATCH` is on (off
  by default; see [File watching](#file-watching)).

Each of those is asserted end to end in CI against a real Docker-in-Docker
daemon and a real kernel NFS mount: [`test/integration.sh`](test/integration.sh).

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
daemon mounts for itself when the container starts
([ADR 0006](docs/adr/0006-per-bind-nfs-volumes.md)).

The endpoint is a **named pipe** on Windows (`\\.\pipe\docker_remote`) and a
unix socket elsewhere, owner-only in both cases, never a TCP port: anything
that can reach it can start containers that read and write your filesystem.
Nothing here needs administrator rights.

Decisions are in [`docs/adr/`](docs/adr/); what this trusts and where the checks
are is in [`docs/threat-model.md`](docs/threat-model.md).

## One account from two machines

A laptop and a desktop enrolled in one account with a key each share the
daemon, images and containers. Files are not shared, and neither are published
ports: each machine opens the number its own containers asked for, and sees the
other machine's containers at whatever port the workspace published them on.

**Compose projects collide.** Compose names a project after the directory it
runs in, so the same compose file on both machines is one project on the shared
daemon, with one set of container names and one network:

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

This is a limitation, not a design
([ADR 0029](docs/adr/0029-one-account-many-machines.md)).

## Commands

**This binary is the Docker CLI.** `remote-docker run`, `ps`, `compose up` are
the real commands with their real flags, talking to the workspace. Everything
that is ours lives under `remote`:

| | |
|---|---|
| `remote-docker remote enroll` | print the public key to hand over for enrolment |
| `remote-docker remote start` | start a background session and return |
| `remote-docker remote start --foreground` | run it in this terminal instead |
| `remote-docker remote stop` | stop it |
| `remote-docker remote restart` | stop and start |
| `remote-docker remote status` | is it working, and what is it talking to; exits 1 when not ready |
| `remote-docker remote gc` | remove share volumes nothing is using |
| `remote-docker remote version` | |
| `remote-docker remote create <name> --host …` | add a workspace and its docker context |
| `remote-docker remote rm <name>` | stop its session and remove both again |
| `remote-docker remote ls` | list them |
| `remote-docker remote use <name>` | make it the default here, and docker's current context |
| `remote-docker remote inspect [name]` | settings, endpoint, context, whether a session is up |
| `remote-docker remote machine …` | `create`, `rebuild`, `start`, `stop`, `status` a [local workspace](#a-workspace-on-this-machine-windows) |

A command about one workspace takes its name as an argument, else
`--workspace`, else the default. `stop`, `restart`, `machine stop`,
`machine rebuild` and `rm` refuse while the session is in use,
as `docker rm` refuses a running container; `-f` goes ahead.

Any command that needs a session starts one, including the embedded CLI. For a
shell on the workspace, use `ssh`; the agent serves one to any enrolled key.

`remote use` sets the default in `~/.remote-docker.json` **and**
`currentContext` in `~/.docker/config.json`, which compose, buildx,
Testcontainers and IDE plugins resolve, so it redirects those tools too.
`--no-context` sets only ours (as on `create`), and an exported `DOCKER_HOST`
overrides both. A missing context is created.

There is no `context` command: a docker context is written when a workspace is
created and removed with it
([ADR 0018](docs/adr/0018-one-way-to-do-each-thing.md)). Re-run
`remote create` to rewrite one that has drifted.

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
| `REMOTE_DOCKER_CONSISTENCY` | `consistency`, `consistencyPaths` | `remote create --consistency` | `read=direct,write=through`. See [Faster access to a shared directory](#faster-access-to-a-shared-directory) |
| `REMOTE_DOCKER_WATCH` | `watch` | `remote create --watch` | `off` |
| `REMOTE_DOCKER_WATCH_BUDGET` | `watchBudget` | | 4096 Linux, 1024 Windows, 512 macOS |
| `REMOTE_DOCKER_WATCH_EXCLUDE` | `watchExclude` | | `.git`, `node_modules`, `.venv`, `venv`, `__pycache__`, `.mypy_cache`, `.pytest_cache`, `.gradle`, `.terraform` |
| `REMOTE_DOCKER_CACHE_FILES` | `cacheFiles` | | 20000, files prefetch may copy into a union |
| `REMOTE_DOCKER_CACHE_BYTES` | `cacheBytes` | | 2 GiB, bytes prefetch may copy into a union |
| `REMOTE_DOCKER_PREFETCH` | `prefetch` | | `off`; `eager` or `tree` fills a `read=cached` union ahead of reads |
| `REMOTE_DOCKER_IDLE_TIMEOUT` | `idleTimeout` | | `1m` before an unused connection is dropped |
| `REMOTE_DOCKER_DAEMON_STANDBY` | `daemonStandby` | | `30m` before an unused session lets go of the workspace, keeping its endpoint |
| `REMOTE_DOCKER_DAEMON_IDLE` | `daemonIdle` | | how long before an unused session EXITS. Unset never does, because that takes the endpoint with it |
| `REMOTE_DOCKER_TRACE` | | | off; `1` logs one line per API request |
| `REMOTE_DOCKER_NFS_TRACE` | | | off; a threshold (`250ms`, or bare milliseconds, least `1ms`) above which a share's filesystem calls are logged |
| `REMOTE_DOCKER_NFS_FDCACHE` | | | `2s` that a file stays open after the request that used it; `0` closes it on every request |
| `REMOTE_DOCKER_NFS_NCONNECT` | | | off; `2`–`16` connections per share. **Needs Linux 5.3 on the workspace** and breaks every mount without it. See [More connections behind a share](#more-connections-behind-a-share) |
| `REMOTE_DOCKER_STATE_DIR` | | | keys, known_hosts, logs. `%APPDATA%\remote-docker`, `~/.config/remote-docker` on Linux, `~/Library/Application Support/remote-docker` on macOS |

Durations are written the way you say them: `90s`, `45m`, `-1s` for never.

`REMOTE_DOCKER_TRACE` and the `REMOTE_DOCKER_NFS_*` variables belong to the
**session**, so set them where it starts:
`REMOTE_DOCKER_TRACE=1 remote-docker remote start`. On a docker command
`REMOTE_DOCKER_TRACE` does nothing, and says so.

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
when you change a file, because NFS carries no change notification, so hot
reload (vite, webpack, nodemon, `air`, `dotnet watch`) silently does nothing.
Watching replays each change here inside the workspace as a real syscall, so
the kernel there emits a genuine inotify event
([ADR 0016](docs/adr/0016-replaying-change-events-as-real-syscalls.md)):

```bash
export REMOTE_DOCKER_WATCH=partial    # writes and creations
export REMOTE_DOCKER_WATCH=coarse     # also deletions, approximately
```

| mode | what happens |
|---|---|
| `off` *(default)* | nothing is watched; hot reload does not work |
| `partial` | writes and creations fire real events. Deletions are not reported at all |
| `coarse` | as `partial`, plus a directory-level event for deletions and renames |

It is off by default because it costs one watch per shared directory (on macOS
one file descriptor per file), within a budget. Costs, deletions and the budget
are in [File watching in detail](#file-watching-in-detail).

## Faster access to a shared directory

Reading through the share costs a round trip per file, and the mount
revalidates any attribute older than a second. The cost is latency, not
bandwidth: a 10 Mbit link costs almost nothing and 160ms RTT costs 400x
([ADR 0042](docs/adr/0042-mount-consistency-modes.md)). Per-mode measurements
are the table in [ADR 0045](docs/adr/0045-prefetch-follows-the-reads.md),
produced by `test/bench.sh` (the `bench` label on a pull request).

Docker's own mount consistency says a directory may be cached, and every client
already parses it:

```bash
docker run -v ./project:/app:ro,cached
docker run --mount type=bind,source=./project,target=/app,consistency=cached
#  compose:  volumes: [{type: bind, source: ./project, target: /app, consistency: cached}]
```

| `read=` | | `write=` | |
|---|---|---|---|
| `direct` *(default)* | revalidates every second | `through` *(default)* | on this machine as it happens |
| `cached` | trusts attributes for a minute | `back` | in a union on the workspace, carried back within seconds |
| | | `ephemeral` | in a union on the workspace, never carried back |

`read=cached` is the mount itself and works wherever a share works. Any
`write=` other than `through` (including Docker's `delegated`) makes a union,
which needs `fuse-overlayfs` in the image the account's daemon runs; a workspace
that cannot make one says so before anything is created.

```bash
docker run -v ./project:/app:read=cached img                       # the one to reach for
docker run -v ./project:/app:ro,read=cached img                    # beside ro, comma-joined
docker run -v ./target:/app/target:write=ephemeral img             # a build directory
docker run --mount 'type=bind,src=./project,dst=/app,"consistency=read=cached,write=back"' img
```

An axis a mount does not name comes from the per-directory rule
(`consistencyPaths`), then the workspace setting, then the default. The third
field of `-v` is a list: `ro,read=cached`, never `:read=cached:ro`. `--mount` is
split on commas by the CLI, so both axes go in one csv-quoted field. Whether
Compose accepts these words in `consistency:` is **unverified**. Docker's own
values keep their meaning: `consistent` and `default` are
`read=direct,write=through`, `cached` is `read=cached,write=through`,
`delegated` is `read=cached,write=back`.

```json
{"consistency": "read=cached", "consistencyPaths": {"/home/me/app/target": "write=ephemeral"}}
```

**`read=cached` is the one to reach for on a slow link.** It roughly halves the
time in ADR 0045's table.

**`write=back` and `write=ephemeral` are write capture**
([ADR 0044](docs/adr/0044-a-delegated-share-is-a-cache.md)). The share becomes a
union on the workspace: the live mount underneath, a local layer on top that
the container writes into. `back` carries those writes here within seconds and
reports a conflict by path when a file changed in both places; `ephemeral`
never carries them anywhere, which keeps a build directory off this machine.
Both are proven end to end in CI; conflict handling only by unit tests.
**Neither is faster today**: a cold union reads no faster than a plain mount,
and scattered reads are slower. Large writes are local.

**Prefetch is off by default.** `prefetch: eager` or `prefetch: tree`
(`REMOTE_DOCKER_PREFETCH`) turns it on for a `read=cached` union
([ADR 0045](docs/adr/0045-prefetch-follows-the-reads.md) says why it is off).

**`read=cached` and every union need [file watching](#file-watching) on**, and
refuse to run without it. An edit to an existing file arrives at once; a file
you create or delete can take up to a minute to appear in a listing unless
watching is `coarse`. A union caches only what the watcher covers; anything
under an excluded directory is read over the mount.

Prefetch copies at most `cacheFiles` and `cacheBytes` (20,000 files and 2 GiB
by default). What does not fit is read over the live mount, so a larger
repository is cached in part and still works. Raise them for a large project,
lower them on a metered link, and see which you are getting with
`remote status`:

```
cache  /home/me/project: 12043 of 47112 files, 180.2MB of 2.1GB, 181.0MB sent, cached in part; the rest is read live
```

The workspace's own image carries `fuse-overlayfs`; stock `docker:dind` does
not (see `WORKSPACE_DIND_IMAGE` in [Workspace settings](#workspace-settings)).
Switching a directory's mode costs a volume rebuild rather than a migration.

### More connections behind a share

**`REMOTE_DOCKER_NFS_NCONNECT` requires Linux 5.3 or newer on the WORKSPACE
(not your machine), and nothing checks that before mounting.** On an older
kernel **every bind mount fails** with `invalid argument`, and the volumes it
made cannot be repaired without removing them (see
[the kernel floor](#running-a-workspace)). Unsetting it is the fix.

```bash
REMOTE_DOCKER_NFS_NCONNECT=4 remote-docker remote start
```

`2` to `16`; `0` and `1` are off. Set it on the session. It applies to volumes
created afterwards; an existing share volume keeps its options until removed.

It is one setting for every share: Linux keeps one RPC transport per server
address, and every share mounts from `127.0.0.1:<tunnel port>`, so this
multiplies the connections behind all of them. The first share mounted sets
the transport's options and later ones are silently ignored, so a change shows
only once none of your shares is mounted. What it buys is a higher ceiling on
requests in flight, which the file server bounds per connection.

**Whether it makes anything faster is unmeasured**: nothing in CI or
`test/bench.sh` has run with it on. If you try it, say in an issue whether it
helped.

## Running a workspace

The workspace runs one binary, `remote-dockerd`. It supervises dockerd,
provisions an account per enrolled key, and serves SSH itself. There is no
sshd, no sudo and no shell scripts in the image.

**The workspace kernel must be 3.10 or newer** (RHEL 7). It is a hard floor:
the NFS client refuses the *whole* option string over one word it does not
know, and a Docker volume's driver options cannot change after creation, so an
option too new fails every mount on that workspace, permanently. Each option's
kernel requirement is tabulated in `core/workspace/kernel_test.go`, and a test
fails on anything above the floor. Your own machine's kernel does not matter:
the client mounts nothing. The one exception is
[`nconnect`](#more-connections-behind-a-share), opt-in only.

Two things about an old workspace kernel that are *not* the floor:

- **`write=back` and `write=ephemeral` need `fuse-overlayfs`**, which the agent
  runs itself, as root, so the workspace's answer decides and no kernel version
  is read. Upstream's man page says it "works with Linux 4.18 or newer"; its
  README scopes 4.18 to running **from a user namespace**, which a per-account
  dind does not. Neither states a floor for a root mount, so expect 4.18. RHEL 7
  shipping the package (`fuse-overlayfs-0.7.2-6.el7_8` in Extras,
  [RHEA-2020:1222](https://access.redhat.com/errata/RHEA-2020:1222)) is **not**
  evidence against that: Red Hat backported the kernel side into its 3.10
  branch and [said so](https://www.redhat.com/en/blog/rhel-78-and-final-update-container-tools).
  *(Checked 2026-09-08 against
  [the README](https://github.com/containers/fuse-overlayfs/blob/main/README.md)
  and [the man page](https://github.com/containers/fuse-overlayfs/blob/main/fuse-overlayfs.1.md);
  4.18 is Linux commit `4ad769f3c346`, "fuse: Allow fully unprivileged mounts".
  Nothing here has run a root mount on a pre-4.18 kernel without a backport,
  which is what would settle it.)*
- **Docker itself has not been built for el7 since 26.1.4** (*checked 2026-09-08
  against `https://download.docker.com/linux/centos/7/x86_64/stable/Packages/`*),
  and [the install docs](https://docs.docker.com/engine/install/centos/) list
  only CentOS Stream. Docker's own documented minimum is still kernel 3.10, and
  `overlay2` is documented as working on `3.10.0-514` and newer, so a workspace
  there is plausible; nothing here has ever run one. RHEL 7 reached end of
  maintenance on 2024-06-30.

### Behind a reverse proxy

The agent also serves SSH over a WebSocket, so any HTTP reverse proxy can front
it ([ADR 0034](docs/adr/0034-ssh-inside-a-websocket.md)):

```
--ws-addr :2280      the WebSocket listener; empty disables it
```

The agent runs both listeners by default, but `deploy/` only publishes 2222
(uncomment the `2280` port in the compose or stack file) and the systemd unit
passes `--ws-addr ""`. Point the proxy at `:2280`, make sure it passes
WebSocket upgrades, and give the client the URL:

```
remote-docker remote create dev --host wss://dev.example.com --user alice
```

The agent accepts an upgrade on **any path**, so it does not matter whether the
proxy strips its prefix:

```
remote-docker remote create dev --host wss://example.com/rd --user alice
```

**The agent never terminates TLS**; the proxy does. Plaintext between proxy and
agent still carries the full SSH handshake, so the host key and your key
authenticate both ends.

For a proxy with a self-signed certificate, pass `--ca-file` or, for that
workspace, `--insecure`, which gives up knowing which proxy answered and
nothing more.

### On Kubernetes

```bash
helm install ws oci://ghcr.io/lhns/charts/remote-docker-workspace \
  --namespace remote-docker --create-namespace \
  --set ingress.host=ws.example.com \
  --set-file authorizedKeys.alice=$HOME/.ssh/id_ed25519.pub

kubectl label namespace remote-docker pod-security.kubernetes.io/enforce=privileged
```

One privileged pod with its image store and host keys on volumes, reached
through an ordinary Ingress (the tunnel is an HTTP upgrade). Then
`remote create dev --host wss://ws.example.com`.

[`charts/remote-docker-workspace/README.md`](charts/remote-docker-workspace/README.md)
has the values, which storage driver your volumes need, and why both volumes are
ReadWriteOnce. CI installs the chart on a kind cluster behind ingress-nginx on
every pull request ([ADR 0035](docs/adr/0035-the-workspace-on-kubernetes.md)).

### The image

Published to GHCR for `linux/amd64` and `linux/arm64`:

```
ghcr.io/lhns/remote-docker-workspace:<version>   # on a v<version> tag
ghcr.io/lhns/remote-docker-workspace:latest      # on a v<version> tag
ghcr.io/lhns/remote-docker-workspace:sha-<short> # every commit to main
```

`latest` follows the most recent `v*` tag and exists from `v0.1.0` onwards.
*(Checked 2026-08-12 with `docker manifest inspect
ghcr.io/lhns/remote-docker-workspace:latest`.)* To build it yourself, use the
repository root as the context:

```bash
docker build -f image/Dockerfile -t remote-docker-workspace:latest .
```

### Plain Docker

```bash
cd deploy
mkdir -p authorized_keys.d state
cp /path/to/alice.pub authorized_keys.d/alice.pub   # filename = account (unix user rd-alice)
docker compose up -d --build
```

`state/` holds the host keys and the uid map and **must persist**. Losing it
gives every client a changed-host-key warning and reassigns every account's
uid, which changes its tunnel port and orphans the ownership of everything it
has written.

The container is privileged: dind runs its own daemon, bridge and iptables, and
mounts NFS.

### On a VM, with no container

The same binary as a systemd service
([ADR 0025](docs/adr/0025-the-agent-as-a-guest.md)). The one setting that
differs is `WORKSPACE_ENABLE_DIND=false`, because the machine already has a
dockerd. The unit file itself is not exercised by any test.

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

The last row catches people: with one shared daemon this machine mounts NFS
itself, and a missing client shows up as a container that will not start,
naming the volume rather than the package.

A per-account daemon runs the image in `WORKSPACE_DIND_IMAGE`, and the unit
sets none, so it falls back to stock `docker:dind`, which cannot serve
`write=back` or `write=ephemeral`. Set it to the workspace image in
`/etc/remote-docker/env` if you want those.

`/etc/workspace` must persist, like `state/` above.

On a machine that does other work: enrolled keys become **real users on that
machine** (`rd-<account>`), and a per-account daemon runs privileged, so an
account that breaks out of one reaches the VM itself
([ADR 0019](docs/adr/0019-a-dockerd-per-account.md)).

### Docker Swarm

Swarm cannot run privileged tasks, so the service starts **unprivileged** and
relaunches itself through the node's Docker socket
([ADR 0013](docs/adr/0013-self-elevation-instead-of-a-launcher.md)).

```bash
# Create the state directories ON THE NODE first. They are bind mounts, so a
# missing path becomes an empty root-owned directory instead of an error.
export WORKSPACE_DATA=/var/lib/remote-docker
ssh <node> "mkdir -p $WORKSPACE_DATA/{state,authorized_keys.d}"

docker stack deploy -c deploy/swarm.yml workspace
```

Those binds are one node's filesystem, so on a multi-node swarm pin the service
to that node with a placement constraint: a task rescheduled elsewhere gets new
host keys and new uids, which moves every account's tunnel port. Port 2222 is
published through the routing mesh; the privileged child joins the task's
network namespace, so `mode: host` is not needed.

The host Docker socket mount in `swarm.yml` is **the whole trust boundary**:
whoever can deploy this stack can already start privileged containers on the
node. The socket is not passed to the privileged child. Swarm itself is
untested; only the elevation mechanism is.

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
| `WORKSPACE_DIND_IMAGE` | `WORKSPACE_IMAGE`, else `docker:28-dind` | image a per-account daemon runs. Stock dind has no `fuse-overlayfs`, so no unions and no fuse-overlayfs storage driver |
| `WORKSPACE_DIND_STORAGE_DRIVER` | inherited from `WORKSPACE_DOCKERD_ARGS` | |
| `WORKSPACE_DIND_MOUNTS` | empty | extra bind mounts for every per-account daemon, and the paths a bind may name; see below |
| `WORKSPACE_DAEMON_READY_TIMEOUT` | `180` | seconds a cold per-account daemon has to answer; see below |
| `WORKSPACE_SHELL` | `/bin/bash` | shell an SSH session lands in |
| `WORKSPACE_UID_BASE` | `10000` | first uid handed to an account |
| `WORKSPACE_PORT_BASE` | `30000` | first reverse-tunnel port; an account's first port is `PORT_BASE + (uid - UID_BASE)` |
| `WORKSPACE_ACCOUNT_PREFIX` | `rd-` | prefix of the unix user behind an account (`rd-alice`) |
| `WORKSPACE_IMAGE` | | the workspace's own image; set by elevation and by `deploy/docker-compose.yml` |
| `WORKSPACE_SELF` | | this task's name, set by `deploy/swarm.yml` |
| `WORKSPACE_HOST_SOCKET` | `/var/run/host-docker.sock` | the node's Docker socket, for Swarm elevation |
| `WORKSPACE_DATA` | `/var/lib/remote-docker` | read by `deploy/swarm.yml`, not by the agent |

### How long a cold daemon has to start

An account's daemon is started when that account connects, and everything the
account does waits for it, a shell included, so an account whose daemon will
not start gets no prompt until the wait is over.

`WORKSPACE_DAEMON_READY_TIMEOUT` is that wait, 180 seconds by default. A healthy
daemon answers in about a second on a GitHub runner; the budget is for a first
start on fuse-overlayfs over Ceph or NFS. Lowering it makes a broken daemon say
so sooner and risks giving up on a slow one. An unusable value is logged at
startup and the default is used.

### A private or insecure registry

With a daemon per account (the default) each account's daemon does its own
pulling, and configuration given to the workspace's daemon does not reach it: a
registry that works on the workspace fails inside every account with
`http: server gave HTTP response to HTTPS client` or an unknown certificate
authority. Give them the same files:

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

The volumes are for the workspace's own daemon. `WORKSPACE_DIND_MOUNTS` passes
the same paths on to each account's daemon, as `source:destination` or
`source:destination:ro`, comma-separated. Both must be absolute: docker reads a
relative source as a volume NAME and would mount an empty volume. A source
missing on the workspace is refused at startup rather than created empty.

**It also declares which paths a bind may name.** A client leaves
`-v /lib/modules:/lib/modules:ro` alone rather than exporting it from the
user's machine, which is what makes tools like `kind` work
([ADR 0041](docs/adr/0041-the-workspaces-own-paths.md)). With a daemon per
account the DESTINATION is what counts; with a shared daemon it is the SOURCE,
and a remap is warned about at startup because it cannot be honoured.

A `daemon.json` that sets `storage-driver` or `hosts` collides with the flags
the agent passes and dockerd refuses to start; use
`WORKSPACE_DIND_STORAGE_DRIVER` instead. A change to this setting reaches an
existing daemon only once that account has nothing running (see
[Changing settings later](#changing-settings-later)).

Operator commands, on the workspace:

| | |
|---|---|
| `remote-dockerd serve` | the agent; the image's default |
| `remote-dockerd elevate` | the Swarm entry point |
| `remote-dockerd healthcheck` | is this workspace serving? `deploy/` and the chart use it |
| `remote-dockerd daemons ls` | which accounts have a daemon |
| `remote-dockerd daemons reset <account> [--purge] [-f]` | rebuild one; `--purge` discards its images; `-f` while it runs containers |

### The storage driver, worth getting right once

If `WORKSPACE_DATA` (or `/var/lib/docker`) is on Ceph- or NFS-backed storage,
you must set `WORKSPACE_DOCKERD_ARGS=--storage-driver=fuse-overlayfs`: overlay2
refuses such a filesystem, and vfs copies every layer. Per-account daemons
inherit it unless `WORKSPACE_DIND_STORAGE_DRIVER` says otherwise.

**If anything it needs is missing, dockerd falls back to vfs rather than
failing.** vfs copies the whole image on every `docker create`: nothing errors,
`docker ps` stays instant, and `docker run` takes minutes. The agent logs it and
`remote-docker remote status` shows it.

It needs `CONFIG_FUSE_FS` (`modprobe fuse` is enough), `/dev/fuse` in the
container, and the `fuse-overlayfs` binary **in the image the daemon runs**,
which stock `docker:dind` lacks and the workspace image has (so check
`WORKSPACE_DIND_IMAGE`).

It also needs a kernel **reporting 4.18 or newer**: moby checks the version and
nothing else (`CheckKernelVersion(4, 18, 0)` in
`daemon/graphdriver/fuse-overlayfs/fuseoverlayfs.go`), so a backported kernel
such as RHEL 7's is refused, silently, as the fall-through to vfs above
([moby#42970](https://github.com/moby/moby/issues/42970)). A `write=back` share
does not pass this gate, since the agent runs the binary itself. *(Checked
2026-09-08 against moby master; re-read that file.)*

It is not the default because overlay2, where it works, is in the kernel and
markedly faster.

### Restarting a workspace container

A container's `/run` is part of its writable layer, and dind's entrypoint
deletes `docker*.pid` but not `containerd.pid`. A workspace that ended
**uncleanly** and restarts on the **same writable layer** comes back with a
stale `/var/run/docker/containerd/containerd.pid`; if that pid is alive again,
dockerd either exits or waits for a containerd it never started. Either way the
workspace accepts SSH and answers nothing.

The agent mounts a tmpfs on that directory before it starts dockerd, in every
deployment. Nothing to configure. Whether you were ever exposed:

- **A clean stop is safe**: a clean shutdown removes the file. Measured on a
  runner: 0 failures in 50 clean stops, 11 in 114 unclean restarts.
- **The exposed case is an unclean end plus a restart on the same layer**:
  `docker restart`, `docker compose restart`, a host reboot under
  `restart: unless-stopped`, an OOM kill, or a SIGKILL after the grace period.
- **Kubernetes was never exposed**: kubelet gives every restart a fresh layer.
- **A VM workspace was never exposed**
  ([ADR 0025](docs/adr/0025-the-agent-as-a-guest.md)): `/run` is already a
  tmpfs there, and the operator starts dockerd, so the agent mounts nothing.

If the agent cannot mount it (not privileged, or a daemon is already serving
from that directory) it says so and starts the daemon anyway.

### Enrolment

Out of band: someone with access drops a `<account>.pub` into the keys
directory, one key per line. The filename is the account name a client logs in
as; the unix user behind it is `rd-<account>`
([ADR 0025](docs/adr/0025-the-agent-as-a-guest.md)). Emptying or removing the
file revokes access but keeps the account and its home directory.

The keys directory is re-read on change and polled every 60 seconds, because
inotify never fires for a change made on another host when that directory is
on shared storage.

### A daemon per account

By default each enrolled account gets its own Docker daemon behind the same SSH
port ([ADR 0019](docs/adr/0019-a-dockerd-per-account.md)). Accounts stop seeing
each other's containers, images and volumes, two accounts can publish the same
port at once, and a shell lands on its own daemon.

**It is separation, not isolation.** Each per-account daemon runs privileged,
so a determined account can still break out and reach another's; what this
buys is that nobody sees anyone else's work *by accident*. Genuine isolation is
one workspace container per account.

It costs:

- **the layer cache is duplicated.** Five accounts on `node:22` is five
  copies. A registry mirror recovers bandwidth but not disk.
- **memory**, roughly 100-150MB per idle daemon plus containerd.
- **disk becomes a shared failure mode.** One account's runaway build can fill
  the volume and take down every other account's daemon.
- **3-10s** for an account's first connection after its daemon has stopped.

#### Changing settings later

A per-account daemon's image, flags and mounts are fixed when its container is
created. The agent applies a changed configuration by itself: a daemon whose
recorded configuration no longer matches is recreated once that account has
nothing running, keeping its graph volume.

**The storage driver is the exception**: a graph written by one driver cannot
be read by another, so the agent says so and leaves it. Deciding is a command:

```bash
docker exec <workspace> remote-dockerd daemons ls
docker exec <workspace> remote-dockerd daemons reset alice           # rebuild it; -f if it runs containers
docker exec <workspace> remote-dockerd daemons reset --all --purge   # and discard images
```

`--purge` is the account's entire Docker state.

The agent that is serving may keep reporting the old failure for up to five
seconds after a reset, until its own record of that failure expires; retry
after that.

#### What persists

| | shared daemon | a daemon per account |
|---|---|---|
| host keys, uid map, workspace id | `/etc/workspace` | `/etc/workspace` |
| images, containers, named volumes | the workspace's `/var/lib/docker` | a named volume per account, `rd-dind-<account>-lib`, **inside** the workspace's `/var/lib/docker` |
| `rd-*` share volumes | the workspace's daemon | that account's daemon |
| the account's docker socket | n/a | `/run/rd/<account>/`, recreated on every start |

Both deployments in `deploy/` persist `/var/lib/docker` and `/etc/workspace`; a
daemon per account nests the same data one level deeper. So:

- **`rd-dind-<account>-lib` is everything that account has.** The daemon
  container in front of it can be removed and recreated; the volume cannot.
- **`docker system prune -a --volumes` on the workspace's own daemon is
  destructive**: an idle account's daemon container goes, then its storage.
  `docker volume ls --filter label=remote-docker.daemon` lists what must not
  be pruned.
- **`/etc/workspace/workspace-id` is how the agent recognises its own daemons
  after a redeploy.** Lose it and they keep running, holding their users' work,
  no longer adopted.

`rd-*` share volumes hold no data, only a pointer to a directory on a client's
machine, and `remote-docker remote gc` removes unused ones. The exception is a
`rd-*-cache` volume behind a `write=back` or `write=ephemeral` share, which
holds a container's writes not yet carried back; leave those to `remote gc`,
which asks the workspace whether they are mounted.

#### Upgrading an existing workspace is a breaking change

Images and volumes an account built under the shared daemon are invisible from
its own, with no cheap migration. **Set `WORKSPACE_PER_USER_DIND=false` before
upgrading** if that matters. The old data stays in the shared `/var/lib/docker`,
so the decision is reversible.

With it off, all enrolled users share one daemon and see each other's
containers ([ADR 0012](docs/adr/0012-shared-dockerd-across-users.md)). That
stays supported: a single-account workspace has nothing to separate.

## Caveats

### A container that is not root

Every file in a share is reported as owned by the workspace account (uid
10000 and up) with mode 0666, every directory 0777, so an image that runs as
its own user can read and write the share. A `chown` inside the container is
accepted and does nothing, and a read-only bind (`ro`) is still read-only
([ADR 0046](docs/adr/0046-a-share-reports-wide-mode-bits.md)).

**A named volume is not a share, and gets none of that.** Where the image has no
directory at the mount path, the daemon creates it root-owned 0755, so a
non-root container gets EACCES on its first write. That is Docker's own rule,
identical without us (`test/volume-ownership.sh` asserts so). The fix is in the
Dockerfile, before `USER`:

```dockerfile
RUN mkdir -p /home/app/.local/share/app && chown -R app:app /home/app/.local
```

### What not to put on the share

- **Build artifacts.** `node_modules`, `.git`, `target/`, package caches.
  Keeping them off the share is worth roughly 20×; protocol tuning is worth
  about 2×.
- **Databases.** `nolock` plus `fcntl` locking is a corruption risk.
- **Very large trees over a WAN.** NFSv3 is synchronous per operation, so
  latency multiplies. Over a LAN it is fine.

### Windows shells

**PowerShell and cmd need nothing.**

**Git Bash rewrites POSIX-looking arguments into Windows paths before this
program sees them.** It converts both halves of `-v` and turns the `:` into a
`;`:

```
you type      -v /c/Users/you/x:/app
docker gets   C:\Users\you\x;C:\Program Files\Git\app
```

The container side is restored automatically
([ADR 0040](docs/adr/0040-git-bash-mangles-argv.md)), so `-v` works from Git
Bash as typed. Where the reversal cannot be exact (Git Bash maps `/bin` and
`/usr/bin` onto one directory) it says what it read.

Only `-v` is repaired. These are mangled too and are not:

| you type | docker gets |
|---|---|
| `-w /src` | `C:/Program Files/Git/src` |
| `-e PATH=/usr/bin:/bin` | `…\usr\bin;…\usr\bin` |

A path the workspace owns (`-v /lib/modules:/lib/modules:ro`) works from Git
Bash too: the client undoes the conversion when the workspace declared the path
and this machine does not have it. `//lib/modules` also works.

For everything else, use `--mount`, which Git Bash does not mangle, or escape:
`MSYS_NO_PATHCONV=1 docker …` disables conversion entirely, and a leading
double slash (`//app`) protects one argument:

```bash
docker run --mount type=bind,source="$PWD",target=/app alpine ls /app
```

### What differs from a bind mount

`test/probes/fsprobe` runs one fixed sequence of filesystem operations inside
a container against a plain bind mount and against a share from a Linux and a
Windows client; CI fails on any difference not listed in
`test/fs-conformance/deviations-*.txt`. The differences:

| behaviour | on a share | why |
|---|---|---|
| owner and mode | every file is the workspace account's; a file reads back `0666`, plus `0111` where the real file is executable, and always `0777` from a Windows host, where there is no execute bit to preserve | ADR 0046, `Attrs.AlwaysExecutable` |
| `chown` | accepted, changes nothing | ownership is synthesised |
| `utime`, `touch -d` | accepted, changes nothing: every SETATTR of times is dropped, so a `touch` does not move a file's mtime | the watcher replays a SETATTR to invalidate, and applying one looped |
| `chmod` | reaches the file on this machine, with the owner's read and write forced on; the mode read back is still the synthesised one, so the execute bit on a Linux host is all a container can observe | the share is served as that owner |
| `git` in a container | a repository on a share is refused as "detected dubious ownership" unless `safe.directory` is set | the share reports the workspace account as owner |
| unlink of an open file | a `.nfs*` entry until the last close | NFS silly-rename |
| Windows host: case | `a` and `A` are one file | NTFS is case-insensitive |
| Windows host: names | `< > : " \| ? *`, a control character, a trailing dot or space, and the device names (`CON`, `PRN`, `AUX`, `NUL`, `COM1`-`COM9`, `LPT1`-`LPT9`) are refused with EINVAL; the probe checks a sample of them | NTFS cannot spell them; native Docker refuses them too |
| Windows host: inode of a recreated name | a new inode number, where ext4 reuses the old one | NTFS file reference numbers |
| Windows host: a symlink | `size=0`, where a Linux host reports the target path's length | a symlink is an NTFS reparse point |
| Windows host: creating a symlink | refused, unless Developer Mode is on or the client runs elevated; the client says so once per share, and nothing stands in for it | Windows needs `SeCreateSymbolicLinkPrivilege`, and NFSv3 cannot answer "I made something else" (`core-client/nfsserve/symlink.go`) |

Everything else the probe does behaves as on a bind mount: `flock` and `fcntl`
byte-range locks across processes, `mmap` MAP_SHARED, concurrent `O_APPEND`
without a torn line, eight processes creating in one directory, sparse files,
rename in every form, hard links, NFC and NFD names kept byte for byte, and a
git repository through `init`, 200 commits, `status`, `checkout`, `gc` and
`fsck`. Not answered: a file over 4 GiB, which no step writes, and creating a
symlink from an ordinary Windows account, since the runner is elevated.

### What cannot be bind mounted

A bind mount becomes an NFS-backed volume, so what crosses is file CONTENT.
These do not work, and say so up front:

- **Sockets** (`/var/run/docker.sock` above all), **devices and FIFOs**, alone
  or inside a shared directory: a file share carries the name, not the kernel
  object. (`--device` is unaffected; it names a device on the workspace.)
- **Windows named pipes** (`npipe` mounts) pass through untouched, so the
  workspace looks for a pipe path that means nothing there.

**A bind source is a path on YOUR machine.** A path that exists only on the
workspace fails, except those the operator lists in `WORKSPACE_DIND_MOUNTS`
([A private or insecure registry](#a-private-or-insecure-registry)), and a path
your machine also has still wins.

`docker inspect` shows every bind as a volume, and a single file as a volume
with a subpath. Mount propagation (`:rshared` and friends) is dropped, since the
mount happens inside the workspace daemon's own namespace.

### A session must be running

The session is the endpoint and the file server, so stopping it takes running
containers' mounts with it. Any command that needs one starts it and leaves it
running.

**It reclaims in two stages, and only one is on by default.** After
`daemonStandby` (30 minutes) idle, the session **stands by**: it drops the
workspace connection and its file watches but keeps the endpoint, so the next
request from compose, buildx, Testcontainers or your IDE wakes it.

`daemonIdle` **ends the process**, and is off unless you set it, because the
endpoint goes with it: tools pointed at the endpoint then fail until a
`remote-docker` command starts a session again.

### File watching in detail

**Deletions are the gap.** `unlink` of a name that is already gone fails
before the kernel generates anything, so a deletion cannot be replayed
faithfully. `coarse` approximates it with a directory-level event (unit tested
only); `partial` says nothing rather than something wrong. A watcher that
rescans notices; one that trusts the event *kind* is misled
([ADR 0014](docs/adr/0014-inotify-does-not-see-client-changes.md)).

**Watching costs one watch per directory** (inotify is not recursive), and on
macOS one open file descriptor per *file*.

| platform | budget | what binds |
|---|---|---|
| Linux | 4096 directories | `fs.inotify.max_user_watches` (`sysctl fs.inotify.max_user_watches` shows yours) |
| Windows | 1024 directories | one `ReadDirectoryChangesW` buffer per watch |
| macOS | 512 directories | `RLIMIT_NOFILE`, because kqueue costs an fd per file |

Build outputs like `dist/` and `target/` are **not** excluded by default,
because serving `dist/` and reloading when it changes is the point; a Rust or
Java tree will spend its budget inside `target/` and say so. Tune with
`REMOTE_DOCKER_WATCH_BUDGET` and `REMOTE_DOCKER_WATCH_EXCLUDE` (comma- or
`PATH`-separated). When the budget runs out, the directory it stopped at is
named. Nothing has run a watcher over a very large tree (10,000 directories).

Watching starts once the session has connected, on the first Docker command;
edits before that are counted and reported.

The per-tool polling flags (`CHOKIDAR_USEPOLLING=1`, `WATCHPACK_POLLING=true`,
`--poll`) still work and are never set for you.

### Why a container takes a few seconds to start

Measured against a workspace whose `/var/lib/docker` is on CephFS:

| | |
|---|---|
| the tunnel | **~10ms per request.** An SSH channel opens in 2-3ms and a `/_ping` round trip is 8ms. Not where the time goes. |
| this binary starting | **~400ms per command.** ~210ms to load a 45MB binary and build the command tree, about as much again for the embedded Docker CLI. Once per command, not per request. |
| `docker create` | **~250ms**, the same measured on the workspace itself. Container creation is many small synchronous metadata writes. |
| `docker start` | **~800ms**, likewise the same locally. |

**The storage is the expensive part, not the remote part**: the same daemon on
local disk creates and starts in tens of milliseconds. If that matters more
than surviving a node move, put `/var/lib/docker` on local disk.

### What is not tested

The integration suites run the Linux client against a real workspace in CI.
Beyond that:

- **macOS has never been executed at all**, in CI or anywhere else.
- **Windows**: one client is exercised end to end on every pull request
  (`machine.yml`: a WSL workspace, a bind mount, GNU tar with attributes, a
  non-root mkdir and the conformance probe), on one runner image and one WSL
  kernel. The rest of the Windows client is unit tested only, and **Hyper-V has
  never run**.
- **Swarm itself** needs a real cluster; only the elevation mechanism is tested.
  The systemd unit is not exercised either.
- **Every suite mounts a share on one kernel**, the runner's 6.x, so a mount
  option newer than the [3.10 floor](#running-a-workspace) would pass CI and fail
  every mount on an older workspace. A table guards it, not a test run.
- **Write-back conflicts** are unit tested only, and whether Compose accepts
  our `consistency:` words is unverified.
- **The Windows MSI** is installed and uninstalled on a runner when the
  installer changes. No MSI from a real release has been installed by anybody,
  none is signed, and no release zip has been unpacked on a machine that did not
  build it.
- **Android is built and inspected, and CI runs nothing on it**: it checks that
  the binary is loadable on a phone and links the system libc, which is what
  makes DNS work there. A session and a container were confirmed by hand from
  Termux on 2026-08-14, on one arm64 device. `android_amd64` has never been
  executed.

What each release changed, and what is still unproven: [`CHANGELOG.md`](CHANGELOG.md).

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
"won't fix, far from trivial". *(Both still closed on 2026-09-23:
`gh issue view 8484 -R docker/compose`, `gh issue view 13358 -R containers/podman`.)*

Sync makes changes land as ordinary local writes, so file watchers work, which
is likely why it won. The replay here is the same idea as Docker Desktop's
"Event Injection": Linux cannot inject a synthetic inotify event, so a real
operation is the only mechanism. Owning the NFS server as well as the agent is
what keeps the replay from echoing back as a change of its own
([ADR 0016](docs/adr/0016-replaying-change-events-as-real-syscalls.md)).

## Project layout

Eight Go modules in one repository
([ADR 0021](docs/adr/0021-the-module-layout.md)). The two binaries are the
glue that knows Docker; `core`, `dircache`, `core-client` and `core-agent` do
not.

```
core/                  what both ends must agree on
  workspace/           the contract: paths, uid→port, volume names
  tunnel/              one bidirectional copy, one answer to half-closing
  notify/  cache/      the change channel and the cache channel, each entire
  logx/                one log handler, so both look the same

dircache/              filling a local copy of a tree, and carrying writes
                       back. Depends on nothing, in-repo or out

machine/               provisioning a workspace on this machine: a WSL
                       distribution or a Hyper-V VM. Depends on nothing in-repo

core-client/           this machine, minus Docker
  tunnelclient/        dialling the tunnel
  nfsserve/            the in-process NFSv3 server
  fswatch/             watching directories, on three platforms
  keys/                this machine's identity to a workspace

core-agent/            the workspace, minus Docker
  tunnelserver/        answering the tunnel
  accounts/            one unix account per enrolled key
  replay/              replaying changes as real syscalls
  union/               the cache over the live export
  netns/  wslisten/    another process's netns; SSH over a WebSocket

client/                the client binary (docker/cli, buildx)
  cmd/remote-docker/   also answers to `docker`
  internal/            api proxy, bind rewriting, ports, session

agent/                 the agent binary (five direct third-party requires)
  cmd/remote-dockerd/  the workspace binary
  internal/            per-account daemons, dockerd supervision, elevate

image/  deploy/        the workspace container and its deployments
charts/                the Helm chart
test/                  the integration suites
  probes/              their instruments, a module of their own
installer/windows/     the MSI
docs/adr/              why everything is the way it is
```

## Development

The repository root is not a module, and `./...` stops at a module boundary, so
every command loops over the eight.

```bash
mods="./core ./dircache ./machine ./core-client ./core-agent ./agent ./client ./test/probes"
for m in $mods; do (cd $m && go build ./... && go test ./...); done
for m in $mods; do (cd $m && golangci-lint run ./...); done
bash test/integration.sh      # needs docker and NFS client support
```

## License

Licensed under Apache 2.0.

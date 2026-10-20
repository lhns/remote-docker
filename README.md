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
# 1. run the line whoever runs the workspace gave you: it enrols this
#    machine's key and adds the workspace, with nothing to hand back
remote-docker remote create dev --token rdt1.eyJ1Ijoi...

# 2. start a session
remote-docker remote start

# 3. and then it is just docker
remote-docker run --rm -v .:/w alpine ls /w
```

`start` prints the endpoint and returns. No terminal has to stay open.

**Without a token**, add the workspace and hand your public key over instead:

```bash
remote-docker remote create dev --host workspace.example
remote-docker remote enroll      # prints the key, to be saved as <account>.pub
```

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

From Git Bash or another MSYS2 shell, read [Windows shells](#windows-shells):
those shells rewrite path arguments before this program sees them.

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

**The Hyper-V workspace feature** ("Hyper-V workspace" in the feature tree,
off by default) installs a service and one Linux VM on this machine, as the
workspace you name on the page after it (default `hyperv`), usable at once by
the user who ran the installer, even a standard user who typed administrator
credentials at the prompt: no sign-out, no administrator rights afterwards. It
can also be added or removed later from Settings > Apps > remote-docker >
Modify. It needs Hyper-V enabled; without it the feature is not offered and a
silent install naming it is refused, naming the command that enables it.

```powershell
msiexec /i remote-docker_0.6.0_windows_amd64.msi /qn ADDLOCAL=Main,HyperV WORKSPACENAME=dev `
  WORKSPACEIMAGE=registry.example/remote-docker-workspace:0.6.0 WORKSPACEPROXY=http://proxy.example:3128
```

`WORKSPACEIMAGE` (a registry mirror) and `WORKSPACEPROXY` are optional. Another
person on the same computer is added with
`net localgroup remote-docker-users <user> /add` and then makes their own
workspace with `remote machine create <name> --backend hyperv`. **Uninstalling
deletes the workspace's containers, images and volumes**, and every other
user's workspace made through the service; their config entries stay behind
until they run `remote rm`. An upgrade touches none of it.

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

A Hyper-V backend exists (`--backend hyperv`). It downloads Flatcar's Hyper-V
disk image itself (pinned and checksum-verified, about 600 MB, once), and
`--proxy <url>` sends that download and the machine's docker daemon through an
HTTP proxy. It needs administrator rights or the Hyper-V Administrators group,
except where the installer's Hyper-V service is set up (above): then members of
`remote-docker-users` use it as standard users. It has run end to end by hand
on one Windows 11 computer, directly and through the installer; no CI can run
it, so `docs/testing-machines.md` is how to check it on yours.

## What works

- **Bind mounts from anywhere on your machine**: another drive, above the
  working directory, unrelated to it. **Single files work too**
  (`-v ./nginx.conf:/etc/nginx/nginx.conf`), sharing only that file
  ([ADR 0039](docs/adr/0039-a-single-file-is-a-one-file-export.md)).
- **Published ports reach your localhost.** `-p 8080:80` means
  `localhost:8080` here, opened as containers start. The workspace publishes
  on a port of its own choosing, so two people sharing a workspace can both
  ask for 8080, and `-p 8080:80 -p 9090:80` gives you both
  ([ADR 0008](docs/adr/0008-published-ports-reach-the-client.md)).
  **`docker ps`, `docker port`, `docker inspect` and `compose ps` show the port
  on your machine**, as `127.0.0.1:8080->80/tcp`, since that is where it is
  open. It is the number you asked for and the address it listens on: always
  loopback, whatever `-p` said. It is what you asked for rather than a check
  that the port is open right now. Only `-p 80` with no host port gets you the
  workspace's number here too, as Docker would. The workspace's own number is
  in `docker exec <workspace> docker ps`. A container reaching the workspace
  host as `host.docker.internal:8080` does not get your port, because that
  address is the workspace's.
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
daemon, images and containers. The second enrols itself: run
`remote token create` on the first, and the line it prints on the second.
Files are not shared, and neither are published ports: each machine opens the number its own containers asked for, and sees the
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

## CI with autoscaled runners

Runners that come and go, such as a pod per job, cannot each be enrolled, and
under one shared key they would all be one machine: the second is refused its
tunnel, and two jobs in the same checkout path share a volume. An **ephemeral**
account fixes that ([ADR 0050](docs/adr/0050-ephemeral-clients.md)). Every
client process of it is a client of its own, with its own tunnel port, exports
and volumes, and the workspace removes them a short grace period after the
process goes away. The daemon, images and build cache stay shared, so a warm
cache serves every job.

1. **Enrol one key for the account**, `ci` here, as for any account. Every
   runner presents the same private key, placed at `id_ed25519` in the client's
   state directory (`REMOTE_DOCKER_STATE_DIR` moves it).
2. **Make the account ephemeral** on the workspace:

   | variable | chart value | default | |
   |---|---|---|---|
   | `WORKSPACE_EPHEMERAL_ACCOUNTS` | `ephemeral.accounts` | empty | accounts whose clients are ephemeral, comma-separated |
   | `WORKSPACE_EPHEMERAL_MAX_CLIENTS` | `ephemeral.maxClients` | `8` | runners at once, counting those in their grace period; the next is refused |
   | `WORKSPACE_EPHEMERAL_GRACE` | `ephemeral.grace` | `2m` | how long a runner's port and volumes outlive its last connection, for a reconnect |
   | `WORKSPACE_EPHEMERAL_CLEANUP_CONTAINERS` | `ephemeral.cleanupContainers` | `false` | also remove the runner's containers and compose networks |

   With the chart:

   ```yaml
   authorizedKeys:
     ci: |
       ssh-ed25519 AAAA... ci@runners
   ephemeral:
     accounts: [ci]
   ```

3. **Point each runner at the workspace** as usual, with `--user ci`.

The embedded `docker compose` names a project `<directory>-<client id>` (or
after the file's `name:`), so the same compose file on two runners is two
projects. `-p` or `COMPOSE_PROJECT_NAME` still win. **A standalone
`docker compose`** reads its own environment and has never heard of the client
id, so it names the project after the directory alone; give it a
`COMPOSE_PROJECT_NAME` per job, such as the CI job id.

Without `cleanupContainers`, a runner whose containers are still there keeps its
volumes, and its slot against `maxClients`, until somebody removes them.

**More runners than one workspace carries:** the chart's `replicas` runs
several workspaces, one per node, behind the same Service name. Runners need
nothing beyond that name: the Service pins each runner's address to one
replica, and every replica reads the same keys Secret, so the shared key works
on all of them. `maxClients` then counts per replica, and images and the build
cache are per replica too. A runner that wants to choose can name a replica,
`<fullname>-N.<fullname>-headless.<ns>.svc`, or look them up under SRV
`_ssh._tcp`. The chart README's "Several replicas" has the caveats.

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
| `remote-docker remote create <name> --token …` | enrol this machine's key with an [enrolment token](#enrolment), then add the workspace |
| `remote-docker remote create <name> --host …` | add a workspace and its docker context |
| `remote-docker remote set <name> --user …` | [change](#changing-a-workspace) some of its settings, keeping the rest |
| `remote-docker remote rm <name>` | stop its session and remove both again |
| `remote-docker remote ls` | list them |
| `remote-docker remote use <name>` | make it the default here, and docker's current context |
| `remote-docker remote inspect [name]` | settings, endpoint, context, whether a session is up |
| `remote-docker remote machine …` | `create`, `rebuild`, `start`, `stop`, `status` a [local workspace](#a-workspace-on-this-machine-windows) |
| `remote-docker remote token create` | a token that enrols another machine into your account; [admins](#admins) may name any `--account`, or `--unbound` |
| `remote-docker remote token ls` / `rm <id>` | your tokens not yet redeemed, or every one for an admin |
| `remote-docker remote key ls` / `add <file\|->` / `rm <fingerprint>` | your account's keys; an admin's with `--account` |
| `remote-docker remote user ls` / `rm <account>` | every account, and removing one ([admins](#admins) only) |

A command about one workspace takes its name as an argument, else
`--workspace`, else the default. `stop`, `restart`, `machine stop`,
`machine rebuild` and `rm` refuse while the session is in use,
as `docker rm` refuses a running container; `-f` goes ahead.

Any command that needs a session starts one, including the embedded CLI. For a
shell on the workspace, use `ssh`; the agent serves one to any enrolled key.

A session reads its settings (`REMOTE_DOCKER_PREFETCH`, `_WATCH`,
`_CONSISTENCY`, `_SHARE_OWNER`, the cache limits and the timeouts) once, when it
starts. A command with one of those variables set in its environment, to a
value the session does not have, restarts an idle session and says what changed;
if the session is in use it warns and leaves it alone, and `remote restart`
applies them once nothing needs it. A variable that is not set is no opinion:
a plain `docker ps` never changes a session. Config-file values are applied by
`remote restart`. `remote status` lists the settings
a session runs with when they are not the defaults.

`remote use` sets the default in `~/.remote-docker.json` **and**
`currentContext` in `~/.docker/config.json`, which compose, buildx,
Testcontainers and IDE plugins resolve, so it redirects those tools too.
`--no-context` sets only ours (as on `create`), and an exported `DOCKER_HOST`
overrides both. A missing context is created.

There is no `context` command: a docker context is written when a workspace is
created and removed with it
([ADR 0018](docs/adr/0018-one-way-to-do-each-thing.md)). Re-run
`remote create`, or `remote machine create` for a machine, to rewrite one
that has drifted.

## Settings

Every setting can be an environment variable, a key in `~/.remote-docker.json`,
and for some a flag. Precedence, highest first: **flag, environment, file,
default.** This table lists every `REMOTE_DOCKER_*` variable the client reads
(`client/internal/config/drift_test.go` fails when one is missing). The words a
single mount can carry are in [Mount options](#mount-options).

| environment | file key | flag | default |
|---|---|---|---|
| `REMOTE_DOCKER_HOST` | `host` | `--host` | none; required. A host, or `ssh://`, `ws://`, `wss://` with one |
| `REMOTE_DOCKER_PORT` | `port` | `--port` | `2222`, or the scheme's (443 for `wss`, 80 for `ws`). Optional |
| `REMOTE_DOCKER_CA_FILE` | `caFile` | `remote create`/`set --ca-file` | system roots |
| `REMOTE_DOCKER_INSECURE` | `insecure` | `remote create`/`set --insecure` | off |
| `REMOTE_DOCKER_USER` | `user` | `--user` | your local username, lowercased as the workspace does |
| `REMOTE_DOCKER_ENDPOINT` | `endpoint` | `--endpoint` | `\\.\pipe\docker_remote`, or a socket in the state directory |
| `REMOTE_DOCKER_WORKSPACE` | (`default`) | `--workspace` | the file's default |
| `REMOTE_DOCKER_CONSISTENCY` | `consistency`, `consistencyPaths` | `remote create`/`set --consistency` | `read=direct,write=through`; the default for mounts naming no `read=`/`write=`. See [Mount options](#mount-options) |
| `REMOTE_DOCKER_WATCH` | `watch` | `remote create`/`set --watch` | `off`; the default for mounts naming no `watch=`. Also takes `on`, `true`, `1`, `yes` (`partial`) and `best-effort` (`coarse`). See [File watching](#file-watching) |
| `REMOTE_DOCKER_WATCH_BUDGET` | `watchBudget` | | 4096 Linux, 1024 Windows, 512 macOS |
| `REMOTE_DOCKER_WATCH_EXCLUDE` | `watchExclude` | | `.git`, `node_modules`, `.venv`, `venv`, `__pycache__`, `.mypy_cache`, `.pytest_cache`, `.gradle`, `.terraform` |
| `REMOTE_DOCKER_CACHE_FILES` | `cacheFiles` | | 20000, files prefetch may copy into a union |
| `REMOTE_DOCKER_CACHE_BYTES` | `cacheBytes` | | 2 GiB, bytes prefetch may copy into a union |
| `REMOTE_DOCKER_PREFETCH` | `prefetch` | | `off`; `eager` or `tree`. The default for mounts naming no `prefetch=` |
| `REMOTE_DOCKER_IDLE_TIMEOUT` | `idleTimeout` | | `1m` before an unused connection is dropped |
| `REMOTE_DOCKER_DAEMON_STANDBY` | `daemonStandby` | | `30m` before an unused session lets go of the workspace, keeping its endpoint |
| `REMOTE_DOCKER_SHARE_OWNER` | `shareOwner` | | `0:0`; the `uid` or `uid:gid` every file in a share reports (gid defaults to the uid). The default for mounts naming no `owner=`. See [A container that is not root](#a-container-that-is-not-root) |
| `REMOTE_DOCKER_DAEMON_IDLE` | `daemonIdle` | | how long before an unused session EXITS. Unset never does, because that takes the endpoint with it |
| `REMOTE_DOCKER_STATE_DIR` | | | keys, known_hosts, logs. `%APPDATA%\remote-docker`, `~/.config/remote-docker` on Linux, `~/Library/Application Support/remote-docker` on macOS |
| `REMOTE_DOCKER_IGNORE_CREDENTIAL_HELPERS` | | | off; set it to proceed when a registry login in `~/.docker/config.json` names a credential helper that is not installed, for a run that needs none of those registries |
| `REMOTE_DOCKER_TRACE` | | | *expert, env only.* Off; `1` logs one line per API request |
| `REMOTE_DOCKER_NFS_TRACE` | | | *expert, env only.* Off; a threshold (`250ms`, or bare milliseconds, least `1ms`) above which a share's filesystem calls are logged |
| `REMOTE_DOCKER_NFS_FDCACHE` | | | *expert, env only.* `2s` that a file stays open after the request that used it; `0` closes it on every request |
| `REMOTE_DOCKER_NFS_NCONNECT` | | | *expert, env only.* Off; `2`–`16` connections per share. **Needs Linux 5.3 on the workspace** and breaks every mount without it. See [More connections behind a share](#more-connections-behind-a-share) |
| `REMOTE_DOCKER_NO_SESSION` | | | *internal, env only.* Set by the client on a docker command it runs itself so that command opens no session; not for you to set |

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

### Changing a workspace

`remote set` changes the settings you name and leaves the others alone:

```bash
remote-docker remote set dev --host wss://dev.example/tunnel
remote-docker remote set dev --user bob --watch off
```

It takes the same flags as `create`. A new `--host` without `--port` goes back
to the default port. A running session keeps its old settings until
`remote restart`.

On a [workspace on this machine](#a-workspace-on-this-machine-windows), the
address, port and user belong to the machine: change the port or user with
`remote machine rebuild`.

`remote create` on an existing name replaces every setting, and refuses to for
a workspace on this machine.

## Mount options

Everything a mount can say is a word in the mount's own option list. This is
the one reference; the sections below explain why each exists. Words go in
`docker run -v`, in `--mount`, and in a compose file, and each writes them a
little differently (examples below the table).

| word | values | default, and what sets it | meaning | needs, or is refused |
|---|---|---|---|---|
| `read=` | `direct`, `cached` | `direct`. `REMOTE_DOCKER_CONSISTENCY` / `consistency`, per directory `consistencyPaths` | `direct` revalidates every attribute each second; `cached` trusts them for a minute | `cached` is watched at least at `partial`, so `watch=off` with it is refused |
| `write=` | `through`, `back`, `ephemeral` | `through`. Same setting as `read=` | `through` writes on this machine as it happens; `back` writes into a union on the workspace and carries it back within seconds; `ephemeral` writes into a union and never carries it back | `back` and `ephemeral` make a union: `fuse-overlayfs` in the account's daemon image, `/dev/fuse`, and a [workspace kernel that has FUSE](#running-a-workspace); watched at least at `partial`, so `watch=off` is refused; a single file is refused |
| `watch=` | `off`, `partial`, `coarse` | `off`. `REMOTE_DOCKER_WATCH` / `watch`, which also takes `on`, `true`, `1`, `yes` (`partial`) and `best-effort` (`coarse`) | replays your edits inside the workspace so file watchers there fire; see [File watching](#file-watching) | a mount word takes only the three canonical values; `read=cached` and every union raise `off` to `partial` |
| `prefetch=` | `off`, `eager`, `tree` | `off`. `REMOTE_DOCKER_PREFETCH` / `prefetch` | fills a union's cache ahead of reads, within `REMOTE_DOCKER_CACHE_FILES` and `_BYTES` | `eager` and `tree` need `read=cached` with `write=back` or `ephemeral`; anywhere else they are refused, not ignored |
| `owner=` | `1000` (uid and gid), or uid and gid joined by one separator, as `1000.2000` | `0:0` (root). `REMOTE_DOCKER_SHARE_OWNER` / `shareOwner` | the uid and gid every file in the share reports, for an image that runs as another user; see [A container that is not root](#a-container-that-is-not-root) | the separator is any one character that is not a digit, `=` or `,`: write `.` in `-v`, whose option list cannot carry a fourth colon; `:` also works in `--mount` and compose. On a union the workspace must be able to give the root an owner, else the mount is refused |
| `default`, `consistent` | | | Docker's own words for `read=direct,write=through` | any other bare word is refused |
| `cached` | | | Docker's word for `read=cached,write=through` | |
| `delegated` | | | Docker's word for `read=cached,write=back` | |
| `ro` and other Docker options | | | untouched; `ro` stays read-only on the share | |

An axis or word a mount does not name comes from the per-directory rule
(`consistencyPaths`), then the setting, then the default. Each combination of
`read=`, `write=` and `owner=` is a share of its own, so one directory can be
mounted differently in each container at the same time; nothing is refused for
differing. `watch=` and `prefetch=` are per share instead: the share runs at the
strongest any of its mounts asked for (`coarse` over `partial` over `off`,
`eager` over `tree` over `off`), and it does not drop back while the session
runs. Our words are removed before the daemon sees the mount.

**`docker run -v`**: the third field is a comma-separated list, `ro,read=cached`,
never `:read=cached:ro`.

```bash
docker run -v ./project:/app:read=cached img
docker run -v ./project:/app:ro,read=cached,watch=partial img
docker run -v ./src:/app:read=cached,write=back,prefetch=tree,owner=1000.2000 img
docker run -v ./target:/app/target:write=ephemeral img
docker run -v ./data:/data:watch=off img      # holds for read=direct,write=through
```

```powershell
docker run -v "${PWD}\project:/app:ro,read=cached,watch=partial" img
docker run -v "${PWD}\src:/app:read=cached,write=back,owner=1000.2000" img
```

**`--mount`** is split on commas by the CLI, so the words go in one csv-quoted
field named `consistency`, the quotes kept for the CLI to read. A single word
needs none, which is also all PowerShell needs:

```bash
docker run --mount 'type=bind,src=./project,dst=/app,"consistency=read=cached,write=back"' img
docker run --mount 'type=bind,src=./project,dst=/app,"consistency=read=cached,owner=1000:2000"' img
```

```powershell
docker run --mount "type=bind,src=${PWD}\project,dst=/app,consistency=read=cached" img
docker run --mount "type=bind,src=${PWD}\project,dst=/app,consistency=owner=1000:2000" img
```

**Compose** carries the words in the long volume syntax, as `consistency:`.
Compose sends a bind through the API with the field only when the bind also
sets an option such as `create_host_path: false`, so write both:

```yaml
    volumes:
      - type: bind
        source: ./workspace
        target: /workspace
        consistency: read=cached,write=back,owner=1000:2000
        bind:
          create_host_path: false
```

Compose's short syntax (`./project:/app:read=cached`) silently drops an option it
does not know, so the mount gets the default mode; the client prints a warning
naming the volume. `REMOTE_DOCKER_CONSISTENCY` changes the default for every
mount, short syntax included.

Defaults for whole directories, in `~/.remote-docker.json`:

```json
{"consistency": "read=cached", "consistencyPaths": {"/home/alice/app/target": "write=ephemeral"}}
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

The setting is the default; a mount can name its own with `watch=`, so one
hot-reloading directory costs watches and the rest do not
([Mount options](#mount-options)). `read=cached` and any `write=` other than
`through` need the watcher to stay coherent, so they are watched at least at
`partial` and refuse `watch=off`.

## Faster access to a shared directory

Reading through the share costs a round trip per file, and the mount
revalidates any attribute older than a second. The cost is latency, not
bandwidth: a 10 Mbit link costs almost nothing and 160ms RTT costs 400x
([ADR 0042](docs/adr/0042-mount-consistency-modes.md)). Per-mode measurements
are the table in [ADR 0045](docs/adr/0045-prefetch-follows-the-reads.md),
produced by `test/bench.sh` (the `bench` label on a pull request).

Docker's own mount consistency says a directory may be cached, and every client
already parses it, so `cached` works as it is. The words, their defaults and
how to write them in `-v`, `--mount` and compose are in
[Mount options](#mount-options).

`read=cached` is the mount itself and works wherever a share works. Any
`write=` other than `through` (including Docker's `delegated`) makes a union,
which needs `fuse-overlayfs` in the image the account's daemon runs and FUSE in
the [workspace kernel](#running-a-workspace); a workspace that cannot make one
says so before anything is created.

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

**Prefetch is off by default**, and applies only to a `read=cached` union
([ADR 0045](docs/adr/0045-prefetch-follows-the-reads.md) says why it is off).
Turn it on with `REMOTE_DOCKER_PREFETCH` or a mount's `prefetch=`.

**`read=cached` and every union need [file watching](#file-watching)**, so they
turn it on for their directory whatever `REMOTE_DOCKER_WATCH` says. An edit to an existing file arrives at once; a file
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
What a union needs from the workspace kernel is under
[the kernel floor](#running-a-workspace).
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

- **`write=back` and `write=ephemeral` (a union) need `fuse-overlayfs`, FUSE
  in the workspace kernel and `/dev/fuse` in the account's daemon container.**
  The agent runs fuse-overlayfs itself, as root, and asks the daemon whether it
  has the binary and the device, so no kernel version is read. fuse-overlayfs's
  own 4.18 is for mounting from a user namespace (rootless engines); moby's
  fuse-overlayfs *storage driver*, a different thing, hard-codes a 4.18 check.
  Neither states a floor for a root mount, and no kernel older than the CI
  runners' 6.x has served a union here: that is a judgement, so expect 4.18.
  RHEL 7 shipping the package (`fuse-overlayfs-0.7.2-6.el7_8` in Extras,
  [RHEA-2020:1222](https://access.redhat.com/errata/RHEA-2020:1222)) is **not**
  evidence against that: Red Hat backported the kernel side into its 3.10
  branch and [said so](https://www.redhat.com/en/blog/rhel-78-and-final-update-container-tools).
  *(Checked 2026-10-14 against
  [the README](https://github.com/containers/fuse-overlayfs/blob/main/README.md),
  [the man page](https://github.com/containers/fuse-overlayfs/blob/main/fuse-overlayfs.1.md)
  and moby `daemon/graphdriver/fuse-overlayfs/fuseoverlayfs.go`, which calls
  `kernel.CheckKernelVersion(4, 18, 0)` at v24.0.9; 4.18 is Linux commit
  `4ad769f3c346`, "fuse: Allow fully unprivileged mounts". On a workspace:
  `uname -r; ls -l /dev/fuse; grep fuse /proc/filesystems; fuse-overlayfs --version`.
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
through an ordinary Ingress (the tunnel is an HTTP upgrade). Enrol a device
with a token, whose invite names `wss://ws.example.com/`, or with
`authorizedKeys` as above:

```bash
kubectl exec -n remote-docker ws-remote-docker-workspace-0 -- \
  remote-dockerd token create --account alice
```

[`charts/remote-docker-workspace/README.md`](charts/remote-docker-workspace/README.md)
has the values, which storage driver your volumes need, why both volumes are
ReadWriteOnce, and what `replicas` above 1 means: several independent
workspaces, each client pinned to one. CI installs the chart on a kind cluster behind ingress-nginx on
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

`state/` holds the host keys, the uid map, the keys enrolled with a token and
the unredeemed tokens, and **must persist**. Losing it gives every client a
changed-host-key warning, revokes every key enrolled with a token, and
reassigns every account's uid, which changes its tunnel port and orphans the
ownership of everything it has written.

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

To enrol with a token instead of a file, mint it as root with the address
devices reach: `sudo remote-dockerd token create --account alice --url
ssh://vm.example:2222`. Run from a shell it does not read
`/etc/remote-docker/env`, so `WORKSPACE_PUBLIC_URL` set there is not seen.

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
| `WORKSPACE_KEYS_DIR` | `<state>/authorized_keys.d` | the operator's keys, one `<account>.pub` per user; a comma-separated list for several directories. The agent only reads them |
| `WORKSPACE_ENROLLED_KEYS_DIR` | `<state>/enrolled_keys.d` | the one directory the agent writes keys into; must not overlap a keys directory |
| `WORKSPACE_HOSTKEY_DIR` | `<state>/host_keys` | |
| `WORKSPACE_TOKENS_DIR` | `<state>/tokens` | single-use [enrolment tokens](#enrolment), one file each, holding a hash of the secret |
| `WORKSPACE_PUBLIC_URL` | empty | the address an enrolment invite names, such as `wss://ws.example/`; `token create --url` overrides it |
| `WORKSPACE_ADMINS` | empty | accounts, comma-separated, that manage every account from their own machine; see [Admins](#admins) |
| `WORKSPACE_USER_TOKENS` | `true` | `false` lets only admins create tokens under `remote`; see [Admins](#admins) |
| `WORKSPACE_KEY_POLL_INTERVAL` | `60` | seconds; every keys directory is polled as well as watched |
| `WORKSPACE_DOCKERD_ARGS` | empty | passed to the workspace's own dockerd |
| `WORKSPACE_ENABLE_DIND` | `true` | |
| `WORKSPACE_PER_USER_DIND` | `true` | a daemon per account; `false` shares one |
| `WORKSPACE_DIND_IMAGE` | `WORKSPACE_IMAGE`, else `docker:28-dind` | image a per-account daemon runs. Stock dind has no `fuse-overlayfs`, so no unions and no fuse-overlayfs storage driver; the agent logs a warning when a daemon starts without it. Changing it recreates a daemon once it is idle |
| `WORKSPACE_DIND_STORAGE_DRIVER` | inherited from `WORKSPACE_DOCKERD_ARGS` | |
| `WORKSPACE_DIND_MOUNTS` | empty | extra bind mounts for every per-account daemon, and the paths a bind may name; see below |
| `WORKSPACE_DAEMON_READY_TIMEOUT` | `180` | seconds a cold per-account daemon has to answer; see below |
| `WORKSPACE_SHELL` | `/bin/bash` | shell an SSH session lands in |
| `WORKSPACE_UID_BASE` | `10000` | first uid handed to an account |
| `WORKSPACE_PORT_BASE` | `30000` | first reverse-tunnel port; an account's first port is `PORT_BASE + (uid - UID_BASE)` |
| `WORKSPACE_ACCOUNT_PREFIX` | `rd-` | prefix of the unix user behind an account (`rd-alice`) |
| `WORKSPACE_EPHEMERAL_ACCOUNTS` | empty | accounts, comma-separated, whose every client process gets its own port and volumes, removed after its grace (ADR 0050) |
| `WORKSPACE_EPHEMERAL_MAX_CLIENTS` | `8` | runs per ephemeral account, connected or in grace; the next is refused |
| `WORKSPACE_EPHEMERAL_GRACE` | `2m` | how long a run keeps its port after its last connection, for a reconnect. A Go duration or whole days, such as `90s` or `1d` |
| `WORKSPACE_EPHEMERAL_CLEANUP_CONTAINERS` | `false` | also remove an expired run's containers and its compose networks; without it a run whose containers remain keeps its slot until they are removed |
| `WORKSPACE_PRUNE_AFTER` | `7d` | remove images no container has used for this long, and build cache unused that long. A Go duration or whole days, such as `36h` or `7d`; `0` or `off` disables it. Never volumes. See [Pruning](#pruning) |
| `WORKSPACE_PRUNE_ABOVE` | `80%` | above this disk usage of the daemons' data directory, remove unused build cache and images whatever their age, oldest first; `off` disables it |
| `WORKSPACE_PRUNE_CONTAINERS` | `false` | also remove containers stopped for longer than `WORKSPACE_PRUNE_AFTER`; never the workspace's own daemon containers |
| `WORKSPACE_METRICS_ADDR` | empty | address such as `:9090` to serve Prometheus metrics on, in plain HTTP at `/metrics`; empty serves none. See [Metrics](#metrics) |
| `WORKSPACE_IMAGE` | | the workspace's own image; set by elevation and by `deploy/docker-compose.yml` |
| `WORKSPACE_SELF` | | this task's name, set by `deploy/swarm.yml` |
| `WORKSPACE_ELEVATED` | | *internal.* Set by the agent on the privileged child it starts for Swarm, so a misconfiguration cannot elevate in a loop ([ADR 0013](docs/adr/0013-self-elevation-instead-of-a-launcher.md)); not for you to set |
| `WORKSPACE_HOST_SOCKET` | `/var/run/host-docker.sock` | the node's Docker socket, for Swarm elevation |
| `WORKSPACE_DATA` | `/var/lib/remote-docker` | read by `deploy/swarm.yml`, not by the agent |

### Pruning

The agent removes what a daemon no longer needs, on by default
([ADR 0056](docs/adr/0056-the-agent-prunes-unused-images-and-build-cache.md)).
It covers the workspace's own daemon and every account's, including a VM's
own dockerd.

- **By age.** An image no container, running or stopped, has used for
  `WORKSPACE_PRUNE_AFTER` is removed. Its age is when a container last used it,
  tracked by the agent under `/etc/workspace/prune/`, not when it was built. The
  first time the agent sees an image counts as used, so an upgrade, or a lost
  record, delays removal and never hastens it. Build cache unused that long goes
  too.
- **By usage.** Every five minutes the agent measures the filesystem holding
  the workspace's `/var/lib/docker`, where every account's storage sits. Above
  `WORKSPACE_PRUNE_ABOVE` it removes build cache unused for an hour, then unused
  images, oldest first, until usage is below the threshold. An image used in the
  last hour is never removed this way. If the disk stays above the threshold with
  nothing left to remove, the log says so once.
- **Never** volumes, an image a container (even a stopped one) still uses, or the
  image an account's daemon runs. Stopped containers are removed only with
  `WORKSPACE_PRUNE_CONTAINERS=true`, and never the workspace's own daemon
  containers. A daemon that is not running is not started to be pruned.
- **Off:** `WORKSPACE_PRUNE_AFTER=off` and `WORKSPACE_PRUNE_ABOVE=off`.
- Each prune that removed something is one log line naming the account, how
  much it removed and the space it freed, and the Prometheus counter
  `remote_docker_prune_reclaimed_bytes_total{account,reason}` adds it up.

An image another machine pulled and has not used for the age is pulled again
the next time somebody needs it, which is the whole cost.

### Metrics

Set `WORKSPACE_METRICS_ADDR` (`metrics.enabled` in the chart) and the agent
serves `GET /metrics` in the Prometheus text format on that address. It is a
listener of its own, never the SSH or WebSocket port. Labels are account names
and fixed words, never keys, client ids or token ids
([ADR 0054](docs/adr/0054-metrics-on-an-opt-in-plain-http-listener.md)).

**It has no authentication.** Anything that reaches the port can read which
accounts exist and what they are doing. `:9090`, which the chart uses, listens
on every interface: restrict it with a NetworkPolicy that admits only your
Prometheus, or bind `WORKSPACE_METRICS_ADDR` to one address, such as
`127.0.0.1:9090` or a private interface.

| metric | type | labels |
|---|---|---|
| `remote_docker_ssh_connections` | gauge | `account`: connections logged in as it |
| `remote_docker_ephemeral_runs` | gauge | `account`, `state` (`live`, `grace`, `cleaning`) |
| `remote_docker_ephemeral_run_ports` | gauge | ports held by ephemeral runs |
| `remote_docker_ephemeral_runs_refused_total` | counter | `reason`: `limit`, `cleaning`, `duplicate` (a connection naming a second run), `malformed` |
| `remote_docker_ephemeral_sweeps_total` | counter | sweeps for expired runs |
| `remote_docker_ephemeral_objects_removed_total` | counter | `kind`: `container`, `network`, `volume` |
| `remote_docker_ephemeral_objects_kept_total` | counter | `kind`, `reason`: `in_use`, `mounted`, `error` |
| `remote_docker_prune_reclaimed_bytes_total` | counter | `account`, `reason`: `age`, `pressure`; bytes the free space grew by around a prune (ADR 0056). `account` is also `_parent` for the workspace's own daemon of a daemon per account, and `_shared` |
| `remote_docker_token_redemptions_total` | counter | `outcome`: `ok`, `pending` (the account is still being created), `refused`, `failed` (the workspace's side) |
| `remote_docker_token_limiter_rejections_total` | counter | redemptions turned away after too many failures |
| `remote_docker_account_daemons` | gauge | per-account daemons the agent has started or adopted; not probed, so one that died is still counted. Absent with a shared daemon |
| `remote_docker_token_redemption_duration_seconds` | histogram | `outcome`, as for redemptions; includes waiting for a new account to be created |
| `remote_docker_daemon_start_duration_seconds` | histogram | a per-account daemon starting or restarting until it answers; failed starts are not observed, and with a shared daemon it stays empty |
| `remote_docker_account_provision_duration_seconds` | histogram | `outcome`: `ok`, `failed`; creating an account's unix user |
| `remote_docker_ephemeral_sweep_duration_seconds` | histogram | one sweep for expired runs, cleanup included |
| `remote_docker_build_info` | gauge | `version`, `goversion`; always 1 |
| `go_goroutines`, `go_memstats_heap_alloc_bytes` | gauge | the Go runtime |
| `process_cpu_seconds_total`, `process_resident_memory_bytes`, `process_open_fds`, `process_start_time_seconds` | counter, gauges | the agent process, under the standard names; Linux only |

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
| `remote-dockerd ephemeral ls` | the runs of ephemeral accounts holding a port, with state and age |
| `remote-dockerd token create --account <name> \| --unbound` | mint an [enrolment token](#enrolment) and print the line a device runs |
| `remote-dockerd token ls` / `rm <id>` | every token not yet redeemed, and withdrawing one |

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

**With a token**, the device enrols its own key. Inside the workspace, mint one:

```bash
docker exec <workspace> remote-dockerd token create --account alice
kubectl exec ws-remote-docker-workspace-0 -- remote-dockerd token create --account alice
```

It prints one line to run on the device:

```text
docker remote create ws --token rdt1.eyJ1Ijoi...
single use, for account alice, expires 2026-10-06 12:00 UTC
```

The invite carries the workspace's address (`--url`, else
`WORKSPACE_PUBLIC_URL`), the fingerprint of its host key, which the client
then requires instead of trusting the first key it sees, and the token. The
device logs in with the token and its own key, and the agent saves that key in
the enrolled directory below. A token is single use and lives 24 hours by
default (`--expires`, at most 7 days). `--account` adds the key to that
account, creating it if need be; `--unbound` creates a new account under the
name the device gives with `--user`, never one that exists or ever existed.
An invite does not carry `--ca-file` or `--insecure`: for a `wss://`
workspace whose certificate the device does not already trust, pass them to
`remote create` beside `--token`.
`token ls` and `token rm <id>` list and withdraw tokens. A workspace whose
enrolled directory is not writable refuses to mint one
([ADR 0051](docs/adr/0051-enrolling-a-key-with-a-single-use-token.md)).

**By file**, out of band: someone with access drops a `<account>.pub` into the
keys directory, one key per line. The filename is the account name a client logs in
as, lowercased, so `Alice.pub` enrols `alice` and `ssh Alice@...` reaches it;
the unix user behind it is `rd-<account>`
([ADR 0025](docs/adr/0025-the-agent-as-a-guest.md)). Emptying or removing the
file revokes access, and ends every connection that key has open, but keeps
the account, its home directory and its daemon's images, containers and
volumes, which go on taking disk space and nothing reports. To remove an
account for good, use `remote user rm <account> --purge` (below) instead; for
one whose file is already gone, `remote-dockerd daemons reset <account>
--purge` on the workspace deletes its Docker state.

`WORKSPACE_KEYS_DIR` may name several directories, separated by commas, and the
agent only ever reads them. It writes keys it enrols itself into one other
directory, `WORKSPACE_ENROLLED_KEYS_DIR`, which defaults to
`<state>/enrolled_keys.d` and must not be, contain or sit inside a keys
directory: the agent refuses to start otherwise. An account's files in all of
them are merged, so `alice.pub` in two directories is one account holding the
keys of both, and it is revoked only once no directory enrols it
([ADR 0052](docs/adr/0052-operator-keys-directories-and-one-enrolled-keys-directory.md)).
Edit by hand only in a keys directory: the agent's writes take a lock that a
hand edit in the enrolled directory does not.

Every directory is re-read on change and polled every 60 seconds, because
inotify never fires for a change made on another host when that directory is
on shared storage.

### Admins

Any account manages its own tokens and keys from its own machine:
`remote token create` prints a line that enrols another machine into the
account, and `remote key ls|add|rm` lists and changes its keys. Removing the
key this machine connects with, or the last one, needs `-f`.
`WORKSPACE_USER_TOKENS=false` (`userTokens: false` in the chart) takes token
creation away from non-admins; they can still list and withdraw their own.

The accounts in `WORKSPACE_ADMINS` (comma-separated; `admins` in the chart)
manage every account:

```bash
docker remote token create --account carol     # or --unbound: the device names it
docker remote user ls
docker remote user rm bob                      # -f while bob's containers run
docker remote user rm bob --purge              # and delete everything bob had
docker remote key rm SHA256:… --account bob
```

`user rm` revokes the account's keys and closes its connections, withdraws its
tokens, and removes its daemon container. Its images, volumes, home directory
and uid are kept, so a new token for the same name brings it back as it was.
`--purge` also deletes its images, containers and volumes, its home directory,
its unix user and its port records, and cannot be undone. Its uid is still
kept, so the name is never given to anybody else, and a new token for it
starts empty. With a shared daemon the containers on it are left alone.
An admin cannot remove its own account, nor the last admin who holds a key,
nor an account or key an operator's keys directory enrols. Only the operator
edits `WORKSPACE_ADMINS`: removing an admin still named there says so, since
the name keeps its rights. The first admin enrols with a token the operator
mints: `remote-dockerd token create --account alice`
([ADR 0053](docs/adr/0053-admins-named-by-the-operator-and-account-management-under-remote.md)).

An admin can create accounts, and an account is a privileged daemon, so name
admins as you would give somebody a shell on the host.

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
  be pruned. The agent's own [pruning](#pruning) touches images and build cache
  only, never a volume.
- **A `rd-*-cache` volume looks unused and is not.** The daemon calls a volume
  in use only while a container names it, and a cache behind a `write=back` or
  `write=ephemeral` share is bound into the container by path, so it always
  looks unused. Deleting "unattached" volumes removes the cache under a running
  container, and with it the container's writes not yet carried back.
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

Every file in a share is reported as owned by root (`0:0`) with mode 0666,
every directory 0777, so an image that runs as its own user can read and write
the share. A `chown` inside the container is accepted and does nothing, and a
read-only bind (`ro`) is still read-only
([ADR 0046](docs/adr/0046-a-share-reports-wide-mode-bits.md)).

git refuses a repository owned by another uid, so an image that runs as a
user needs the share to report that user. Name it on the mount with `owner=`, or
for every mount with `REMOTE_DOCKER_SHARE_OWNER`, as `uid` or `uid:gid` (gid
defaults to the uid); the separator rules for `-v` are in
[Mount options](#mount-options). It applies to every kind of mount, a
`write=back` union's root included. One directory can have several owners at
once: a mount naming another owner is a share of its own, beside the first,
with its own volume. Containers running together under different owners see
the same files.

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

For everything else, use `--mount`, which Git Bash does not mangle, or turn the
conversion off:

```bash
docker run --mount type=bind,source="$PWD",target=/app alpine ls /app

MSYS_NO_PATHCONV=1 docker run -w /src -v "$(pwd -W)":/src alpine ls  # Git Bash
MSYS2_ARG_CONV_EXCL='*' docker run -w /src -v "$(pwd -W)":/src alpine ls  # other MSYS2 shells
```

`MSYS_NO_PATHCONV` is Git for Windows' own variable; plain MSYS2 and MinGW
shells ignore it and read `MSYS2_ARG_CONV_EXCL`. With conversion off, write
this machine's paths the Windows way: `"$PWD"` is `/c/Users/alice/x`, which
Windows reads as `C:\c\Users\alice\x`, so use `"$(pwd -W)"` or `C:/Users/…`.
A leading double slash (`//src`) protects a single argument instead.
*(Checked 2026-10-19: both variables in Git Bash with `cmd.exe /c echo -w /src`;
MSYS2 documents only `MSYS2_ARG_CONV_EXCL`, at
<https://www.msys2.org/docs/filesystem-paths/>.)*

### What differs from a bind mount

`test/probes/fsprobe` runs one fixed sequence of filesystem operations inside
a container against a plain bind mount and against a share from a Linux and a
Windows client; CI fails on any difference not listed in
`test/fs-conformance/deviations-*.txt`. The differences:

| behaviour | on a share | why |
|---|---|---|
| owner and mode | every file is owned by root (`0:0`), or by `REMOTE_DOCKER_SHARE_OWNER`; a file reads back `0666`, plus `0111` where the real file is executable, and always `0777` from a Windows host, where there is no execute bit to preserve | ADR 0046, `Attrs.AlwaysExecutable` |
| `chown` | accepted, changes nothing | ownership is synthesised |
| `utime`, `touch -d` | accepted, changes nothing: every SETATTR of times is dropped, so a `touch` does not move a file's mtime | the watcher replays a SETATTR to invalidate, and applying one looped |
| `chmod` | reaches the file on this machine, with the owner's read and write forced on; the mode read back is still the synthesised one, so the execute bit on a Linux host is all a container can observe | the share is served as that owner |
| `git` in a container | works as root; an image running as another user sets `REMOTE_DOCKER_SHARE_OWNER` to its uid, or git refuses the repository as "detected dubious ownership" | git compares the share's owner with its own uid |
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

- **Sockets, devices and FIFOs**, alone or inside a shared directory: a file
  share carries the name, not the kernel object. (`--device` is unaffected; it
  names a device on the workspace.)
- **The one exception is the Docker socket.** `-v
  /var/run/docker.sock:/var/run/docker.sock` is passed through, not exported,
  and reaches the daemon the container runs on: your account's own, never the
  workspace's parent (ADR 0049).
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
faithfully. `coarse` approximates it with a directory-level event (`integration.sh` 14b); `partial` says nothing rather than something wrong. A watcher that
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
  run by hand on one computer**, never in CI.
- **Swarm itself** needs a real cluster; only the elevation mechanism is tested.
  The systemd unit is not exercised either.
- **Every suite mounts a share on one kernel**, the runner's 6.x, so a mount
  option newer than the [3.10 floor](#running-a-workspace) would pass CI and fail
  every mount on an older workspace. A table guards it, not a test run.
- **Write-back conflicts** are unit tested only.
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
  enrol/               enrolment tokens and account management, entire
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
  tokens/              single-use enrolment tokens
  replay/              replaying changes as real syscalls
  union/               the cache over the live export
  netns/  wslisten/    another process's netns; SSH over a WebSocket

client/                the client binary (docker/cli, buildx)
  cmd/remote-docker/   also answers to `docker`
  internal/            api proxy, bind rewriting, ports, session

agent/                 the agent binary (five direct third-party requires)
  cmd/remote-dockerd/  the workspace binary
  internal/            per-account daemons, dockerd supervision, elevate,
                       ephemeral runs, metrics

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

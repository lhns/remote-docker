# remote-docker

[![build](https://github.com/lhns/remote-docker/actions/workflows/release.yml/badge.svg)](https://github.com/lhns/remote-docker/actions/workflows/release.yml)
[![integration](https://github.com/lhns/remote-docker/actions/workflows/integration.yml/badge.svg)](https://github.com/lhns/remote-docker/actions/workflows/integration.yml)

Use Docker from a machine that cannot have Docker installed, with your own
directories **really mounted** into the containers — not copied, not synced —
and published ports reachable locally.

One binary. Nothing else to install: the SSH client, the NFS server and the
Docker CLI are all inside it.

```
docker run --rm -v D:\data:/data alpine ls /data     # a directory on YOUR machine
docker compose up -d                                  # ports land on YOUR localhost
```

## Read this first

**File watchers need `REMOTE_DOCKER_WATCH` turned on.** By default, a container
watching a directory on the share receives *no* inotify events when you change
a file — measured, not assumed. NFS carries no change-notification protocol,
so the container's kernel is simply never told. **Hot reload silently does
nothing**: vite, webpack, nodemon, `air` and `dotnet watch` sit there while
the file is plainly present.

Turning it on makes remote-docker watch this machine and replay each change
inside the workspace as a real syscall, so the kernel there emits a genuine
inotify event:

```bash
export REMOTE_DOCKER_WATCH=partial    # writes and creations
export REMOTE_DOCKER_WATCH=coarse     # also deletions, approximately
```

| mode | what happens |
|---|---|
| `off` *(default)* | nothing is watched; hot reload does not work |
| `partial` | writes and creations fire real events. Deletions are not reported at all — nothing is ever misrepresented |
| `coarse` | as `partial`, plus a directory-level event for deletions and renames. A watcher that rescans notices; one that trusts the event *kind* is told something untrue |

**Deletions are the honest gap.** `unlink` of a name that is already gone fails
before the kernel generates anything, so a deletion cannot be replayed
faithfully. `coarse` approximates it; `partial` says nothing rather than
something wrong. [ADR 0014](docs/adr/0014-inotify-does-not-see-client-changes.md)
stays open on exactly this, and
[ADR 0016](docs/adr/0016-replaying-change-events-as-real-syscalls.md) records
how the rest works.

**Off by default, because watching is not free.** inotify is not recursive:
`inotify_add_watch` covers exactly one directory and reports only its direct
entries, so a tree costs one watch per directory in it. macOS is worse — kqueue
needs an open file descriptor per *file*, not per directory. Only the
directories you actually share are watched, never your whole disk, and there is
a cap:

| platform | default cap | what binds |
|---|---|---|
| Linux | 4096 directories | `fs.inotify.max_user_watches` (8192 by kernel default; many distros raise it) |
| Windows | 1024 directories | one `ReadDirectoryChangesW` buffer per watch |
| macOS | 512 directories | `RLIMIT_NOFILE`, because kqueue costs an fd per file |

`.git`, `node_modules`, `.venv`, `__pycache__`, `.gradle` and `.terraform` are
skipped. Build outputs like `dist/` and `target/` are **not**, because serving
`dist/` and reloading when it changes is exactly the workflow this is for — so
a Rust or Java tree will spend its budget inside `target/` and say so. Tune
with `REMOTE_DOCKER_WATCH_BUDGET` and `REMOTE_DOCKER_WATCH_EXCLUDE` (comma- or
`PATH`-separated). When the budget runs out the directory it stopped at is
named, never silently dropped.

Watching starts delivering once the session has actually connected, which
happens on the first Docker command rather than when the session starts. Edits made
before that are counted and reported, not silently lost — and nothing is
watching inside a container that does not exist yet.

The per-tool polling flags — `CHOKIDAR_USEPOLLING=1`, `WATCHPACK_POLLING=true`,
`--poll` — still work and are never set for you: silently changing your build
tool's behaviour is worse than telling you about the limitation.

## Getting started

```bash
# 1. tell it where the workspace is
export REMOTE_DOCKER_HOST=workspace.example        # or --host, or ~/.remote-docker.json

# 2. get your key enrolled (out of band -- hand it to whoever runs the workspace)
remote-docker enroll

# 3. start a session in the background
remote-docker start
```

`start` prints the endpoint and returns -- no terminal to keep open. Point
Docker at it:

```bash
docker --context <workspace> ps        # the context was created with the workspace
# or
export DOCKER_HOST=unix:///…/docker.sock
```

Then use Docker normally. If you have no Docker CLI at all, the binary carries
one: `remote-docker docker ps`, `remote-docker docker build .`

The endpoint is a **named pipe** on Windows (`\\.\pipe\docker_remote`) and a
unix socket elsewhere, owner-only in both cases. Never a TCP port: anything
that can reach it can start containers that read and write your filesystem.
Nothing here needs administrator rights.

## What works

- **Bind mounts from anywhere on your machine** — another drive, above the
  working directory, unrelated to it. Not only a synced project folder.
- **Published ports reach your localhost.** `-p 8080:80` means
  `localhost:8080` here, opened automatically as containers start. No manual
  tunnels.
- **The real tooling, unmodified.** `docker`, `docker compose`,
  Testcontainers, IDE plugins — anything that speaks the Docker API. The
  translation happens at the API, not in a command wrapper.
- **Named volumes stay named volumes.** Only host paths are rewritten.
- **File watchers can see your edits**, once `REMOTE_DOCKER_WATCH` is on —
  writes and creations arrive as genuine inotify events inside the container,
  not as a poll. See above for what that costs and what it does not cover.

Every one of those is asserted end to end on each push, against a real
Docker-in-Docker daemon and a real kernel NFS mount — see
[`test/integration.sh`](test/integration.sh).

## Several workspaces

```bash
remote-docker workspace create dev --host dev.example --user alice --watch partial
remote-docker workspace create ci  --host ci.example  --user alice
```

which writes `~/.remote-docker.json` for you, and creates a docker context per
workspace as it goes — there is no case where you want one and not the other:

```json
{
  "workspaces": {
    "dev": {"host": "dev.example", "user": "alice", "watch": "partial"},
    "ci":  {"host": "ci.example",  "user": "alice"}
  },
  "default": "dev"
}
```

Every setting can also live at the top level, where it applies to all of them.
`watch`, `watchBudget` and `watchExclude` are the file-form spellings of the
`REMOTE_DOCKER_WATCH*` variables above — worth setting per workspace, since
the one you edit against wants watching and a CI one does not.

Each gets its own endpoint, so sessions run side by side:

```bash
remote-docker start --workspace dev
remote-docker start --workspace ci

docker --context dev ps
docker --context ci ps
```

`remote-docker workspace ls` shows them and which is the default, and
`remote-docker workspace inspect <name>` shows one in full.

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
daemon mounts for itself when the container starts — so nothing has to
propagate into a running container, and a bind source anywhere on your disk
works.

The reasoning behind each decision is in [`docs/adr/`](docs/adr/).

## Other things that will bite you

- **Keep build artifacts off the share.** `node_modules`, `.git`, `target/`,
  package caches. Worth roughly 20×; protocol tuning is worth about 2×.
- **No databases on the share.** `nolock` plus `fcntl` locking is a corruption
  risk.
- **Latency multiplies.** NFSv3 is synchronous per operation, so a large tree
  over a WAN is painful. Over a LAN it is fine.
- **A session must be running**, though not in a terminal you are watching.
  `remote-docker start` puts one in the background; it is the endpoint and the
  file server, so stopping it takes running containers' mounts with it. Any
  command that needs one starts it for you, including the built-in Docker CLI.
- **A background session reclaims itself** after 30 minutes with nothing to do,
  and never while a container of yours is running or a stream is open. Change
  it with `REMOTE_DOCKER_DAEMON_IDLE`; a negative value means never.

## Prior art

Nothing else appears to do this. The pieces exist separately —
[docker-injector](https://github.com/rse/docker-injector) mutates
container-create requests, [sshocker](https://github.com/lima-vm/sshocker)
serves files from the client to a VM — but not the combination.

Everyone else answers this problem with **file sync** instead: Mutagen, Okteto,
Docker's own Synchronized File Shares. Testcontainers Cloud proxies a local
socket to a remote runtime and states that mounting local files "is not
implemented". VS Code and Codespaces document it as unsupported.
[docker/compose#8484](https://github.com/docker/compose/issues/8484) was closed
by a stale bot; [podman#13358](https://github.com/containers/podman/issues/13358)
was closed "won't fix, far from trivial".

Sync is not obviously the wrong answer — it makes changes land as ordinary
local writes, so file watchers work. That is very likely the whole reason it
won.

remote-docker answers it differently, and not originally: Docker Desktop ships
the same idea as "Event Injection", forwarding host events into its VM so a
replay thread reproduces them. Linux offers no way to inject a synthetic
inotify event — `fanotify(7)` says so outright — so performing a real
operation is the only mechanism available to anyone. The difference here is
that we own the NFS server as well as the agent, which is what keeps the
replay from echoing back as a change of its own
([ADR 0016](docs/adr/0016-replaying-change-events-as-real-syscalls.md)).

## Running a workspace

The workspace runs one binary, `remote-dockerd`: it supervises dockerd,
provisions an account per enrolled key, and serves SSH itself. There is no
sshd, no sudo and no shell scripts in the image.

### The image

Published to GHCR for `linux/amd64` and `linux/arm64`:

```
ghcr.io/lhns/remote-docker-workspace:<version>   # on a v<version> tag
ghcr.io/lhns/remote-docker-workspace:latest      # on a v<version> tag
ghcr.io/lhns/remote-docker-workspace:sha-<short> # every commit to main
```

`latest` only exists once a `v*` tag has been pushed. Before that, pin a
`sha-` tag — `deploy/swarm.yml` takes `WORKSPACE_IMAGE` to override its
default.

Or build it yourself. The context is the repository root, because the agent is
compiled from source:

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
uid — which changes its tunnel port and orphans the ownership of everything it
has written.

The container is privileged, because dind runs its own daemon, sets up its own
bridge and iptables rules, and mounts NFS in its own namespace.

### Docker Swarm

Swarm cannot run privileged tasks, so the service starts **unprivileged** and
relaunches itself through the node's Docker socket — `remote-dockerd elevate`,
no launcher image involved ([ADR 0013](docs/adr/0013-self-elevation-instead-of-a-launcher.md)).

Two things must be true before `deploy/swarm.yml` will work, and neither
fails in an obvious way:

```bash
# 1. Label the node that will run it. Without this the service is accepted
#    and then never schedules, with no error anywhere.
docker node update --label-add workspace=true <node>

# 2. Create the state directories ON THAT NODE. They are bind mounts, so a
#    missing path is created as an empty root-owned directory rather than
#    reported.
export WORKSPACE_DATA=/var/lib/remote-docker        # the default; override freely
ssh <node> "mkdir -p $WORKSPACE_DATA/{state,authorized_keys.d}"

docker stack deploy -c deploy/swarm.yml workspace
```

If you point `WORKSPACE_DATA` at Ceph- or NFS-backed storage — worth doing if
the workspace should survive moving nodes — you must also set
`WORKSPACE_DOCKERD_ARGS=--storage-driver=fuse-overlayfs`. overlay2 refuses such
a filesystem outright, and vfs, the only other fallback, copies every layer.

Port 2222 is published with `mode: host`, so it lands on the node actually
running the task rather than being round-robined by the routing mesh to nodes
where the privileged container does not exist. That node's 2222 is what
clients connect to.

**The host Docker socket mount in `swarm.yml` is the whole trust boundary.**
Whoever can deploy this stack can already start privileged containers on the
node; elevation does not widen that, it just avoids a second image doing it.
The socket is deliberately *not* passed to the privileged child.

### Enrolment

Out of band: someone with access drops a `<name>.pub` into the keys directory,
and the filename becomes that user's unix account. Removing a key revokes
access but keeps the account and its home directory — a key file is removed
far more often than a person leaves for good.

The keys directory is re-read on change and polled every 60 seconds, because
inotify never fires for a change made on another host when that directory is
on shared storage.

### A daemon per account

Set `WORKSPACE_PER_USER_DIND=true` and each enrolled account gets its own
Docker daemon, behind the same single SSH port
([ADR 0019](docs/adr/0019-a-dockerd-per-account.md)). Accounts stop seeing each
other's containers, images and volumes, two accounts can publish the same port
at once, and a shell lands on its own daemon.

**It is separation, not isolation, and the difference matters.** Each
per-account daemon runs privileged, which is root on whatever hosts it, so a
determined account can still break out and reach another's. What this buys is
that nobody sees anyone else's work *by accident* -- which is the failure that
actually happens. A workspace is still a shared machine; treat it as one.
Genuine isolation is still one workspace container per account.

It costs real resources, and there is no mitigation worth implying:

- **the layer cache is duplicated.** Five accounts on `node:22` is five copies.
  A registry mirror recovers bandwidth but not disk -- Docker has no shared
  read-only image store.
- **memory**, roughly 100--150MB per idle daemon plus containerd.
- **disk becomes a shared failure mode.** One account's runaway build can fill
  the volume and take down every other account's daemon.
- **3--10s** for an account's first connection after the daemon has stopped.

`WORKSPACE_DIND_IMAGE` overrides the dind image;
`WORKSPACE_DIND_STORAGE_DRIVER` is **not** inherited from the parent, so a
Ceph-backed deployment that sets `--storage-driver=fuse-overlayfs` below must
set it here too.

#### Turning it on is a breaking change

Images and volumes an account built under the shared daemon are invisible from
its own, and there is no cheap migration. Set `WORKSPACE_PER_USER_DIND=false`
to keep the old behaviour; the old data is still in the shared
`/var/lib/docker` if you change your mind.

**With it off, all enrolled users share one Docker daemon** and can see each
other's containers ([ADR 0012](docs/adr/0012-shared-dockerd-across-users.md)).

## Commands

| | |
|---|---|
| `remote-docker enroll` | print the public key to hand over for enrolment |
| `remote-docker workspace create <name> --host …` | add a workspace and create its docker context |
| `remote-docker workspace rm <name>` | remove both again |
| `remote-docker workspace ls` | list them (`workspaces` still works) |
| `remote-docker workspace use <name>` | choose which one commands use by default |
| `remote-docker workspace inspect [name]` | its settings, endpoint, docker context and whether a session is up |
| `remote-docker start` | start a background session and return |
| `remote-docker start --foreground` | run it in this terminal instead |
| `remote-docker stop` | stop it |
| `remote-docker restart` | stop and start, refusing while something depends on it |
| `remote-docker status` | what the workspace reports about this account |
| `remote-docker docker …` | the embedded Docker CLI |
| `remote-docker gc` | remove share volumes nothing is using |
| `remote-docker version` | |

The workspace verbs are docker's, and the old spellings -- `add`, `remove`,
`list`, `default` -- still work. There is no `context` command: a docker
context is written when a workspace is created and removed when it is, because
there is no case where you want a workspace configured and not reachable as
`docker --context <name>` ([ADR 0018](docs/adr/0018-one-way-to-do-each-thing.md)).
Re-run `workspace create` to rewrite a context that has drifted.

For a shell on the workspace, use `ssh`. The agent serves one to any enrolled
key.

On the workspace: `remote-dockerd serve` is the agent (the image's default),
and `remote-dockerd elevate` is the Swarm entry point.

## Layout

```
cmd/remote-docker/     the client
cmd/remote-dockerd/    the workspace agent
pkg/workspace/         the contract both sides share
internal/client/       ssh, nfs server, api proxy, bind rewriting, ports,
                       filesystem watching
internal/server/       the agent, including change replay
image/  deploy/        the workspace container and its deployments
docs/adr/              why everything is the way it is
test/                  integration suite and probes
```

## Development

```bash
go test ./...                 # no daemon needed
golangci-lint run ./...
bash test/integration.sh      # needs docker and NFS client support
```

Licensed under Apache 2.0.

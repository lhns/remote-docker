# remote-docker

[![ci](https://github.com/lhns/remote-docker/actions/workflows/ci.yml/badge.svg)](https://github.com/lhns/remote-docker/actions/workflows/ci.yml)
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

**File watchers do not see your edits.** A container watching a directory on
the share receives *no* inotify events when you change a file — measured, not
assumed. Polling sees the change immediately; inotify sees nothing at all.

This is inherent to NFS, which carries no change-notification protocol. It
means **hot reload does not work**: vite, webpack, nodemon, `air` and
`dotnet watch` will sit silently doing nothing while the file is plainly
there.

Workarounds are per-tool polling flags — `CHOKIDAR_USEPOLLING=1`,
`WATCHPACK_POLLING=true`, `--poll`. They are not set for you, because silently
changing your build tool's behaviour is worse than the limitation.

remote-docker is good at builds, tests, `docker compose up`, and any tool that
reads its inputs when it starts. It is not currently good at the edit-reload
loop. See [ADR 0014](docs/adr/0014-inotify-does-not-see-client-changes.md),
which is deliberately left open.

## Getting started

```bash
# 1. tell it where the workspace is
export REMOTE_DOCKER_HOST=workspace.example        # or --host, or ~/.remote-docker.json

# 2. get your key enrolled (out of band -- hand it to whoever runs the workspace)
remote-docker enroll

# 3. open a session and leave it running
remote-docker up
```

`up` prints the endpoint. Point Docker at it, in another terminal:

```bash
remote-docker context install --use    # official docker/compose CLIs, no env vars
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

Every one of those is asserted end to end on each push, against a real
Docker-in-Docker daemon and a real kernel NFS mount — see
[`test/integration.sh`](test/integration.sh).

## Several workspaces

```json
{
  "workspaces": {
    "dev": {"host": "dev.example", "user": "alice"},
    "ci":  {"host": "ci.example",  "user": "alice"}
  },
  "default": "dev"
}
```

Each gets its own endpoint, so sessions run side by side:

```bash
remote-docker up -w dev &
remote-docker up -w ci &
remote-docker context install --all

docker --context dev ps
docker --context ci ps
```

`remote-docker workspaces` lists them.

## How it works

```
YOUR MACHINE                                 WORKSPACE (privileged dind)
─────────────────────────────                ───────────────────────────
docker / compose / IDE
        │ DOCKER_HOST
        ▼
┌──────────────────────┐
│  remote-docker up    │═════ ONE SSH CONNECTION ═════▶ sshd
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
- **The session must stay running.** `remote-docker up` *is* the endpoint and
  the file server; close it and running containers lose their mounts.

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
local writes, so file watchers work. That may be the whole reason it won, and
it is the open question in ADR 0014.

## Running a workspace

```bash
cd deploy
mkdir -p authorized_keys.d state
cp /path/to/alice.pub authorized_keys.d/alice.pub   # filename == unix account
docker compose up -d --build
```

Under Swarm, use `deploy/swarm.yml`:

```bash
docker stack deploy -c deploy/swarm.yml workspace
```

Swarm cannot run privileged tasks, so the service starts unprivileged and
relaunches itself privileged through the node's Docker socket -- no launcher
image involved. The host socket mount in that file is the whole trust
boundary: whoever can deploy the stack can already start privileged containers
on the node. See [ADR 0013](docs/adr/0013-self-elevation-instead-of-a-launcher.md).

The workspace runs one binary, `remote-dockerd`: it supervises dockerd,
provisions an account per enrolled key, and serves SSH itself.

Enrolment is out of band: someone with access drops a `<name>.pub` into the
keys directory, and the filename becomes that user's unix account. Removing a
key revokes access but keeps the home directory.

**All enrolled users share one Docker daemon** and can see each other's
containers ([ADR 0012](docs/adr/0012-shared-dockerd-across-users.md)). A
workspace is a shared machine; treat it as one.

## Layout

```
cmd/remote-docker/     the client
cmd/remote-dockerd/    the workspace agent
pkg/workspace/         the contract both sides share
internal/client/       ssh, nfs server, api proxy, bind rewriting, ports
internal/server/       the agent
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

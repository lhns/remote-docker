# Changelog

Kept by hand, and deliberately not a list of commits: the GitHub release notes
carry those, generated from the git history. This file is the curated view —
what changed that a person using this would notice, and what is still not
proven.

Dates are the day a claim was checked, which matters for the ones about other
software.

## Unreleased

The first release. There is no earlier version to compare against, so what
follows is what the thing is rather than what moved.

### The client is the Docker CLI

`remote-docker run`, `remote-docker ps`, `remote-docker compose up` are the real
Docker commands with their real flags, talking to a remote daemon. Rename the
binary to `docker` and they are spelled the way they are everywhere else — that
rename is the entire installation, with no code behind it and nothing to put on
PATH ([ADR 0024](docs/adr/0024-the-docker-cli-is-the-root.md)).

The Docker CLI, Buildx and Compose v5 are embedded, so `docker build` is
BuildKit and `docker compose up` works, from one file, on a machine where
nothing can be installed ([ADR 0009](docs/adr/0009-embedding-the-docker-cli.md)).

Everything of this program's own lives under `remote`: `remote ls`,
`remote create`, `remote status`, `remote start`, `remote stop`, `remote gc`,
`remote enroll`.

### Your directories are really mounted

A bind mount naming a path on your machine is rewritten into an NFS volume that
the workspace daemon mounts for itself, from an NFS server running inside the
client, over a reverse tunnel in the SSH connection
([ADR 0002](docs/adr/0002-nfsv3-as-the-file-transport.md),
[0003](docs/adr/0003-client-serves-workspace-mounts.md),
[0006](docs/adr/0006-per-bind-nfs-volumes.md)).

- Not copied and not synced. A write in a container lands on your disk.
- Any path you can read, not only the working directory.
- **Read-only stays read-only.** `-v src:/w:ro` and `--mount …,readonly` both
  survive the rewrite, and the integration suite asserts that the container is
  refused *and* that nothing appeared on your machine.
- Published container ports become reachable locally as containers start
  ([ADR 0008](docs/adr/0008-automatic-port-forwarding.md)).
- File changes made on your machine can be replayed inside the workspace as
  real syscalls, so watchers in containers fire
  ([ADR 0016](docs/adr/0016-replaying-change-events-as-real-syscalls.md)). Off
  by default; see `--watch`.

### Workspaces are docker contexts

`remote create` writes a docker context, `remote use` selects it, and
`--context` chooses which workspace a docker command talks to. A context this
program did not create is left completely alone — no session opened, no
`DOCKER_HOST` set — so a machine with a real Docker Desktop keeps working.

Sessions run in the background with no terminal held open, are shared between
commands, connect on demand, and disconnect when idle
([ADR 0015](docs/adr/0015-connections-on-demand.md),
[0017](docs/adr/0017-a-background-session-per-workspace.md)).

### The workspace is one binary

`remote-dockerd` replaces sshd, sudo and the shell scripts that came before it.
It provisions one unix account per enrolled public key, serves SSH itself, and
supervises the daemon ([ADR 0010](docs/adr/0010-go-ssh-server-agent.md)).

- **A dockerd per account** by default, so accounts do not see each other's
  containers ([ADR 0019](docs/adr/0019-a-dockerd-per-account.md)). A single
  shared daemon remains supported
  ([ADR 0012](docs/adr/0012-shared-dockerd-across-users.md)).
- Runs **as a container** (`deploy/docker-compose.yml`), **on Swarm**
  ([ADR 0013](docs/adr/0013-self-elevation-instead-of-a-launcher.md)), or
  **directly on a VM** under systemd
  ([ADR 0025](docs/adr/0025-the-agent-as-a-guest.md)).
- Enrolled keys become unix accounts named `rd-<account>`, so the workspace
  does not take names in a machine's own passwd file.

### Platforms

| | client | workspace agent |
|---|---|---|
| Linux amd64 / arm64 | yes | yes |
| Windows amd64 / arm64 | yes | — |
| macOS amd64 / arm64 | yes | — |
| Android arm64 | yes | — |

Android is its own build target, not the Linux one: Android requires
position-independent executables and rejects the Linux binary's TLS alignment
([ADR 0023](docs/adr/0023-running-where-the-loader-is-not-us.md)).

### Security

[`docs/threat-model.md`](docs/threat-model.md) states what is trusted, what is
not, and where the checks are, per flow, with the accepted risks named rather
than buried. The load-bearing ones:

- Reaching the local endpoint **is** the authorisation. It is owner-only and
  never a TCP port.
- The NFS export answers `AuthFlavorNull`; loopback-only binding and per-account
  port ownership are the entire control.
- A per-account daemon is **separation, not isolation**. Each runs privileged.

## Not proven

Kept honest deliberately. Everything above is exercised end to end in CI
against a real dind daemon and a real kernel NFS mount, except:

- **Swarm itself.** The elevation mechanism is tested; the Swarm wiring —
  templated task names, `mode: host` publishing, placement — needs a real
  cluster.
- **macOS, entirely.** Cross-compiled on every push, executed never. The
  endpoint code and the kqueue file-watching backend are where it genuinely
  differs.
- **Windows beyond unit tests.** The named-pipe endpoint and process handling
  are unit tested on every pull request; no Windows machine has taken a session
  end to end, because the integration suite needs a Linux kernel's NFS client.
- **Android beyond running.** `status`, `start`, `stop` and `docker run` work on
  a real device; no automated test runs there.
- **systemd.** The unit file ships and is not exercised; `test/vm.sh` starts the
  agent directly.
- **`coarse` watch mode**, and watching at scale — the budget and exclude list
  are unit tested against a fake backend, and nothing has run a watcher over a
  10,000-directory tree.
- **The release pipeline.** No tag has been pushed. This is that tag.

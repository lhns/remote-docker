# Changelog

Kept by hand, and deliberately not a list of commits: the GitHub release notes
carry those, generated from the git history. This file is the curated view —
what changed that a person using this would notice, and what is still not
proven.

Dates are the day a claim was checked, which matters for the ones about other
software.

## Unreleased

### A shared directory can stop revalidating every attribute

Reading a project through the share costs a round trip per file per second,
because the mount revalidates any attribute older than that. Over a link with
real latency that is the whole cost: measured on a GitHub runner over 300
files, reading them takes 0.4s unshaped, 59s at 40ms RTT and 292s at 160ms,
while a 10mbit link costs almost nothing. Latency, not bandwidth.

Docker already has a word for the fix, and every client already parses it:

```bash
docker run -v ./project:/app:ro,cached
docker run --mount type=bind,source=./project,target=/app,consistency=cached
#  compose:  consistency: cached
```

`cached` says the container may cache read data and directory structure, and
that this machine is authoritative. Here that becomes a long attribute cache
on the NFS mount. What keeps it coherent is the watcher: an edit here is
replayed into the workspace as a real syscall, which refreshes exactly the
inode that changed. So `cached` needs watching on, and asking for it without
says so rather than serving a mount that goes stale.

Measured on the same run: 292s becomes 98s at 160ms RTT, 59s becomes 24s at
40ms, and the 1,888 attribute revalidations behind those numbers become 12.
What is left is the files' own bytes, which no cache can avoid fetching.

Per workspace as `consistency`, per directory as `consistencyPaths`, and
`REMOTE_DOCKER_CONSISTENCY` for a CI run. A mount outranks a rule, a rule
outranks the workspace setting. Switching costs a volume rebuild and no
migration.

### And `delegated` stops mounting altogether

What `cached` cannot remove is the file's own bytes: a live mount has to fetch
what it is asked for, which was 300 reads and 422 permission checks in every
row above. `delegated` is Docker's word for a copy the container owns, and here
it is exactly that -- a plain local volume on the workspace, filled from this
machine as one tar stream before the container is created. Reads after that
cost the workspace's own disk.

Measured on the same run: reading 300 files takes 0.06s whatever the latency,
against 98s for `cached` at 160ms RTT. Starting the container is cheaper too,
0.25s against 1.43s, because the tree crosses in one stream rather than a round
trip per file. That last number is about a small tree -- the copy is
bandwidth-bound where a mount is latency-bound, so a large project is a wait
once, at container start.

It is a snapshot, and this release says so rather than implying otherwise:
nothing is written back, and a container already running does not see an edit
made here. The next container gets the tree as it is then. Write-back and a
live refresh are separate work, recorded in ADR 0043 as the reason they are
not here.

### The numbers are reproducible now

`test/bench.sh` walks, reads and writes a project-shaped tree through a real
session, with netem shaping the workspace's loopback for both delay and rate,
and reports the NFS per-operation counts beside the wall-clock. It runs from a
pull request labelled `bench`, or on request: it is a measurement, not a gate.

## 0.5.1 — 2026-08-26

### A workspace path works from Git Bash too

The two changes in 0.5.0 did not compose. Git Bash converts BOTH halves of a
`-v`, and only the container side can be restored blind, so
`-v /lib/modules:/lib/modules:ro` -- the command kind issues, and the one you
would type to test it -- arrived with a source under the Git installation and
matched none of the workspace's declared paths. It worked for kind, whose flags
never pass through a shell, and failed for the person checking the same thing by
hand.

The client now offers the other reading of such a source and takes it when the
workspace declared that path and this machine does not have it. Restoring it
blind is not possible: `C:\Program Files\Git\etc` is what Git Bash makes of
both `/etc` and `/c/Program Files/Git/etc`.

## 0.5.0 — 2026-08-26

### The workspace no longer logs a tini warning that means nothing

Starting a workspace printed `Tini is not running as PID 1 and isn't registered
as a child subreaper`, which reads like a fault and is not one: that tini is the
one dind's entrypoint starts for dockerd, under the agent, and the orphans it
mentions reparent to PID 1 -- the workspace's own tini, whose job is reaping
them. It is registered as a subreaper now, so it collects its own subtree and
says nothing.

### A bind may name a path the workspace owns

A tool that builds its own `docker run` flags cannot be told to spell them
differently -- `kind` hardcodes `-v /lib/modules:/lib/modules:ro`, meaning the
DAEMON's modules tree, while every bind source here means a path on your machine.
It failed on the client, and creating the directory to silence it delivered an
empty tree to the node.

Paths listed in `WORKSPACE_DIND_MOUNTS` are now resolved by the workspace's own
daemon instead of being exported from your machine
([ADR 0041](docs/adr/0041-the-workspaces-own-paths.md)). Nothing new to set: the
operator already declares those mounts, and the client learns them at connect.
A source your machine also has still wins, and a typo still fails, because it
matches nothing.

Two smaller changes come with it. `WORKSPACE_DIND_MOUNTS` is now read in
shared-daemon mode as well, where it declares without mounting -- so a malformed
value that was silently ignored there now fails at startup. And a mount whose
source is not on the workspace is refused rather than mounted: docker creates a
missing bind source, so a typo used to hand the daemon an empty directory and
surface inside a container much later.

### `-v` works from Git Bash

Git Bash rewrites arguments before this program starts, and it cannot know that
`-v` has two halves meaning different things -- so `-v /c/Users/you/x:/app`
arrived as `C:\Users\you\x;C:\Program Files\Git\app` and the mount failed naming
a path nobody typed. The container side is restored now
([ADR 0040](docs/adr/0040-git-bash-mangles-argv.md)), and the source keeps the
Windows spelling Git Bash correctly gave it.

Only `-v`, and only where the shape proves the rewrite happened. `-w /src` and
`-e PATH=/usr/bin:/bin` are mangled too and are not repaired: for those,
`MSYS_NO_PATHCONV=1` or a leading `//` still works. `--mount` was never affected.


## 0.4.0 — 2026-08-22

### Single files can be bind mounted

`-v ./nginx.conf:/etc/nginx/nginx.conf` was refused outright, which anyone
bringing an existing compose file met immediately. It works now, and only that
file is shared, not the directory holding it
([ADR 0039](docs/adr/0039-a-single-file-is-a-one-file-export.md)). It needs
Docker 26 or newer on the workspace, and says so when it is older.

Sockets, devices and FIFOs are still refused, now with the reason: what crosses
a file share is the name, not the kernel object behind it. That was always true
of a socket inside a shared directory too, and the old message ("not a
directory") hid it. README now lists what cannot be bind mounted, which was
written down nowhere before.

### Two fields left the workspace-info reply

`WORKSPACE_MOUNTPOINT` and `WORKSPACE_MOUNTED` described the `~/workspace`
convenience mount deleted in 0.2.0. They were still sent on every connection
while nothing set or read them. Anything parsing that reply keeps working:
unknown keys have always been carried through rather than rejected, so an older
agent's copies are still accepted.

## 0.3.1 — 2026-08-19

### Release archives carry an SBOM and third-party notices

The workspace image and the Helm chart have been published with an SPDX SBOM
since they existed; the archives people actually download had none, and now get
one each beside them on the release page. They also carry
`THIRD-PARTY-NOTICES.md`: every module linked into the binary with its licence
text, which Apache-2.0 asks for and which the MPL-2.0 components reached through
buildx require.

## 0.3.0 — 2026-08-19

### Two people can publish the same port

`-p 8080:80` was first come, first served on a workspace where everybody shares
one daemon: the second person got `port is already allocated`. The workspace now
publishes on a port of its own choosing and your machine keeps the number you
asked for, so both work. So does `-p 8080:80 -p 9090:80`, and so do two machines
of one account.

On the workspace, `docker ps` reports the port it picked, so anything reaching a
service there without going through the tunnel has to look it up. The clash
moves to your own machine: two of your own containers asking for 8080 get the
same error as before, from the client instead of the daemon.

### Published UDP ports work

They never did in any earlier version, because SSH forwards TCP and nothing
carried datagrams back. They now travel through the same connection as
everything else, with no port, setting or flag to turn on
([ADR 0038](docs/adr/0038-udp-crosses-the-tunnel.md)).

Datagrams ride inside the SSH stream, so a delayed one delays those behind it:
unremarkable for DNS, syslog or metrics, and not the same as a real UDP path if
you are measuring latency. A workspace running an older agent carries none, as
before, rather than failing.

### A container label can no longer ask for unlimited local ports

Which local ports a client opens comes from a label on the container, and with
one daemon for everybody any account can write one. A label is now capped at
1024 ports, which no real publication reaches. `docs/threat-model.md` flow 5 has
the reasoning.

### Per-account daemons can be given a registry configuration

A workspace with a private or insecure registry mounts `daemon.json`, or a CA
under `/etc/docker/certs.d`, into its own daemon. Each account's daemon does its
own pulling and saw none of it, so a registry that worked on the workspace
failed inside every account.

```yaml
WORKSPACE_DIND_MOUNTS: /etc/docker/daemon.json:/etc/docker/daemon.json:ro
```

Comma-separated, `source:destination[:ro]`, both absolute. The README has the
two
traps: `storage-driver` or `hosts` in that file collides with the flags the
agent
passes, and the change reaches an account that already has a daemon once that
account has nothing running.

### A refused connection says what was actually wrong

The client used to answer every refused tunnel port with "another session for
this account may still be open". It cannot know that — SSH's refusal carries no
reason — and in the case that prompted this it was wrong, sending somebody after
a session that did not exist while their daemon was down. It now asks the
workspace and prints the answer, with the one thing worth trying under it.

Two other messages were guesses in the same shape: a 404 from a reverse proxy
named a `--ws-path` setting that does not exist, and a workspace that could not
read which port a machine's volumes need handed out a different one instead of
saying so.

### A broken per-account daemon repairs itself instead of looping forever

A daemon that would not start blocked every session for that account, and stayed
broken: the agent would only rebuild one after proving nothing was running
inside it, which it asked the daemon that was down. A daemon that is not running
no longer counts as busy, so it gets rebuilt, keeping its images and containers.

It also no longer carries a restart policy — the agent starts it when its
account
connects and is the only thing that starts it — so after a workspace restart an
account's detached containers come back when that account next connects rather
than immediately. Switching a workspace to one shared daemon now stops the
per-account daemons it leaves behind, without removing them.

### Known: compose projects collide between two machines

One account used from two machines shares a daemon, so the same compose file
from both is one project: they recreate each other's containers, or, if the
paths match, one silently serves the other's files. Give each machine its own
`COMPOSE_PROJECT_NAME`. The README has it, and
[ADR 0029](docs/adr/0029-one-account-many-machines.md) records why neither
namespacing nor detection is built.

## 0.2.2 — 2026-08-14

### Security: the Kubernetes pod no longer mounts a ServiceAccount token

**Upgrade if you run the Helm chart.** A projected token is mounted at mode
0644, and an enrolled account gets a shell in that container as its own uid, so
any account able to run containers could also read the pod's cluster identity
and act as it against the API server. The agent never calls the Kubernetes API,
so the chart sets `automountServiceAccountToken: false` and there is no token in
the pod. There was no Role or ClusterRole before and there is none now, so what
an attacker gained depended on what your namespace's default ServiceAccount can
do.

`helm upgrade` is the whole fix. Nothing else in an install changes, and no
client or workspace setting is affected.

### The Android build can resolve a hostname

It could not, and the error named an address nobody had configured:

```
lookup docker.lhns.de on [::1]:53: ... connection refused
```

Android has no `/etc/resolv.conf`, so Go's own resolver had nothing to read and
fell back to loopback, where nothing answers. DNS there belongs to the system
resolver, and reaching it means linking the system libc, so that target is now
built with the NDK. Nothing else changes, and no other platform is built any
differently.

Confirmed on a phone on 2026-08-14: a session over `wss://` and a container,
from Termux. CI still runs nothing on Android, so what it checks is the file —
that the binary is loadable there and links the system libc, which is what makes
DNS work.

**There is an `android_amd64` archive again**, for emulators and Chromebooks.
It was left out because it would have required exactly this.

### The threat model covers the WebSocket transport and Kubernetes

[`docs/threat-model.md`](docs/threat-model.md) had not been revisited since a
workspace could be reached through a reverse proxy or run in a cluster. It now
models both, says who each control defends against, what is deliberately not
modelled, and what the project's artifacts are signed with. The ServiceAccount
token above is what writing it turned up.

**One limit is written down rather than fixed.** With one daemon for everybody
(`WORKSPACE_PER_USER_DIND=false`), the NFS exports are bound in the namespace
account shells run in, so an account can reach another's export with an ordinary
socket. No forwarding rule can prevent that, because no forwarding is involved.
The default mode binds each export inside its own account's namespace and the
test suite now asserts that no shell can reach it there.

## 0.2.1 — 2026-08-14

### The published Helm chart is signed

Nothing in the binaries or the image changed. The 0.2.0 chart was pushed
unsigned, because the signing step authenticated to the registry the way helm
does and cosign reads the docker credential store, so `cosign verify` on
`ghcr.io/lhns/charts/remote-docker-workspace:0.2.0` fails. It works from 0.2.1
on.

Charts built from a commit rather than a tag are published too, as
`0.0.0-dev.<short-sha>`, so a specific commit can be installed with
`--version 0.0.0-dev.abc1234`. They sort below every release, so an install
that names no version never picks one up.

## 0.2.0 — 2026-08-14

### Run a workspace on Kubernetes

```bash
helm install ws oci://ghcr.io/lhns/charts/remote-docker-workspace \
  --set ingress.host=ws.example.com \
  --set-file authorizedKeys.alice=$HOME/.ssh/id_ed25519.pub
```

One privileged pod, its image store and its host keys on volumes of their own,
reached through an ordinary Ingress. No load balancer and no node port: the
tunnel is an HTTP upgrade, so a cluster that gives a namespace nothing but an
ingress is enough.

- **Installed on a real cluster by CI on every change** — kind, ingress-nginx,
  and the client reading a file from the runner inside a container in the
  cluster.
- **Only ingress-nginx and kind's local-path storage are proven.** The chart's
  annotations for other controllers are suggestions, and its default storage
  driver is a guess about your volumes: `fuse-overlayfs`, because overlay2
  refuses to start on Ceph- and NFS-backed storage, which is a daemon that never
  comes up rather than a warning. On local or block volumes, `dockerdArgs: ""`
  is faster.

The chart and the workspace image are now signed with cosign, keyless, and carry
SBOM attestations. Nothing published by this project was signed before.

### Reach a workspace through a reverse proxy

The agent serves SSH over a WebSocket as well as over TCP, so a workspace can be
reached on 443 through any HTTP reverse proxy instead of needing an SSH port
open to it:

```
remote-docker remote create dev --host wss://dev.example.com --user alice
```

`host` now takes a scheme — `ssh://`, `ws://` or `wss://` — and a bare host
still
means SSH on 2222, so nothing already configured changes. `--ca-file` verifies a
proxy holding a private certificate; `--insecure` accepts any certificate, for
one workspace. Neither changes whether the session is authenticated: the
workspace's host key and your own key do that inside the tunnel, as they do over
TCP.

The agent never terminates TLS and has no certificate settings, so there is
nothing to renew and nothing to expire. It listens on `:2280` alongside SSH on
`:2222`, and `--ws-addr ""` turns it off. Upgrades are accepted on any path, so
the proxy may route on one or not.

- **Proven in CI on every change**, with a real nginx in front: a session
  through it, a bind mount resolving (which is the reverse tunnel working
  through the proxy), and the agent dropping a connection whose peer stopped
  answering.
- **Only nginx has been tested.** Any proxy that forwards WebSocket upgrades
  should work; none other has been run.

### Sessions and mounts survive drops that used to strand them

Three faults, each of which presented as something other than what it was:

- **A workspace kept the tunnel port of a client that had vanished.** A client
  whose network dropped without closing anything left a connection that was dead
  and looked alive, so its reconnect was refused the only port its volumes can
  mount from. It presented as `docker compose up` failing with `connection
  refused` against a port nothing on screen explained. The workspace now bounds
  how long a connection may go unanswered.
- **Reconnecting minted new NFS file handles**, because the file server was
  built per connection rather than per session.
- **Restarting the client stranded every running container** with `Stale file
  handle` against a mount that still looked fine. The handle for a share's root
  is now derived from the export path, so a new process can answer for it.

Measured while proving those, and worth knowing:

- **Docker refcounts a volume's mount.** A mount that has gone wrong stays wrong
  until the last container using it is gone, which is why `compose down && up`
  cured it where restarting the session did not.
- **A container holding a file open across a client restart still gets
  `ESTALE`** on that descriptor. There is no path lookup left to retry, and that
  is correct rather than fixable.
- **A mount into a port that accepts nothing costs about 180 seconds** before it
  fails, not the 60 the mount options imply.

The workspace also reads a machine's tunnel port back off that machine's own
volumes when its record of it has been lost, so volumes made for it still mount.

### One account, used from more than one computer

An account can be enrolled for several machines at once, and each keeps its own
view of its own files. The daemon is shared, so containers and images are the
same from either machine, which is the point of using one account from both.
What is not shared is anything derived from a machine's filesystem: the NFS
export, the tunnel port behind it and the volumes that name it are per machine.
The machine is identified by the digest of the key the workspace already
authenticated, so it cannot be claimed by sending a different one.

Three defects were fixed with it, each reachable by opening the client on a
second computer:

- **A second machine's failed bind deleted the first machine's live port
  reservation**, after which any other account on a shared daemon could reach an
  NFS export that authenticates nobody. A reservation now belongs to the session
  that took it and is released with a token, not by name.
- **A dropped tunnel was detected and then handed out anyway.** The keepalive
  closed the connection and told nothing, so every later request got the dead
  one and failed as though the workspace had refused. `remote restart` was the
  only way out, and it refused too. A dead connection is now dropped rather than
  asked whether anything still depends on it.
- **A probe waited on the transport's clock**, so a link that stopped carrying
  traffic without breaking — a laptop suspended, a NAT idling the flow out —
  took minutes to notice rather than seconds.

Proven by `test/two-clients.sh` on every change: two client machines, one
account, each reading its own file through its own bind mount, both seeing a
container the other started, and a collection on one leaving the other's volumes
alone.

### Commands fail in a way you can act on

- **`docker run` with nothing configured says so.** It used to report the stock
  Docker CLI's "cannot find the file specified" against a named pipe, while
  `remote status` in the same terminal gave the real reason. Both failure paths
  were being discarded.
- **A container's exit code is yours again.** This binary is the Docker CLI, so
  `docker run ...; echo $?` has to answer what the container answered; it was
  collapsing every status to 1, and printing a bare `remote-docker:` line when a
  container exited non-zero. One part of docker's contract still cannot be
  matched, and the code says so: Ctrl-C exits 1 rather than 130, because the
  error docker uses for it is unexported.

### A workspace can now be a machine on your own computer

`remote machine create <name>` provisions a Linux system here and registers it
as an ordinary workspace: `remote ls` lists it, the docker context is created
for it, and bind mounts, published ports and streams work exactly as they do
against a host somewhere else. `remote rm` removes the workspace and destroys
the machine with it. This is the answer to "I have no Linux host to point at",
which on Windows is most people.

Nothing is installed into a machine and no package manager runs on any path.
The machine IS the workspace image's filesystem — the same artifact CI builds
and tests on every push — so changing versions replaces it rather than upgrading
it, and there is no half-finished install to be in. `remote machine rebuild` is
therefore not a repair mode: it is the ordinary path run again. It discards the
images and containers inside the machine; your files are never at risk, because
they live on your machine and are served to it.

- **WSL backend: proven end to end in CI** on every change, on a Windows runner.
  Create, a `docker run` with a bind mount from the Windows side, idempotent
  create, surviving stop and start with its containers, a background session
  outliving three minutes of idleness, and `rm` taking the distribution with it.
- **Hyper-V backend: shipped and NEVER EXECUTED.** No CI anywhere offers
  Hyper-V and nobody working on this has it. Its decisions are unit tested as
  far as a string can be and everything past `powershell.exe` is unproven.
  `docs/testing-machines.md` is a runbook for whoever runs it first. Do not
  believe it works.
- **`remote machine create` needs nothing but a name.** It pulls the workspace
  image and flattens it into the machine's filesystem, cached by digest, so a
  second machine or a `rebuild` costs nothing; `--rootfs` builds from a file you
  supply instead. Nothing extra is published for this: a container image IS a
  rootfs, so the artifact the container deployment already uses is the artifact
  a machine is built from, and the registry picks the right architecture.

Four things were found by measurement while building this, each of which had
presented as an unexplained refused connection (ADR 0026):

- `docker export` writes a filesystem, not an image: `ENV` and `PATH` are not in
  the tarball, so a machine's environment has to be restored explicitly.
- WSL2's localhost relay did not carry the connection at all on the runner. A
  machine is reached at its own address.
- That address is handed out at boot, so it is asked for at every connection and
  never stored.
- A machine with nobody in it shuts down, and neither an open TCP connection nor
  a command that runs and exits counts as somebody. A session holds its machine
  open for as long as it is connected.

### The Swarm stack file does less

Four settings that only restated Docker's own defaults are gone: one replica,
restarting on failure, and a placement constraint that is not needed on a
single-node swarm and never scheduled anything without a label somebody had to
know to add.

The SSH port is published through the routing mesh rather than `mode: host`.
The privileged child joins the task's network namespace, so the mesh delivers
where it is listening; host mode is still worth setting if you want the
client's own address to survive, and the file says so.

**If you run a swarm of more than one node, pin the service yourself.** The
binds are directories on one node, so a task rescheduled elsewhere gets new
host keys and new uids, which moves each account's tunnel port. That was the
constraint's job and it is now yours.

### Under the hood, with nothing to notice

The repository is five Go modules rather than one, so the client and the agent
each build without the other's dependencies, and the shared code between them
has no third-party dependency at all. The transport, the tunnel protocol and
half-closing a stream moved into modules both ends share, which found four
latent bugs where the two copies had drifted — including opposite answers to
what half-closing means. Nothing about this is visible from outside; it is here
because a release compared against the last one otherwise looks like it changed
a great deal.

The README and the architecture records were checked against what the code and
the outside world actually do, rather than trusted. Two claims had expired
without anyone noticing: that embedding Compose would pin the Docker CLI back a
major version, which stopped being true when Compose v5 shipped, and that
Windows had no standalone Docker CLI, which `winget install Docker.DockerCLI`
disproves. Both had been quoted as current fact in the README and in `--help`.

## 0.1.0 — 2026-08-11

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
nothing can be installed ([ADR
0009](docs/adr/0009-embedding-the-docker-cli.md)).

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
  ([ADR 0008](docs/adr/0008-published-ports-reach-the-client.md)).
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

### Not proven

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

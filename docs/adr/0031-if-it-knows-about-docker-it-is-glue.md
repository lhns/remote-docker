# 0031 — If it knows about Docker, it is glue

- Status: Accepted; extends [ADR 0021](0021-three-modules.md) and
  [ADR 0030](0030-a-core-module-for-the-tunnel.md)
- Date: 2026-08-12

The mechanisms this project is built from — an SSH tunnel, an NFS export, port
forwards, filesystem watching, unix accounts — are things a person could want
without Docker existing. Making a bind mount into a volume, telling a hijacked
API response from a chunked one, supervising dockerd and resolving an account to
its daemon are not.

That sentence is the sorting rule, and it decides where a package lives:

> **If it knows about Docker, it is glue.**

## What forced it

ADR 0021 split the binaries so the agent would not inherit docker/cli. ADR 0030
split the transport so the two ends could not disagree about it. Both were
answers to the same failure mode, and neither reached the machinery around the
transport: an NFS server, the watchers, the accounts. All of it was `internal/`
inside a binary module, so anybody wanting the mechanism got the Docker CLI with
it, which makes it a folder rather than a foundation.

Moving code under this rule found four duplicated decisions, all of which fail by
succeeding rather than by breaking:

- two bidirectional copies with OPPOSITE half-close fallbacks (ADR 0021);
- three protocol names defined twice, where a mismatch is a shell exiting 127;
- two copies of the reverse-forward permission check, the originals left
  compiling quietly after the machinery moved;
- a fallback group list in `accounts` — `{"docker", "workspace"}` — unreachable
  because the caller always sets one, and free to disagree with it.

Four in one restructuring is the argument that the rule is worth its cost.

## The layout

```
github.com/lhns/remote-docker          SHARED: what both ends must agree on
github.com/lhns/remote-docker/core-client     THE USER'S MACHINE, minus Docker
github.com/lhns/remote-docker/core-agent    THE WORKSPACE, minus Docker
github.com/lhns/remote-docker/agent    the agent binary: glue
github.com/lhns/remote-docker/client   the client binary: glue
```

`core-client` holds `nfsserve`, `fswatch` and `keys`; `core-agent` holds `accounts`,
`notify` and `netns`.

**The names are places, not roles.** `client` and `server` invert depending on
the mechanism: for the Docker API the user's machine is the client, and for NFS
it is the SERVER while the workspace is the client. Naming the modules for the
two ends of the connection avoids a word that means the opposite thing one
paragraph later. `core-agent` rather than `workspace` because `pkg/workspace` already
means the contract, and two import paths ending in the same word read as the
same thing.

## What could not be moved, and why that is the interesting part

The plan for this assumed `agent/internal/sshd` was Docker-free, on the grounds
that the daemon resolver is already an interface (ADR 0020). It is not:
`session.go` names Docker thirty-four times — the socket, `DOCKER_HOST`, the
CLI, volume mountpoints. So the split fell *through* the package rather than
around it. The forwarding protocol went to `pkg/tunnel/server` and the decisions
it asks for stayed behind.

`notify` split along a seam that already existed, because `Volumes` was an
interface before any of this: `dockercli.Volumes` asks a daemon where a volume
is, and `notify.Relocate` maps the answer into the agent's filesystem. Relocate
stayed with the replayer deliberately — what it enforces is containment, the
mountpoint it checks is reported by a daemon the account is root inside, and
`path.Join` is not containment because it CLEANS.

`ports` was expected to split down the middle, with a generic forwarder moving
out and the container discovery staying. It stays whole instead, for two reasons
found by looking: the generic part -- a local listener whose connections are
carried to an address on the workspace -- is ALREADY `pkg/tunnel/client.Forward`,
and what remains is keyed on container ids from end to end. Splitting further
would have invented an abstraction with exactly one user.

The same rule kept `keys` and the enrolment hint apart. A keypair and a
known_hosts file are this machine's identity and know nothing about what they
authenticate to; the hint names a file in the workspace's `authorized_keys.d`,
which is this project's rule for who may log in. So `core-client/keys` produces the two
values and `client/internal/sshx` is where they meet the transport.

## The membership test is checkable, and the obvious check does not work

`go list -deps ./... | grep -c docker` was the proposed verification. It does not
work, for two reasons found by running it:

- it matches every package in a module named `remote-docker`;
- with the module path excluded it reports zero everywhere on the agent side,
  **including in `dockercli` itself**, because the agent runs the docker BINARY
  and imports no Go client at all. That is ADR 0021's 24 go.sum lines working as
  intended, and it makes the import graph silent about exactly the thing being
  tested.

What does distinguish them is coupling to the packages that are about Docker:

```bash
# must print nothing, and does
(cd core-agent && go list -deps ./... | grep -E 'internal/(dockercli|daemons|supervise|elevate)')
```

On the host side the original check does work, because docker/cli is a real
import there, and it is the sharpest number in this record:

```bash
(cd core-client   && go list -deps ./... | grep -v lhns/remote-docker | grep -ci docker)  # 0
(cd client && go list -deps ./... | grep -v lhns/remote-docker | grep -ci docker)  # 191
```

Two different checks for two modules is not elegant, and saying so is better
than shipping one that passes without proving anything.

## What it costs

- **Five modules, and `./...` stops at every boundary.** The
  build/test loop and CI grow an entry each time. This was already the project's
  documented trap.
- **A change spanning core and binary needs two steps.** `go.work` hides that
  locally, which is why CI and the image build ignore it.
- **`image/Dockerfile` copies module trees by name.** A new module the agent
  imports must be added there, or the image build is the only thing that fails.
- **`internal/` becomes public API.** Anything moved out stops being free to
  change. That is the point, and it is a cost.

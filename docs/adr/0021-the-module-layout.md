# 0021 — The module layout

- Status: Accepted. Consolidates ADR 0011 (shared contract), ADR 0030 (tunnel in
  `core`) and ADR 0031 (the glue rule): stages of one decision, no longer
  separate records.
- Date: 2026-08-07, last decided 2026-09-02, consolidated 2026-08-19
- Current answer: **two axes**. Modules split by SIDE; packages inside `core`
  split by FEATURE. Seven modules, of which one is test-only.

## The decision

```
github.com/lhns/remote-docker/core          SHARED: what both ends must agree on
github.com/lhns/remote-docker/dircache      A CACHE ENGINE, usable on its own
github.com/lhns/remote-docker/core-client   THE USER'S MACHINE, minus Docker
github.com/lhns/remote-docker/core-agent    THE WORKSPACE, minus Docker
github.com/lhns/remote-docker/client        the client binary: glue
github.com/lhns/remote-docker/agent         the agent binary: glue
github.com/lhns/remote-docker/test/probes   instruments; linked into nothing
```

## The two axes, and why naming only one was a bug

**Modules split by SIDE**: what may link what. **Packages inside `core` split by
FEATURE**: what must change together. Both are needed and only the first was
ever written down here, which is how the same defect happened five times before
anyone saw it (2026-09-02).

What it looked like: `core/tunnel` held each channel's NAME while
`core/workspace` held its version and message types. `NotifyCommand` in one
package, `NotifyVersion` in another -- though opening a name an agent does not
know IS how the version is checked, as `NotifyCommand`'s own comment said. Both
packages stated the same membership test, "spoken by one binary and understood
by the other", so what actually separated them was *is it a string constant or a
struct*. That is not an axis.

The controlled experiment was already in the tree. `protocol.go` held six
declarations; the three with a payload type of ours were all split from it, and
the three without -- `DialStdioCommand` (the payload is raw Docker HTTP),
`KeepAliveRequest`, and `UDPChannelType` with `ForwardPayload` and the datagram
framing -- were all cohesive. UDP is the shape to copy.

The same shape was found four more times, in descending severity:

| agreement | halves were | how it failed |
|---|---|---|
| the `"cwd"` share id | unexported in `core/workspace`, hardcoded in `core-agent/union` | silently: a cache volume reported that does not exist, so the collector empties one under a running container |
| `Info.Mode`'s values | the field in `core`, `"shared"`/`"per-account"` in `agent/internal/daemons` | silently: the client only prints the string, so a rename compiles and passes |
| the five container labels | defined in `core`, re-exported by `client/internal/rewrite`, read from there by six sites | loudly, but it made `session` import the WRITER for a constant |
| info / notify / cache names | `core/tunnel` vs `core/workspace` | an unknown command exits 127, so loudly-ish; the pairing with the version was unforced |

**The rule that follows: a protocol package holds the whole agreement.** Its
channel name, its version, its frames and its payload format. If any of those
lives elsewhere, nothing makes them change together.

Three rules place everything:

1. **`core` if both binaries must AGREE on it** — on a format, `core/workspace`;
   on behaviour, `core/tunnel`. Not merely if both use it.
2. **If it knows about Docker, it is glue.** SSH tunnel, NFS export, port
   forwards, file watching, unix accounts: wanted without Docker existing.
   Bind→volume rewriting, hijack detection, supervising dockerd, resolving an
   account to its daemon: not.
3. **Its own module only if it is worth taking WITHOUT the rest of the one it
   would otherwise sit in.** This is a claim about the dependency graph, not
   about tidiness, and there is exactly one thing that meets it. See
   "The third question" below.

Contents:

| module | packages |
|---|---|
| `core` | `workspace` (the names and numbers both ends derive, and the handshake), `notify` and `cache` (one protocol each, whole), `tunnel` (how bytes and datagrams cross a connection), `logx` |
| `dircache` | the cache policy: fill order, invalidation, write-back |
| `core-client` | `nfsserve`, `fswatch`, `keys`, `tunnelclient` |
| `core-agent` | `accounts`, `replay`, `netns`, `tunnelserver`, `union`, `wslisten` |
| `client`, `agent` | everything that names Docker |
| `test/probes` | `watchprobe`, `pokeprobe`, `udpecho` |

Two things did NOT become feature packages, and the reasons are the useful part:

- **`info.go` stays in `workspace`.** `workspace.Info` is what the type is
  called and `info.Info` would be a stutter. A package is worth it when it
  removes a prefix, not when it adds one.
- **`ports.go` and `mapping.go` stay together.** They are two different port
  concepts -- the published port and the reverse-tunnel port -- and splitting
  one out would put two similar names in two packages. `client/internal/ports`
  also already owns the name.

Structural rules:

- **The repository root is not a module.** `go build ./...` there fails
  outright; a module at the root would let it pass while compiling almost none
  of the repository.
- **`logx` is public, not `core/internal/logx`.** Go's internal rule would
  restrict a handler every module imports to `core/...` alone.
- **Relative `replace`, and the agent never requires the client module** — that
  would pull the graph straight back in.
- **`go.work` is for editors only.** CI and the image build take one module at a
  time, so a missing `require` fails where it is wrong.
- **Names are places, not roles.** `client`/`server` invert per mechanism: for
  the Docker API the user's machine is the client, for NFS it is the SERVER.
  `core-agent` rather than `workspace`, because `core/workspace` already means
  the contract.

## What forced it

Three stages, each a silent failure.

**One place for the contract (2026-08-07).** uid→port lived in two shell scripts
sourcing one config file. Drift meant the client tunnelled to one port while the
mount read another, presenting as a network fault. There is one `PortForUID` now,
and the agent's ownership check and the client's tunnel target are the same
function.

**One module was too few (2026-08-10).** `docker/buildx` (ADR 0009) needs a newer
Go than the image's `golang:1.25-alpine` builder, so the directive moved to
`go 1.26.3`; the golang images set `GOTOOLCHAIN=local`, turning that into a hard
failure. **The agent stopped compiling** — naming the toolchain, not the
dependency that moved it — and the image could not be built for two days. The
agent imports none of buildx; a module is the unit of version resolution and they
shared one.

| | third-party modules | `go.sum` |
|---|---|---|
| agent | 7 | 24 lines |
| client | ~130 | 786 lines |
| core | 1 | 2 lines |

As of 2026-08-10. The agent's has grown since; the Consequences below carry the
current figure and what moved it.

`GOTOOLCHAIN=auto` fixes that day and converts the class of breakage into a
silent toolchain download. The coupling had to go.

**The split did not reach the machinery (2026-08-12).** NFS server, watchers,
accounts were `internal/` inside a binary module, so taking the mechanism meant
taking docker/cli with it. Applying rule 2 found four duplicated decisions, all
of which fail by *succeeding*:

- two bidirectional copies with OPPOSITE half-close fallbacks — one did nothing
  when `CloseWrite` was unavailable, the other closed the whole connection, which
  this project's invariant forbids. Presented as `docker run` exiting 0 having
  printed nothing;
- three protocol names defined twice, where a mismatch is a shell exiting 127:
  `docker system dial-stdio` (client proxy and agent session handler),
  `workspace-info` (constant on one side, literal at the other's call site),
  `workspace-notify` (in the contract package, which owns the frame FORMAT and
  not the command name carrying it);
- two copies of the reverse-forward permission check, the originals left
  compiling after the machinery moved;
- an unreachable fallback group list in `accounts` — `{"docker", "workspace"}` —
  free to disagree with the caller that always sets one.

## `core/tunnel`

- **The root package imports NEITHER SSH library.** Go links what is imported, so
  one package importing both would put a server in the client binary. It is why
  the agent's `go.sum` did not move when the transport was extracted.
  Corollary: anything inexpressible without an SSH library is an implementation,
  so `tunnelclient` dials from `core-client` and `tunnelserver` answers from
  `core-agent`.
- **Auth and dialling are handed in, never chosen**: `ssh.Signer`,
  `ssh.HostKeyCallback`, `Dial`. Only the caller knows the deployment —
  `core-client/keys` produces the identity, `client/internal/session` holds the
  enrolment hint (the one part that is policy), nil `Dial` means TCP and the
  WebSocket transport passes its own (ADR 0034). What the package does with the
  connection is identical either way.

## The third question, and why `ports` does not answer it

`ports` was expected to split and did not: *"splitting further invents an
abstraction with one user."* That precedent reaches every candidate here except
one, and the difference is not size or elegance.

| | `ports` | `dircache` |
|---|---|---|
| what a second user would supply | a forward, which `tunnelclient` already is | a `Store`: somewhere files live |
| what a package inside `core-client` would drag along | nothing; the caller is in `client` regardless | that module's seven third-party requires |
| third-party requires of its own | n/a | **0** |

The cache engine decides what to copy, in what order, what a local change means
for a cache, and what a cached change means for somebody's source tree. None of
that names a transport or a storage, and in this repository both are exotic --
an SSH channel, and the upper layer of a fuse-overlayfs union across a network
(ADR 0044).

**No external consumer is expected, and that is not the argument.** Decided
2026-09-02: these boundaries exist as discipline inside this repository, and an
earlier version of this record claimed a reuse benefit that nobody wants. The
honest reason is that a module boundary is the only thing Go enforces -- a
package inside `core-client` cannot refuse that module's dependencies, and a
module can. The property is checked rather than trusted:

```bash
# A dot in the FIRST path element is a domain, which is what a third party is.
# Matching a dot anywhere counts crypto/internal/entropy/v1.0.0, and reports 1.
(cd dircache && go list -deps ./... | grep -v lhns/remote-docker | grep -cE '^[^/]+\.[^/]+/')  # 0
```

**The two side-boundaries do not buy the same thing**, and it is worth saying
which is which. `core-client` isolates real weight: 54 `go.sum` lines against
`client`'s 861, which is `docker/cli` plus buildx plus compose. `core-agent`
isolates none -- `agent` has 28 lines in total -- so what that boundary enforces
is LAYERING, not weight. Both are worth keeping; only one is worth keeping for
the reason above.

**`core` requires nothing at all** since 2026-09-02, when the test-only probes
moved to `test/probes`. They were the only thing that ever put a dependency into
the module every other module imports, and they were imported by no Go file,
linked into neither binary and shipped in nothing.

What it cost, honestly: a module must be enumerated in `go.work`,
`.github/dependabot.yml`, `.goreleaser.yaml`, four workflow files, this record,
`docs/adr/README.md` and CLAUDE.md's layout and two loops. Two of those fail
SILENTLY -- `integration.yml`'s change-detection regex, where a miss means the
suite quietly stops running on changes to it, and the `cache-dependency-path`
lists, where a miss is a cache miss nobody sees. `dircache` has no `go.sum` to
list, because it has nothing to cache.

## What could not be moved

The test is AGREEMENT, not "looks like infrastructure".

- **`nfsserve`** — the client serves, the agent only mounts. Moving it to `core`
  puts `go-nfs` and `go-billy` in the agent's graph for no agreement.
- **`ports`** — expected to split; did not. The generic half is already
  `tunnelclient.Forward`, and the rest is keyed on container ids end to end.
  Splitting further invents an abstraction with one user.
- **Watching** — the agreement (frame format, mode names) is already shared
  (ADR 0016). `fswatch` is three backends the agent never runs; `notify` does
  syscalls on the user's files and its knowledge is one-ended.
- **`agent/internal/sshd`** — assumed Docker-free because the daemon resolver is
  an interface (ADR 0019). `session.go` names Docker thirty-four times, so the
  split fell *through* the package: the forwarding protocol went to
  `core-agent/tunnelserver`, the decisions it asks for stayed.
- **`keys` vs the enrolment hint** — a keypair and known_hosts are this machine's
  identity; the hint names a file in the workspace's `authorized_keys.d`, which is
  policy.
- Also not shared: `envOr`/`envInt` (small, differently shaped), and the
  path-containment helpers — `notify.relocate` guards untrusted daemon output and
  the client's lookalike is unrelated.

## The membership test, and why the obvious one fails

`go list -deps ./... | grep -c docker` does not work: it matches every package in
a module named `remote-docker`, and with the module path excluded it reports zero
everywhere on the agent side **including `dockercli` itself**, because the agent
runs the docker BINARY and imports no Go client. The import graph is silent about
exactly the thing being tested.

```bash
# agent side: coupling to the Docker-shaped packages. Must print nothing.
(cd core-agent && go list -deps ./... | grep -E 'internal/(dockercli|daemons|supervise|elevate)')

# host side: the original check works, and is the sharpest number here.
(cd core-client && go list -deps ./... | grep -v lhns/remote-docker | grep -ci docker)  # 0
(cd client      && go list -deps ./... | grep -v lhns/remote-docker | grep -ci docker)  # 191
```

Two different checks for two modules is not elegant; saying so beats shipping one
that passes without proving anything.

## Consequences

- **A client dependency cannot break the agent's build.** The whole purchase.
  Unplanned dividend: the agent ships as its own release artifact for a VM
  workspace (ADR 0025) as a goreleaser block with `dir: agent` and 28 `go.sum`
  lines.

  That number is watched rather than fixed. It went from 24 to 28 on 2026-09-01
  when the cache channel took `klauspost/compress` for zstd (ADR 0044) -- a
  dependency bought on purpose for the one bulk transfer this protocol makes,
  and the point of stating the count is that such a purchase is visible rather
  than that it never happens.
- **`core` has no third-party dependency at all.** Its `go.mod` carries no
  require; the `x/sys` it used to hold went to `test/probes` with the probes
  that read raw inotify.
- **`./...` stops at every module boundary**, so the seven-module loop is the
  only thing that covers the repository. With no root module the naive command fails
  rather than passing.
- **`golangci-lint` runs nine times**: one per module, plus `GOOS=linux` for
  `agent` and `core-agent`, whose Linux-only files a host lint never sees.
- **Dependabot needs one entry per module.** It does not discover nested modules;
  a directory missing from `dependabot.yml` stops being updated silently.
- **`image/Dockerfile` copies module trees by name.** A new module the agent
  imports must be added there or the image build alone fails.
- **`go.work` must not reach the image build** — it would resolve the agent
  through the workspace, so a missing require builds there and fails everywhere
  else.
- **Publishing gains an ordering constraint.** An outside consumer of a contract
  change needs a tag on `core` first; in-repo the `replace` hides it, which is
  how it will be forgotten.
- **`internal/` became public API.** What moved out is no longer free to change.
- **The standing temptation**: putting helpers in `core` because both sides want
  them today. That is how a contract package becomes a utility dump. Both rules
  are narrow on purpose.

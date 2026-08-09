# 0009. Embedding the Docker CLI, Buildx and Compose

- Status: Accepted
- Date: 2026-08-07

## Context

ADR 0005 gives us a Docker API endpoint, which any Docker client can use. But
the premise of the project is a machine that cannot have software installed,
and the `docker` CLI is software. Solving the daemon and the filesystem while
still requiring a local Docker installation leaves the original problem
half-solved.

The requirement is therefore a **fully fledged Docker client**: not a curated
subset of commands, and specifically including `docker build`, which is the one
most likely to be dismissed as too hard.

All three pieces are Go programs built on cobra, and all three expose their
command trees as libraries:

- `github.com/docker/cli` — the command tree, `cli/command`
- `github.com/docker/buildx` — `commands.NewRootCmd`, normally a CLI plugin
- `github.com/docker/compose/v2` — likewise a plugin

Buildx matters because it is *not* part of `docker/cli` core. Embedding the CLI
alone leaves `docker build` on the deprecated classic builder, which is a real
functional gap rather than a packaging detail.

`docker build` turns out to be better-behaved through a proxy than expected.
BuildKit uploads the build context over the API as a stream, so the context
comes from the client machine and never needs to reach the workspace through
NFS at all. What it does need is a `/session` connection, which is an HTTP
upgrade carrying gRPC.

## What integration actually found

Measured rather than assumed:

| | |
|---|---|
| builds at `CGO_ENABLED=0` | yes — the single-binary premise survives |
| binary, `docker/cli` embedded | 42 MB |
| subcommands registered | 57 |
| `docker build` | **present in the tree** |
| `docker compose` | **absent** |

Two of this record's original assumptions were wrong, and the corrections
matter more than the confirmations:

**~~Buildx is no longer a separate plugin binary for our purposes.~~** This
correction was itself wrong, and running a build shows it:

    $ remote-docker docker build .
    Sending build context to Docker daemon  30.93MB
    Step 1/1 : FROM debian
    Successfully built 826a5616954e

That is the CLASSIC builder. `build` being present in `docker/cli`'s command
tree is not the same as BuildKit being available: buildx is still a separate
plugin binary, it is not in go.mod, and without it `docker build` falls back to
the pre-BuildKit path -- silently, and even with `DOCKER_BUILDKIT=1`.

The daemon is not the limitation. It advertises `Builder-Version: 2` on
`/_ping`; the embedded client cannot use it.

So the functional gap this record was most worried about DOES exist, and it is
the one thing embedding costs that nothing else does: no cache mounts, no
parallel stages, and the whole context re-uploaded on every build rather than
streamed incrementally. Anyone who wants BuildKit can point a real docker CLI
at the workspace's docker context, which is written for exactly this reason --
`docker --context <workspace> build .` uses that machine's buildx.

**Compose is still a separate module** and is not obtained by embedding the
CLI. `docker compose` falls through to the parent's help.

### Compose: attempted, and deliberately not taken

Three attempts, each failing differently, which together explain why:

| Attempt | Result |
|---|---|
| `docker/cli@latest` + Compose | buildx v0.29 uses `github.com/docker/docker` types; cli v29.7 uses `github.com/moby/moby` — the same types under two module paths |
| force buildx v0.36 | Compose needs `moby/buildkit/util/tracing/env`, absent from the buildkit that buildx pins |
| let Compose choose the whole stack | **builds** — cli v28.5.1, buildx v0.29.1, buildkit v0.25.1, 86 MB, 36 subcommands |

### 2026-08: buildx is now embedded, and compose still is not

The blocker moved rather than vanished. buildx v0.36 builds against
`docker/cli` v29.7.2 -- no downgrade, no three-way pin -- so `docker build`
routes through BuildKit, which is what a real docker CLI does when the plugin
is present. `build` is REPLACED rather than added beside: upstream, `docker
build` IS `docker buildx build`, and the classic builder is only the fallback.

Two things this needed that the plugin harness usually does:

- **The drivers register themselves via blank imports in buildx's own main.**
  Without them the command is present, correctly wired, and answers "no drivers
  available".
- **buildx's ROOT command is not registered.** Its subcommands expect the
  plugin harness to have run and `docker buildx version` panics on a nil
  dereference without it. `build` is what docker's tree exposes anyway.

It cost 45 MB: 45 -> 91 MB. That is the price of the feature this record was
originally most worried about losing, and it buys the `/session` hijack its
first exercised caller -- BuildKit streams the context over it, and until now
nothing did.

**Compose remains out, for the reason below with new numbers.** compose v2.40.3
requires buildx v0.29.1, buildkit v0.25.1 and `docker/cli` v28.5.1, while
buildx v0.36.1 requires buildkit v0.32.2 -- which no longer contains
`moby/buildkit/util/tracing/env`, the package compose imports. Taking compose
means pinning `docker/cli` back a major version AND buildx back seven minors,
losing the modern builder to gain a command that already works through the
proxy as a separate binary.

**Compose's unreleased main was tried too, and does not help.** The obvious
guess is that compose simply lags -- v2.40.3 predates buildx v0.36 -- so
`@main` should have caught up. It has not: the November 2025 commit still
imports `moby/buildkit/util/tracing/env`, a package buildkit deleted somewhere
between v0.25 and v0.32. Compose has not adapted at all, so there is no
version of it that builds against the buildkit buildx now requires.

**Pinning buildkit back does not work either, and this was measured rather
than assumed.** With `replace github.com/moby/buildkit => v0.25.1` -- the
version compose wants -- buildx v0.36 fails to build: it needs
`moby/buildkit/util/pgpsign`, which v0.25.1 does not have. The two have
diverged in BOTH directions, so no single buildkit satisfies them. That closes
the last option that did not involve owning somebody else's source.

**What compose actually needs is one line**, which makes the situation more
annoying rather than less. The import is blank:

	_ "github.com/moby/buildkit/util/tracing/env" //nolint:blank-imports

Compose calls nothing from that package. It is pulled in for an `init()` that
wires OTEL trace-context propagation from the environment -- no API surface at
all. So compose is not blocked on a migration; nobody upstream has deleted or
repointed the line since buildkit dropped the package.

**Go has no per-file override, and that is what makes a one-line problem
expensive.** `replace` works at module granularity and `go mod vendor` is
all-or-nothing:

| route | cost |
|---|---|
| `go mod vendor`, then delete the import | vendors EVERYTHING -- cli, buildx, buildkit, containerd, the k8s libraries -- tens of thousands of files in git, and the edit reapplied by hand after every regeneration |
| `replace` to a local directory | a complete copy of the replaced module in-tree; for buildkit that is the builder itself, rebased on every release |

Either way the price of a one-line fix is owning a fork of the component that
builds the images, to restore a package compose imports for a side effect it
does not use. Not proportionate: `docker compose` already works through the
proxy as a separate binary, so what is missing is the "nothing to install"
premise, not the functionality.

When compose adopts a current buildkit, embedding it becomes two lines --
`installCompose` in cmd/remote-docker/docker.go is written and was reverted
rather than never attempted. Dependabot will surface the release that makes it
possible.

So it is possible, and it is **not taken**. The working combination requires
pinning `docker/cli` back from v29.7.2 to v28.5.1 — a major version — and
roughly doubles the binary, in exchange for a three-way version pin that any
independent upgrade breaks.

The cause is that the Docker ecosystem is mid-migration from
`github.com/docker/docker` to `github.com/moby/moby`, and cli, buildx and
Compose are not mutually consistent across it right now. Downgrading a major
version of our primary dependency to work around somebody else's in-progress
rename is the kind of decision that is cheap today and expensive later.

**What is actually lost is small.** Compose already works *through* the proxy,
and the integration suite proves it — that is the point of translating at the
API rather than in a command wrapper (ADR 0005). The gap is only someone with
*nothing* installed, and in practice the Docker CLI and Compose ship together,
so a machine with one usually has both.

Revisit when buildx and Compose have completed the `moby/moby` migration. The
working combination above is recorded so that revisit does not repeat these
three attempts.

Two integration hazards, neither of which a dependency-weight spike could have
surfaced, because both need the command tree actually built and run:

- **Client options must go on `Flags()`, not `PersistentFlags()`.** Cobra
  merges persistent flags into every subcommand, and `--context` has the
  shorthand `-c`, which `build` already uses for `--cpu-shares`. Installing
  them persistently makes `docker build --help` *panic*. The real CLI uses
  `Flags()` with `TraverseChildren: true`, which is what still lets
  `docker --context x ps` parse.
- **The genproto exclusion has to be at whole-module scope.** `docker/cli` is
  a `+incompatible` module, so its `go.mod` constraints do not propagate
  through MVS, and the pre-split `google.golang.org/genproto` monolith still
  provides `googleapis/api/annotations` — as does the split
  `genproto/googleapis/api`. Requiring the newer monolith is not enough:
  `go mod tidy` drops a require that nothing imports directly, and MVS then
  picks the old one back up from a transitive dependency. An `exclude` of the
  old version in `go.mod` is what actually holds.

42 MB is acceptable for a tool whose entire premise is that nothing has to be
installed. Compose will add to it.

## Decision

Embed `docker/cli`, `buildx` and `compose/v2` into the `remote-docker` binary,
registering their cobra command trees so that everything the real CLI can do,
this binary can do, against our own endpoint.

Ship the whole tree rather than curating it. Swarm commands (`docker swarm`,
`service`, `stack`, `node`) are neither the point of this project nor tested
against it, but removing them means maintaining a fork of someone else's
command tree forever. They are documented as untested, not deleted.

## Consequences

- One binary provides the daemon connection, the filesystem, the builder and
  the client. Genuinely zero-install, which is the whole premise.
- **The proxy must be transparent to hijacked and streamed connections.** This
  follows directly from `docker build` and is the main technical consequence:
  `/session` is an HTTP upgrade carrying gRPC, and `/containers/*/attach`,
  `/exec/*/start`, `/build`, `/events` and `/logs` are all long-lived or
  bidirectional. A proxy that buffers, or that only understands
  request/response, appears to work for `docker ps` and then fails at exactly
  the commands people care about. See ADR 0005.
- Build contexts upload from the client over the SSH connection. Large contexts
  are therefore bound by the tunnel, which makes `.dockerignore` hygiene matter
  as much as keeping build artifacts off the NFS share does (ADR 0002).
- We inherit three large dependency trees and their release cadence. A CVE
  anywhere in them becomes ours to ship a fix for, and binary size is
  substantially larger than a client that assumed a local `docker`. Accepted:
  the alternative does not meet the requirement.
- Version skew is now ours to manage. The embedded CLI, buildx and Compose must
  agree with each other and negotiate an API version the workspace daemon
  supports. This wants an integration test that runs the real command tree
  against the real daemon, not a unit test of our own code.
- The three trees must be registered so their `--help`, completion and plugin
  discovery behave like the real thing. A client that is fully fledged except
  that `docker build --help` is wrong is not fully fledged.

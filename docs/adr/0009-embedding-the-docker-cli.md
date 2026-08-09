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

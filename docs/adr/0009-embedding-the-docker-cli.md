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

## Spike result

Measured, rather than assumed, before accepting:

| | |
|---|---|
| builds at `CGO_ENABLED=0` | yes — the single-binary premise survives |
| stripped binary | 28.4 MB, `docker/cli` alone |
| modules in the graph | 205 |
| subcommands registered | 57 |

One snag worth recording, because it will recur for anyone adding these
dependencies: `docker/cli` is a `+incompatible` module, so its own `go.mod`
constraints do not propagate through MVS. Resolving from an empty module
produces an ambiguous import of
`google.golang.org/genproto/googleapis/api/annotations`, which is provided by
both the pre-split monolith and the split module. Pinning
`google.golang.org/genproto@latest` first, then tidying, resolves it.

28 MB is acceptable for a tool whose entire premise is that nothing has to be
installed. Buildx and Compose will add to it.

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

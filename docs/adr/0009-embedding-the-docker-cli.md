# 0009 — Embedding the Docker CLI, Buildx and Compose

- Status: Accepted
- Date: 2026-08-07, last measured 2026-08-19
- Current answer: **all three are embedded** — `docker/cli`, `buildx` (so
  `docker build` is BuildKit) and `compose/v5`. 95 MB, no fork, no downgrade.

## Context

- ADR 0005 gives a Docker API endpoint any client can use, but the premise is a
  machine that cannot have software installed, and the `docker` CLI is software.
- The requirement is a **fully fledged client**, not a curated subset, and
  specifically `docker build` — the piece most likely to be dismissed as too
  hard.
- All three are Go programs on cobra exposing their command trees as libraries.
  Buildx matters because it is *not* part of `docker/cli` core.
- `docker build` is better behaved through a proxy than expected: BuildKit
  uploads the context over the API as a stream, so it never reaches the workspace
  through NFS. What it needs is `/session`, an HTTP upgrade carrying gRPC.

## The decision

Embed all three, registering their cobra trees against our own endpoint. Ship
the whole tree rather than curating it: removing Swarm commands means
maintaining a fork of somebody else's command tree forever, so they are
documented as untested instead (`docker swarm`, `service`, `stack`, `node`).

| | size |
|---|---|
| `docker/cli` alone | 42 MB, 57 subcommands |
| + buildx | 91 MB |
| + compose v5 | 95 MB — it shares cli, buildx and buildkit |

**Registration is the plugin harness done by hand.** Each of these is normally a
separate binary docker execs, and using their plugin entry points would
initialise a second CLI over the one already pointed at our endpoint:

- **buildx's drivers register via blank imports in buildx's own `main`.**
  Without them the command is present, correctly wired, and answers "no drivers
  available".
- **buildx's ROOT command is not registered.** Its subcommands expect the harness
  to have run, and `docker buildx version` panics on a nil dereference without
  it. `build` is what docker's tree exposes anyway, and upstream `docker build`
  IS `docker buildx build` — so `build` is REPLACED, not added beside.
- **compose is `pluginMain` minus the harness**: build a `BackendOptions`
  carrying the confirmation prompt, call `commands.RootCommand(cli, opts)`, add
  `HooksCommand`. `plugin.Run` is deliberately not used.

## Two integration hazards, neither visible from a dependency graph

Both need the command tree actually built and run.

- **Client options go on `Flags()`, never `PersistentFlags()`.** Cobra merges
  persistent flags into every subcommand, and `--context` has the shorthand `-c`,
  which `build` already uses for `--cpu-shares`: installing them persistently
  makes `docker build --help` *panic*. The real CLI uses `Flags()` with
  `TraverseChildren: true`, which is what still lets `docker --context x ps`
  parse.
- **The genproto exclusion has to be at whole-module scope.** `docker/cli` is a
  `+incompatible` module, so its `go.mod` constraints do not propagate through
  MVS, and the pre-split `google.golang.org/genproto` monolith still provides
  `googleapis/api/annotations` — as does the split `genproto/googleapis/api`.
  Requiring the newer monolith is not enough: `go mod tidy` drops a require that
  nothing imports directly, and MVS picks the old one back up transitively. An
  `exclude` of the old version in `go.mod` is what holds.

## `build` in the tree is not BuildKit

The trap that cost this record two wrong assumptions. `build` being present in
`docker/cli`'s tree says nothing about which builder runs it:

```
$ remote-docker docker build .
Sending build context to Docker daemon  30.93MB
Step 1/1 : FROM debian
Successfully built 826a5616954e
```

That is the CLASSIC builder — silently, even with `DOCKER_BUILDKIT=1`, and with
the daemon advertising `Builder-Version: 2` on `/_ping`. Only linking buildx
fixes it. `test/integration.sh` therefore asserts the build is BuildKit rather
than asserting that `docker build` succeeded.

## Compose, and why the exclusion expired

Compose was excluded twice for a dependency deadlock in the ecosystem's
`github.com/docker/docker` to `github.com/moby/moby` migration: taking compose
v2.40.3 meant pinning `docker/cli` back a major version and buildx back seven
minors, over one blank import (`moby/buildkit/util/tracing/env`) that no single
buildkit version satisfied for both. compose v5 builds against the stack this
binary already carries, so embedding it was `go get` plus one file.

**A revisit trigger nobody evaluates is a decision that has quietly stopped
being true.** "Compose cannot be embedded" was quoted as current fact in the
README, in `--help` and in advice to a user, for months after this was the
check:

```bash
(cd client && go get github.com/docker/compose/v5 && go build ./...)
```

## Consequences

- **One binary provides the daemon connection, the filesystem, the builder and
  the client.** Genuinely zero-install, which is the whole premise.
- **The proxy must be transparent to hijacked and streamed connections**
  ([ADR 0005](0005-docker-api-proxy-over-cli-wrapper.md) states the rule).
  `docker build` is what forces it here: `/session` is an HTTP upgrade carrying
  gRPC.
- **Build contexts upload from the client over the SSH connection**, so large
  contexts are bound by the tunnel and `.dockerignore` hygiene matters as much as
  keeping build artifacts off the NFS share (ADR 0002).
- **Three large dependency trees and their release cadence are ours.** A CVE
  anywhere in them is ours to ship a fix for, and the binary is far larger than a
  client assuming a local `docker`. Accepted: the alternative does not meet the
  requirement.
- **Version skew is ours to manage.** The embedded CLI, buildx and compose must
  agree with each other and negotiate an API version the workspace daemon
  supports. That wants an integration test running the real command tree against
  a real daemon, not a unit test of our own code.
- **The trees must be registered so `--help`, completion and plugin discovery
  behave like the real thing.** A client that is fully fledged except that
  `docker build --help` is wrong is not fully fledged.

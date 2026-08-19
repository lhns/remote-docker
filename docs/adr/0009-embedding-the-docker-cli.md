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

## How compose got here, and the lesson that outlives it

Compose was excluded twice, both times for a real dependency deadlock in the
ecosystem's `github.com/docker/docker` → `github.com/moby/moby` migration:

- compose v2.40.3 wanted cli v28.5.1, buildx v0.29.1 and buildkit v0.25.1, while
  buildx v0.36.1 wanted buildkit v0.32.2. Taking compose meant pinning `docker/cli`
  back a major version and buildx back seven minors.
- The blocker was one blank import — `moby/buildkit/util/tracing/env`, wired for
  an `init()` compose calls nothing from — deleted from buildkit between v0.25
  and v0.32. Compose's `main` had not adapted either, and no single buildkit
  satisfied both, so the only routes were `go mod vendor` (all of it, re-edited
  after every regeneration) or a `replace` onto a fork of the builder itself.
  Not proportionate for a side effect nobody used.

**compose v5 resolved it**: it builds against buildx v0.36, buildkit v0.32 and
`moby/moby/api` v1.55 — the stack this binary already carries — so embedding it
was `go get` plus one file, with no downgrade and no fork.

**The lesson is about the record, not the dependency.** This ADR said "revisit
when buildx and Compose have completed the migration", and then its conclusion
outlived the condition: "compose cannot be embedded" was quoted as current fact
in the README, in `--help`, and in advice to a user to install a standalone
compose. A revisit trigger nobody evaluates is a decision that has quietly
stopped being true. The check was one command:

```bash
(cd client && go get github.com/docker/compose/v5 && go build ./...)
```

## Consequences

- **One binary provides the daemon connection, the filesystem, the builder and
  the client.** Genuinely zero-install, which is the whole premise.
- **The proxy must be transparent to hijacked and streamed connections.** The
  main technical consequence, following directly from `docker build`: `/session`
  is an HTTP upgrade carrying gRPC, and `/containers/*/attach`, `/exec/*/start`,
  `/build`, `/events` and `/logs` are long-lived or bidirectional. A proxy that
  buffers, or only understands request/response, works for `docker ps` and fails
  at exactly the commands people care about (ADR 0005).
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

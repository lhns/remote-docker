# 0021. Three modules: shared, client, agent

- Status: Accepted; supersedes the single-module half of
  [ADR 0011](0011-one-module-shared-contract.md)
- Date: 2026-08-10

## Context

ADR 0011 put both binaries in one module so the things they must agree on could
live in one place. The contract half of that decision was right and is kept.
The single-module half broke the build.

`docker/buildx` was vendored into the client (ADR 0009). Buildx requires a
newer Go than the workspace image's `golang:1.25-alpine` builder, so the module
directive moved to `go 1.26.3` — and the golang images set `GOTOOLCHAIN=local`,
which turns "go.mod asks for more than I have" into a hard failure rather than
a toolchain fetch. **The agent stopped compiling**, with an error naming the
toolchain rather than the dependency that moved it, and the workspace image
could not be built for two days.

The agent imports **none** of buildx. Measured rather than assumed:

| | third-party modules | `go.sum` |
|---|---|---|
| agent | 7 | 24 lines |
| client | ~130 | 786 lines |
| shared | 1 | 2 lines |

A binary that imports nothing of a dependency was broken by that dependency,
because a module is the unit of version resolution and they shared one.

The immediate failure has a smaller fix — `GOTOOLCHAIN=auto`, already applied —
but it only converts that class of breakage into a silent toolchain download.
The coupling remains: any client dependency can raise the floor for a binary
that never sees it.

## Decision

Three modules, with the **shared one at the repository root** and both binaries
beneath it:

```
go.mod          github.com/lhns/remote-docker           pkg/workspace, internal/logx, test probes
client/go.mod   github.com/lhns/remote-docker/client    cmd/remote-docker, internal/…
agent/go.mod    github.com/lhns/remote-docker/agent     cmd/remote-dockerd, internal/…
```

Both binaries `require` the root module with a relative `replace`, so an
in-repo build never needs a published tag for a change made in the same commit.
**The agent must never require the client module**, which would pull the graph
straight back in.

The root is the shared module rather than a fourth one because of what that
placement makes possible: `pkg/workspace` stays *exported*, for the third-party
client ADR 0011 wants to allow, while `internal/logx` stays *internal* and is
still importable by both binaries — Go's internal rule is path-based, and both
module paths sit under `github.com/lhns/remote-docker/`. No package has to
choose between being internal and being shared.

`go.work` exists for local development across all three. CI and the image build
deliberately do **not** use it: each builds one module, resolving through that
module's own `go.mod`, so a missing `require` fails where it is wrong rather
than being covered by the workspace.

### What belongs in the shared module

The same discipline ADR 0011 wrote for the contract, one step wider:

> It goes in the shared module if both binaries must behave the **same way**,
> not merely if both do something similar.

- **`pkg/workspace`** — the contract. Unchanged rule: it goes here if the two
  sides must *agree* on it.
- **`internal/logx`** — one person reads both programs' output.
- **The bidirectional copy with half-close** (not yet moved) — the two sides
  are the two ends of one stream. They currently have two `closeWrite` helpers
  with *opposite* fallbacks: the agent's does nothing when `CloseWrite` is
  unavailable, the client's calls `Close()`, which is what the project's own
  invariant forbids. It cannot fire today; that it can differ at all is the
  argument.

Deliberately **not** shared: `envOr`/`envInt` (trivially small and differently
shaped), `netns`, `dockercli`, `elevate` (one side only), and the
path-containment helpers — `notify.under` is a security check on untrusted
daemon output and the client's lookalike is unrelated.

## Consequences

- **A client dependency can no longer break the agent's build.** That is the
  whole purchase.
- **`./...` stops at a module boundary**, and this is the trap. `go test ./...`
  at the root now covers the shared module and nothing else — it would pass
  while testing almost none of the repository. Every CI job loops over the
  three modules explicitly, and CLAUDE.md's build commands do too.
- **`golangci-lint` runs four times**: once per module, plus `GOOS=linux` for
  the agent, whose session handling is invisible to a host-only lint.
- **Dependabot needs one entry per module.** It does not discover nested
  modules; a directory missing from `dependabot.yml` is a module whose
  dependencies silently stop being updated.
- **Publishing gains an ordering constraint.** An outside consumer of a
  contract change needs a `pkg/workspace` tag on the root module before it can
  take it. In-repo the `replace` makes this invisible, which is exactly how it
  will be forgotten — hence this paragraph.
- **`go.work` must not be copied into the image build.** It would resolve the
  agent through the workspace rather than through its own `go.mod`, so a
  missing require would build there and fail everywhere else.
- The client tree moved to `client/`, so `goreleaser`'s `dir:`, the
  Dockerfile's `COPY`s, the test suites' build lines and every documented
  command changed with it. One-time cost, paid here.

*(2026-08-11: the split paid a dividend it was not designed for. Shipping the
agent as its own release artifact for a VM workspace (ADR 0025) is a goreleaser
block with `dir: agent` and 24 lines of go.sum, where the same binary built out
of the single module would have dragged docker/cli and buildx behind it.)*

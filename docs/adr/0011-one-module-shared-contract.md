# 0011. One module with a shared contract package

- Status: Accepted; the single-module decision is superseded by
  [ADR 0021](0021-three-modules.md). The contract rule below is unchanged and
  still governs what may live in `pkg/workspace`.
- Date: 2026-08-07

## Context

The client and the server agree on several things that are not written down in
any one place: the uid → port mapping, the `KEY=VALUE` shape of the workspace
info reply, the naming of managed volumes, and the layout of the NFS export
namespace.

The mapping in particular has already caused trouble. It appeared in two shell
scripts, `workspace-info` and `workspace-mount`, both sourcing a config file
written once by the entrypoint. The project's own notes flag it as an invariant
that "fails quietly": if the two copies ever disagree, the client tunnels to one
port while the mount reads another, and the failure presents as a network
problem rather than as the configuration drift it is.

Once both sides are Go, the duplication is avoidable rather than merely
documented.

## Decision

One Go module containing both binaries, with everything the two sides must
agree on in `pkg/workspace`, imported by both.

`internal/client/…` and `internal/server/…` hold what belongs to one side only.
A type used by one binary does not go in `pkg/workspace`.

## Consequences

- **Drift becomes a compile error.** There is one `PortForUID`, and the agent's
  port-ownership check and the client's tunnel target are the same function.
  The invariant is enforced rather than described.
- The wire format is a tested round trip rather than a shell `echo` and a
  regular expression.
- `pkg/workspace` is exported, not internal, because the contract is worth
  depending on from outside — a third-party client is a legitimate thing to
  build against it.
- Two binaries share a release cadence. In practice they already did: a client
  and a workspace image that disagree about the mapping have never worked
  together anyway.
- The temptation this creates is to put convenience helpers in the shared
  package because both sides happen to want them today. That is how a contract
  package turns into a utility dump and stops meaning anything. The rule is
  narrow: it goes in `pkg/workspace` only if the two sides must *agree* on it,
  not merely if both use it.

## Superseded in part (ADR 0021)

One module turned out to have a cost this record did not foresee: a module is
the unit of version resolution, so the client's dependencies set the Go
toolchain floor for the agent as well. A `docker/buildx` bump raised the
directive past the workspace image's pinned builder and the agent -- which
imports no buildx -- stopped compiling.

The repository now has three modules. **The rule above is unchanged**: what may
live in `pkg/workspace` is still only what both sides must agree on, and making
it a separate module is what finally makes the "a third-party client can build
against the contract" claim true rather than aspirational.

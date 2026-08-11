# 0026. A machine is a workspace we provision

- Status: Accepted; extends [ADR 0025](0025-the-agent-as-a-guest.md)
- Date: 2026-08-11

## Context

The project can talk to a Docker daemon on any Linux system, and ADR 0025 made
the agent run on one that is a machine rather than a container. On Windows
there is no such system, and making one is the entire reason Docker Desktop
exists.

The client already carries everything needed to *use* a workspace. What it
cannot do is *make* one.

## Decision

**A machine is a workspace this program provisioned, and nothing else about it
is special.** `remote ls` lists it, `remote use` selects it, `remote status`
reports on it, and a session reaches it over SSH and serves files back over NFS
exactly as it does for a host on another continent. There is **no second data
path**: the session, the export, the port forwarding and the bind rewriting are
untouched.

What is added is a lifecycle, held in one new config block:

```json
"machine": { "backend": "wsl", "name": "rd-dev", "image": "...", "generation": "..." }
```

Its presence is the only thing that changes anywhere: `remote rm` has a machine
to destroy as well as an entry to delete.

**Nothing is installed at provisioning time.** No package manager runs, on any
path, ever. The unit of change is a published artifact named in that block, and
moving between versions replaces the artifact rather than mutating what is
there. This is the property the whole design is arranged around: there is no
half-finished install to be in, because there is no install.

It follows that **rebuild is not a repair mode**. It is the ordinary path run
again, which is the only kind of self-healing that can be trusted. It discards
what was inside the machine, and says so: the thing most likely to be corrupt
is exactly what a "preserve" option would preserve. The user's files are never
at risk — they live on the host and are served *to* the machine.

**A generation marker** identifies the settings a machine was built from, so
"out of date" is a fact rather than a guess. Same trick `daemons.reconcile`
uses for per-account daemons. A backend that cannot read one is treated as a
match, never as a mismatch: recreating somebody's machine because a label could
not be read would destroy their work to satisfy our bookkeeping.

## Consequences

- **`remote rm` refuses rather than orphaning.** If the machine cannot be
  destroyed — wrong platform, backend unavailable — the config entry stays,
  because it is the only record that a Linux system was built. Deleting it
  anyway leaves a machine running with nothing naming it.
- **The decisions are pure and the platform calls are an interface**
  (`client/internal/machine`), the shape `elevate` and `daemons` already use.
  It matters more here: **nobody working on this project has WSL or Hyper-V**,
  so anything that is not a pure function is code that ships without having
  run. The interface exists to make that surface as small as it can be.
- **A GitHub Windows runner can run WSL2**, which halves that problem for one
  backend. Measured rather than assumed: `HypervisorPresent: True`, an imported
  rootfs reporting `6.18.33.2-microsoft-standard-WSL2`, and `wsl -l -v` showing
  `VERSION 2`. *(Checked 2026-08-11, run 31496228112. Re-check by restoring the
  spike workflow from that commit.)* Hyper-V is not available on those runners
  and will have no automated coverage at all.
- **This is where "nothing needs to be installed" stops being true.** WSL needs
  a one-time install, and Hyper-V lifecycle needs administrator rights. The
  premise held for every earlier decision; it does not hold for this one, and
  saying so is better than quietly elevating.
- **Two backends that share nothing** but the interface. That is the cost of
  covering both the machine that has WSL and the one that cannot.

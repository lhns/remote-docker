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

Its presence changes two things, and this record originally claimed one.

`remote rm` has a machine to destroy as well as an entry to delete. And a
session **locates** the machine before it dials it, which a workspace on another
host never needs: that host is simply there, at an address that was written down
once. A machine is not. It is started on demand and given its address at boot,
so `host` in the entry is a placeholder and the real answer is asked for at
every connection, in `session.connect` and nowhere else.

Measured on 2026-08-11, in the `a machine on wsl` job, which is why the
placeholder is not simply `127.0.0.1`: with the machine running and its agent
listening, Windows could not reach `127.0.0.1:2222` at all and reached the
machine's own `172.24.110.158:2222` immediately. WSL2 forwards localhost through
a relay that did not carry it there, and re-checking that is one CI run.

The same job established the other half: a machine nobody is using goes away.
WSL shuts an idle distribution down, and a TCP connection from Windows is not
use it counts -- the first version of `create` spent three minutes dialling a
machine that had stopped two and a half minutes after its own agent reported it
was listening. So locating a machine starts it, and starting a running machine
is what keeps it running.

## Both backends are located the same way, and Hyper-V uses no hvsock

The design for Hyper-V originally called for Hyper-V sockets, on the argument
that they avoid discovering a NAT address that changes on every boot -- the
fragile thing tools in this space get wrong first.

That argument was answered by building WSL. Discovering the address is one
platform call per connection, it is measured, and it costs no agent change at
all. hvsock would have meant a pluggable listener in the agent, an AF_HYPERV
listener on the Linux side, a service GUID registered per machine on the host,
and a new dependency -- all of it in code no CI can execute and nobody involved
can run. The same reasoning that makes the decisions pure functions says to pick
the transport that adds nothing untestable.

So a Hyper-V machine is on the Default Switch, which is NAT with DHCP that
Hyper-V maintains itself, and `Address` asks `Get-VMNetworkAdapter`. If that
turns out not to work on a real machine, hvsock is still there, and this record
is where to start.

Two things do differ from WSL, and both are the platform rather than a choice:

- **A Hyper-V machine has no idle timeout**, so its `Hold` is nothing. A WSL
  distribution shuts down when nobody is in it, which is the harder case and the
  one the interface is shaped by.
- **A key can only be enrolled at creation.** The guest is Linux, so PowerShell
  Direct does not apply, and the only door is the SSH the key is for. The key
  goes into the Ignition document, its fingerprint into the VM's Notes, and
  `Enrol` on an existing machine reports a mismatch rather than writing
  anything. It is deliberately not part of the generation: a rotated key would
  then trigger a rebuild on its own, and a rebuild discards every image.

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

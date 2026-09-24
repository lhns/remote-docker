# 0026. A machine is a workspace we provision

- Status: Accepted; extends [ADR 0025](0025-the-agent-as-a-guest.md)
- Date: 2026-08-11

## Context

ADR 0025 runs the agent on a Linux machine rather than a container. Windows has
no such machine, and making one is the reason Docker Desktop exists. The client
can already *use* a workspace; it cannot *make* one.

## Decision

**A machine is a workspace this program provisioned, and nothing else about it
is special.** `remote ls`, `use` and `status` treat it like any workspace, and
a session reaches it over SSH and serves files over NFS as it would a host on
another continent. There is **no second data path**. What is added is a
lifecycle, in one config block:

```json
"machine": { "backend": "wsl", "name": "rd-dev", "image": "...", "generation": "..." }
```

Its presence changes two things:

- **`remote rm`** has a machine to destroy as well as an entry to delete.
- **A session locates the machine** before dialling it, in `session.connect`
  and nowhere else. A machine is started on demand and given its address at
  boot, so `host` in the entry is a placeholder and the address is asked at
  every connection.

Both were measured on 2026-08-11 in `machine.yml`'s `a machine on wsl` job
(re-check: one run of that workflow):

- With the machine running and its agent listening, Windows could not reach
  `127.0.0.1:2222` and reached the machine's own `172.24.110.158:2222` at once:
  WSL2's localhost relay did not carry it. Hence not simply `127.0.0.1`.
- WSL shuts an idle distribution down, and a TCP connection from Windows does
  not count as use: a machine stopped two and a half minutes after its agent
  reported listening. So locating a machine starts it, and a hold keeps it
  running (a `wsl.exe` session held open; `machine.Hold`).

## The Hyper-V backend was merged unverified, on purpose

The plan was that a backend merges once somebody has run
`docs/testing-machines.md` against it and reported. WSL cleared that. Hyper-V
cannot: no CI offers it and nobody involved has it, so the bar would hold the
code in a branch indefinitely. It merged unrun because it costs nothing until
somebody types `--backend hyperv`: no other path reaches it, and the WSL
backend imports none of it.

The price is honesty, in four places that must stay in step:

- a warning the program prints on every `machine create` with that backend;
- `--backend`'s help;
- CLAUDE.md's NOT-tested list, where it is the strongest entry;
- the README, which says it has never been run by anybody.

The first goes away when somebody has run the runbook and reported. Until
then it is a written-down attempt, and anything saying otherwise is wrong.

## Both backends are located the same way, and Hyper-V uses no hvsock

Hyper-V sockets would avoid discovering a NAT address that changes every boot.
But WSL showed discovery is one measured platform call per connection with no
agent change, while hvsock needs a pluggable listener in the agent, AF_HYPERV
on the Linux side, a service GUID per machine on the host and a new
dependency, all in code no CI can run. So a Hyper-V machine is on the Default
Switch (NAT with DHCP Hyper-V maintains) and `Address` asks
`Get-VMNetworkAdapter`. If that fails on a real machine, hvsock is the
fallback and this record is where to start.

Two platform differences from WSL:

- **No idle timeout**, so a Hyper-V `Hold` is nothing. WSL's shutdown is the
  harder case and shapes the interface.
- **A key can only be enrolled at creation.** The guest is Linux, so
  PowerShell Direct does not apply and the only door is the SSH the key is for.
  The key goes into the Ignition document and its fingerprint into the VM's
  Notes; `Enrol` on an existing machine reports a mismatch rather than writing
  anything. It is not part of the generation, or a rotated key would trigger a
  rebuild, which discards every image.

## The image is the artifact, and there is no second one

A machine is built by pulling the workspace image and flattening it in the
client, with no docker: a container image IS a rootfs, and `mutate.Extract`
(`machine/rootfs.go`) is what `docker export` does. Nothing is published for
machines.

- **Rejected: a rootfs tarball per release** (built, then removed). It is a
  second name for one thing: the config's image reference said one version,
  an arch-named tarball another, and a release whose upload failed would
  silently have served the previous version.
- Pulling means the config's version IS the machine, the registry resolves the
  architecture, and the digest verifies the download. `--rootfs` stays for
  air-gapped use, an image of your own, or a version pinned by hand.
- No new dependency: go-containerregistry came with docker/cli. If client-side
  flattening ever fails, the fallback is a flattened tar pushed as an OCI
  artifact, which reintroduces a publish step.

**Nothing is installed at provisioning time.** No package manager runs, on any
path. Moving between versions replaces the artifact rather than mutating what
is there, so there is no half-finished install to be in.

So **rebuild is not a repair mode**: it is the ordinary path run again, the
only self-healing that can be trusted. It discards what was inside the machine
and says so, since the thing most likely corrupt is what a "preserve" option
would preserve. The user's files live on the host and are never at risk.

**A generation marker** records the settings a machine was built from, so "out
of date" is a fact (the trick `daemons.reconcile` uses with its spec label). A
backend that cannot read one counts it as a match: recreating a machine because
a label could not be read destroys work for bookkeeping.

## Consequences

- **`remote rm` refuses rather than orphaning.** If the machine cannot be
  destroyed (wrong platform, backend unavailable) the config entry stays: it is
  the only record a Linux system was built.
- **The decisions are pure functions and the platform calls an interface**
  (the `machine` module, ADR 0021), as in `elevate` and `daemons`. Here it
  matters more: **nobody working on this has WSL or Hyper-V**, so everything
  that is not a pure function ships without having run.
- **A GitHub Windows runner can run WSL2**: `HypervisorPresent: True`, an
  imported rootfs reporting `6.18.33.2-microsoft-standard-WSL2`, `wsl -l -v`
  showing `VERSION 2`. *(Checked 2026-08-11, run 31496228112. Re-check by
  restoring the spike workflow from that commit.)* Hyper-V is not available
  there and has no automated coverage.
- **"Nothing needs to be installed" stops being true here.** WSL needs a
  one-time install and Hyper-V needs administrator rights; the program reports
  that rather than quietly elevating.
- **Two backends that share nothing** but the interface: the cost of covering
  both the machine that has WSL and the one that cannot.

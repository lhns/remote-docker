# 0026. A machine is a workspace we provision

- Status: Accepted; extends [ADR 0025](0025-the-agent-as-a-guest.md)
- Date: 2026-08-11; amended 2026-10-04 (host keys), 2026-10-05 (Hyper-V run by hand, Ignition over KVP)
- Current answer: a machine is an ordinary workspace plus a lifecycle, with its
  host key made and pinned by the client. WSL runs in CI; Hyper-V has run by
  hand on one computer and takes its Ignition document over KVP, removed once
  the agent answers.

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
"machine": { "backend": "wsl", "name": "rd-dev", "image": "...", "generation": "...", "hostKey": "ssh-ed25519 ..." }
```

Its presence changes three things:

- **`remote rm`** has a machine to destroy as well as an entry to delete.
- **A session locates the machine** before dialling it, in `session.connect`
  and nowhere else. A machine is started on demand and given its address at
  boot, so `host` in the entry is a placeholder and the address is asked at
  every connection.
- **A session accepts the host key the machine was built with**, not
  known_hosts' answer for its address (see below).

Locating rests on two measurements, made on 2026-08-11 in `machine.yml`'s
`a machine on wsl` job (re-check: one run of that workflow):

- With the machine running and its agent listening, Windows could not reach
  `127.0.0.1:2222` and reached the machine's own `172.24.110.158:2222` at once:
  WSL2's localhost relay did not carry it. Hence not simply `127.0.0.1`.
- WSL shuts an idle distribution down, and a TCP connection from Windows does
  not count as use: a machine stopped two and a half minutes after its agent
  reported listening. So locating a machine starts it, and a hold keeps it
  running (a `wsl.exe` session held open; `machine.Hold`).

## A machine's host key is made with it, and pinned per machine

- **Forced by:** `machine rebuild` on a WSL machine, after which every command
  failed with `host key for 172.18.12.79:2222 has CHANGED` (2026-10-04).
  known_hosts keys trust on an address, and a machine's address names nothing:
  it is given out at boot and every WSL distribution shares the WSL VM's. A
  rebuild, `rm` then `create`, or another machine on the same port all offer a
  new key at a recorded address.
- **Decision:** the client makes the host key when it builds the machine
  (`keys.NewHostKey`) and writes the private half where the agent loads it,
  before the agent first starts:

  | Backend | How the private half goes in |
  |---|---|
  | WSL | `wsl.exe` stdin into `sh -c 'umask 077 && cat > ...'`, never argv |
  | Hyper-V | one more file in the Ignition document, mode 0600, handed over KVP |

  The public half is the machine block's `hostKey`, recorded before the agent
  is waited for, so a build that fails after it cannot leave the destroyed
  machine's key pinned. A session accepts that key and no other, at any
  address, and never reads or writes known_hosts for it
  (`session.hostKeyRule`).
- **Rejected:** forgetting known_hosts entries on rebuild and `rm`. Still trusts
  whatever answers first, and misses the address being reused by another
  machine.
- **Rejected:** reading the agent's generated key out of the machine. WSL could;
  Hyper-V cannot, as there is no way into a Linux guest but the SSH it opens.
- **Costs:**
  - The private key passes through the client. For Hyper-V it also sits in
    the VM's KVP items, readable by any Hyper-V administrator on this
    computer, until the machine has applied it (see the KVP section below).
  - A record without `hostKey` falls back to known_hosts: a machine built
    before this, and `rm --keep-machine` then `create`, which builds nothing.

## The Hyper-V backend was merged unverified, and has been run by hand once

- **Merged unrun, on purpose.** The plan was that a backend merges once
  somebody has run `docs/testing-machines.md` against it and reported. WSL
  cleared that. Hyper-V could not: no CI offers it, so the bar would have held
  the code in a branch indefinitely. It cost nothing until somebody typed
  `--backend hyperv`: no other path reaches it, and the WSL backend imports
  none of it.
- **The first run, 2026-10-05:** Windows 11 Pro 25H2 build 26200.9457, Hyper-V
  vmms 10.0.26100.8875, Flatcar stable 4757.2.1 (kernel 6.12.111) from
  `flatcar_production_hyperv_vhdx_image.vhdx.zip`, a Gen 2 VM with secure boot
  off on the Default Switch, the user in Hyper-V Administrators. Three bugs,
  each of which alone stopped every machine:

  | Found | Cause | Fix |
  |---|---|---|
  | `create` always failed: `cannot tell what is there: exit status 1:`, nothing printed | `powershell.exe -Command` exits 1 when the LAST statement left `$?` false, and `Get-VM -Name x -ErrorAction SilentlyContinue` on a missing VM does | every script runs in `psScript`: errors stop it, reaching the end is success |
  | Ignition never applied: guest at `localhost login:`, no key, no unit, 2222 closed | Flatcar's Hyper-V image reads Ignition over KVP, never from a file beside the disk | the KVP section below |
  | `has no address yet` right after `Start-VM`, and on the first command after `machine stop` | the guest reports its address about 30s after boot; `Locate` asked once | `Locate` waits for it within the same budget as the agent |

  With those fixed, on the same computer: create in one go (93s), status
  answering, a bind mount read, create again a no-op, rebuild then `docker run`
  with the new host key pinned and no known_hosts edit, stop then `docker run`
  (19s, a new address), and `rm` leaving no VM, no disk and no context. A
  second run the same day found the Ignition items present from `New-VM`
  until the agent answered (93s) and gone when `create` returned, a rebuilt
  machine booting configured, and `rm` completing on a VM deleted by hand.
- **It still warns on every `machine create`**, because one computer is not
  coverage. Four places say so and must stay in step: that warning,
  `--backend`'s help, CLAUDE.md's NOT-tested list and the README. All four say
  "run by hand once", never "tested".

## A Hyper-V machine's Ignition goes over KVP

- **Forced by:** the first run. `config.ign` written beside the disk was never
  read: the VM had 0 KVP items and 0 DVD drives, so the guest had nothing to
  read, and booted with no key, no host key and no unit. Flatcar's Hyper-V
  provider reads the KVP exchange, as `kvpctl add-ign` (containers/libhvee)
  writes it. *(Checked 2026-10-05 at
  https://www.flatcar.org/docs/latest/deploy/virt-options/hyper-v/; re-check
  there, since no command asks.)*
- **Decision:** `Create` adds the document as host KVP items through WMI
  (`Msvm_VirtualSystemManagementService.AddKvpItems`, Source 0) after
  `New-VM` and before the first `Start-VM`, split as `ignition.config.0`,
  `ignition.config.1` and so on, which the guest concatenates.
- **Limit, measured 2026-10-05 on vmms 10.0.26100.8875:** Hyper-V refuses a
  host value of 1024 characters or more (job `ErrorCode` 32773) and accepts
  1023. Chunks are 1000 bytes (`ignitionKVPChunk`), cut on rune boundaries.
  A machine's document is about 1.4 KB, so two items.
- **The data never touches a command line or a file.** It holds the private
  host key, so the script (`machine/hyperv_kvp.ps1`) reads it on stdin as a
  JSON array, and an error names the item and its code, never its value.
- **Removed once applied.** Hyper-V keeps host items for the VM's life: both
  were still in its `HostExchangeItems` after first boot (2026-10-05). So
  `machine create` removes every `ignition.config.*` item
  (`RemoveKvpItems`, `machine.Retracter`) once the agent answers, which only
  an applied Ignition document can make happen. It does so on every run:
  removing an item that is not there succeeds (measured 2026-10-05), so a
  later `create` retries a removal that failed.
- **Costs:**
  - From creation until the agent first answers, about 90 seconds, the
    private host key is readable through WMI by anybody who may administer
    Hyper-V on this computer, without access to the disk under
    `%LOCALAPPDATA%`.
  - If the removal fails, `create` warns and succeeds, and the key stays in
    the KVP items until a later `create` removes it or `rebuild` or `rm`
    destroys the VM.
  - Hyper-V has no cmdlet for this, so it is WMI, and the call may finish as a
    job (4096) whose state has to be polled for the real answer.

## Both backends are located the same way, and Hyper-V uses no hvsock

Hyper-V sockets would avoid discovering a NAT address that changes every boot.
But WSL showed discovery is one measured platform call per connection with no
agent change, while hvsock needs a pluggable listener in the agent, AF_HYPERV
on the Linux side, a service GUID per machine on the host and a new
dependency, all in code no CI can run. So a Hyper-V machine is on the Default
Switch (NAT with DHCP Hyper-V maintains) and `Address` asks
`Get-VMNetworkAdapter`.

- **Measured 2026-10-05, it works:** the guest's `hv_kvp_daemon` reports an
  address about 30 seconds after `Start-VM`, a different one after every boot
  (`172.19.86.205`, then `172.19.85.188` after a stop and start), so no hvsock
  fallback is needed.

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

- **The generation hashes what was ASKED for, not where it was cached.** A
  `--rootfs` path is a setting; a rootfs `EnsureRootfs` fetched is a cache path
  the image reference already identifies, so `Spec.Fetched` leaves it out.
  Hashed, `machine create` without `--rootfs`, whose flags name no path,
  refused on every run after the first (found by hand 2026-10-04;
  `machine.yml` always passes `--rootfs`).
- The record holds only the path, so `machine.IsFetched` (the path is in the
  cache directory) is what a rebuild decides by. A machine built before
  `Spec.Fetched` carries the old hash, and one rebuild repairs it.

## Consequences

- **`remote rm` refuses rather than orphaning.** If the machine cannot be
  destroyed (wrong platform, backend unavailable) the config entry stays: it is
  the only record a Linux system was built.
- **The decisions are pure functions and the platform calls an interface**
  (the `machine` module, ADR 0021), as in `elevate` and `daemons`. Here it
  matters more: **no CI has Hyper-V and nobody working on this has WSL**, so
  everything that is not a pure function ships having run once by hand at
  best.
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

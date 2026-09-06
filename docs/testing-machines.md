# Testing a machine backend by hand

Nobody who works on this has WSL or Hyper-V. The WSL backend is exercised in CI
(`.github/workflows/machine.yml`), which is real coverage but one Windows
version on one runner image. Hyper-V has none at all.

So this is the procedure, written down properly, for somebody with the
platform. It is a runbook rather than an ADR: it records what to do, not what
was decided. What was decided is [ADR 0026](adr/0026-a-machine-is-a-workspace-we-provision.md).

Report what happened either way. "It worked" is a useful result and is
currently not known.

## Before anything

```powershell
wsl --version                 # WSL 2.x and a kernel version
wsl --status                  # default version should be 2
Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-All
```

Note what these say, including the Windows build (`winver`). A failure below
means nothing without them.

## The rootfs

A machine is the workspace image's filesystem, so you need it as a tar file.
On a machine with docker:

```powershell
docker pull ghcr.io/lhns/remote-docker-workspace:latest
$id = docker create ghcr.io/lhns/remote-docker-workspace:latest
docker export $id -o rootfs.tar
docker rm $id
```

If this machine has no docker at all — which is the case this feature exists
for — take `rootfs.tar` from a machine that does, or from a CI run of
`machine.yml`, which uploads one.

## WSL, the whole path

```powershell
remote-docker remote machine create dev --rootfs .\rootfs.tar
remote-docker remote ls                       # dev, marked (wsl)
remote-docker remote machine status dev       # running, settings current
remote-docker run --rm -v .:/w alpine ls /w   # the point of all of it
```

The last line is the one that matters. It exercises the session, the SSH
transport, the NFS export, the bind rewriting and the daemon inside the
machine — everything, in one command, with nothing installed but this binary.

Expected: the contents of the current directory. A hang means the session
never came up; capture `remote-docker remote status` and the client log path it
prints.

### That it is idempotent

```powershell
remote-docker remote machine create dev --rootfs .\rootfs.tar
```

Expected: `"dev" already matches; nothing to do`. It must NOT create a second
distribution or restart anything.

### That a changed setting is reported, not acted on

```powershell
remote-docker remote --port 2299 machine create dev --rootfs .\rootfs.tar
```

Expected: a refusal naming `machine rebuild`. It must not destroy the machine:
a create command deciding to discard somebody's images is the surprise this
design exists to avoid.

### That rebuild repairs a genuinely broken machine

Break it first, or the test proves nothing:

```powershell
wsl -d rd-dev --user root -- rm -f /usr/local/bin/remote-dockerd
remote-docker remote machine status dev       # expect trouble
remote-docker remote machine rebuild dev --rootfs .\rootfs.tar
remote-docker run --rm -v .:/w alpine ls /w   # works again
```

### That it leaves nothing behind

The half usually skipped, and the half that matters when somebody wants this
program off their machine:

```powershell
remote-docker remote rm dev
wsl -l -v                                     # no rd-dev
remote-docker remote ls                       # no dev
docker context ls                             # no dev
Get-ChildItem $env:LOCALAPPDATA\remote-docker\machines
```

`rm` must destroy the distribution **before** removing the config entry. If it
cannot, it must refuse and leave the entry — that entry is the only record the
machine exists.

## What CI already proves about WSL

Since 2026-08-11, `.github/workflows/machine.yml` runs this whole WSL section on
a windows-latest runner on every change to the backend: create, a `docker run`
with a bind mount from the Windows side, create again for idempotence, and `rm`
taking the distribution with it. So a report from a real machine is now about
what differs from that runner -- a different Windows build, a WSL configured by
hand, an existing distribution list, a machine left running for days -- rather
than about whether the path works at all.

Four things it found, worth knowing before reading a failure here:

- The machine is reached at its OWN address, not through `localhost`. WSL2's
  localhost relay did not carry the connection on the runner at all.
- A machine with nobody in it shuts down, so the client holds a `wsl.exe`
  session open for as long as it is using one. If a machine stops mid-session,
  that hold is what to look at.
- The agent's output goes to `/var/log/remote-dockerd.log` inside the machine.
- The machine's environment is written into `/etc/wsl.conf`, because a rootfs
  tarball does not carry the image's `ENV` or `PATH`.

## Hyper-V

**Nothing below has ever been executed.** The WSL section describes a path CI
runs on every change; this one describes code that compiles, is unit tested as
far as a string can be, and has never met Hyper-V. GitHub's runners do not offer
it and nobody working on the project has it. If you run this, you are the first,
and a report saying "step 3 printed X" is worth more than a patch.

### Before you start

```powershell
# Is Hyper-V there at all? Windows Pro/Enterprise only.
Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V

# The machine needs a disk image. Flatcar publishes one for Hyper-V; this is
# the only download in the whole procedure, and it is a published artifact
# rather than an installer that runs.
# (Checked 2026-08-11: https://www.flatcar.org/docs/latest/installing/vms/hyper-v/)
curl.exe -LO https://stable.release.flatcar-linux.net/amd64-usr/current/flatcar_production_hyperv_image.vhdx.bz2
# unpack it with 7-Zip or bunzip2 to flatcar.vhdx
```

Hyper-V machine management needs administrator, or membership of the local
Hyper-V Administrators group. `remote machine create` reports that and stops; it
does not elevate itself. **This is where the project's "nothing needs to be
installed" premise ends**, and ADR 0026 says so.

### The procedure

```powershell
remote-docker remote machine create dev --backend hyperv --rootfs .\flatcar.vhdx
remote-docker remote machine status dev
remote-docker remote ls

# The one that matters. Everything else is setup.
"hello" | Out-File -Encoding ascii marker
remote-docker run --rm -v "${PWD}:/w" alpine:3 cat /w/marker
```

Expected: `create` prints that it is creating the machine, waits for the agent,
and returns. The `docker run` prints `hello`.

### What to check, and what is most likely to be wrong

In the order they are likely to fail:

1. **The machine has no address.** `Get-VMNetworkAdapter -VMName rd-dev | Select
   -ExpandProperty IPAddresses`. Empty or only `169.254.x.x` means the guest is
   not telling Hyper-V its address: either Ignition did not run, or the Default
   Switch gave it nothing. The client waits rather than connecting to a
   link-local address, so this presents as a create that times out.
2. **Ignition did not apply.** The config is written to
   `%LOCALAPPDATA%\remote-docker\machines\dev\config.ign`. Whether Flatcar's
   Hyper-V image reads it from there is the single least certain thing in this
   backend -- it may need the config attached another way, and if so this is
   where it fails and the fix belongs in `hyperVBackend.Create`.
3. **The workspace container is not running.** Connect to the VM's console
   (`vmconnect.exe localhost rd-dev`) and look at
   `systemctl status remote-dockerd` and `journalctl -u remote-dockerd`.
4. **Secure boot.** A machine that never boots at all, with no console output,
   is usually this -- the create command turns it off, so if you see it, say so.

### Leaving nothing behind

```powershell
remote-docker remote rm dev
Get-VM                                   # no rd-dev
Get-ChildItem $env:LOCALAPPDATA\remote-docker\machines
docker context ls                        # no rd-dev
```

`rm` removes the disk as well as the machine. Remove-VM on its own leaves it,
which quietly keeps gigabytes per machine somebody believes they removed, so
the directory being gone is the thing to check.

## What to capture when something fails

- the command and its whole output, not the last line;
- `remote-docker remote machine status <name>`;
- `wsl -l -v`;
- the agent's own view:
  `wsl -d rd-<name> --user root -- cat /var/log/remote-dockerd.log`, and
  `wsl -d rd-<name> --user root -- ps aux` to see whether the agent is running
  at all;
- if it is running, what it is listening on:
  `wsl -d rd-<name> --user root -- sh -c "netstat -lnt || ss -lnt"`;
- the client log, whose path `remote start` prints;
- `wsl --version` and `winver`.

The most useful single thing is whether `remote-dockerd` is running inside the
distribution. If it is not, the boot command in `/etc/wsl.conf` is where to
look:

```powershell
wsl -d rd-dev --user root -- cat /etc/wsl.conf
```

If it *is* running and Windows still cannot reach it, check the address it
bound. WSL2's default networking is NAT with a localhost relay, and the relay
connects to the machine's own address, so an agent on the machine's loopback is
reachable from inside it and nowhere else. The boot command binds every
interface for exactly this reason, and a listener showing `127.0.0.1:<port>`
rather than `0.0.0.0:<port>` is the bug reappearing.

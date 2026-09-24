# Testing a machine backend by hand

Nobody who works on this has WSL or Hyper-V. The WSL backend runs in CI
(`.github/workflows/machine.yml`), on one Windows version and one runner image;
Hyper-V has no coverage at all. This is the procedure for somebody with the
platform; what was decided is [ADR 0026](adr/0026-a-machine-is-a-workspace-we-provision.md).
Report what happened either way: "it worked" is currently not known.

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

With no docker here (the case this feature exists for), take `rootfs.tar`
from a machine that has it, or the `workspace-rootfs` artifact of a
`machine.yml` run: a `rootfs.tar.gz` that `--rootfs` takes as it is.

## WSL, the whole path

```powershell
remote-docker remote machine create dev --rootfs .\rootfs.tar
remote-docker remote ls                       # dev, marked (wsl)
remote-docker remote machine status dev       # running, settings current
remote-docker run --rm -v .:/w alpine ls /w   # the point of all of it
```

The last line exercises the session, the SSH transport, the NFS export, the
bind rewriting and the daemon in the machine, in one command. Expected: the
contents of the current directory. A hang means the session never came up;
capture `remote-docker remote status` and the client log path it prints.

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

Expected: a refusal naming `machine rebuild`. It must not destroy the machine
or discard its images.

### That rebuild repairs a genuinely broken machine

Break it first, or the test proves nothing:

```powershell
wsl -d rd-dev --user root -- rm -f /usr/local/bin/remote-dockerd
remote-docker remote machine status dev       # expect trouble
remote-docker remote machine rebuild dev --rootfs .\rootfs.tar
remote-docker run --rm -v .:/w alpine ls /w   # works again
```

### That it leaves nothing behind

```powershell
remote-docker remote rm dev
wsl -l -v                                     # no rd-dev
remote-docker remote ls                       # no dev
docker context ls                             # no dev
Get-ChildItem $env:LOCALAPPDATA\remote-docker\machines
```

`rm` must destroy the distribution **before** removing the config entry. If it
cannot, it must refuse and leave the entry: the only record the machine exists.

## What CI already proves about WSL

Since 2026-08-11, `machine.yml` runs this WSL section on windows-latest on every
change to the backend: create, a `docker run` with a bind mount from the Windows
side, create again for idempotence, and `rm` taking the distribution with it. A
report from a real machine is about what differs from that runner: another
Windows build, a hand-configured WSL, an existing distribution list, a machine
left running for days. Before reading a failure:

- The machine is reached at its OWN address, not `localhost` (ADR 0026).
- An idle machine shuts down, so the client holds a `wsl.exe` session open
  while it uses one. A machine stopping mid-session means look at that hold.
- The agent logs to `/var/log/remote-dockerd.log` inside the machine.
- The environment is written into `/etc/wsl.conf`, because a rootfs does not
  carry the image's `ENV` or `PATH`.

## Hyper-V

**Nothing below has ever been executed.** The code compiles and is unit tested
as far as a string can be; GitHub's runners do not offer Hyper-V and nobody on
the project has it. If you run this you are the first, and "step 3 printed X"
is worth more than a patch.

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

Hyper-V machine management needs administrator, or the local Hyper-V
Administrators group. `remote machine create` reports that and stops; it does
not elevate itself (ADR 0026).

### The procedure

```powershell
remote-docker remote machine create dev --backend hyperv --rootfs .\flatcar.vhdx
remote-docker remote machine status dev
remote-docker remote ls

# The one that matters. Everything else is setup.
"hello" | Out-File -Encoding ascii marker
remote-docker run --rm -v "${PWD}:/w" alpine:3 cat /w/marker
```

Expected: `create` waits for the agent and returns; `docker run` prints
`hello`.

### What to check, most likely failure first

1. **The machine has no address.** `Get-VMNetworkAdapter -VMName rd-dev | Select
   -ExpandProperty IPAddresses`. Empty or only `169.254.x.x` means the guest is
   not telling Hyper-V its address: either Ignition did not run, or the Default
   Switch gave it nothing. The client waits rather than connecting to a
   link-local address, so this presents as a create that times out.
2. **Ignition did not apply.** The config is written to
   `%LOCALAPPDATA%\remote-docker\machines\dev\config.ign`. Whether Flatcar's
   Hyper-V image reads it from there is the single least certain thing in this
   backend. It may need the config attached another way; if so, the fix
   belongs in `hyperVBackend.Create`.
3. **The workspace container is not running.** Connect to the VM's console
   (`vmconnect.exe localhost rd-dev`) and look at
   `systemctl status remote-dockerd` and `journalctl -u remote-dockerd`.
4. **Secure boot.** A machine that never boots at all, with no console output,
   is usually this. The create command turns it off, so if you see it, say so.

### Leaving nothing behind

```powershell
remote-docker remote rm dev
Get-VM                                   # no rd-dev
Get-ChildItem $env:LOCALAPPDATA\remote-docker\machines
docker context ls                        # no dev
```

`rm` removes the disk too. `Remove-VM` alone leaves it, silently keeping
gigabytes per machine, so the directory being gone is the thing to check.

## What to capture when something fails

- the command and its whole output, not the last line;
- `remote-docker remote machine status <name>`;
- `wsl -l -v`;
- `wsl -d rd-<name> --user root -- cat /var/log/remote-dockerd.log`;
- `wsl -d rd-<name> --user root -- ps aux`: is the agent running at all? The
  most useful single fact. If not, look at the boot command in
  `/etc/wsl.conf` (`wsl -d rd-<name> --user root -- cat /etc/wsl.conf`);
- if it is, what it listens on:
  `wsl -d rd-<name> --user root -- sh -c "netstat -lnt || ss -lnt"`;
- the client log, whose path `remote start` prints;
- `wsl --version` and `winver`.

If the agent is running and Windows still cannot reach it, check the address it
bound. Windows reaches the machine at its own address (ADR 0026), so the boot
command binds every interface (`--addr :<port>`, `wslConf` in
`machine/wsl.go`); a listener on `127.0.0.1:<port>` rather than
`0.0.0.0:<port>` is that bug reappearing.

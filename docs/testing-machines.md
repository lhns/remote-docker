# Testing a machine backend by hand

The WSL backend runs in CI (`.github/workflows/machine.yml`), on one Windows
version and one runner image; Hyper-V has no automated coverage and is tested by
hand. This is the procedure for somebody with the platform; what
was decided is [ADR 0026](adr/0026-a-machine-is-a-workspace-we-provision.md).
Report what happened either way: one run on one machine is all that is known.

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
remote-docker remote machine status dev       # running, settings current, agent answering
remote-docker remote use dev                  # if create's hint names it: dev is not the default
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

### That the published image is fetched, and creating again changes nothing

With network access to ghcr.io, and no `--rootfs`:

```powershell
remote-docker remote machine create pub
remote-docker remote machine create pub
remote-docker remote machine status pub       # running, settings current
remote-docker remote rm pub
```

Expected: the first fetches the filesystem into
`%LOCALAPPDATA%\remote-docker\rootfs` and creates the machine; the second
says `"pub" already matches; nothing to do`. A refusal naming `machine rebuild`
means the cache path is being hashed as a setting again (ADR 0026).

### That a changed setting is reported, not acted on

```powershell
remote-docker remote --port 2299 machine create dev --rootfs .\rootfs.tar
```

Expected: a refusal naming `machine rebuild`. It must not destroy the machine
or discard its images.

### That rebuild repairs a genuinely broken machine

Break it first, or the test proves nothing. Removing the binary alone leaves the
running agent serving from the deleted file, so stop it as well:

```powershell
wsl -d rd-dev --user root -- rm -f /usr/local/bin/remote-dockerd
wsl -d rd-dev --user root -- pkill remote-dockerd
remote-docker remote machine status dev       # agent not answering on <address>:<port>, exit 1
remote-docker remote machine rebuild dev --rootfs .\rootfs.tar
remote-docker run --rm -v .:/w alpine ls /w   # works again
```

Nobody should have to touch `known_hosts` for that, nor after `remote rm dev`
and a create on the same port: the entry pins the key the build made (ADR
0026), so `host key ... has CHANGED` here is a failure.

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
side, create again for idempotence, a rebuild then a `docker run` past a stale
`known_hosts` entry for the machine's address (since 2026-10-04), and `rm`
taking the distribution with it. A
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

**Run by hand once, on 2026-10-05, and never in CI**: GitHub's runners do not
offer Hyper-V. That run is recorded in [ADR
0026](adr/0026-a-machine-is-a-workspace-we-provision.md), with the versions it
used and the three bugs it found. Any other Windows build, Flatcar release or
network setup is unproven, so a report from one is worth more than a patch.

### Before you start

```powershell
# Is Hyper-V there at all? Windows Pro/Enterprise only.
Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V
Get-VMSwitch                   # must list "Default Switch", and must not fail
```

Hyper-V machine management needs administrator, or the local Hyper-V
Administrators group. `remote machine create` reports that and stops; it does
not elevate itself (ADR 0026). **Membership takes effect at the next logon**:
add yourself, then sign out and in again, or `Get-VMSwitch` above keeps
failing with a permission error. Nothing else in the procedure needs
elevation.

The machine needs Flatcar's Hyper-V disk image. This is the only download in
the whole procedure, and it is a published artifact rather than an installer
that runs. About 584 MB zipped and 1.2 GB unpacked, plus a copy of the same
size per machine.
*(Checked 2026-10-05: stable 4757.2.1. The file names changed since the
`.vhdx.bz2` this runbook used to name, which is now 404. Re-check with
`curl.exe -sIL <url>` on the two URLs below, and the docs at
https://www.flatcar.org/docs/latest/deploy/virt-options/hyper-v/.)*

```powershell
$base = "https://stable.release.flatcar-linux.net/amd64-usr/current"
curl.exe -fLO "$base/flatcar_production_hyperv_vhdx_image.vhdx.zip"
curl.exe -fLO "$base/flatcar_production_hyperv_vhdx_image.vhdx.zip.DIGESTS"

# Compare with the SHA512 line in the DIGESTS file.
(Get-FileHash -Algorithm SHA512 flatcar_production_hyperv_vhdx_image.vhdx.zip).Hash.ToLower()
Select-String -Path flatcar_production_hyperv_vhdx_image.vhdx.zip.DIGESTS -Pattern '^[0-9a-f]{128} '

Expand-Archive flatcar_production_hyperv_vhdx_image.vhdx.zip -DestinationPath .
Rename-Item flatcar_production_hyperv_vhdx_image.vhdx flatcar.vhdx
Remove-Item flatcar_production_hyperv_vhdx_image.vhdx.zip
```

### The procedure

```powershell
remote-docker remote machine create dev --backend hyperv --rootfs .\flatcar.vhdx
remote-docker remote machine status dev       # running, settings current, agent answering
remote-docker remote ls

# The one that matters. Everything else is setup.
"hello" | Out-File -Encoding ascii marker
remote-docker run --rm -v "${PWD}:/w" alpine:3 cat /w/marker
```

Expected: `create` returns once the agent answers, about a minute and a half
the first time (93 seconds on 2026-10-05, most of it the machine pulling the
workspace image); `docker run` prints `hello`. The warning `create` prints
about the backend is expected; one saying the private host key is still in
the KVP items is not. Once `create` has returned, the snippet under "Ignition
did not apply" below lists no `ignition.config.*` item.

Then the rest of the WSL procedure applies with `--backend hyperv` added:
create again (`already matches; nothing to do`), `machine rebuild dev -f`
followed by a `docker run` with no `host key ... has CHANGED`, and:

```powershell
remote-docker remote machine stop dev
remote-docker run --rm -v "${PWD}:/w" alpine:3 cat /w/marker
```

Expected: `hello`, after the machine boots again. The guest reports its new
address about 30 seconds after `Start-VM` and the client waits for it, so
`has no address` here means it waited three minutes and none came.

### What to check, most likely failure first

1. **The machine has no address.** `Get-VMNetworkAdapter -VMName rd-dev | Select
   -ExpandProperty IPAddresses`. For the first 30 seconds or so of a boot it is
   empty, which is normal. Empty for minutes, or only `169.254.x.x`, means the
   guest is not telling Hyper-V its address: the Default Switch gave it
   nothing, or its KVP daemon is not running. The client waits rather than
   connecting to a link-local address, so this presents as a create that times
   out naming `no address`.
2. **Ignition did not apply.** The console (`vmconnect.exe localhost rd-dev`)
   shows `localhost login:`, port 22 answers and the agent's port does not.
   The configuration is handed to the guest as KVP items before its first
   boot, never as a file, and `create` removes them only once the agent
   answers. Check they are there, without printing them, since they hold the
   machine's private host key:

   ```powershell
   $vm = Get-VM rd-dev
   $cs = Get-WmiObject -Namespace root\virtualization\v2 -Class Msvm_ComputerSystem -Filter "Name='$($vm.Id)'"
   $set = $cs.GetRelated('Msvm_VirtualSystemSettingData') |
     Where-Object VirtualSystemType -eq 'Microsoft:Hyper-V:System:Realized' |
     ForEach-Object { $_.GetRelated('Msvm_KvpExchangeComponentSettingData') }
   foreach ($x in $set.HostExchangeItems) {
     $p = ([xml]$x).INSTANCE.PROPERTY
     $n = ($p | Where-Object Name -eq 'Name').VALUE
     $d = ($p | Where-Object Name -eq 'Data').VALUE
     "$n : $($d.Length) characters"
   }
   ```

   Expected while it is failing: `ignition.config.0`, `ignition.config.1`,
   ..., none over 1000. Ignition runs only at first boot, so a machine that
   booted without them needs `machine rebuild`, not a restart.
3. **The workspace container is not running.** Its unit is
   `remote-dockerd.service`, but there is no way in to look at it: Ignition
   gives no account a password, and Flatcar's own sshd on port 22 has no key
   for anybody. What is visible from here is the agent's port staying closed
   while the machine has an address, and the first boot pulls the workspace
   image, so give it a few minutes on a slow link. Say how long you waited.
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
gigabytes per machine, so the directory being gone is the thing to check. A
VM already removed by hand (`Remove-VM`) does not stop it, though for about
half a minute after that `rm` may refuse because the session cannot say
whether it is in use; that passes once the session notices the connection is
dead, or `rm -f` goes ahead.

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

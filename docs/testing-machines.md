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

## Hyper-V

Not implemented yet. When it is, this section gets the same treatment, plus:
whether it asks for elevation and says why, and `Get-VM` / `Get-VMSwitch`
showing nothing left behind.

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

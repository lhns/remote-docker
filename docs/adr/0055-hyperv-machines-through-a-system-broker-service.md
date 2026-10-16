# 0055 — Hyper-V machines through a SYSTEM broker service

- Status: Accepted
- Date: 2026-10-15
- Current answer: a Windows service `remote-docker-machines`, running as
  LocalSystem from the same binary, makes, starts, stops and destroys Hyper-V
  machines for the callers it identifies on a named pipe. It authorises them
  against the LIVE direct members of a local group before reading a request,
  takes typed operations only, and records which SID owns which machine. A
  machine created through it has `broker: true` in its config entry.

## What forced it

- Managing Hyper-V takes administrator rights or Hyper-V Administrators
  (ADR 0026). Every docker command on a Hyper-V workspace runs `Start-VM` and
  reads the VM's address (`machine.Locate`), so a standard user could not use
  one at all, and the installer could not give one a machine at install time.
- Docker Desktop's answer is a SYSTEM service that does the Hyper-V work for
  members of `docker-users` (checked 2026-10-15,
  docs.docker.com/desktop/setup/install/windows-permission-requirements).
  A service is the only way a standard user gets this without a rights grant
  that is itself administrator-equivalent.

## The decision

| Piece | Choice |
|---|---|
| Service | `remote-docker-machines`, LocalSystem, demand start; `remote machine service run` (hidden). The installer grants Authenticated Users `SERVICE_START` and nothing else on it |
| Pipe | `\\.\pipe\remote-docker-machines`, `D:P(D;;GA;;;NU)(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x12019b;;;AU)`: network logons denied first, SYSTEM and Administrators full, Authenticated Users read and write WITHOUT `FILE_CREATE_PIPE_INSTANCE` (`0x4`, which is what `GENERIC_WRITE` includes on a pipe), so no user can add an instance of the running pipe |
| Identity | `ImpersonateNamedPipeClient` on a locked thread, `OpenThreadToken`, the token's user SID and whether Administrators is ENABLED in it, `RevertToSelf`. Nothing is read from the pipe. The SID is never in a request |
| Authorisation | SYSTEM or an Administrators-enabled token, or a SID that is a DIRECT member of local group `remote-docker-users`, read with `NetLocalGroupGetMembers` (level 0, SIDs) on EVERY connection. A group that cannot be read allows administrators only |
| Operations | `available look create start stop address retract destroy`, plus `purge` (administrators only). JSON, one request and one answer per connection, unknown fields refused. No op takes a path, a script or a command |
| Names | `^[a-z0-9][a-z0-9-]{0,62}$`, bare; the executor adds `rd-` |
| Ownership | `%ProgramData%\remote-docker\machines.json`: name -> owner SID. Every op but `create` and `available` needs the machine recorded for the caller. `create` refuses a name that is recorded OR exists in Hyper-V, whoever owns it |
| Create | takes public key, host key, account, port, cpus, memory, workspace image, proxy (each validated to a narrow shape). The service downloads and verifies Flatcar into its own cache and builds the Ignition with the code the direct path uses |
| State | `%ProgramData%\remote-docker\`, created with an explicit PROTECTED DACL `D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)`; `machines\<name>` for VM files, `rootfs\` for the Flatcar cache |
| One path | the service runs the same `hyperVBackend` over `directHyperV` with a ProgramData root; the client's `hyperv-broker` backend is the same `hyperVBackend` over `brokerExecutor` (pipe client). No Hyper-V step exists twice |
| Client | `remote machine create --backend hyperv` uses the service when the SCM says it is installed, starts it when stopped, and records `broker: true`. Every later operation follows the record (`config.Machine.BackendKey`). A machine without the flag keeps running PowerShell directly |

### Why the live group and not the token

A group in the login token takes effect at the next logon. The installing user
is added to `remote-docker-users` by the installer and must work at once, and
an administrator adding a colleague must not ask them to sign out. Reading the
group's members per connection costs one call and gives that. The price:
nested and domain groups are NOT expanded (a user in a domain group that is a
member of `remote-docker-users` is refused). That is documented in the refusal
fix, not worked around.

## Threats

- **Unauthorised input.** A caller who is not allowed gets one refusal and none
  of its bytes are read or parsed. The pipe ACL is a coarse filter only;
  authorisation is the code above.
- **A group member acting on another's machine.** Refused: ownership is by
  SID from the pipe. A member can start, stop and destroy only machines they
  created. They cannot touch a Hyper-V VM the service did not create, even
  under an `rd-` name, because it is not recorded.
- **Path or script injection.** There is no field for either. The machine's
  directory is `machines\` plus a validated name; image, account, key and proxy
  are held to shapes that cannot break out of the systemd unit, the Ignition
  JSON or a PowerShell literal.
- **A pre-created ProgramData directory.** ProgramData lets Users create
  folders. `service setup` (the installer, as SYSTEM) creates the directories
  with the protected DACL at install, before any user can. One that exists
  anyway is adopted only if SYSTEM or Administrators own it and it is not a
  reparse point; otherwise the service refuses to start. That refusal is a
  denial of service a user can cause by pre-creating the folder before
  install, and nothing worse: the service reads and writes nothing there.
  Teardown and `service purge` make the same check before reading
  `machines.json` or deleting, and refuse while the service is running (two
  processes would write the record). Startup failures go to the Windows event
  log, never to a file: SYSTEM writing a path in a directory users can plant a
  link in is an arbitrary overwrite.
- **A squatted pipe.** While the service is stopped a user can create a pipe
  of the same name, and while it runs a second instance of it if the DACL lets
  them; either would be sent a request, private host key included. The DACL
  withholds instance creation, and the client compares the process answering
  on the pipe (`GetNamedPipeServerProcessId`) with the service's own process
  id from the service manager before sending anything. The pipe's owner would
  not do: it belongs to the name, not to the instance.
- **Proxy.** The proxy is the caller's choice, as it is on the direct path;
  what is pinned is the Flatcar URL and its SHA512, so a proxy cannot change
  the image. A caller's proxy URL is also visible in the machine they created.
- **A member wedging the service.** Every operation has a deadline (2 minutes;
  45 for create, which includes the Flatcar download; 15 for purge) and ends
  when the caller's connection drops (a read on the connection returns). A
  stop the guest ignores falls back to `Stop-VM -TurnOff` after 90 seconds. A
  caller may have 2 operations running (administrators 4, counted apart, so
  purge is never starved), 32 connections are served at once, 30 seconds to
  send a request once authorised. Operations on one machine queue behind its
  name's lock, which waits with the caller's deadline; `machines.json` is
  locked only for a file read or write, never across a Hyper-V call. The
  Flatcar download is capped in size and time before its hash is checked.
- **Cleanup that fails.** A create that fails destroys what it made. If that
  also fails the record is kept, so the machine stays destroyable by its
  owner or by a purge; a destroy removes a record only while it still names
  the caller. The owner's way is `remote rm <name>`, which asks the service
  when no workspace entry has the name: a create that failed after the
  service answered and before the entry was written leaves exactly that
  (2026-10-16).
- **Quota.** A member can create as many machines as the disk holds; there is
  no per-user quota. Accepted: members are trusted users.
- **Purge.** Destroys every recorded machine; administrators only; run at
  uninstall.

## What it costs

- A SYSTEM-level surface reachable by every member of the group
  (`docs/threat-model.md`, flow 11). Its worst case is one member exhausting
  disk or CPU, not reading or changing another member's machine.
- The service is tested against a stand-in for Hyper-V, over a real named pipe
  in CI. Real Hyper-V is exercised by hand (`docs/testing-machines.md`).
- A second code path exists for how an operation REACHES Hyper-V, but not for
  what it does. A change to a Hyper-V step is made in `hyperVBackend` or
  `directHyperV` and nowhere else.
- A machine cannot move between the two paths: the service has no record of
  one made directly, and the direct path cannot see the service's directory.
  `remote rm` then `create` is the move.
- A caller added to the group is allowed at once; removed from it, refused at
  the next connection, but machines they made keep running.

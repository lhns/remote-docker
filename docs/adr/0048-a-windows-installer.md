# 0048. A Windows installer, and it will not take the name `docker`

- Status: Accepted
- Date: 2026-09-08; amended 2026-10-15 (Hyper-V workspace feature), 2026-10-16
  (rollback guard, Restart Manager, run by hand), 2026-10-21 (PATH feature),
  2026-10-28 (installed per user without admin rights)
- Current answer: one MSI per architecture that installs for all users as an
  administrator, or for one user with no administrator rights, with three
  features besides the required one: `Path` (on by default), `DockerName` and `HyperV`. The
  `docker.exe` feature is a second file name and runs no code. The Hyper-V
  workspace feature runs this program's own hidden commands as custom actions
  on install, upgrade and removal; the service they set up is
  [ADR 0055](0055-hyperv-machines-through-a-system-broker-service.md). Nothing
  else runs code at install time. An upgrade closes a running background
  session through Restart Manager. The Hyper-V feature needs all users.

## Context

The release carries a zip per platform and nothing else. On Windows that means
unpack it somewhere, put that somewhere on PATH by hand, and repeat both on
every upgrade. An MSI is what Windows expects to be handed for that.

The part that needs a decision is not the packaging. ADR 0024 made the file's
name the whole installation: rename the binary to `docker.exe` and `docker run`
on this machine is this program, with no code behind it. An installer is the
first thing here that could do that renaming for the user, on a machine whose
PATH it also controls, which is the same power as shadowing another vendor's
tool.

## Decision

**One MSI per Windows architecture, attached to every tag release.**

| | |
|---|---|
| tool | WiX v5, `dotnet tool install --global wix` |
| built on | `windows-latest`, in an `installers` job after the `binaries` one |
| payload | the two Windows binaries goreleaser built, passed as an artifact |
| scope | per machine or per user, by `MSIINSTALLPERUSER` (see the 2026-10-28 section): Program Files and the system PATH, elevated, or `%LocalAppData%\Programs` and the user's PATH |
| PATH | the `Path` feature, on by default; `Environment Part="last"` — appended, never prepended |
| upgrades | `MajorUpgrade`, on a fixed `UpgradeCode` |
| when | tag releases only |

**WiX only runs on Windows, whatever .NET says.** It is a dotnet tool and loads
on Linux; `wix build` there then prints

```
warning WIX0000: The WiX Toolset only supports Windows. ...
  All behavior after this point is undefined.
error WIX0389: The Directory/@Name attribute's value, 'remote-docker',
  is not a relative path.
```

on a name that is relative and that the same source builds on Windows. Measured
on `ubuntu-latest`, 2026-09-08; re-check by running the `wix build` line of
`installer/windows/build.ps1` there. So the build script is PowerShell and the
job is a Windows one, and the payload crosses from the ubuntu build as an
artifact.

GoReleaser's own `msi` block is Pro-only (checked 2026-09-08 at
<https://goreleaser.com/customization/msi/>: "This feature is exclusively
available with GoReleaser Pro"), so this is a job after goreleaser and a
`gh release upload`, not a line in `.goreleaser.yaml`.

**`docker.exe` is a WiX Feature, off by default.** `<CopyFile FileId=...>` is a
`DuplicateFile` row: the binary ships once, the second name is made at install
and removed at uninstall. The feature needs no custom action, no second payload
and no code in the Go program: the name is the installation. So the option costs nothing to
download, and the MSI is smaller than the binary it carries:

| | bytes |
|---|---|
| amd64 binary | 74,101,248 |
| amd64 MSI | 19,636,224 |
| arm64 MSI | 17,940,480 |

Measured in the `msi` workflow, 2026-09-08; re-check with `Get-ChildItem
dist/msi` after a `build.ps1` run.

- `Level="2"` against the default `INSTALLLEVEL=1`: visible in
  `WixUI_FeatureTree`, unselected. `Level="0"` would hide it entirely.
- Silently: `msiexec /i ... /qn ADDLOCAL=Main,DockerName`.

**The feature is refused when a `docker.exe` is already installed.** A type 19
error custom action, after `CostFinalize` in both sequences, conditioned on
`NOT Installed AND &DockerName=3 AND &Path=3 AND DOCKERONPATH AND NOT ALLOWDOCKERSHADOW`.
The message names the file it found, and `ALLOWDOCKERSHADOW=1` overrides it.

Why refuse rather than win: a Windows machine commonly already has a
`docker.exe`, and the README points at `winget install Docker.DockerCLI`
itself. Quietly getting in front of Docker Desktop's CLI is the larger version
of a promise ADR 0024 already makes, that a docker context we did not create is
left completely alone.

**Version.** An MSI `ProductVersion` is `major.minor.build`, and the installer
compares only those: major and minor are one byte, build is two. A snapshot
version (`sha-9370b24`) has no mapping to that at all, so
`installer/windows/build.ps1` validates the shape and the ranges and exits
naming the version. Truncating instead would produce an upgrade that never
fires, which is silent for a release cycle.

## The PATH entry is a feature (2026-10-21)

- **`Path` is a sibling of `DockerName` and `HyperV` under `Main`, `Level="1"`.**
  Its component, `C.Path`, is unchanged (same Id and directory, so an upgrade
  sees the same component). Leaving it out installs the exe and touches no
  PATH; Modify adds and removes it.
- **The `docker.exe` refusal applies only with `Path`** (`&Path=3`): a
  `docker.exe` that is not on PATH shadows nothing.
- **Scripts that list features with `ADDLOCAL` must name `Path`** to keep the
  entry: `ADDLOCAL=Main,Path,DockerName`. A default install and an upgrade
  need nothing.
- Checked by `test/msi.ps1` 9b to 9d, the last of which upgrades from the
  latest released MSI, where the entry lived in `Main`.

## Installed per user without admin rights (2026-10-28)

- **Forced by:** the MSI was `perMachine`, so a user without administrator
  rights could not install it, though nothing they need requires one: the
  exe, their own PATH and the `docker` name. Only the Hyper-V workspace (a
  SYSTEM service, a group, a VM; ADR 0055) does.
- **Decision:** one package for both contexts, Windows Installer's own
  dual-purpose mechanism. WiX 5 has no `perMachineOrUser` scope, so it is
  written out: `Scope="perUser"` (the summary's "elevation not required" bit,
  and no `ALLUSERS`) plus `ALLUSERS=2`. `MSIINSTALLPERUSER=1` is per user,
  empty is per machine, and `ALLUSERS=2` alone is per machine.
- **The default.** Run by an administrator: per machine, which is also what
  finds the product older releases installed, so upgrades are unchanged. With
  a user interface and without administrator rights: per user, set in the
  client-side sequence by `SetProperty` before `FindRelatedProducts` on
  `NOT AdminUser AND NOT MSIINSTALLPERUSER AND NOT ALLUSERS="1"`. **A silent
  install cannot have an identity-dependent default.** Windows Installer
  resolves the context from the command line and the Property table before
  any sequence runs; measured on `windows-latest` as a standard local user,
  `msiexec /i x.msi /qn` logged `Modifying ALLUSERS property ... new value: '1'`
  and then `Error 1925. You do not have sufficient privileges to complete this
  installation for all users`, exit 1603. So a silent per-user install passes
  `MSIINSTALLPERUSER=1`,
  and a default in the Property table would make every administrator's, and
  SYSTEM's, silent install per user. Explicit: `MSIINSTALLPERUSER=1`, or
  `MSIINSTALLPERUSER=""` or `ALLUSERS=1`. A dialog after the licence asks "Only
  for me" or "For all users" and sets `MSIINSTALLPERUSER`, `ALLUSERS` and
  `INSTALLFOLDER`; the stock Install button then shows its shield only for all
  users.
- **Windows Server refuses any install by a standard user by default.** The
  machine policy `DisableMsi` defaults to 1 there, and the log says `Rejecting
  product ...: Non-assigned apps are disabled for non-admin users`, exit 1625,
  in either context. It is the OS's, not the package's: `windows-latest` is a
  Server image, so `test/msi.ps1` sets the policy to 0 around 9e to 9k and puts
  it back. Windows 11 defaults to 0.
- **What differs per user.**

  | | per machine | per user |
  |---|---|---|
  | folder | `ProgramFiles64Folder` | `%LocalAppData%\Programs\remote-docker` |
  | PATH | `C.Path`, system, appended | `C.UserPath`, user, appended |
  | markers | HKLM | HKCU (`HKMU`) |
  | Hyper-V | available | level 0, and `ADDLOCAL=...HyperV` is refused by name |

- **A per-user install over an all-users one is refused; the reverse warns.** `FindRelatedProducts`
  sees only the context it runs in, so each context looks for the other by
  what it wrote: `HKLM\Software\lhns\remote-docker\Installed` (or `Path`,
  from 0.16.x and earlier) and the same value under HKCU. A per-user
  install over an all-users one is refused. An all-users install over the
  installing user's own copy succeeds and logs `USEROVERMACHINEWARNING` (a
  `SetProperty` whose value is the message): the copy stays installed and
  unused, since the system PATH comes first, and the user removes it with
  Settings > Apps or `msiexec /x` of the package it came from, as that user and
  without administrator rights. CI shows `/x` of the older package picks the
  per-user product while the all-users one exists (9j). The same package across
  contexts is refused by Windows Installer itself (1625 for a standard user:
  "installed per-machine", maintenance mode), so this holds for a different
  version. The scope dialog does not show the warning (WiX 5's UI element takes
  no `ControlCondition`). **Not detected:** another user's per-user copy, whose
  hive this install cannot read.
- **Each context has its own marker components** (`C.Installed` and
  `C.UserInstalled`, `C.DockerAlias` and `C.UserDockerAlias`, like the PATH
  ones). One `HKMU` component has one component code in both contexts, and
  removing the per-user copy while an all-users install held that code left
  the per-user `Installed` value behind (measured on `windows-latest`, 9j).
- **The `docker.exe` refusal reads Docker Desktop's directories through
  `%ProgramW6432%`.** `ProgramFiles64Folder` is `%LocalAppData%\Programs` in
  a per-user install, so searching it would have stopped refusing.
- **Covered by** `test/msi.ps1` 9e to 9k, run as a standard local user: the
  silent no-argument refusal (error 1925), the install with
  `MSIINSTALLPERUSER=1`, PATH, the `docker` feature and its refusals, the Hyper-V
  refusal, the per-user-over-all-users refusal, the all-users-over-per-user
  warning and the user then removing their copy (9j), an in-place upgrade and
  the uninstall.
  Each premise above is printed from the log it came from.
- **Not covered:** the dialog (built, never clicked: whether the identity
  default, `MSIUSEREALADMINDETECTION` included, and the folder it sets work as
  intended), and another user's copy. In an unelevated administrator's shell
  `AdminUser` is false, so the dialog defaults to "Only for me" there; choose
  "For all users" for the other. A silent install there is per machine and
  fails with 1925, like any install without administrator rights.

## The Hyper-V workspace feature (2026-10-15)

The feature `HyperV` ("Hyper-V workspace", off by default; silently
`ADDLOCAL=Main,HyperV`) runs the program's own hidden commands through
`WixQuietExec` (WixToolset.Util.wixext 5.0.2, which logs their output and fails
the action on a non-zero exit). The logic is Go that unit tests reach; the MSI
only sequences it. A dialog after the feature tree, shown only when this
session adds the feature, takes `WORKSPACENAME`, `WORKSPACEIMAGE` and
`WORKSPACEPROXY`.

| When | As | Action |
|---|---|---|
| feature added | SYSTEM | `sc create` + `sc sdset` for `remote-docker-machines` (also in an upgrade) |
| feature added | SYSTEM | `remote machine service setup --user-sid [UserSID]` |
| feature added | the installing user (impersonated) | `remote machine service refuse-existing <name>`, then the rollback `rm` |
| feature added | the installing user (impersonated) | `remote machine create <name> --backend hyperv --image ...` |
| feature removed, first | the uninstalling user | `remote rm -f <remembered name>`, before StopServices |
| feature removed, then | SYSTEM | `remote machine service teardown`, after DeleteServices |

- **A feature, not a checkbox.** Every action keys on the feature's state
  transition (`&HyperV`, `!HyperV`), so a fresh install, Modify adding it,
  Modify removing it and an uninstall are one mechanism. Without Hyper-V
  (`HYPERVPRESENT` empty) the feature is level 0 and hidden, and naming it in
  `ADDLOCAL` is refused, because MSI skips a level 0 feature silently. The
  checkbox it replaced could not be used (found by hand, 2026-10-16): its
  fields had a `DisableCondition` and no `EnableCondition`, and MSI applies
  only the control conditions that have rows, so ticking the box left them
  grey. CI installs silently and never shows a dialog.
- **`sc.exe`, not `ServiceInstall`.** MSI takes a service's binary from the key
  FILE of its component; that is the exe in the always-installed component, and
  the service must exist only with the feature. The descriptor is the stock
  LocalSystem one plus Authenticated Users: start, query, interrogate, so a
  standard user can start it after a reboot and stop or change nothing.
- **Each install action has a rollback** (`rm`, stop, teardown, `sc delete`),
  so a refused name or a failed download leaves nothing. A failed create fails
  the install with 1603 and create's own message in the log. A machine the
  service built whose config entry create never wrote is destroyed by the
  rollback `rm`, which asks the service when no entry has the name, and by the
  rollback teardown regardless. Teardown destroys every machine the service
  recorded, and since this install created the service, each is its own or one
  an earlier uninstall's teardown failed to remove.
- **The rollback `rm` is scheduled only once nothing has the name**
  (2026-10-16). `remote rm -f <name>` removes whatever has the name, so an
  install whose create failed because the user already had a workspace called
  `hyperv` deleted that workspace and its machine.
  `remote machine service refuse-existing <name>`, deferred and as the
  installing user, runs just before it and fails on any workspace entry of that
  name, machine or not, and on any Hyper-V machine of it. Only what is already
  in the rollback script is rolled back
  ([Rollback Custom Actions](https://learn.microsoft.com/en-us/windows/win32/msi/rollback-custom-actions),
  read 2026-10-16), so after a refusal there is no `rm` to run, and once the
  check passed anything of that name is this install's. Rejected: a marker in
  the machine block, which needs a per-run token the MSI has no property for
  and leaves a plain workspace's entry to be overwritten by create.
- **An upgrade runs none of it but the service.** MajorUpgrade migrates the
  feature's state, so in the new product it arrives as an install; the service
  is recreated after the old product's removal deletes it, and create, setup
  and teardown are conditioned on `NOT WIX_UPGRADE_DETECTED` and
  `NOT UPGRADINGPRODUCTCODE`. The flag and name in
  `HKLM\Software\lhns\remote-docker` (strings, found by a `RegistrySearch`)
  keep the name and tell the new product a workspace exists. An upgrade that
  would add or drop the feature is refused naming Modify, since it would run
  half of either.
- **Uninstall is best effort.** `rm` runs for the user who uninstalls; another
  user's config entry cannot be reached and stays. Both actions ignore failure,
  so a machine that cannot be removed does not make the product impossible to
  uninstall.
- **`HYPERVSERVICE`** (default `vmms`) names the Windows service whose key says
  Hyper-V is enabled. Only the tests set it, because runners have no Hyper-V.
- **CI runs the real service against a stand-in for Hyper-V**, compiled in by
  the `rdstandin` build tag, and asserts that no release binary contains it
  (`test/msi.ps1` sections 11 to 19). Modify adding and removing the feature
  runs silently there (`ADDLOCAL=HyperV`, `REMOVE=HyperV`); the dialog is built
  and never clicked.

## An upgrade closes a running background session (2026-10-16)

- **Forced by:** upgrading over a running session, by hand, the installer said
  it could not close applications until the session was killed. Windows
  Installer asks Restart Manager to stop whatever holds a file it replaces
  (always in a silent install, from the files-in-use dialog otherwise), and
  Restart Manager stops a console program by sending CTRL_C_EVENT to its
  console
  ([Using Windows Installer with Restart Manager](https://learn.microsoft.com/en-us/windows/win32/msi/using-windows-installer-with-restart-manager),
  [Guidelines for Applications](https://learn.microsoft.com/en-us/windows/win32/rstmgr/guidelines-for-applications),
  both read 2026-10-16). The session was started `DETACHED_PROCESS`, with no
  console, so `RmShutdown` failed with 351 (ERROR_FAIL_SHUTDOWN).
- **Decision:** the session gets a console of its own with no window
  (`CREATE_NO_WINDOW`, `client/cmd/remote-docker/spawn_windows.go`) and takes
  the event as Ctrl-C: it closes, releasing its endpoint and lock, and the next
  docker command starts it again.
- **Covered by** `spawn_windows_test.go`, which calls `RmShutdown` on both ways
  of starting it, and `test/msi.ps1` 17b: another user's session gone after an
  upgrade and starting again.
- **Cost:** a session started by 0.11.0 or earlier is still detached, so the
  first upgrade from such a version still asks for it to be closed
  (`remote stop`, or end the process).

## Run by hand (2026-10-16)

- **Once, on one computer** (Windows 11 Pro 25H2, real Hyper-V), as a standard
  user who typed an administrator's credentials at the over-the-shoulder UAC
  prompt, following `docs/testing-machines.md`: an install creating the Hyper-V
  workspace, a reboot, an upgrade, Modify adding and removing the feature, an
  uninstall, and the files-in-use dialog closing a running session. The
  checkbox and the detached session above were found this way.
- **Not covered:** any other Windows build or Hyper-V setup, a domain account
  or a user who is a member only through a nested group (ADR 0055), the arm64
  MSI, and an MSI built by a tag release. Real Hyper-V has no automated
  coverage at all.

## Consequences

- **The `UpgradeCode` can never change.** It is the only thing relating one
  release's MSI to the next; changing it leaves two installs side by side, two
  entries in Programs and Features, and two copies on PATH. Written in capitals
  in the `.wxs` for that reason.
- **The refusal reads four directories, not PATH.** Windows Installer's
  `AppSearch` cannot enumerate `PATH`, and there is no locator that does. So
  `DOCKERONPATH` is a `DirectorySearch` over the two system directories and
  Docker Desktop's two locations, each of which is on PATH when it holds a
  `docker.exe`. In a 64-bit package `SystemFolder` is **SysWOW64** and
  `System64Folder` is **System32**, the reverse of what the names suggest;
  searching only the first builds, installs, and misses the directory that was
  meant. A `docker.exe` elsewhere on PATH is not noticed and is shadowed,
  because the install directory is appended and therefore loses: the failure is
  that the user does not get the name they asked for, not that they lose the
  other tool.
  The alternative was a custom action DLL, which is a C project and a second
  thing to build.
- **The MSI is unsigned.** The repository has no code-signing certificate; the
  only secret any workflow uses is `GITHUB_TOKEN`. SmartScreen will warn. This
  is in the README and on CLAUDE.md's not-tested list.
- **It is not in `checksums.txt`.** goreleaser writes that before this step
  runs. Adding it would mean re-signing the checksum file after the fact, which
  is worse than saying so.
- **A tag can publish an installer no suite has installed.** `msi.yml` is
  triggered by changes under `installer/windows/`, not by tags, so the release
  path does not wait on a Windows runner. What closes it in practice: any
  change to the installer touches those paths. The release path is unexercised
  in the other direction too: the `installers` job, the `dist/artifacts.json`
  lookup that finds goreleaser's Windows binaries and the `gh release upload`
  are all behind `github.ref_type == 'tag'`, and none of them has ever run.
- **A Windows runner installs an MSI built from this repository**, runs the
  binary out of a fresh shell's PATH and uninstalls it (`test/msi.ps1`). Still
  never installed: the release archives, an MSI built by an actual tag release,
  and the arm64 MSI, which is built on every run and installed by nothing,
  GitHub having no Windows arm64 runner.

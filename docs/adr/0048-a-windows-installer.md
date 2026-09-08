# 0048. A Windows installer, and it will not take the name `docker`

- Status: Accepted
- Date: 2026-09-08

## Context

The release carries a zip per platform and nothing else. On Windows that means
unpack it somewhere, put that somewhere on PATH by hand, and repeat both on
every upgrade. An MSI is what Windows expects to be handed for that.

The interesting part is not the packaging. ADR 0024 made the file's name the
whole installation: rename the binary to `docker.exe` and `docker run` on this
machine is this program, with no code behind it. An installer is the first
thing in this project that could do that renaming for the user, on a machine
whose PATH it also controls — which is the same power as shadowing another
vendor's tool.

## Decision

**One MSI per Windows architecture, attached to every tag release.**

| | |
|---|---|
| tool | WiX v5, `dotnet tool install --global wix` |
| built on | `windows-latest`, in an `installers` job after the `binaries` one |
| payload | the two Windows binaries goreleaser built, passed as an artifact |
| scope | `perMachine`: Program Files, system PATH, elevated |
| PATH | `Environment Part="last"` — appended, never prepended |
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
and removed at uninstall. No custom action, no second payload, and no code in
the Go program — the name is the installation. So the option costs nothing to
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
`NOT Installed AND &DockerName=3 AND DOCKERONPATH AND NOT ALLOWDOCKERSHADOW`.
The message names the file it found, and `ALLOWDOCKERSHADOW=1` overrides it.

Why refuse rather than win: the install directory is on PATH and a Windows
machine commonly already has a `docker.exe` — this README points at
`winget install Docker.DockerCLI` itself. Quietly getting in front of Docker
Desktop's CLI is the larger version of a promise ADR 0024 already makes, that a
docker context we did not create is left completely alone.

**Version.** An MSI `ProductVersion` is `major.minor.build`, and the installer
compares only those: major and minor are one byte, build is two. A snapshot
version (`sha-9370b24`) has no mapping to that at all, so
`installer/windows/build.ps1` validates the shape and the ranges and exits
naming the version. Truncating instead would produce an upgrade that never
fires, which is silent for a release cycle.

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
- **Part of "installing a release has never been done" is closed.** A Windows
  runner now installs an MSI built from this repository, runs the binary out of
  a fresh shell's PATH, and uninstalls it. Still unproven: the archives, an MSI
  built by an actual tag release, and the arm64 MSI, which is built on every run
  and installed by nothing, GitHub having no Windows arm64 runner.

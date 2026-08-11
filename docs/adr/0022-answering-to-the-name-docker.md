# 0022. The client answers to the name `docker`

- Status: **Superseded by [ADR 0024](0024-the-docker-cli-is-the-root.md)**

*(2026-08-11: the Docker CLI is the root now, so the binary does not answer to a
second name -- it simply is one, and renaming the file is the whole
installation. The argv[0] dispatch and `shim install` are deleted. The record
stands as written for the reasoning that led there.)*
- Date: 2026-08-10

## Context

ADR 0009 embedded the Docker CLI so that nothing would have to be installed on
the machine using this. It was reached as `remote-docker docker ps`, and that
prefix quietly kept the original problem alive.

A Windows user with `remote-docker` had the whole Docker CLI on their machine
and still could not type `docker`.

*(Corrected 2026-08-10. This record originally said there was no supported way
to install a standalone docker CLI on Windows. There is: `winget install
Docker.DockerCLI` ships v29.7.2, and download.docker.com publishes static zips.
The decision does not rest on that and is unchanged, but the reason it gave
was wrong. Re-check with `winget search --id Docker.DockerCLI`.)*

The premise still holds where it matters: a machine that cannot have software
installed cannot install that CLI either, and on every other machine a second
CLI is a second thing to update and keep in step with this one.

The prefix is not only a matter of typing. It is the name that everything else
looks for:

- muscle memory, and every command anybody has ever copied from a README;
- scripts and Makefiles;
- IDE integrations and tools that shell out to `docker`;
- **this program itself** — `reportContext` calls `exec.LookPath("docker")` to
  write a docker context, and on a machine with no docker it reported that it
  could not. Which is the premise machine, so the context was missing exactly
  where it was needed.

## Decision

The binary answers to a second name. If the name it was invoked by is
`docker`, the entire command line belongs to the Docker CLI. This is what
busybox has done for thirty years, and it costs one `if` in `main`.

The name is taken from **`os.Args[0]`, never `os.Executable()`**. The second
resolves symlinks — on Linux it reads `/proc/self/exe` — so it reports
`remote-docker` for exactly the installation this creates, and the feature
would be silently dead on the platform where a symlink is the right answer.

*(2026-08-11: this turned out to hold somewhere it was not written for. On
Android the process really is the system linker and `/proc/self/exe` says so,
so `argv[0]` is the only identity left standing. `shim install` works there,
which is the measurement. See [ADR 0023](0023-running-where-the-loader-is-not-us.md);
the shim's own `os.Executable` call had to become `selfPath`.)*

`remote-docker shim install` arranges for that name to exist, and is
deliberately not the mechanism: renaming the downloaded binary to `docker.exe`
is a complete installation on its own.

The shim is a **symlink, then a hardlink, then a copy**, in that order, on
every platform. The order is entirely about what survives an upgrade:

| form | costs | survives replacing the binary | Windows |
|---|---|---|---|
| symlink | nothing; stores a path | **yes** | needs Developer Mode or admin |
| hardlink | nothing; a second name for one file | no | no special rights; same volume only |
| copy | ~45MB | no | always available |

The copy is the only one that **asks first**, because it is the only one that
duplicates the binary and silently goes stale. With no terminal to ask, it
refuses; `--copy` is how a script says yes.

On Windows, `install` also adds its directory to the user PATH, through
`HKCU\Environment` and never `setx`. It is **appended**, so a real Docker
installed later still wins.

## Consequences

- **A `docker` we did not write is never touched.** A machine may get Docker
  Desktop tomorrow, and a shim that overwrote a real CLI is a broken machine.
  Ours is identified by `os.SameFile` against this binary, or by a marker file
  written beside it — never by executing the file to ask what it is, which is
  precisely what must not happen to a `docker.exe` of unknown provenance.
- **`setx` is forbidden.** It truncates PATH at 1024 characters, silently, and
  what is past the cut is gone. It is the obvious way to do this and it is
  destructive; the registry is the actual interface.
- **A hardlink or a copy can serve an old build after an upgrade**, and nothing
  about that announces itself — the same failure the session version check
  exists for, in a different place. `shim status` and `remote-docker status`
  both report it, and re-running `install` fixes it.
- **The client can now invoke itself.** `exec.LookPath("docker")` may find us,
  so every docker command this program runs carries
  `REMOTE_DOCKER_NO_SESSION=1`. Without it, `workspace create` writing a
  context would open an SSH connection, an NFS server and a reverse tunnel in
  order to write a line of JSON, and tear them all down again. Commands that
  reach no daemon at all — `context`, `completion`, `help`, and a bare
  `docker` — are excluded for the same reason.
- **Docker contexts start working on machines that had no CLI**, and without
  waiting for `shim install`: when PATH has no docker, the context commands
  invoke this binary directly, with the `docker` subcommand in front. Giving up
  instead is what left a premise machine with no context at all, so compose
  fell through to the Docker Desktop pipe and reported that the daemon was not
  running.
- **`docker compose` works under the new name.** This record originally said it
  did not, because ADR 0009 had ruled compose out as too expensive to embed.
  That stopped being true when compose v5 shipped and it is embedded now, so it
  answers under both names. *(Corrected 2026-08-11.)*
- **Not a `.cmd` wrapper**, which would have avoided the whole symlink
  question. It puts a `cmd.exe` between the user and every `docker run -ti`,
  mangling quoting, exit codes and Ctrl-C, and a tool that execs `docker`
  directly rather than through a shell may not find it at all.

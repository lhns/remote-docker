# CLAUDE.md

Project context for Claude Code. Read `docs/adr/` for why the architecture is
what it is — each record states a decision, what forced it, and what it costs.
`DESIGN.md` is the original shell-era design brief, retained for history and
superseded by ADRs 0001–0003.

## What this is

`remote-docker` is a single Go binary that makes a remote Docker-in-Docker
container behave like a local Docker installation. It embeds an SSH client, an
NFSv3 server, a Docker API proxy and the Docker CLI itself, so nothing has to
be installed on the machine using it.

Your directories are genuinely mounted into the containers — not copied, not
synced — from anywhere on the machine, not only the working directory. Bind
mounts are rewritten into NFS-backed volumes the workspace daemon mounts for
itself. Published ports become reachable locally as containers start.

## Layout

```
cmd/remote-docker/       the client binary
cmd/remote-dockerd/      the server agent (not built yet — ADR 0010)

pkg/workspace/           THE SHARED CONTRACT, imported by both binaries

internal/client/
  config/                settings precedence, state paths
  sshx/                  ssh client, keys, known_hosts, forwards, pty shell
  nfsserve/              in-process NFSv3 server, virtual export namespace
  proxy/                 Docker API proxy + a small API client of our own
  rewrite/               binds -> NFS volumes, owner labelling, volume GC
  ports/                 published ports -> local forwards
  session/               wires the above into one live connection

image/                   the workspace container (still sshd + shell helpers)
deploy/                  compose and swarm deployments
client/                  the ORIGINAL shell clients — superseded, not yet deleted
test/                    integration.sh, plus the legacy bash suites
docs/adr/                architecture decision records
```

## Build and test

```bash
go build ./...
go test ./...                    # unit tests; no daemon needed
golangci-lint run ./...          # must be clean

# the client
go build -o remote-docker ./cmd/remote-docker

# end to end — needs docker and a kernel with NFS client support
bash test/integration.sh
```

Lint is installed with the project's own toolchain
(`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`),
because the GitHub action's pinned binary is built against an older Go and
refuses a module targeting a newer one.

## The development constraint that shapes everything

**There is no Docker and no WSL on the development machine.** That is the
premise of the project, and it applies to building it too. So:

- Unit tests must run without a daemon, and they do — including a full NFSv3
  conversation against a real NFS client over a real socket, and the proxy
  against real HTTP framing.
- **CI is the only integration environment.** The first time any change meets
  a real dind daemon or a real kernel NFS mount is on a GitHub runner.
- Batch work locally and verify end to end in CI, rather than iterating there.
  A round trip is minutes.

## Invariants — break these and things fail quietly

- **`pkg/workspace` is the contract, and only the contract.** A type goes in
  it if both binaries must *agree* on it, not merely if both use it. The
  uid→port formula lives there because it used to live in two shell scripts
  and drifting copies presented as a network fault.
- **The proxy must be transparent to hijacked and streamed connections — and
  must not over-detect them.** Both directions of this are load-bearing and
  both have been got wrong:
  - Treating a hijack as an ordinary response loses container stdout, and
    `docker run` exits 0 having printed nothing.
  - Treating an ordinary chunked response as a hijack feeds chunk-size lines
    to the stdcopy demultiplexer (`Unrecognized input header: 49`).
  A hijack is 101, or a docker stream content type with no content length and
  no transfer encoding.
- **Half-close the upstream, never close it.** `docker run` without `-i`
  closes its stdin as soon as attach is established; closing the whole stream
  in response tears down the session carrying the container's output.
- **Only `/containers/create` is ever decoded.** Everything else is copied
  through. The body is handled as generic JSON, never typed structs, so
  unknown fields survive.
- **Never rewrite a named volume**, and never delete a volume without both the
  `rd-` prefix *and* the managed label. A user may legitimately name a volume
  `rd-backups`.
- **`Session.Close` must not wait on the caller's context.** The session owns
  its own; a one-shot command's context is never cancelled, and Close
  deadlocked on exactly that.
- **`git` line endings are forced to LF** by `.gitattributes`. A CRLF
  `#!/bin/sh\r` in the image fails as "not found", naming the interpreter
  rather than the carriage return.
- **`key-watcher` polls as well as using inotify.** The keys directory is
  expected to be on CephFS/NFS, where inotify never fires for changes made on
  another host. This must survive the rewrite to Go.
- **Accounts use `usermod -p '*'`, not a locked (`!`) password.** Some sshd
  builds refuse public-key auth for locked accounts.

## Retired invariants

These were true of the shell design and are no longer. Do not reintroduce
them:

- sudoers argument pinning, `workspace-mount --force`, and the mount
  propagation workaround — dissolved by per-bind volumes (ADR 0006).
- The ControlMaster split between the two clients — multiplexing is inherent
  to one `ssh.Client` (ADR 0004).
- The duplicated uid→port formula — one function now (ADR 0011).

## State of play

Working, proven end to end in CI: the tunnel, the NFS export, bind rewriting
including sources outside the working directory, automatic port forwarding,
and managed volume creation.

Not done:
- `cmd/remote-dockerd`, the Go server agent (ADR 0010). The image still runs
  sshd, `key-watcher` and the mount helpers.
- `docker compose` — a separate module, not obtained by embedding the CLI.
- The original shell clients are superseded but not yet deleted.
- No tag has been pushed, so the release pipeline is unexercised.

## Conventions

- Comments explain *why*, not *what*. Several encode findings that cost real
  debugging — the hijack rules, the half-close, the genproto exclusion, the
  go-nfs refusal panic, mount propagation. Do not strip them.
- POSIX `sh` for anything remaining in `image/bin/` (the image is Alpine);
  bash is fine in `test/`.
- A finding that contradicts an ADR gets the ADR corrected, not ignored.

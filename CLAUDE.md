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
go.mod                   THE SHARED MODULE (ADR 0021), depends on ~nothing
  pkg/workspace/         THE CONTRACT, imported by both binaries
  internal/logx/         the one log handler, so both look the same
  internal/iox/          the one bidirectional copy, and the one answer to
                         what half-closing means (ADR 0021)
  test/                  lib.sh, integration.sh, per-user-dind.sh, probes

client/go.mod            the client module: docker/cli, buildx, 786 go.sum lines
  cmd/remote-docker/     the client binary
  internal/
    config/              settings precedence, state paths
    fswatch/             watches shared dirs, streams changes to the agent
    sshx/                ssh client, keys, known_hosts, forwards
    nfsserve/            in-process NFSv3 server, virtual export namespace
    proxy/               Docker API proxy + a small API client of our own
    rewrite/             binds -> NFS volumes, owner labelling, volume GC
    ports/               published ports -> local forwards
    session/             wires the above into one live connection

agent/go.mod             the agent module: 7 third-party modules, 24 go.sum lines
  cmd/remote-dockerd/    the server agent (ADR 0010)
  internal/
    accounts/            one unix account per enrolled key
    sshd/                the SSH server: auth, sessions, forwards
    supervise/           starts and watches the workspace's own dockerd
    elevate/             relaunch privileged, for Swarm (ADR 0013)
    notify/              replays the client's changes as real syscalls
    daemons/             a dockerd per account, and the one resolver both
                         modes answer through (ADR 0020)
    netns/               run a function inside another process's netns
                         (an empty path means this one -- ADR 0020)
    dockercli/           the one way this side runs the docker binary

image/                   the workspace container (Dockerfile only)
deploy/                  compose and swarm deployments
docs/adr/                architecture decision records
```

## Build and test

```bash
# THREE MODULES (ADR 0021), and `./...` stops at a module boundary. A bare
# `go build ./...` at the root covers the shared module and nothing else --
# it will pass while compiling almost none of this repository.
for m in . ./agent ./client; do (cd $m && go build ./... && go test ./...); done

# lint, four passes: one per module, plus the agent under Linux. Its session
# handling is Linux-only, so a lint on the development machine does not see the
# file at all. CI does, and will fail on what you did not lint.
for m in . ./agent ./client; do (cd $m && golangci-lint run ./...); done
(cd agent && GOOS=linux golangci-lint run ./... && CGO_ENABLED=0 GOOS=linux go build ./...)

# the client
(cd client && go build -o ../remote-docker ./cmd/remote-docker)

# end to end -- needs docker and a kernel with NFS client support
bash test/integration.sh
```

`go.work` ties the three together for editors and local commands. CI and the
image build deliberately ignore it and build one module at a time, so a missing
`require` fails where it is wrong rather than being covered by the workspace.

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
  shared module around it (ADR 0021) is one step wider and no wider: something
  goes there if both binaries must behave the *same way*, which is true of the
  log handler and of half-closing a stream, and not of an env-var helper. The
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
  in response tears down the session carrying the container's output. This now
  lives once, in `internal/iox`, because the two binaries are the two ends of
  one stream and their copies had drifted to OPPOSITE fallbacks. `Splice`
  leaves a stream that cannot half-close alone; `SpliceAndClose` closes it, and
  that difference is deliberate -- a port forward carries no output stream and
  must not leak a blocked reader. A test pins both.
- **Only `/containers/create` is ever decoded.** Everything else is copied
  through. The body is handled as generic JSON, never typed structs, so
  unknown fields survive.
- **Never rewrite a named volume**, and never delete a volume without both the
  `rd-` prefix *and* the managed label. A user may legitimately name a volume
  `rd-backups`.
- **Binding the endpoint is not a lock.** On Unix a bind used to remove any
  existing socket first, so a second process silently unlinked a *running*
  one's socket and took its place -- the first kept accepting on an inode
  nobody could reach. Clearing a stale socket is only safe once the lock is
  held (ADR 0017). On Windows the pipe bind does exclude, which is why the two
  platforms failed differently and neither failure named the owner.
- **A stream holds its gate lease until it closes.** Releasing it when the
  stream opened meant `docker attach`, `exec -it` and `logs -f` pinned nothing,
  and survived an idle release only because their container happened to be
  running. It is also the only reliable way to tell a stream in use from an
  idle keep-alive connection, which the background session's lifetime depends
  on.
- **`Session.Close` must not wait on the caller's context.** The session owns
  its own; a one-shot command's context is never cancelled, and Close
  deadlocked on exactly that.
- **`git` line endings are forced to LF** by `.gitattributes`. A CRLF
  `#!/bin/sh\r` in the image fails as "not found", naming the interpreter
  rather than the carriage return.
- **Never range a map to assign something durable.** Account uids are handed
  out in `accounts.reconcile`, which used to range the `found` map -- so which
  account got which uid, and therefore which reverse-tunnel port, differed
  between runs on a fresh workspace. `Sync` sorts the key files precisely so
  collisions resolve deterministically; passing the result on as a map threw
  that away. It presented as a test failing about one run in eight.
- **The keys watcher polls as well as using inotify.** The keys directory is
  expected to be on CephFS/NFS, where inotify never fires for changes made on
  another host.
- **Accounts use `usermod -p '*'`, not a locked (`!`) password.** Some sshd
  builds refuse public-key auth for locked accounts. Kept even though the agent
  authenticates itself, since a deployment may run sshd alongside.
- **`shadow` must stay in the image.** The agent shells out to `useradd`, which
  handles the locking between passwd, group and gshadow that hand-editing gets
  wrong.
- **Replay must never mutate.** `internal/server/notify` performs syscalls on
  the user's own files, through the export it is notifying about. `O_CREAT`,
  `O_TRUNC` and a non-identity `utimensat` are all forbidden, even where they
  would produce a better event: the file may have been deleted again between
  the client observing a change and the agent replaying it, and the cost of
  being wrong is data appearing in someone's project. The measured
  `IN_CREATE` from `open(O_CREAT)` is deliberately not used for this reason.
- **The replay primitives are measured, not remembered.** `utimensat` with
  `atime=UTIME_OMIT` gives `IN_MODIFY`; with *both* times set it gives
  `IN_ATTRIB`, which most watchers ignore. That asymmetry is the whole reason
  the feature works, and `test/integration.sh` section 11d keeps both rows so
  a kernel change cannot quietly take it away.
- **An account is resolved to its daemon exactly once, through
  `daemons.Targets`.** Never reintroduce `if cfg.Daemons != nil` at a use site.
  There were nine such branches and the invariant they guarded is one that
  fails by *succeeding*: a session sent to the wrong daemon runs, against
  another account's containers, with nothing logged and nothing failing. The
  shared daemon of ADR 0012 is `daemons.Shared`, an implementation, so there is
  no second path that could drift from the first. The empty string is how "no
  redirection" travels -- `netns.Do("")` stays in this namespace, an empty
  `Host` leaves `DOCKER_HOST` unset -- and that is what lets both modes be one
  code path.
- **A per-account dind is separation, not isolation.** Each one runs
  privileged, so a determined account can still break out and reach another's.
  What ADR 0019 buys is that nobody sees anyone else's work by accident. ADR
  0012's revisit trigger is NOT satisfied by it, and anything claiming
  otherwise -- release notes, README, a commit message -- is wrong.
- **A netns helper must never return a thread whose namespace it could not
  restore.** `socket(2)` uses the calling thread's namespace, so the switch and
  the socket call are pinned to one `LockOSThread`ed thread. If the restoring
  `Setns` fails, that thread is parked forever rather than unlocked: an
  unlocked thread rejoins the runtime's pool still in someone else's namespace,
  and the next goroutine scheduled onto it opens sockets there, invisibly.
  Leaking a thread is the cheap and correct answer.
- **A per-account daemon's answers are untrusted input.** It reports its own
  volume mountpoints and the account is root inside it. `path.Join` is not
  containment -- it CLEANS, so `/proc/42/root` joined to `/../../etc/shadow` is
  `/proc/etc/shadow`, outside the root and looking correct. `relocate` checks
  the result; `O_NOFOLLOW` and `AT_SYMLINK_NOFOLLOW` in the poker stopped being
  tidiness the moment those paths left the agent's own filesystem.
- **`rd-dind-<account>-lib` is the account, and the container in front of it is
  disposable.** The graph volume is named and labelled so the daemon container
  can be removed and recreated without losing anything, and so an operator can
  tell which volumes must never be pruned. Anonymous storage here would make an
  ordinary `docker system prune -a --volumes` on the workspace's own daemon
  destroy every account's work with nothing on screen naming it.
- **Never `--rm` a per-account daemon**, and never copy `elevate`'s
  `docker rm -f` opener into `daemons`. elevate's child is a singleton whose
  state is worthless; this one holds somebody's containers, images and volumes.
  `Ensure` on a stopped daemon runs `docker start`.
- **Adoption keys on the persisted workspace id, never a container id.** An id
  changes on every redeploy, so adopting by it orphans every account's daemon
  on the first `compose up -d` -- still running, unadoptable, holding their
  users' work, while the agent starts a second set under names already taken.
- **`test/watchprobe` reads raw inotify, not fsnotify.** fsnotify's mask omits
  `IN_OPEN` and `IN_CLOSE_WRITE`, so a probe built on it cannot see the
  primitive under test and would report "nothing happened" convincingly.

## Retired invariants

These were true of the shell design and are no longer. Do not reintroduce
them:

- sudoers argument pinning, `workspace-mount --force`, and the mount
  propagation workaround — dissolved by per-bind volumes (ADR 0006).
- The ControlMaster split between the two clients — multiplexing is inherent
  to one `ssh.Client` (ADR 0004).
- The duplicated uid→port formula — one function now (ADR 0011).

## State of play

The shell implementation is gone. The image ships one binary; sshd, sudo, the
key watcher and the mount helpers are deleted, and the agent passed the suite
written against sshd, unchanged, before they were removed.

### Proven end to end, in CI, on every push

Against a real dind daemon, a real kernel NFS mount and the real client
binary: the tunnel, the NFS export, bind rewriting including sources outside
the working directory, automatic port forwarding, managed volume creation,
`docker compose` including one service reaching another over its network, a
stock `ssh` still getting a shell on a pty as the enrolled account, the
embedded Docker CLI, `gc`, idle disconnect and reconnect, cross-user port
hijack refusal, `elevate`, the replay primitive matrix (which syscall produces
which inotify event), an edit here firing inotify inside a container with
`REMOTE_DOCKER_WATCH=partial`, the background session (detached start, version
mismatch, self-reclaim), and the workspace lifecycle with the docker context
appearing and disappearing alongside it.

A second suite, `test/per-user-dind.sh`, runs the same workspace with two
enrolled accounts and a daemon each (the default since ADR 0019): that they reach
different daemons, that neither can list or stop the other's containers, that
each account's bind mount resolves (which is the only real proof the reverse
tunnel was bound inside that account's netns), that both publish the same port
at once, that a shell's `DOCKER_HOST` is its own daemon, that neither account
is in the `docker` group, and that restarting the agent adopts the running
daemons with their containers intact.

### NOT tested, and do not claim otherwise

Keep this list honest. An audit found paths described in summaries as tested
that had no coverage at all -- `elevate` most of all, which had been asserted
as "the docker run mechanism under it is tested" when only the pure planning
function was.

- **Swarm itself.** `elevate`'s `docker run` mechanism is tested; the Swarm
  wiring -- templated `{{.Task.Name}}`, `mode: host` publishing, placement --
  needs a real cluster. CI cannot cover it.
- **`docker build` through the proxy.** The `/session` hijack it depends on is
  unit tested; no integration test runs an actual build. A legacy build DOES
  work end to end (verified by hand against a real workspace).
- **BuildKit is not available through the embedded CLI**, and this contradicts
  what ADR 0009 originally corrected itself to say. buildx is a separate plugin
  binary and is not vendored, so `docker build` silently uses the classic
  builder even with `DOCKER_BUILDKIT=1` -- while the daemon advertises
  `Builder-Version: 2`. The `/session` hijack therefore has no exercised
  caller. Point a real docker CLI at the workspace's docker context to get
  BuildKit.
- **The Windows and macOS clients.** Cross-compiled on every push, executed
  never. The integration suite runs the Linux client, and the endpoint code is
  the one place they genuinely diverge.
- **The release pipeline.** No tag has been pushed.
- **`docker compose` embedded.** Not attempted -- it works through the proxy,
  and embedding would pin docker/cli back a major version (ADR 0009).
- **`coarse` watch mode.** The directory-level poke for deletions is unit
  tested; no integration test asserts that a real watcher notices a deletion
  through it.
- **Watching at scale.** The budget, the exclude list and overflow reporting
  are unit tested against a fake backend. Nothing has run a watcher over a
  10,000-directory tree, and the macOS backend (kqueue, one fd per *file*) has
  never been executed at all.

## Conventions

- Comments explain *why*, not *what*. Several encode findings that cost real
  debugging — the hijack rules, the half-close, the genproto exclusion, the
  go-nfs refusal panic, mount propagation. Do not strip them.
- bash in `test/`. There is no shell left in the image: `image/` is a
  Dockerfile and nothing else.
- A finding that contradicts an ADR gets the ADR corrected, not ignored.

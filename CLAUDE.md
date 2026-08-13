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
THE REPO ROOT IS NOT A MODULE, deliberately. `go build ./...` here fails
outright, where a module at the root would instead let it pass while compiling
almost none of the repository.

core/go.mod              THE SHARED MODULE (ADR 0021). Its library packages have
                         NO third-party dependency at all; the one x/sys in
                         go.mod is the probes reading raw inotify.
  workspace/             THE CONTRACT, imported by both binaries
  tunnel/                THE AGREEMENT (ADR 0030): the one bidirectional copy
                         and the one answer to what half-closing means, plus
                         the names both ends speak. Imports no SSH library --
                         the two implementations live with the ends that run
                         them, in core-client/ and core-agent/.
  logx/                  the one log handler, so both look the same. PUBLIC,
                         not internal/: every module imports it, and under
                         core/internal/ Go would let only core/ reach it.
  probes/                watchprobe, pokeprobe -- Go, so they need a module

core-client/go.mod       YOUR OWN MACHINE, minus Docker. 0 docker packages in
                         its graph, against the client's 191 -- which is the
                         claim this whole split was for, and it is measured.
  tunnelclient/          dialling the tunnel: sessions, streams, both forwards.
                         Given a signer and a host key rule; decides neither.
  nfsserve/              in-process NFSv3 server, virtual export namespace
  fswatch/               watches shared dirs on three platforms, budget,
                         excludes, overflow
  keys/                  the keypair and known_hosts: this machine's identity

client/go.mod            the client module: THE GLUE. docker/cli, buildx
  cmd/remote-docker/     the client binary, whose root command IS the Docker
                         CLI; ours lives under `remote` (ADR 0024)
  internal/
    config/              settings precedence, state paths
    machine/             provisioning a workspace on this machine (ADR 0026)
    proxy/               Docker API proxy + a small API client of our own
    rewrite/             binds -> NFS volumes, owner labelling, volume GC
    ports/               published ports -> local forwards. Stays glue whole:
                         its manager is keyed on container ids throughout, and
                         the generic forward is already tunnelclient's
    session/             wires the above into one live connection, dials the
                         tunnel, and holds the enrolment hint -- the one part
                         of authentication that is this project's policy

core-agent/go.mod        THE WORKSPACE SIDE, minus Docker. Reaches none of
                         dockercli, daemons, supervise or elevate, and that
                         is the whole membership test.
  tunnelserver/          answering the tunnel: the forwarding protocol, given
                         who may bind what and which namespace it goes in.
  accounts/              one unix account per enrolled key, and the ports
  notify/                replays the client's changes as real syscalls
  netns/                 run a function inside another process's netns
                         (an empty path means this one -- ADR 0020)

agent/go.mod             the agent module: THE GLUE. 4 direct third-party
                         requires, 24 go.sum lines
  cmd/remote-dockerd/    the server agent (ADR 0010)
  internal/
    sshd/                the SSH server: auth, sessions, and the forwarding
                         POLICY core-agent/tunnelserver asks. Its session
                         handling is docker all the way down and stays here.
    supervise/           starts and watches the workspace's own dockerd
    elevate/             relaunch privileged, for Swarm (ADR 0013)
    daemons/             a dockerd per account, and the one resolver both
                         modes answer through (ADR 0020)
    dockercli/           the one way this side runs the docker binary, and
                         the volume lookup notify asks for

image/                   the workspace container (Dockerfile only)
deploy/                  compose, swarm, and the systemd unit for a VM
                         workspace (ADR 0025)
docs/adr/                architecture decision records
```

## Build and test

```bash
# FIVE MODULES (ADR 0021, ADR 0031), and `./...` stops at a module boundary,
# so the loop is the only thing that covers the repository. Running it at the
# root fails outright, which is the point: there is no module there to build.
for m in ./core ./agent ./core-agent ./core-client ./client; do (cd $m && go build ./... && go test ./...); done

# lint, seven passes: one per module, plus the agent AND core-agent under
# Linux. Both carry Linux-only files -- session handling, netns, the unix
# provisioner, the inotify poker -- which a lint on the development machine
# does not see at all. CI does, and will fail on what you did not lint.
for m in ./core ./agent ./core-agent ./core-client ./client; do (cd $m && golangci-lint run ./...); done
for m in agent core-agent; do (cd $m && GOOS=linux golangci-lint run ./... && CGO_ENABLED=0 GOOS=linux go build ./...); done

# gofmt is a SEPARATE CI step and golangci-lint here does not cover it. It bites
# after a scripted import rewrite: changing the text of an import without moving
# it leaves the block unsorted, which compiles and tests clean.
gofmt -l .   # must print nothing

# the client
(cd client && go build -o ../remote-docker ./cmd/remote-docker)

# end to end -- needs docker and a kernel with NFS client support
bash test/integration.sh
```

`go.work` ties the five together for editors and local commands. CI and the
image build deliberately ignore it and build one module at a time, so a missing
`require` fails where it is wrong rather than being covered by the workspace.
`image/Dockerfile` copies the module trees it needs by name, so a new module the
agent imports must be added there or the image build fails on it alone. CI reads
its Go version from `core/go.mod`, and `.goreleaser.yaml` tidies in `core/`:
both used to name a root `go.mod` that no longer exists.

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

- **`core/workspace` is the contract, and only the contract.** A type goes in
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
  lives once, in `core/tunnel`, because the two binaries are the two ends of
  one stream and their copies had drifted to OPPOSITE fallbacks. `Splice`
  leaves a stream that cannot half-close alone; `SpliceAndClose` closes it, and
  that difference is deliberate -- a port forward carries no output stream and
  must not leak a blocked reader. A test pins both.
- **The transport is handed its auth and decides none of it** (ADR 0030).
  `core-client/tunnelclient` takes an `ssh.Signer` and an `ssh.HostKeyCallback`;
  `client/internal/session` builds both and is the only place that knows
  enrolment is a file in `authorized_keys.d`. There is no default host key rule, because
  every default is either a prompt nobody is there to answer or an acceptance of
  anybody -- so a nil callback is refused by name rather than mid-handshake.
  The root `core/tunnel` imports NEITHER SSH library: Go links what is imported,
  and that is the whole reason the agent's 24 go.sum lines did not move.
- **Only `/containers/create` is ever decoded.** Everything else is copied
  through. The body is handled as generic JSON, never typed structs, so
  unknown fields survive.
- **A rewritten mount keeps every option it arrived with, `ro` above all.**
  A bind becomes an NFS volume the workspace daemon mounts for itself, and that
  export is read-write -- so the read-only flag surviving the rewrite is the
  only thing between a container and the user's files. Both paths carry it
  because neither interprets what it does not have to: `Binds` keeps the
  trailing option field verbatim, and a `Mounts` entry stays a generic map with
  only `Type` and `Source` touched. `BindOptions` is the one deliberate
  deletion, because the daemon rejects it on a volume mount. Unit tests pin
  both, and `test/integration.sh` section 9b pins the end of it: the container
  is refused AND the directory on this machine is unchanged.
- **Never rewrite a named volume**, and never delete a volume without both the
  `rd-` prefix *and* the managed label. A user may legitimately name a volume
  `rd-backups`.
- **A volume this session exports is in use, whatever the daemon says.** The
  daemon calls a volume in use only once a container names it, which is
  strictly after the volume is created -- and the collector runs in that gap,
  because the connection it rides on is opened lazily by the very request
  creating the volume. Losing that race is silent: the daemon RECREATES a
  missing named volume as an empty local one, so the container starts with an
  empty directory where the project should be. `rewrite.Guard` is the answer --
  the share registry decides, and one lock spans registering a share and
  creating its volume, so the two orders both end correctly.
- **The Docker CLI is the root, and nothing of ours sits beside it.** Ours is
  all under `remote` (ADR 0024). Putting a flag or a command at the top level
  puts it in docker's own namespace, where `--host` and `--user` already exist
  and where pflag skips the duplicate silently while a clashing SHORTHAND
  panics the whole subtree. This is also what deleted the shim: renaming the
  file to `docker` is the installation, and it needs no code.
- **This binary asks `self.go` which file it is and how to run itself again,
  never `os.Executable` or `exec.Command` directly.** Android refuses to
  execute files in app data directories, so Termux runs a program as
  `linker64 <absolute path>` and the process really IS the linker.
  libtermux-exec hides that in libc, which a Go binary never loads. So
  `selfPath` for the path, because `/proc/self/exe` is the linker and the help
  text would have named THAT as the program; and `selfCommand` for a respawn,
  because the file cannot be exec'd at all and `start` could not launch its own
  session. Both failed naming the linker or nothing:
  `expected absolute path: "start"`, then `fork/exec ...: permission denied`.
  No linker path is written down -- `os.Executable` names the loader already
  running us, and hardcoding one would be a guess about a platform nothing
  tests.
- **A docker command this program runs itself carries
  `REMOTE_DOCKER_NO_SESSION=1`.** `exec.LookPath("docker")` may find *us*, so
  without it `remote create` writing a context opens an SSH connection, an NFS
  server and a reverse tunnel to write a line of JSON.
- **One scan reads a docker command line, and it knows which root flags take a
  value.** `invokingDocker` and the context rule both have to read argv before
  cobra parses it, and a scan that treats every non-flag word as the subcommand
  reads `docker --context remote ps` as our own namespace and runs the command
  with no session. `scanRootArgs` is the one walk and `rootFlags` the list;
  they were two loops with the same rules, and one of them had it wrong.
- **Binding the endpoint is not a lock.** On Unix a bind used to remove any
  existing socket first, so a second process silently unlinked a *running*
  one's socket and took its place -- the first kept accepting on an inode
  nobody could reach. Clearing a stale socket is only safe once the lock is
  held (ADR 0017). On Windows the pipe bind does exclude, which is why the two
  platforms failed differently and neither failure named the owner.
- **Closing the endpoint listener asks more than once.** go-winio's pipe
  listener signals its accept goroutine over an unbuffered channel and then
  waits to be told it finished, and a client connecting at that moment can have
  the signal consumed by the connect path and reported as ERROR_PIPE_CONNECTED
  or ERROR_NO_DATA -- neither of which it recognises as a close
  (microsoft/go-winio#85, PR #369 unmerged as of 2026-08-11). The signal is
  spent, the listener waits for another, `Close` waits for the listener, and
  `Accept` never returns, so the session hangs behind it. Signalling again lands,
  because the listener is back in a receptive select on both paths. It presented
  as one CI run in many timing out after ten minutes on Windows.
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
- **A recorded export is a capability the workspace may name, never a path it
  may supply.** The registry is per process and a volume outlives one, so
  `compose up -d` on containers that already exist starts them without creating
  them, registers nothing, and the mount for the volume made last time is
  answered "no such file or directory" against a directory that is right there
  (ADR 0027). The record fixes that, and it is checked again every time it is
  read: the id is RECOMPUTED from the path, the file is bound to this host and
  account and refused wholesale if either differs, and `/cwd` is never restored.
  Restore only from a MOUNT that missed; `Lookup` and `Shares` must never
  resurrect, or "in use" depends on who asked. And never feed the record to
  `rewrite.Guard`: a stopped container already pins its volume, so the collector
  was never the hazard, and doing so would keep every recorded volume alive
  until the record expired.

- **A gate hands out a connection only after asking whether it is alive, and a
  dead one is dropped rather than asked whether anything depends on it.**
  Detection existed and went nowhere: the keepalive closed the SSH client and
  told nobody, so `held` still meant "we have one" and every later request got
  the corpse. The sweep then asked that corpse whether it was busy, got an
  error, and "cannot tell means keep" made it unreleasable, so the session
  wedged until `remote restart` -- which also refused, because `IdleFor` asked
  the same dead connection. `alive` must never do I/O: it runs before every
  request, where `busy`'s round trip cannot.

- **The account is the identity and the machine is the client.** One account's
  machines share the daemon, and therefore containers and images, which is the
  point of using one account from both. They do not share files, because those
  are on one machine, so the export, its port and the volumes behind it are per
  CLIENT (ADR 0029). The client is the digest of the key the agent has already
  authenticated: stable per machine, and impossible to claim, which an id the
  client sent would not be. The uid still decides an account's FIRST port, so
  nothing renumbers; `accounts.Ports` allocates the rest and `Allow` asks it
  rather than recomputing `PortForUID`, because recomputing would refuse a port
  the agent had just handed out.

- **A port reservation belongs to a session, not to an account.** One listener
  can hold a port, so `Bind` refuses anybody who is not already nobody,
  including a second session of the same account, and `Release` takes the token
  minted when the reservation was taken. Releasing by name meant a second
  machine's FAILED bind deleted the first machine's live reservation, after
  which `AllowDial` reported the port as free and, on a shared daemon (ADR
  0012), any other account could reach an NFS export that authenticates nobody.
  An ordinary action reached it: opening the client on a second machine.

- **Never range a map to assign something durable.** Account uids are handed
  out in `accounts.reconcile`, which used to range the `found` map -- so which
  account got which uid, and therefore which reverse-tunnel port, differed
  between runs on a fresh workspace. `Sync` sorts the key files precisely so
  collisions resolve deterministically; passing the result on as a map threw
  that away. It presented as a test failing about one run in eight.
- **The keys watcher polls as well as using inotify.** The keys directory is
  expected to be on CephFS/NFS, where inotify never fires for changes made on
  another host.
- **Revoking on an unusable key file takes two reads; a missing one takes
  one.** An account is enrolled exactly while its file holds a key, so emptying
  the file is how you revoke somebody and must keep working. But a file being
  saved is empty for a moment, and a single read cannot tell that moment from
  an emptying meant on purpose. It used to revoke on the first read and log
  "its key file is gone" about a file that was right there, which presents as
  access being withdrawn at random rather than as a race. A file that is
  actually absent has no write window and revokes at once.
- **A key file is parsed line by line.** Several keys per file is the format,
  and reading it as one stream stopped at the first line it could not parse, so
  a typo or a BOM on the top line silently dropped every key under it. A bad
  line costs that line and is counted in the warning.
- **Accounts use `usermod -p '*'`, not a locked (`!`) password.** Some sshd
  builds refuse public-key auth for locked accounts. Kept even though the agent
  authenticates itself, since a deployment may run sshd alongside.
- **The unix account name is not the account name, and the uid is what
  identifies it.** An enrolled `alice` logs in as `alice`; the unix user is
  `rd-alice` (ADR 0025). `Ensure` keys on the uid, because that is what the
  uidmap binds and what the port and the file ownership come from -- so an
  older workspace's `alice` is adopted as it stands, and a uid held by someone
  this workspace did not create is REFUSED rather than adopted. Adopting one
  hands an enrolled key another user's files, which is a failure that succeeds.
  Only `UnixProvisioner` ever sees the prefixed name: the keys filename, the
  login name, the port ownership and `rd-dind-<account>` all use the account
  name, and a test that asks the unix side must ask for `rd-<account>` --
  spelling it `<account>` made `id -nG` fail and the suite read the failure as
  a pass.
- **The WSL backend's decisions live in `wsl.go`, which has no build tag.**
  Only running wsl.exe is Windows-only. Two things there are worth keeping:
  wsl.exe writes UTF-16, so its output read as bytes looks like text with NULs
  between the characters and every `Contains` against it fails silently; and
  `wsl -l -v` marks the default distribution with an asterisk COLUMN, so the
  name of a default distribution is not the first field. Both are tested on a
  machine with no WSL, which is the only way they are tested at all.
- **A machine-backed workspace is an ordinary workspace with a lifecycle.**
  The `machine` block in the config is the only thing that differs anywhere,
  and two commands read it: `rm`, which has a machine to destroy (ADR 0026),
  and `session.connect`, which has one to locate. Never add a second data path
  beyond those: the export, the port forwarding and the rewriting do not know a
  machine from a host in another country. And `rm` REFUSES when it cannot
  destroy the machine, because the config entry is the only record that one was
  ever built.
- **A machine is located and held, every time, and `core-client` in its config is a
  placeholder.** Both halves were measured on a Windows runner
  (`.github/workflows/machine.yml`, 2026-08-11) and both fail as a refused
  connection that names nothing:
  - Windows could not reach `127.0.0.1:2222` while the machine was running and
    its agent listening, and reached the machine's own `172.24.110.158:2222` at
    once. WSL2 forwards localhost through a relay that did not carry it. The
    address is also given out at boot, so a stored one is wrong from the moment
    the machine restarts -- hence asked at every connection, never saved.
  - A machine with nobody in it shuts down, and neither an open TCP connection
    nor a command that runs and exits is somebody. Poking one every ten seconds
    produced a machine that ran for thirty seconds, stopped, and started again
    on the next poke, so its dockerd never became ready and its agent never
    listened. A hold is one wsl.exe session that STAYS OPEN, and the session
    closes it last, after everything that wanted the machine there.
- **A rootfs is a filesystem, and the image's environment is not in it.**
  `docker export` writes layers; `ENV`, `PATH` and the entrypoint live in the
  image config beside them. A machine imported from one starts with the
  backend's environment and none of the image's, which surfaces a long way from
  the cause: dockerd's entrypoint is not on a `PATH` without `/usr/local/bin`,
  the agent restarts it every two seconds forever and blocks its own listener
  for ninety seconds waiting for a socket that will never appear.
  `DOCKER_TLS_CERTDIR` must be EMPTY rather than unset, which is how
  `image/Dockerfile` turns dind's TLS off.
- **A VM workspace is the same agent, not a mode.** ADR 0025 moves two things
  to the operator -- starting dockerd (`WORKSPACE_ENABLE_DIND=false`) and, in
  shared-daemon mode only, the NFS client -- and changes nothing else. Never
  add an `if onAVM`: both daemon modes already read one switch and a VM obeys
  it unchanged, which is the same argument ADR 0020 makes about daemon targets.
  The asymmetry that is easy to get wrong: with a daemon per account the NFS
  mount happens inside `docker:dind`, which ships a client; in shared mode the
  machine itself mounts.
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
- **`core/probes/watchprobe` reads raw inotify, not fsnotify.** fsnotify's mask omits
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
`docker compose` including one service reaching another over its network and
the EMBEDDED compose bringing a stack up on its own (ADR 0009), a
stock `ssh` still getting a shell on a pty as the enrolled account, the
embedded Docker CLI, `gc`, idle disconnect and reconnect, cross-user port
hijack refusal, `elevate`, the replay primitive matrix (which syscall produces
which inotify event), an edit here firing inotify inside a container with
`REMOTE_DOCKER_WATCH=partial`, the background session (detached start, version
mismatch, self-reclaim), `start && docker run` and `stop && start && docker
run` with nothing between the commands, `docker build` through the proxy --
asserted to be BuildKit and not the classic builder wearing its name, with
`COPY`, `ADD` and `.dockerignore` checked through file CONTENT -- and the
workspace lifecycle with the docker context appearing and disappearing
alongside it.

Since the root became the Docker CLI (ADR 0024): the binary working under the
name `docker` as a symlink AND as a copy with `remote` still reachable through
it, `remote` being findable in the root's help, `--context <ours>` reaching
that workspace rather than the default, and **a docker context we did not
create being left completely alone** -- which is a promise to other software on
the user's machine and the only one of these that fails silently.

A third suite, `test/vm.sh`, runs the agent ON THE RUNNER with no container
around it (ADR 0025), which is the VM deployment: `WORKSPACE_ENABLE_DIND=false`,
a real unix account provisioned on the runner itself, a session, and a bind
mount resolving through NFS in both daemon modes. The runner is an Ubuntu
machine with docker, so that is exactly what is proven -- not systemd, which
starts nothing here, and not any other distro.

A second suite, `test/per-user-dind.sh`, runs the same workspace with two
enrolled accounts and a daemon each (the default since ADR 0019): that they reach
different daemons, that neither can list or stop the other's containers, that
each account's bind mount resolves (which is the only real proof the reverse
tunnel was bound inside that account's netns), that both publish the same port
at once, that a shell's `DOCKER_HOST` is its own daemon, that neither account
is in the `docker` group, and that restarting the agent adopts the running
daemons with their containers intact.

A suite of its own, `test/two-clients.sh`, runs ONE account from TWO client
machines at the same time (ADR 0029): two state directories with a key each,
both enrolled in one key file. It proves neither is refused its reverse tunnel,
that the workspace recorded a different port for each, that each container reads
ITS OWN machine's file through a bind mount, that both see a container the other
started, and that a collection on one leaves the other's volumes alone and its
mounts working.

A fourth suite, `.github/workflows/machine.yml`, is the only one that runs a
WINDOWS machine end to end. A Linux job exports the workspace image as a rootfs;
a windows-latest job imports it with the real client and proves the thing the
whole backend is for: `docker run --rm -v ${PWD}:/w alpine:3 cat /w/marker`
reading, inside a container in a machine created ninety seconds earlier, a file
the runner wrote on the Windows side. That single command covers the session,
the SSH transport, the NFS export, the bind rewriting and the daemon in the
machine. It also proves create is idempotent and that `remote rm` takes the
distribution with it, which is the failure worth catching: a running Linux
system with nothing naming it.

### NOT tested, and do not claim otherwise

Keep this list honest. An audit found paths described in summaries as tested
that had no coverage at all -- `elevate` most of all, which had been asserted
as "the docker run mechanism under it is tested" when only the pure planning
function was.

- **Swarm itself.** `elevate`'s `docker run` mechanism is tested; the Swarm
  wiring -- templated `{{.Task.Name}}`, `mode: host` publishing, placement --
  needs a real cluster. CI cannot cover it.
- **Hyper-V, entirely.** Implemented and NEVER EXECUTED. GitHub's runners do
  not offer it and nobody working on this has it, so it has no automated
  coverage and cannot get any: `docs/testing-machines.md` is its whole
  verification. Its decisions are unit tested as far as a string can be -- the
  PowerShell it builds, the Ignition document, the state and address parsing,
  the key fingerprint -- and everything past `powershell.exe` is unproven. The
  least certain part, named in the runbook, is whether Flatcar's Hyper-V image
  reads the Ignition config from where `Create` writes it. This is the
  strongest entry on this list: WSL at least runs on a runner.
- **WSL beyond one runner image.** `machine.yml` runs the backend end to end on
  windows-latest, which is real coverage and is one Windows version on one
  image. Nobody working on this has WSL on their own machine.
- **macOS, entirely.** Cross-compiled on every push, executed never -- no test
  of any kind has run on it. The endpoint code and the fswatch backend are
  where it genuinely diverges, and the kqueue backend (one fd per *file*) is
  the larger risk of the two.
- **Windows, beyond the unit tests.** `test (windows)` runs the client and
  shared modules' tests on every pull request, which covers the named-pipe
  endpoint and `processAlive`. What has never run there is the client itself:
  the integration suite needs a Linux kernel's NFS client, so no Windows
  machine has taken a session end to end in CI. Say "unit tested on Windows",
  never "the Windows client is tested".
- **Installing a release.** The pipeline itself now HAS run: `v0.1.0` is tagged
  and published with ten archives, client and agent, for every target the
  matrix builds. What has never happened is somebody downloading one and
  running it — no archive has been unpacked on a machine that did not build it,
  so the thing unproven is the artifact, not the workflow that makes it.
  *(Checked 2026-08-12. Re-check with `gh release view v0.1.0`.)*
- **systemd.** `deploy/remote-dockerd.service` is not exercised by anything.
  `test/vm.sh` starts the agent directly, because what it tests is the agent as
  a guest rather than systemd's ability to run a binary.
- **`coarse` watch mode.** The directory-level poke for deletions is unit
  tested; no integration test asserts that a real watcher notices a deletion
  through it.
- **Watching at scale.** The budget, the exclude list and overflow reporting
  are unit tested against a fake backend. Nothing has run a watcher over a
  10,000-directory tree, and the macOS backend (kqueue, one fd per *file*) has
  never been executed at all.

## Conventions

- Comments explain *why*, not *what*, and are written for somebody reading the
  code to understand it rather than for whoever debugged it. Keep the finding
  and the way it fails silently; drop the transcript, the re-derivation and
  what was tried first. Several findings here cost real debugging (the hijack
  rules, the half-close, the genproto exclusion, the go-nfs refusal panic,
  mount propagation) and must survive the edit that shortens them. The long
  version belongs in an ADR, which the comment then links to.
- No `--` interjections, and few em-dashes. A dash almost always carries a
  clause that wanted its own sentence, and reading around one means holding
  the first half open while the second runs underneath it.
- bash in `test/`. There is no shell left in the image: `image/` is a
  Dockerfile and nothing else.
- **An assertion matches captured output, never `cmd | grep -q`.** `outputs
  <regex> <cmd...>` in `test/lib.sh` is the one way. grep -q exits on the first
  match, the producer's next write gets EPIPE, and Go turns EPIPE on fd 1 or 2
  into a fatal SIGPIPE: `set -o pipefail` then fails the assertion BECAUSE it
  matched, depending only on scheduling. Measured both ways on 2026-08-13: a
  producer still writing when grep exits gives 141 every time, and the real
  `remote ls` gives 0 failures in 5,067 runs under contention, because it
  finishes writing first. So this is a hazard removed, not a bug fixed, and it
  does NOT explain section 17's intermittent failures, which are still
  unexplained. Windows cannot show it at all: no SIGPIPE, the write is ignored.
- CLI output is one line of diagnosis, and where there is a remedy, one
  indented `fix:` line under it. Never a wrapped paragraph: the terminal's
  width is not ours to guess, so the answer is a message short enough not to
  need wrapping. Help text is the exception, since cobra does not reflow it.
- A finding that contradicts an ADR gets the ADR corrected, not ignored.
- **A claim about the outside world carries the date it was checked and the one
  command that re-checks it.** Claims about our own code are covered by tests;
  claims about anything else are covered by nothing and expire silently. Two
  did, on the same day: ADR 0009 said embedding Compose would pin docker/cli
  back a major version, which stopped being true when compose v5 shipped, and
  ADR 0022 said Windows had no standalone docker CLI, which `winget install
  Docker.DockerCLI` disproves. Both were quoted as current fact in the README,
  in `--help`, and in advice to a user. If the check cannot be a command, say
  the claim is a judgement and name who would re-make it.

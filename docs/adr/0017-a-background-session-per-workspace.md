# 0017. A background session per workspace

- Status: Accepted; `up` is superseded by `start --foreground` in
  [ADR 0018](0018-one-way-to-do-each-thing.md)
- Date: 2026-08-08

## Context

Using remote-docker meant remembering `remote-docker up` and keeping a terminal
open for it. The embedded Docker CLI worked around that by opening a session
inside its own process for the duration of one command — which died with the
command, so `docker run -d` left a container whose filesystem stopped working
and a warning saying exactly that.

The two arrangements also conflicted, differently on each platform, for a
reason that turned out to be a bug rather than a design:

**Binding the endpoint was not a lock.** On Unix, `Listen` removed any existing
socket before binding. Removing a stale one is necessary — a process that dies
without cleaning up would otherwise make every later run fail with "address
already in use" — but doing it unconditionally meant a second process silently
unlinked a *running* one's socket and took its place. The first kept accepting
on an inode nobody could reach, and when the second exited the path was bound to
nothing while the first still looked healthy. Nothing reported anything.

On Windows the same case hard-failed, because `winio` takes the pipe with
`FILE_FLAG_FIRST_PIPE_INSTANCE`. That is correct behaviour and it surfaced as
`Access is denied`, so `remote-docker status` could not run while `up` was
running — the one moment anybody would want to.

Compounding both: `status` and `gc` declined the workspace's single
reverse-tunnel port with some care, and then bound the local endpoint anyway,
which neither ever uses.

## Decision

One background session per workspace, which every command talks to.

*(2026-08-11: `up` no longer exists as a command; see ADR 0018. What it named
is still the daemon body, reached as `remote start --foreground`.)*

**`up` is the daemon body.** It keeps its behaviour — foreground, blocking,
reporting — and `start` spawns exactly that, detached, with its output going to
a log. There is no second implementation of a session to keep in step with the
first, and the thing being spawned is precisely the command a person would have
run by hand.

- `start` — ensure one, print the endpoint. Idempotent.
- `stop` — ask it to go.
- `up` — unchanged. What the integration suite and anyone debugging uses.
  (Renamed to `start --foreground` by ADR 0018 and kept as a hidden alias. The
  paragraph above is what survives: one implementation, spawned rather than
  reimplemented. Only its spelling changed.)
- the embedded CLI — starts one rather than opening its own, so a detached
  container keeps its mounts after the command exits.

**Only a session that serves the endpoint binds it.** The default is not to,
so a command added later has to ask.

This began as three independent switches -- serve the endpoint, export files,
report progress -- which were only ever set together, and each of which
prevented a different one of the failures above. They are now one
`session.Role`: `Query` for a command that asks the workspace something,
`Host` for the session that serves it. Two call sites exist and always did.
The reasoning for each refusal is kept on the constants, because the reasoning
is the part worth having; and `Query` not binding the endpoint is now covered
by a unit test that runs on Windows named pipes, which is where the failure was
worst and where CI does not reach.

**The endpoint has one owner.** A `flock` on Unix — chosen over a pid file with
a liveness check because the kernel releases it when the holder dies, including
on `kill -9`, so a stale lock is impossible. Holding it is what earns the right
to clear a stale socket, turning the theft back into the recovery it was meant
to be. On Windows the pipe bind already excludes; the lock file carries the pid
so a refusal can name the process instead of saying `Access is denied`.

**The control channel rides the Docker endpoint**, under `/_remote-docker/`,
intercepted where `/containers/create` already is. One endpoint means one lock,
one ACL and one thing to find — and that ACL already says who may drive the
session, because anyone who can reach it can already start containers that read
and write this machine's filesystem. No version of the Engine API has served
anything under a leading underscore, so the prefix cannot collide.

**Lifetime is gated on the same predicate as a connection release**, and for a
stronger reason: a released connection reopens on the next request, while an
ended process takes the NFS export with it and a running container's filesystem
with that. A session exits when it has been idle for `REMOTE_DOCKER_DAEMON_IDLE`
(default 30 minutes, negative never) **and** nothing depends on it — with
"cannot tell" counting as a reason to stay, exactly as it does for a release.

## Consequences

- **No terminal to keep open**, and `docker run -d` through the embedded CLI now
  behaves like `docker run -d` anywhere else. The warning about containers
  losing their mounts is deleted, because there is nothing left to warn about.
- **A stale daemon shadows a new binary.** An old background session keeps
  serving the endpoint, so a freshly built client talks to it and appears not to
  have changed. This cost real debugging time during development — a `stop`
  failing with Docker's own `page not found`, because the request was being
  forwarded by a daemon that predated the control channel. Idle-exit limits how
  long it can happen; `stop` is the cure.
- **`status` reports without connecting.** `status` the *command* still connects
  deliberately — reporting what the workspace says is its whole job — but a
  daemon asked to describe itself must not establish a connection it had let go,
  or asking the question would change the answer.
- **Shutdown is acknowledged before it acts**, because it closes the very
  connection carrying the request. Replying afterwards would be replying into a
  socket just closed, and the caller would see an unexplained EOF instead of an
  answer. `GET` is refused so a stray probe cannot stop a session.
- Detaching is platform-split for one reason in two dialects: the child must
  survive the terminal that started it. `DETACHED_PROCESS` plus
  `CREATE_NEW_PROCESS_GROUP` on Windows, `Setsid` on Unix. Without it, starting
  a session and then pressing Ctrl-C on the next command takes it down.
- ADR 0015 stands and matters more, not less. A session that lives for days
  should not hold an SSH connection, a keepalive and an events stream for all of
  them.

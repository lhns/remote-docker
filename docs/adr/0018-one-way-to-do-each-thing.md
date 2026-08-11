# 0018. One way to do each thing

- Status: Accepted
- Date: 2026-08-08
- Supersedes part of [ADR 0006](0006-per-bind-nfs-volumes.md) and
  [ADR 0017](0017-a-background-session-per-workspace.md)
- Corrects [ADR 0005](0005-docker-api-proxy-over-cli-wrapper.md) and
  [ADR 0010](0010-go-ssh-server-agent.md)

## Context

The CLI grew a second way to do several things and never lost the first. None
of the duplicates arrived by design; each was the residue of a change that
added the better spelling without removing the worse one.

- **`up` and `start` both brought a session up.** ADR 0017 made `start` spawn a
  detached session, and what it spawned was `up`. So `start` already gave you
  the endpoint and `up` added nothing a user needed — but both were in the help,
  which asked people to choose between two spellings of one idea.
- **`workspace` and `context` overlapped.** `workspace add` already created the
  docker context; `context install` created it again. Nothing on screen said
  which to reach for, and the difference between a workspace and a context is
  not something a user should have to work out.
- **`shell` cost more than it looked.** It opened a shell in the workspace
  container — Alpine, as your account, landing in a `~/workspace` NFS mount of
  your files. It was the *only* user of that mount, and the mount was not free:
  a package to make and recover it, a stale-mount path, a release hook on
  forward teardown, an extra root in the notify replayer, and a whole
  integration test whose subject it was.

## Decision

Collapse each to one.

*(2026-08-11: `up` is deleted. ADR 0024 moved every command of ours under
`remote`, so the scripts this alias was kept for are broken whatever it does,
and `remote up` is a spelling nobody has ever had. `add` and `list` remain,
still exercised. The consequence below about hidden aliases being a
maintenance claim is the reason this was easy to retire, not a reason it should
not have existed.)*

**`up` becomes `start --foreground`.** The body is the same code, so there is
no second behaviour to keep in step, and running a session in a terminal shows
exactly what the background one does. `up` survives as a hidden alias: it is in
shell history and possibly in somebody's unit file, and a command that still
works costs one line while a command that stopped existing costs somebody an
afternoon.

**`context` folds into `workspace`, which takes docker's verbs** — `create`,
`ls`, `use`, `rm`, `inspect`, with `add`/`list`/`default`/`remove` kept as
aliases. Docker contexts become purely a side effect: `create` writes one,
`use` selects it, `rm` removes it, and re-running `create` is the repair path
that `context install` used to be.

**Borrowing a verb means inheriting what it does.** This was written as though
`use` were ours alone, and it set only the default in `~/.remote-docker.json`,
which nothing but this binary reads. `docker context use` writes
`currentContext` in `~/.docker/config.json`, and that is what compose, buildx,
Testcontainers and every IDE plugin resolve. So `workspace use dev` announced a
default and left every other tool on the machine talking to whatever was
selected before, usually a Docker Desktop pipe that is not there. A command
named after docker's verb has to do docker's half too.

The verbs are docker's; the noun stays ours. A workspace IS the thing a docker
context points at, so borrowing the vocabulary costs nothing and saves
explaining — but the config file's key is `workspaces`, the wire protocol is
`workspace-info`, the server's variables are `WORKSPACE_*`, the image is
`remote-docker-workspace`, and every ADR here says workspace. A CLI that
disagreed with all of them would trade one confusion for another.

`inspect` is new and earns its place. The pieces were scattered across four
derivations: the config file holds the host and account, the endpoint is
derived from the name, the docker context is named after it too, and a session
may or may not be running against it. Answering "what is this workspace,
actually" meant knowing all four.

**`shell` and the `~/workspace` mount go.** What `shell` offered was a shell on
a machine that already answers SSH. `ssh you@workspace` gets the same shell,
from a client every one of these machines already has, and does not require a
mount to exist for it to land in.

## What stays, and this is the part to read before deleting anything

**`serveExec` and `servePTY` stay.** Deleting the client's `shell` does not
make them dead code, and the reasoning that would conclude otherwise is exactly
the reasoning to distrust here.

They are the default arm of the agent's session dispatch, so anyone with an
enrolled key still gets a shell from a stock `ssh` — and `internal/server/sshd/server.go`
leans on precisely that. Its justification for allowing unrestricted local
forwarding is that everything reachable that way "is inside the workspace,
which the account can already reach with a shell". Remove the PTY and that
sentence stops being true, which makes a security argument false as a side
effect of a cleanup.

`test/integration.sh` section 13b is their only coverage, and it deliberately
uses a stock `ssh -tt` rather than anything of ours. It was narrowed rather
than deleted for this reason: it asserted the mount, and the obvious move when
the mount went was to delete the section. That would have left ADR 0010's
central claim — one binary replacing sshd — with no coverage at all, and
nothing on screen saying so.

**`workspace.Info.Mountpoint` and `.Mounted` stay in the wire contract**,
unset. They have no consumer in the client. Removing a field from a format both
binaries parse is a change worth making on purpose, with both sides of a
version skew thought about, rather than as a side effect of deleting a command.

**`Replayer.roots` keeps returning a slice** for what is now always one path.
The reason it was ever plural is a real finding — separate mounts of one export
do not share an inode the way dockerd's bind mount does, so each needs its own
poke — and the shape is what would make a second mount cheap to add rather than
a rewrite.

## Consequences

- **Two commands and one whole package leave the tree**, along with a mount
  helper, a stale-mount recovery path, a teardown hook, a replayer field and a
  test image. ADR 0006's `rslave` finding is NOT among the things deleted: the
  mount it justified is gone, but "a replacement mount is invisible to a
  running container, silently, as an empty directory rather than an error" was
  found by experiment and costs real debugging to rediscover. That ADR says so
  itself and is left standing.
- **Anyone who used `remote-docker shell` has to type `ssh` instead.** That is
  a real regression for them and it is accepted: the alternative is carrying a
  mount, a package and a recovery path so that one command is four characters
  shorter than the one every machine already has.
- **`workspace use` now changes a machine-wide setting.** `currentContext` is
  one value per user, shared by every docker tool, so selecting a workspace
  redirects all of them. That is what the command is for and it is said on
  screen, but it is wider than writing our own config file, and it is the
  reason `use` is the only verb here that reaches outside our own state.
- **`golang.org/x/term` falls to an indirect dependency**, which is a small
  sign the deletion was real rather than cosmetic.
- **Hidden aliases are a maintenance claim, not free.** `up`, `add`, `list`,
  `default` and `remove` all still work, so all of them can still break.
  Section 16 exercises `up` and section 17 exercises `list` deliberately for
  that reason — an alias nothing exercises is an alias nobody notices breaking.
- **The help is now a list of things that each do one job.** That is the whole
  return on this, and it is worth being honest that it is a usability return
  and not a technical one.

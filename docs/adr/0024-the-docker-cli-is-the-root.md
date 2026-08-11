# 0024. The Docker CLI is the root

- Status: Accepted; supersedes [ADR 0022](0022-answering-to-the-name-docker.md)
- Date: 2026-08-11

## Context

ADR 0009 embedded the Docker CLI so that nothing would have to be installed on
the machine using this. ADR 0022 then noticed that `remote-docker docker ps` is
the right thing under the wrong name, and made the binary answer to `docker` as
well, by dispatching on `os.Args[0]` and by shipping `shim install` to create
that name on PATH.

That worked, and it cost 550 lines: a symlink-then-hardlink-then-copy ladder, a
marker file so the shim could be recognised without executing it, registry PATH
editing on Windows with `setx` forbidden, an upgrade-staleness state to report,
and a second identity for the process to reason about. All of it in service of
one thing — that the user gets to type `docker`.

There was a simpler answer the whole time, and it was hiding behind the
assumption that our own commands owned the top level.

## Decision

**The root command is the Docker CLI.** `remote-docker run`, `remote-docker ps`
and `remote-docker compose up` are the real commands with their real flags.
Renaming the file to `docker` is then a complete installation with **no code
behind it at all**: there is no name to dispatch on, because there is no
second shape to dispatch to.

Everything of ours is under **`remote`**, and nothing of ours is anywhere else:

```
docker remote ls | create | rm | use | inspect      the workspaces
docker remote status | start | stop | restart       this machine's session
docker remote enroll | gc | version
```

`workspace` disappears as a level. A remote *is* the workspace, so `remote ls`
lists them; the verbs stay docker's own, as they already were.

**Our flags move off the root onto `remote`.** This is the part that is load
bearing rather than tidy. `--host` and `--user` are docker's own root flags,
and they coexisted with ours only because pflag silently skips a duplicate long
name; a clashing *shorthand* panics the subtree outright, which is why ADR 0022
had to forbid shorthands everywhere. Off the root, the whole hazard is gone.

**Which workspace a docker command talks to is the docker context**, which is
docker's own mechanism and one we already write a context for. `remote use`
selects it. That is the one integration point, and it is not an invention: a
workspace has been a context since ADR 0018.

Making that true meant giving up the shortcut it replaced. Docker resolves a
target in a fixed order:

```
--host / -H  >  DOCKER_HOST  >  --context / DOCKER_CONTEXT  >  current context
```

Setting `DOCKER_HOST` whenever it was empty, which is what we did, put us at
the top of that list and overrode everything below it. Two things were wrong
and only one of them was about us:

- `docker --context ci ps` reached the **default** workspace, not `ci`;
- `docker --context desktop ps`, a context we never created, reached **us**
  instead of Docker Desktop.

The second is the one that matters. A machine may have real docker contexts,
and a tool that quietly redirects them is worse than one that needs a prefix.

So the decision is now four outcomes, in `target.go`:

| the invocation says | what we do |
|---|---|
| `--host`/`-H`, or `DOCKER_HOST` naming something not ours | nothing |
| a context that is ours | a session for **that** workspace, and `DOCKER_HOST` left unset so docker reads the endpoint off the context |
| a context that is not ours | nothing |
| no context and no host | the default workspace, session ensured, `DOCKER_HOST` set |

It is a pure function of its inputs, because it runs before cobra has parsed
anything and the case it exists for is the one where the correct behaviour is
to do nothing, which cannot be observed from outside the process.

We also call docker's own `cli.SetupRootCommand`, which was hand-rolled before.
Its help layout — Common, Management and Commands — is what makes sixty
subcommands readable, and `remote` lands among the management commands where it
belongs.

## Consequences

- **The shim is deleted**, with `pathlist.go`, the Windows registry editing,
  the marker file, the copy ladder, and the `os.Args[0]` dispatch. The
  invariants that guarded them go too, including "never `setx`" — correct while
  it lasted, and about code that no longer exists.
- **`remote-docker docker ps` stops working.** No tag has ever been pushed, so
  there are no released users; the 13 references in tests and docs were
  updated, and a stray `docker` verb now gets the ordinary unknown-command
  error naming it.
- **We are in docker's namespace now, and it is not ours.** A future docker
  release adding a `remote` command would collide, and the answer would be to
  rename ours. That is the price of the shape and it is worth saying out loud
  rather than discovering it.
- **An argv scan has to know docker's root flags.** `invokingDocker` reads argv
  before cobra parses, and a scan that took the first non-flag word as the
  subcommand read `docker --context remote ps` as our namespace and ran the
  command with no session. A test caught it; `valuedRootFlags` is the fix.
- **ADR 0022's `os.Args[0]` rule ends here**, and it was right for its whole
  life. ADR 0023 records that it was the only identity surviving Termux's
  loader, which remains a true and useful finding about that platform even
  though the rule it described is gone.
- **A context we did not create is never touched.** That is a new promise, and
  the integration suite makes it: a context pointing at a dead address must
  FAIL rather than succeed against our daemon.
- **`--help` is docker's now.** The program's own description has to fit in
  docker's Long, which is a smaller space than a dedicated root gave it, and
  discovering `remote` depends on it being visible in that list.

# 0020. One daemon target, not a mode branch

- Status: Accepted
- Date: 2026-08-10

## Context

ADR 0019 gave each account its own dockerd. It arrived as a nullable field on
an existing configuration -- `Daemons *daemons.Manager`, nil meaning ADR 0012's
shared daemon -- so every place that needed a daemon grew a branch:

```go
socket := s.cfg.DockerSocket
if s.cfg.Daemons != nil {
    d, err := s.cfg.Daemons.Ensure(ctx, account.Name())
    ...
    socket = d.Socket
}
```

There were **nine** of them, across `sshd`: the Docker socket, the notify
replayer's volume host and filesystem root, a shell's `DOCKER_HOST`, the
version and storage-driver queries in the info reply, both forward paths, the
warm-up at authentication, and the mode string. Each one answered the same
question -- *which daemon serves this account, and where are its socket, its
namespace and its filesystem* -- and each answered it separately.

Two things made that worse than ordinary duplication.

**The failure mode is success.** The project's own invariants already say it:
"getting it wrong does not fail -- it succeeds, against the wrong daemon." A
missed branch does not raise an error, log anything, or fail a health check. It
runs somebody else's containers, or binds an unauthenticated NFS export into
another account's network namespace.

**It was untestable.** The call sites took a concrete `*daemons.Manager`, which
needs a real Docker daemon to do anything at all. So the one path where a
mistake is invisible had no unit test, and the only evidence it worked was an
integration suite that runs on a Linux runner.

## Decision

One resolver, asked everywhere, chosen once.

```go
type Target struct {
    Socket    string // splice a dial-stdio session here
    Host      string // DOCKER_HOST; "" means the default socket
    NetNSPath string // "" means this process's namespace
    Root      string // the daemon's filesystem as we see it; "/" when it is ours
}

type Targets interface {
    Ensure(ctx, account) (Target, error) // waits, starting it if needed
    Lookup(ctx, account) (Target, bool)  // never waits
    Warm(account)
    Mode() string
}
```

`*daemons.Manager` implements it for ADR 0019. `daemons.Shared(socket)`
implements it for ADR 0012 -- **an implementation, not a nil check**. The mode
is selected in `cmd/remote-dockerd/serve.go`, where it is read from the
environment, and nowhere else in the agent branches on it.

The empty string carries the "no redirection needed" cases deliberately:
`netns.Do("")` runs in this namespace rather than entering one, and an empty
`Host` means a login shell gets no `DOCKER_HOST` rather than a redundant one.
That is what lets both modes share a single code path instead of two that must
be kept in step.

## Consequences

- **Routing is unit tested**, for the first time, on a machine with no Docker.
  `internal/server/sshd/routing_test.go` asserts that each session resolves its
  own account, that two accounts never share a target, that the shared daemon
  answers identically for everybody, and that the info queries use `Lookup` and
  never start anything. The tests were verified by breaking the resolver on
  purpose and watching them fail.
- **`Lookup` versus `Ensure` is now a contract rather than a habit.** The
  distinction is load-bearing: `workspace-info` is the client's first round
  trip and must not wait for a cold dind. Both are on the interface, and the
  info path's use of `Lookup` has a test that fails if it changes.
- **`sshd.Config` lost two fields.** `DockerSocket` and `Volumes` were only
  ever the shared mode's half of the branch.
- **`notify.DockerVolumes.Host` became lazy**, matching `Root`. Resolving it
  eagerly would have started the account's daemon when the notify session
  opens rather than when it first replays something -- turning connect into a
  wait for a boot.
- **A zero `sshd.Config` no longer means "shared".** `New` defaults it, which
  is tested; a `Server` built by hand with no resolver would panic on its first
  session rather than quietly serving the wrong daemon. Loud beats plausible.
- The cost is one interface between the agent and its daemons where there used
  to be a direct type. That is the price of being able to test the thing that
  fails by succeeding, and it is worth it.

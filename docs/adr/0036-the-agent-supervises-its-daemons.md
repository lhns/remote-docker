# 0036 — The agent supervises the per-account daemons

- Status: Accepted; extends [ADR 0019](0019-a-dockerd-per-account.md)
- Date: 2026-08-15

> A daemon starts when its account connects, and nothing else starts it. The
> restart policy is given up, because a lifecycle the agent does not own is one
> it cannot repair.

## What forced it

A workspace in per-account mode refused every session for minutes:

```
reserving 127.0.0.1:30000 on the workspace: ... tcpip-forward request denied
by peer (another session for this account may still be open)
```

No session held that port. The account's daemon was crash-looping: `exitCode=1`,
four restarts in eighty seconds, each one logged by the parent dockerd and by
nothing of ours. In per-account mode the reverse forward is bound inside that
daemon's network namespace, so a daemon that will not start denies every
forward, and the client reported the one cause it can never check.

It stayed broken, and that is the part worth recording. `reconcile` rebuilds a
daemon whose settings have drifted, but only once it can prove nothing is
running inside, and it proved that by asking the daemon. A crash-looping
container never answers, "cannot tell" counted as busy, and the daemon that most
needed rebuilding was the one that rule protected. It logged *"has containers
running"* about a container that was restarting every nineteen seconds.

Underneath both: the daemon carried `--restart unless-stopped`, so the parent
dockerd was a second supervisor running beside the agent, with no backoff and
nothing in this project's log. In shared mode (ADR 0012) there is no manager at
all, so those containers kept restarting with nobody watching them.

## The decisions

**No restart policy on a per-account daemon.** `Ensure` already starts a stopped
daemon when its account connects, which makes the parent's policy a duplicate
supervisor rather than a safety net. Removing it makes the agent the only thing
that starts a daemon, which is what lets it count failures, log them, report
them and rebuild the container.

**A daemon that is not running is not busy.** `idle` asks the parent daemon for
the container's state before asking the container what it holds. Exited,
restarting, created or dead means nothing is running inside, by definition. The
old rule survives for the case it was written for: a daemon that IS running and
cannot be asked still counts as busy, because the cost of being wrong there is
an account's containers.

**Shared mode stops what it does not serve.** Every account is routed to the one
socket, so a per-account daemon left over from the other mode answers nobody.
They are stopped, never removed: the container is somebody's daemon and the
volume behind it holds their images and containers, and both come back when the
workspace is put back into per-account mode. Filtered by this workspace's id, so
a parent shared with another workspace is untouched, and refused outright when
there is no id to filter by.

## Consequences

- **An account's detached containers no longer survive a workspace restart on
  their own.** They come back when that account next connects, because that is
  when its daemon starts. This is the property the restart policy was bought for
  and it is deliberately given up. It is smaller than it sounds: anything using
  a bind mount cannot run without its client session anyway, since the files are
  served from that machine.
- **The first thing an account does after a workspace restart pays the boot.**
  A shell sets `DOCKER_HOST` from `Ensure`, so `ssh workspace` waits for a cold
  dind rather than finding one the parent had already restarted. Bounded by
  `DefaultReadyTimeout`, which is 90 seconds on fuse-overlayfs. Found by CI:
  `per-user-dind.sh` probed a shell for the account that had not reconnected and
  timed out at 60 seconds waiting for a prompt.
- **A broken daemon is repairable now, and was not.** Whatever made it fail, the
  next reconcile replaces the container and keeps the graph volume, which is
  where the account's images and containers live.
- **Nothing here explains why a daemon exits 1.** It makes the failure visible,
  attributable and repairable. The cause of the incident that produced this
  record was never established, and `lastWords` carrying the daemon's own log
  tail into the error is what would have.
- **`runner.go` has tests for the first time.** It shelled out to `docker` where
  it stood, so no rule in it could be exercised without a real daemon; the
  supervision rule now has five, including the one that was wrong.

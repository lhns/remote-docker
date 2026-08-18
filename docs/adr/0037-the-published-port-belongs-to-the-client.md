# 0037 — The published port belongs to the client

- Status: Accepted; extends [ADR 0008](0008-automatic-port-forwarding.md)
- Date: 2026-08-18

> The workspace daemon publishes wherever it likes, and the client opens the
> number the user typed. Two people can then ask for 8080 at the same time.

## What forced it

With one daemon for everybody (ADR 0012) a published port is a workspace-wide
resource. Two people running `-p 8080:80` collide, and the second is refused by
the daemon:

```
Bind for 0.0.0.0:8080 failed: port is already allocated
```

The answers were all bad. Per-account daemons (ADR 0019) avoid it, but the
shared daemon is a supported configuration and a single-account workspace has
good reason to use it. Publishing on a random port works and leaves the user
looking up a different number after every run. A convention about who owns
which numbers is a rota, enforced by nobody.

Asked from the other end: **who actually needs the number to be 8080?** Not the
workspace. Since ADR 0008 the client opens a local listener in front of each
published port, and that listener is what the user's browser, their tests and
their tools connect to. The number that matters is on their machine.

## The decision

**`HostPort` is emptied on the way through, and the client records what it
was.** `docker run -p 8080:80` reaches the daemon asking for any free port; the
client stamps `80/tcp=8080` into a label and opens `127.0.0.1:8080` in front of
whatever came back. The user sees no difference. Two people asking for 8080 no
longer meet, because neither of them binds it on the workspace.

**A label, not memory.** The ports manager rebuilds its forwards from the
daemon's container list, so after a client restart or a reconnect nothing else
knows what was asked for. The label is keyed by container port, which is the
half that does not change: the published port is chosen by the daemon and is
exactly the thing being looked up.

**Three cases keep their port**, and each is a case where remapping would be
worse than the collision:

- an empty `HostPort`, where the user already asked for any port;
- several bindings for one container port (`-p 8080:80 -p 9090:80`), because
  the daemon reports the assigned ports in no defined order and they cannot be
  paired back to what was asked for;
- UDP, because the tunnel carries TCP. A moved UDP port would be neither
  reachable nor predictable, so it stays where it was and still collides.

**The requested number is honoured only on the machine that asked.** An
account's machines share the daemon and each client forwards the whole
account's containers, which is what lets somebody start a container on the pc
and reach it from the phone ([ADR 0029](0029-one-account-many-machines.md)).
The number in the label is a fact about the machine that typed it, so anywhere
else the container is forwarded at the port the daemon published, exactly as it
was before this record. Two machines of one account can then both ask for 8080
and both get it, each seeing the other's container wherever the workspace put it.

Without that rule the two contend for one local listener and the winner is
whichever the reconciliation reached first, which is a map iteration and
therefore a coin toss that can land differently on the next container event.

**The refusal moves to the client.** The scarce resource is now a port on the
user's own machine, so that is where a clash is reported, in the daemon's own
words (`port is already allocated`) because that is the failure being replaced
and what tooling matches on. It is asked twice: of the session's own forwards,
and of the machine, by opening and closing a listener.

**Both modes, not only the shared one.** The client would otherwise behave
differently depending on a workspace setting it does not control, and every
explanation of a port number would have to start by asking which mode this is.

## Consequences

- **On the workspace, a published port is not the number you chose.**
  `docker ps`, `docker port` and `compose ps` report what the daemon assigned.
  Anything reaching a service at `workspace-host:8080` from outside the tunnel
  has to find the assigned port instead. Inside the workspace nothing changes:
  containers reach each other by container port on a shared network, which is
  what they already did.
- **One user with two stacks that both want 8080 still gets an error**, from
  the client rather than the daemon, and the second container is not created.
- **A container this client did not create is forwarded as before**, at the
  number the daemon published. It carries no label, and that is the fallback.
- **Nothing about the trust assumption in ADR 0012 changes.** This removes an
  everyday collision between people who already trust each other; it is not
  isolation and does not pretend to be.
- **The port was one collision of several, and the others remain.** Container
  names, compose project names and networks are still one namespace per daemon,
  so the same compose file from two machines of one account still collides in
  every way except the port.
  [ADR 0029](0029-one-account-many-machines.md) records that as a requirement
  rather than a quirk; nothing here addresses it.


# 0038 — UDP does not cross the tunnel, and should

- Status: Accepted, and states an intention that is NOT implemented; extends
  [ADR 0008](0008-automatic-port-forwarding.md) and
  [ADR 0037](0037-the-published-port-belongs-to-the-client.md)
- Date: 2026-08-19

> A published UDP port has never been reachable from the client, in any version
> of this project. It is remapped like a TCP one so it stops colliding, and it
> is still not forwarded. That is a gap, not a decision about what users need.

## What is true today

`-p 53:53/udp` publishes on the workspace and nothing carries it back. The
tunnel is one SSH connection, and SSH forwards TCP: `direct-tcpip` for a
connection out and `forwarded-tcpip` for a listener. There is no datagram
channel in the protocol and none in this codebase, so the ports manager filters
published ports to TCP before it opens anything (`publishedTCP`).

Nobody has reported it, which says more about what people run in a dev workspace
than about whether it should work.

## The decision

**Remap UDP anyway**, as ADR 0037 does for TCP: the daemon assigns the published
port and the requested number goes in the label. Two accounts publishing 53/udp
then stop colliding on the workspace, which is the one thing that can be fixed
without new protocol.

Nothing is lost by moving it, because nothing could reach it from here in the
first place. What changes is on the workspace, where the number is now the
daemon's choice: anything reaching a published UDP port from the workspace's own
network has to look it up rather than assume.

**The requested number is recorded even though nothing reads it.** It costs a
few bytes in a label, it makes `docker inspect` explain itself, and it is
exactly the data the forwarding below would need.

**Forwarding is wanted and not built.** Stated here so that it is a known gap
with a shape rather than an omission somebody rediscovers:

- a channel type of our own, since SSH has none to borrow, carrying
  length-prefixed datagrams both ways;
- the framing in `core/tunnel`, which is where the two ends agree on what they
  speak (ADR 0030);
- the agent opening a UDP socket inside the account's network namespace, under
  the policy `AllowDial` already applies to a local forward: loopback only, and
  not a port another account holds;
- the client listening on the requested number and holding one channel per
  source address, with an idle timeout, because a datagram flow has no close;
- an integration test with a UDP echo server, since none of this can be believed
  from a unit test.

## Consequences

- **Two people can publish the same UDP port** and neither is refused. Neither
  can reach it through the tunnel, which was already the case.
- **A UDP port is unpredictable on the workspace now.** If something there
  depended on a fixed published UDP port, it needs the assigned one.
- **The cost of the forwarding above is not only code.** Datagrams inside a TCP
  stream inherit head-of-line blocking, so a delayed datagram delays the ones
  behind it. For DNS, syslog and metrics that is unremarkable; for anything
  latency-shaped it is a different service than the one the user thinks they
  have, and that has to be said where they will read it rather than discovered.
- **An older agent will not know the channel type**, so the client has to
  degrade to today's behaviour rather than failing the session. Whatever gets
  built starts there.

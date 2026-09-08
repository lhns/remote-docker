# 0038 — UDP crosses the tunnel

- Status: Accepted and implemented 2026-08-19; amended 2026-09-08 with a flow
  timeout; extends [ADR 0008](0008-published-ports-reach-the-client.md)
- Date: 2026-08-19
- Current answer: a channel of our own
  (`direct-udp@remote-docker.lhns.de`) with a two-byte length in front of each
  datagram, one flow per source address, an agent that refuses the channel type
  as the version check, and a flow closed after 2 minutes carrying nothing.

> SSH forwards TCP, so a published UDP port was unreachable in every version of
> this project. It is carried now, in a channel of our own with a length in
> front of each datagram, because a stream is all SSH offers and a boundary is
> all a datagram needs.

## What forced it

`-p 53:53/udp` published on the workspace and nothing carried it back. The
tunnel is one SSH connection, and SSH forwards TCP: `direct-tcpip` for a
connection out and `forwarded-tcpip` for a listener. There is no datagram
channel in the protocol and none in this codebase, so the ports manager filters
published ports to TCP before it opens anything (`publishedTCP`).

## The decision

**Remap UDP anyway**, as ADR 0008 does for TCP: the daemon assigns the published
port and the requested number goes in the label. Two accounts publishing 53/udp
then stop colliding on the workspace, which is the one thing that can be fixed
without new protocol.

Nothing is lost by moving it, because nothing could reach it from here in the
first place. What changes is on the workspace, where the number is now the
daemon's choice: anything reaching a published UDP port from the workspace's own
network has to look it up rather than assume.

**The requested number goes in the label**, which is what the forwarding below
reads and what makes `docker inspect` explain itself.

**And forward it**, since SSH has no datagram channel to borrow. How, in order:

**Not L2TP and not a tun device**, which were the first suggestions and are both
networks where the need is datagrams for a handful of ports. They want kernel
devices and privileges at both ends, `ssh -w` needs root on both sides, and a
Windows client cannot join at all. What SSH is missing here is not transport,
it is framing.

**A channel type of our own**, `direct-udp@remote-docker.lhns.de`, in the
namespace SSH keeps for extensions, carrying `direct-tcpip`'s payload because
the question is identical: which address and port inside the workspace. A
workspace too old to know it REJECTS the channel, and that refusal is the whole
version check: no handshake, no capability exchange, no flag. The client opens
no listener and the session is otherwise exactly as it was.

**A two-byte length in front of each datagram** (`core/tunnel`, where both ends
agree on what they speak). A channel is a byte stream: it preserves order and
content and says nothing about where one write ended. Without the length, "abc"
and "de" arrive as "abcde" and the receiver cannot tell. A truncated stream is
an error rather than an EOF, because half a datagram delivered as a whole one is
the failure this framing exists to prevent.

**The agent side is almost nothing**, which is the sign the design fits: the
same `AllowDial` policy answers both protocols, and `netns.Dial(path, "udp",
addr)` already existed. A connected UDP socket is a `net.Conn` whose reads and
writes are whole datagrams, so only the channel needed framing.

**One flow per source address.** The workspace socket is connected to the
container port, so what comes back on it belongs to exactly one local sender.
Anything else would need the source in every datagram and a demultiplexer at
each end.

**A flow ends with the forward, with its channel, or after 2 minutes idle**
(amended 2026-09-08; `client/internal/session/udpforward.go`). The first two
are the rule a TCP forward already follows. The third is what UDP needs and TCP
does not:

| | TCP forward | UDP forward |
| --- | --- | --- |
| what ends a flow | FIN or RST on the connection | nothing the protocol carries |
| flows per exchange | one per connection | one per SOURCE PORT, and a resolver picks a new one per query |

- **2 minutes**, on a per-flow `lastUsed` stamped in both directions. The
  workloads carried here are request/response shaped (resolver, syslog,
  metrics), so a two-minute gap means the exchange is over.
- **Swept every 30s**, the cadence `shareReconcileInterval` and the port
  manager already use.
- **Not a bound**, which the consequence below already ruled out: a cap evicts
  a flow somebody is using, a timeout evicts one nobody is.
- **A flow is dropped by IDENTITY, not by key.** A sweep may have expired a
  source port and the sender may have opened another under the same address,
  so `drop` refuses a flow the map no longer holds.

**One path through the client, not two.** `Forwarder.Forward` takes the network,
so the ports manager has no UDP code at all: the listener, the flows and the
framing live behind that one method.

**An integration test with a UDP echo server** (`test/probes/udpecho`), since
none of this can be believed from a unit test.

## Consequences

- **Two people can publish the same UDP port** and neither is refused.
- **A UDP port is unpredictable on the workspace now.** If something there
  depended on a fixed published UDP port, it needs the assigned one.
- **An older agent does not know the channel type** and rejects it. The client
  opens no listener and the session is otherwise unchanged, which is the whole
  version check.
- **Datagrams inherit head-of-line blocking** from the TCP stream carrying
  them, so a delayed one delays those behind it. For DNS, syslog and metrics
  that is unremarkable; for anything latency-shaped it is a different service
  than the user thinks they have, which is why the README says so rather than
  only this record.
- **A sender whose source port changes per datagram costs a flow per datagram
  for 2 minutes, not forever.** A resolver does exactly that. It was the
  revisit trigger this record named, and the timeout is the answer.
- **A flow that is quiet longer than 2 minutes and then speaks pays one channel
  open.** A datagram is not lost by it: the flow is opened by the datagram that
  finds none.
- **Nothing carries datagrams the other way.** A container reaching a UDP
  service on the user's machine is a reverse forward, which nothing has asked
  for.
- **Broadcast and multicast are not carried**, and cannot be by a connected
  socket.

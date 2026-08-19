# 0003. The client serves, the workspace mounts

- Status: Accepted
- Date: 2026-08-07 (retrospective; decided during the original build)

## Context

Two topologies can put the client's files in front of the remote daemon.

**The client serves and the workspace mounts.** Nothing needs installing on the
client beyond a portable binary, and the workspace never initiates a connection
back.

**The workspace serves and the client mounts** — a Samba container and a mapped
drive letter. Genuinely good: no inbound connection to the client machine at
all, and containers get local-disk performance. But it inverts where the files
live, which was not what was wanted.

Given the first topology, the file server has to be reachable from inside the
workspace container, which sits behind whatever network the deployment happens
to have.

## Decision

The **client serves** its files, and the workspace reaches them through a
**reverse SSH tunnel** (`-R`) over the connection the client already opened.

The tunnel port is **derived from the account's uid**, not allocated:

```
port = PORT_BASE + (uid - UID_BASE)
```

## Consequences

- The workspace never connects out to the client. No firewall rule, no inbound
  exposure, and the file server binds loopback only.
- No coordination between users and no collisions, because the mapping is a
  pure function rather than an allocation.

  **Amended by ADR 0029.** One port per uid is one port per PERSON, and a
  person has more than one computer. The uid still decides an account's first
  port, so nothing renumbers and a workspace reached from one machine still
  allocates nothing; further machines of that account are allocated a port each
  by the agent, which is coordination and is the cost of the second machine
  working at all.
- **The port is stable**, which turned out to matter more than the collision
  property. A dropped tunnel reconnects to the same endpoint, so the existing
  NFS mount keeps working and no remount is needed. See ADR 0006 for why
  avoiding a remount was worth designing around.

  Still true under ADR 0029, and re-based: stable per CLIENT rather than per
  uid. The allocation is remembered in `clientports` beside `uidmap`, so a
  machine reconnecting is offered the port it had and the volumes it created go
  on mounting. This is the property the allocation had to preserve, and the one
  that would have made a per-session port wrong.
- The formula must be identical on both sides. Originally it lived in two shell
  scripts, and when those disagreed the client tunnelled to one port while the
  mount read another — a failure that presents as a network fault. ADR 0021
  removes the duplication.
- A shared loopback interface inside the workspace means port ownership is not
  self-enforcing: any account that can request a reverse forward can try to
  bind another account's port and serve them a hostile filesystem. ADR 0010
  makes ownership structural.

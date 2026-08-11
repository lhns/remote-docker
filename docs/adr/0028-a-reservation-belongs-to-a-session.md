# 0028: A port reservation belongs to a session, not to an account

Accepted, 2026-08-11.

## Context

The agent keeps one map of which loopback ports inside the workspace are
spoken for. Two rules read it: `Allow`, which decides whether a session may bind
a reverse forward, and `AllowDial`, which decides whether an account may open a
local forward *to* a port. The second is the one that matters, because on a
shared daemon (ADR 0012) every account's forwards live in one network
namespace, so another account's reverse-tunnel port is dialable from there and
what answers is an NFS export with `AuthFlavorNull`: read and write over
somebody's files. `docs/threat-model.md` flow 4 names that map as the control.

The map was keyed `port -> account name`, and release was "delete if the entry
names me". That reads as ownership and is not: an account is a person, and a
person has more than one computer.

Opening the client on a second machine was enough to break it. The second
session's bind fails with EADDRINUSE, because the first machine's listener
genuinely holds the port. Its failure path then releases — and "release the
entry naming `lhns`" deleted the entry the **first** machine had put there,
while that machine's listener was still serving. `Holder` then reported the port
free, and any other account could dial straight into an export that
authenticates nobody. The control was gone while the thing it protects was
still running.

A second instance of the same mistake was reachable without any of that: each
session arms a release for when its connection ends, so a session ending late
deleted whatever entry existed by then, including one a later session had
taken.

Neither is a race. Both are the straight-line consequence of an identity that
is too coarse for what it is being asked.

## Decision

A reservation carries a **token**, minted when it is taken, and only that token
releases it.

```go
type reservation struct {
	account string
	token   uint64
}

func (p *ForwardPolicy) Bind(account Account, host string, port uint32) (uint64, bool)
func (p *ForwardPolicy) Release(token uint64, host string, port uint32)
```

`Bind` refuses if anybody at all already holds the port, including another
session of the same account: one listener can hold a port, and pretending
otherwise is what let a failed attempt speak for the one that had succeeded.
`Release` deletes only on a token match. `Holder` still answers with the account
name, so the `AllowDial` control is unchanged.

**The token is a per-process counter**, incremented under the mutex that already
guards the map, and it is deliberately none of the things it might look like:

- **Not stable per host or per client.** Its scope is one `Bind`/`Release`
  lifecycle and a reconnect gets a fresh one. Stability would recreate the bug
  at finer granularity: a machine's second session would hold a token matching
  its own first session's live reservation, and could release it. The token
  answers "did you create this entry", which is a question about an instance,
  not about an identity. `ClientID` (ADR 0029) is the identity, and is stable
  for exactly the opposite reason: volumes must be found again.
- **Not unguessable, and not a secret.** It never leaves the process, is never
  persisted, and is never sent to a client. The only code that can present one
  is the code `Bind` handed it to. A counter is sufficient; making it random
  would imply a boundary that is not there.
- **Never zero.** It is pre-incremented, so the first is 1, and `Bind` returns
  0 when it refuses. A caller that released without checking whether it got a
  reservation therefore cannot match anybody's entry. That is load-bearing:
  the failure path in `handleForwardRequest` is exactly where the original bug
  lived.

An agent restart resets the counter, but it empties the map too, so no old
token can collide with anything.

## Consequences

The failure path can release safely, which is what it was added to do
(threat-model finding D), without being able to release anything else (finding
E).

A second session of one account is now refused at the policy layer rather than
by the operating system, so the log names the holder instead of reporting
EADDRINUSE from inside a netns. Two machines still cannot share one port; what
lets them work at once is a port each (ADR 0029), not a shared reservation.

One case gets worse before ADR 0029 lands: a client reconnecting before its
previous connection has finished tearing down is refused, where the old
same-account tolerance would have let it through. That tolerance was not a
feature — it let the new session take a port its own live listener was still
using, which breaks the mounts on the containers already running against it.
Being refused is the honest answer, and the per-client port makes the question
stop arising.

## Verification

`TestAFailedBindDoesNotReleaseTheLiveHolder` walks the reported sequence and
ends at the other account's dial being refused, which is the security claim
rather than the bookkeeping one. `TestAStaleTokenReleasesNothing` covers the
late-ending connection, and `TestTheZeroTokenReleasesNothing` covers the
unchecked release.

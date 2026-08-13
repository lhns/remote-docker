# 0034 — SSH inside a WebSocket, so a reverse proxy can front a workspace

- Status: Accepted; extends [ADR 0030](0030-a-core-module-for-the-tunnel.md)
- Date: 2026-08-14

> SSH is the session. A WebSocket is one way to carry it, and the agent never
> terminates TLS.

## What forced it

Reaching a workspace meant an open SSH port. On a network that allows little but
443, behind a reverse proxy that speaks only HTTP, or anywhere a port cannot be
published, that is the thing that makes a workspace hard to get to — and the
project exists to make one easy to use.

## Why SSH was not replaced

Replacing it with mTLS and a stream multiplexer is the obvious alternative, and
it costs everything the transport already provides:

- the **reverse forward** that carries the NFS export, which is the whole of how
  a bind mount works (ADR 0003)
- **`direct-tcpip`** for the workspace daemon's socket and for published
  container ports
- **public-key authentication**, whose digest IS `workspace.ClientID` — the
  identity a machine cannot claim because it is derived from the key the agent
  already authenticated (ADR 0029)
- a **stock `ssh` still getting a shell**, which CI pins

All of that would have to be rebuilt to arrive at what SSH already does. So the
WebSocket carries SSH rather than replacing it, and everything above the
transport is untouched.

Two hooks were enough, and both follow ADR 0030's rule that the transport is
handed its decisions rather than making them: `tunnelclient.Config` takes a
`Dial` function where it dialled TCP itself, and the SSH server accepts from a
second listener.

## The decision

**Optional, additional, and never a replacement.** Both listeners run by default
(`--addr :2222`, `--ws-addr :2280`), and `--ws-addr ""` turns the WebSocket off.
Off by default would mean every existing workspace needed a redeploy before a
client could reach it through a proxy, which is most of the difficulty this
removes.

**The agent serves `ws` and never terminates TLS.** No certificate paths, no
renewal, no expiry. An agent that owned a certificate would eventually present
an expired one, and that presents as a workspace being unreachable for a reason
nothing on screen names. The proxy terminates TLS and is already good at it.

Serving plaintext is not the weakness it appears to be: **the same SSH handshake
runs inside, so this door has the same lock as the TCP one.** The proxy is there
for reachability and to share 443, not to protect a weak endpoint. Closing the
SSH port afterwards is the operator's choice and not ours to make.

**The scheme lives on the client's `host`** — `wss://ws.example/tunnel` — so
there is one setting for where a workspace is rather than two that can disagree.
A bare host still means SSH on 2222, so nothing already configured changes.

**Certificates verify against the system roots**, with a CA file for a private
one and `--insecure` per workspace for a self-signed proxy. `--insecure` gives
up knowing WHICH front door answered and nothing else: the host key still proves
this is the workspace, the client key still proves which machine is calling, and
a hostile proxy sees ciphertext it can neither read nor forge. There is no
trust-on-first-use, for the reason ADR 0030 gives about host keys: every default
is either a prompt nobody is there to answer or an acceptance of anybody.

## Consequences

- **A WebSocket carries its own liveness, and this is the part not to lose.**
  `sshd.armDeadPeerDetection` works on a `*net.TCPConn`; what arrives through a
  proxy is a WebSocket wrapping one, so none of it applies. Without a ping, a
  client that vanishes keeps its reverse-tunnel port reserved and the symptom is
  not a lost connection but a REFUSED FORWARD on some later reconnect, with
  containers mounting against a port bound to nothing. `wslisten` pings on the
  same 60s budget, which also keeps the tunnel alive through a proxy's idle
  timeout.
- **`ctx.RemoteAddr()` becomes the proxy's address** for a WebSocket session.
  It is a log field and must not become anything else; authentication is by key
  and is unaffected.
- **One more dependency**, `github.com/coder/websocket`, chosen over hand-rolling
  RFC 6455 because a framing bug in an SSH stream is corruption rather than a
  crash. It brings no transitive dependencies of its own (`go list -m all` in
  `agent/`, checked 2026-08-14), and takes the agent from 6 direct requires to 7.
- **Two ways in means two things to secure.** The SSH port stays open unless the
  operator closes it, so this adds a surface rather than moving one.
- **A machine workspace cannot use it.** A machine is told its address at boot
  and reached over ssh (ADR 0026), so a WebSocket host for one is refused rather
  than half-honoured.

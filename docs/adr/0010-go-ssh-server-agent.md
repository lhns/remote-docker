# 0010. A Go SSH server agent, not sshd and sudo

- Status: Accepted
- Date: 2026-08-07

## Context

The workspace server is currently OpenSSH plus a collection of shell programs:
`key-watcher` provisions a unix account per public key, `workspace-info` prints
the account's parameters as `KEY=VALUE` text for the client to parse, and
`workspace-mount` / `workspace-umount` perform the mount under a `sudoers`
policy.

Most of that machinery exists to work *around* the fact that the server is a
shell. `sudoers.workspace` must pin argument forms — `workspace-mount ""` and
`workspace-mount --force` — because a bare command entry would permit any
arguments; this is noted in the codebase as the usual way such rules go wrong.
The mount helpers must derive everything from `$SUDO_USER` and accept no path
argument, because an argument is how one user would mount into another's home.
`workspace-info` must serialise to text because there is no other way to return
a value.

The port-ownership problem is the clearest case. Each account's reverse-tunnel
port is a function of its uid, but sshd does not know that. Any account granted
`port-forwarding` can bind any loopback port inside the container, including
one belonging to another user who has not connected yet — and then serve them a
filesystem of the attacker's choosing. Enforcing this under sshd means
generating a `permitlisten="127.0.0.1:<port>"` option into every key's
`authorized_keys` entry, correctly, every time, forever. The policy is one
comparison; the implementation is string generation in a file format with no
schema.

## Decision

Replace sshd and the shell helpers with a single Go binary, `remote-dockerd`,
running as PID 1 in the workspace container. It embeds an SSH server
(`gliderlabs/ssh` over `golang.org/x/crypto/ssh`) implementing `session`,
`exec`, `tcpip-forward` and `direct-tcpip`.

The client is unaffected. It speaks standard SSH, so which implementation
answers is invisible to it — which is what makes this a substitution rather
than a rewrite.

| today | with the agent |
|---|---|
| `sudoers.workspace` with pinned argument forms | gone; the agent is already root |
| helpers deriving state from `$SUDO_USER` | a method on the authenticated session |
| `workspace-info` as text the client parses | a typed value |
| `key-watcher` + `authorized_keys` files | in-process public-key auth |
| generated `permitlisten` strings | `if !mapping.OwnsPort(uid, port) { reject }` |

## Consequences

- **Port ownership becomes structural.** The agent owns the listener, so a
  cross-user bind is refused by construction rather than by an option string
  that has to be generated correctly. `Mapping.OwnsPort` is the entire policy
  and it is unit-tested.
- The whole `sudo` surface disappears — no `sudoers` file, no argument pinning,
  no `$SUDO_USER` derivation, and none of the invariants that guarded them.
- Server behaviour becomes testable without root. `test/key-watcher.sh` needs
  real `useradd` and therefore real privileges, which is why it cannot run in
  ordinary CI; account provisioning behind an interface can be faked in unit
  tests and exercised for real only in integration.
- **We give up a hardened, audited SSH implementation and own authentication
  ourselves.** This is the real cost of the decision and it should not be
  glossed. `gliderlabs/ssh` is widely deployed for exactly this shape of
  problem — Gitea and Soft Serve among others — which is the basis for accepting
  it, but it is a smaller and less-attacked codebase than OpenSSH.
- The agent must implement a PTY session to keep `remote-docker shell` working.
  The library supports it and `bash` is in the image.
- Behaviour that must survive the rewrite, because it was learned the hard way:
  **poll the keys directory as well as watching it** — inotify never fires for
  changes made on another host when that directory is CephFS- or NFS-backed;
  and **revoke, do not delete** — removing a `.pub` empties access but keeps the
  account and its home directory, because auto-deleting users is a silent way
  to lose data.
- Sequencing follows from the substitution property: the client is built and
  proven against stock sshd first, and the integration suite written against it
  becomes the agent's conformance test. The agent is not done until it passes
  tests written before it existed.

## What it actually cost

Written after building it, because the estimate above was a guess.

**The client did not change at all.** Not one line. The agent speaks the same
SSH, answers `workspace-info` from the same `pkg/workspace` type the client
parses with, and keeps the `docker system dial-stdio` command name even though
there is no CLI behind it any more — the name is now a protocol constant rather
than a command, and keeping it is what made the substitution free.

**Roughly 1,000 lines replaced roughly 350 lines of shell.** That is the honest
number and it is not flattering on its own. What it buys:

- account provisioning is unit tested, where the shell suite needed root and
  therefore never ran in CI at all
- the collision bug is fixed rather than documented — two files deriving the
  same account name used to mean one silently overwrote the other's access
- dockerd is supervised, where the entrypoint started it and never looked again
- port ownership is a comparison rather than generated `authorized_keys` text

**The port-ownership claim is now measured.** It was the whole justification
for this record and had never been tested in either implementation. The
integration suite enrols a second account and has it ask for the first
account's reverse port; the suite runs against both servers, so sshd's
`permitlisten` and the agent's comparison are held to the same assertion.

**What the trade actually looks like in practice.** Giving up OpenSSH is real,
and the mitigation is narrower than "gliderlabs is widely deployed": this
server exposes public-key authentication and two channel types, and everything
behind it drops privilege to the authenticated account exactly as sshd would.
It is a small surface. It is still a surface OpenSSH had already hardened, and
that has not changed.

**One thing did not survive intact.** The agent's session handling is
Linux-only — dropping privilege needs `syscall.Credential` — so it is
build-tagged, and a lint or vet run on a non-Linux development machine does not
see it. That cost a CI round trip before it was noticed, and the fix is a
documented `GOOS=linux` pass rather than anything structural.

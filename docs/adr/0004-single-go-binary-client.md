# 0004. A single Go binary with SSH and NFS embedded

- Status: Accepted
- Date: 2026-08-07

## Context

The original clients (`client/dockerbox.ps1`, `client/dockerbox.sh`) orchestrate
three external programs: `ssh.exe` for the tunnel, `rclone.exe` for the NFS
server — downloaded from the internet on first use — and a locally installed
`docker` CLI. The premise of the project is a machine that cannot have software
installed, so every one of those is a problem to be solved rather than a
dependency to be assumed.

Shelling out also costs correctness. Each process boundary is a layer of
quoting, which the clients work around by base64-encoding every remote snippet.
Errors arrive as exit codes and stderr text rather than as values. And the two
implementations must be kept behaviourally identical by hand — they are not,
today: the POSIX client accepts `-L` only before the subcommand, and the
PowerShell client's stderr redirection is unsafe under its own strict-mode
settings.

Capability diverges too. Connection multiplexing is a large win — one handshake
instead of one per command — but `ControlMaster` is a feature of the OpenSSH
client, and Win32-OpenSSH does not implement it. So the POSIX client is fast
and the Windows client is slow, permanently, for a reason neither can fix.

## Decision

Replace both clients with **one Go binary** that embeds the SSH client and the
NFS server as libraries.

- SSH: `golang.org/x/crypto/ssh`. `client.Listen` replaces `-R`,
  `client.Dial` replaces `-L`, `client.NewSession` replaces remote exec, and
  `x/crypto/ssh/knownhosts` replaces `StrictHostKeyChecking`.
- NFS: `github.com/willscott/go-nfs` — the same library rclone's `serve nfs` is
  built on, used directly.

## Consequences

- One artifact to deliver, cross-compiled for `windows|linux|darwin ×
  amd64|arm64` with `CGO_ENABLED=0`. Nothing is downloaded at runtime.
- **Multiplexing becomes inherent.** One `ssh.Client` carries many channels on
  every platform, so the `ControlMaster` split disappears rather than being
  worked around.
- One implementation means one set of behaviours to test, and platform
  differences become explicit parameters rather than parallel source files.
- Errors are values. The base64 wrapping, the exit-code plumbing and the stderr
  parsing all go away.
- Owning the NFS server removes a documented limitation: rclone's `--uid`,
  `--gid` and `--umask` are unsupported on Windows, so files always appeared as
  uid 1000 and `chown` inside a container failed. Serving the filesystem
  ourselves means attributes can be synthesised for the account that will read
  them.
- We take on the maintenance of an NFS server and an SSH client, where before
  we consumed two mature programs. `go-nfs` is a smaller and less-exercised
  codebase than rclone; ADR 0009's fallback reasoning applies here too, and a
  spike precedes the commitment.
- The old clients are not deleted until the Go client passes the integration
  suite. Until then they are the only thing that works.

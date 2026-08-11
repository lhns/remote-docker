# 0025. The agent as a guest on a machine it does not own

- Status: Accepted; extends [ADR 0010](0010-go-ssh-server-agent.md)
- Date: 2026-08-11

## Context

The workspace has always been a Docker-in-Docker container, and the agent is
written as though it owns the machine: it starts a dockerd, creates unix
accounts, and assumes the passwd file, the network namespace and the init
system are all its own. Inside a container that is true and costs nothing.

Somebody wanting to run the workspace directly on a VM is not asking for a
different product. They are asking for the same agent to behave as a guest:
there is already a dockerd started by systemd, already users in the passwd
file, already an init system, and the machine may be doing other things.

Most of that already worked and nobody knew, which is its own problem.
`WORKSPACE_ENABLE_DIND=false` has skipped supervising a dockerd since the agent
was written, the agent only ever shells out to `docker` from PATH, and both
daemon modes read one switch that a VM obeys unchanged. What was missing was
everything around the code: no agent binary in any release, no unit file, no
statement of what the machine has to provide, nothing that had ever run the
agent outside a container, and a threat model that describes a container.

## Decision

**The same agent, the same switches, a second deployment shape.** No VM mode,
no `if onAVM` anywhere. Two things move from the image to the operator:

- **starting dockerd.** `WORKSPACE_ENABLE_DIND=false`, which already existed.
- **the NFS client, in shared-daemon mode only.** This is the asymmetry worth
  knowing: with a daemon per account (the default, ADR 0019) the NFS mount
  happens inside `docker:dind`, which ships `nfs-utils`; in shared mode
  (ADR 0012) this machine's own dockerd mounts, so this machine needs
  `nfs-common` and a kernel with NFS client support.

**Shipped as a binary and a unit file**, not a package. `deploy/remote-dockerd.service`
runs it as root under systemd, and the release carries `remote-dockerd_<v>_linux_<arch>.tar.gz`
separately from the client's archive so that an operator setting up a workspace
does not download 70MB of Docker CLI to do it.

Its own goreleaser build block with `dir: agent`, which is cheap for the reason
ADR 0021 predicted: the agent's module is 24 lines of go.sum against the
client's 786.

**No tini.** The image needs it because the agent is PID 1 there and has to reap
the processes it forks per session. Under systemd those reparent to init.

## Consequences

- **Enrolled accounts are real users on a real machine.** In a container they
  are disposable; here they persist, own files on the machine's own disks, and
  sit in the same passwd file as its service accounts. That is what the unix
  account prefix and the uid-adoption rule are for, and they are a decision of
  their own rather than a detail of this one.
- **ADR 0019's sentence gets harder.** A per-account dind is separation, not
  isolation: each daemon runs privileged, so a determined account can break
  out. In a container it breaks out into the workspace container. On a VM it
  breaks out onto **the VM** -- its files, its other services, its network
  position. Nothing about the code changed; the blast radius did.
- **Shared mode is a different bargain here.** Sharing a dind's daemon means
  accounts see each other's containers. Sharing a VM's means they also see
  whatever else that machine runs, and can stop it.
- **The agent is now testable in CI in a way it never was.** The ubuntu runner
  *is* a VM with docker on it, so running the agent natively is available in a
  way macOS and Android are not. Until `test/vm.sh` exists this record is a
  design and not a proven one.
- **Two deployment shapes to keep working**, and the image is still the one
  that CI exercises most. The switches are shared rather than parallel, which
  is what keeps this from becoming two implementations -- the same argument
  ADR 0020 makes about daemon targets.

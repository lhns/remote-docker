# 0041 — The workspace's own paths are the ones it mounted into the daemon

- Status: Accepted; extends [ADR 0006](0006-per-bind-nfs-volumes.md) and
  [ADR 0019](0019-a-dockerd-per-account.md)
- Date: 2026-08-26
- Current answer: a bind source at or under a path from `WORKSPACE_DIND_MOUNTS`
  is left alone for the workspace's daemon to resolve. Nothing new to configure,
  and a source this machine has still wins.

## What forced it

`kind` runs a Kubernetes node as a container: systemd, containerd and kubelet
inside it. kubelet and kube-proxy load kernel modules, which belong to the
running kernel and live in `/lib/modules/$(uname -r)` on the machine whose
kernel it is — so kind hardcodes `--volume /lib/modules:/lib/modules:ro` into
every node it creates.

One string, two meanings that cannot both hold:

| who | `-v /lib/modules:…` means |
|---|---|
| kind | the daemon's own filesystem, correct by construction with a local daemon |
| this client | the user's filesystem, exported over NFS (ADR 0003, ADR 0006) |

So `IsLocalPath` claimed it, the registry stat'd it on the CLIENT, and a Windows
machine has no `/lib/modules`. Creating `C:\lib\modules` to silence the error
delivered an EMPTY tree to the node and systemd died behind a mount that had
succeeded — which is the proof that the client resolved it.

**Nothing else could intervene.** kind's flags never pass through a shell, so
they cannot be spelled differently, and ADR 0040's argv repair never sees them.

## The decision

**Derived from `WORKSPACE_DIND_MOUNTS`, never configured twice.**

That is the definition rather than a shortcut: a per-account daemon's filesystem
is the dind image, its socket directory, its graph volume and these mounts — and
only the last come from outside, so they are exactly the paths that are both
meaningful (a real modules tree, not the dind image's own `/etc`) and deliberate.

**Which side is published depends on which daemon resolves the bind**, decided
once in the agent:

| mode | the daemon has | why |
|---|---|---|
| per-account | the **destination** | the account's dind is where the mount lands |
| shared | the **source** | no dind, so the workspace's own dockerd sees the path as it exists in this container |

Identical for the usual `/lib/modules:/lib/modules:ro`; the difference between
working and silently wrong for anyone who remaps. In shared mode a remap cannot
be honoured at all, and is warned about at startup rather than discovered from a
container.

**Published through `workspace-info`** as `WORKSPACE_DAEMON_PATHS`, which
already carries `WORKSPACE_*` keys and tolerates unknown ones. No new protocol
command, no round trip per bind, and an agent that predates the key sends
nothing — which reads as "none" and is exactly the old behaviour.

**The client never learns the mode.** It matches sources against a list and
branches on nothing, which is the rule that removed the nine `if Daemons != nil`
branches (ADR 0019).

Rules at the use site:

- **A typo still fails.** `/hme/me/project` matches no declared path, so it is
  exported and errors as before. This is what makes the rule safe where a blanket
  passthrough is not.
- **This machine wins when both could claim it**, so a Linux client's own `/etc`
  is still its own. The list is consulted only for a source that is not here.
- **A passed-through bind is not touched at all**, so `ro` and every other option
  survive by construction.

**A missing source is refused at startup.** docker CREATES a missing bind source,
so `WORKSPACE_DIND_MOUNTS=/typo:/lib/modules` would hand the daemon an empty
directory and the failure would surface inside somebody's container with nothing
naming the setting. `ParseMounts` stays pure — the package is written to be
testable without a workspace — and `MissingSources` asks the filesystem once, at
startup.

## What was rejected

- **Passing every unresolvable source through.** A typo stops failing and becomes
  an empty directory the daemon creates: a container that starts with nothing
  where the project should be, which is the failure class this codebase spends
  its guards on.
- **Probing the workspace for whether a path exists.** Existing is not the same
  as being meant for this, and it costs a round trip and a protocol command.
- **A second variable naming bindable paths.** Its one advantage would be
  separating "the daemon has it" from "clients may bind it", which is worth
  little: an account is root inside its own daemon and can read anything mounted
  there. Against it, drift — it would always list the same paths, and an operator
  who updated one and forgot the other would get exactly the failure this record
  removes. One fact, one place, as with the uid→port formula.

## Consequences

- **One mount now carries two meanings**: the daemon has the path, and clients
  may bind it. Benign for the reason above.
- **`WORKSPACE_DIND_MOUNTS` is parsed in shared-daemon mode**, where it was
  ignored. It still mounts nothing there, but it declares — and a malformed value
  that was silently skipped before now fails at startup naming the variable.
- **The published side differs by mode**, invisible until somebody remaps a mount
  and then decides whether it works.
- **This removes OUR blocker for kind and promises nothing more.** kind-in-dind
  still has to satisfy cgroup delegation, a writable `/sys/fs/cgroup` and systemd
  as PID 1 inside a container that is itself inside a privileged dind. Never
  claim it works without having run it; record what an attempt found.
